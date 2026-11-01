// llm-request-tollgate: 单二进制 OpenAI 兼容代理 (设计与行为见 docs/)。
//
// 一个进程里做这些事:
//  1. 别名表 (白名单 + 路由): 逻辑模型名 -> [(上游, 真实模型 id), ...];
//     未命中的模型名直接返 404, 不猜上游。
//  2. 按上游分组的调度: 每个上游一条流量队列 (FIFO, 只保证出队节拍) + 自适应并发上限
//     (起点 1, 试探成功 +1), 出队后进目标模型的队列 (不等节拍, 只在熔断锁 / 退避时才
//     等); 被挡下而没发出去的请求返 429 带 Retry-After。
//  3. 手动转发: 认证头替换, 删 Accept-Encoding 让 Transport 协商并透明解压,
//     不加 X-Forwarded-For。
//  4. 用量提取: 强写 stream_options.include_usage, 流末 usage chunk / 非流式响应体
//     usage; 缺 usage 按字符口径兜底并计数。
//  5. 限制反应: 上游报错按每个上游自己的规则分成并发 / 额度 / 其他三类; 额度按「记账组」
//     共享, 命中只锁该组 10min (一个上游可以有多组, 组由模型的 billing_group 声明,
//     不写 = 默认组); 其他按 10/20/30s 退避、连续 4 次熔断, 熔断只锁出错的 上游/模型
//     5min (同家别的模型不受影响), 锁与退避状态落 var/gate-state.json (重启仍生效)。
//     服务不记账, 只管写日志。
//  6. 响应整形: 非流 / 流式 chunk 里的 model 字段改回请求的逻辑名。
//  7. 对外面: GET /v1/models 返回逻辑名清单; 日志记 source 标识; -summary 汇总命令;
//     -check 配置自检 + deploy/ 常驻与自检脚本。
//  8. 集中 fallback: 候选按序尝试, 首字节前只有 200 算成功; 其余状态码与传输失败都进
//     候选分支, 按错误规则上锁或退避, 全败透传最后错误。
//  9. 定时锁: 熔断只锁出错的 上游/模型, 额度只锁 上游/记账组, 低流量 (窗口内解码吞吐的
//     峰值与平均都低于门槛) 也只锁该 上游/模型; 锁内不真发, 排队中的请求整队迁移到下一个
//     候选。
//  10. 探测与延迟观测 (只观测, 不接入调度): 真实请求被动记 ttfb_ms/tps; 开启 probe 后
//     只主动探测 fallback 顺序里的下一个候选 (scope=next), 结果进 GET /v1/probe 与
//     var/probe.json; 不改候选顺序、不动失败计数与锁, 已建立的流不迁移。
//
// 配置: config.local.json (gitignored, 指向私有配置); SIGHUP 热加载别名与上游。
// key: 支持 key_env 字段只存环境变量名, 真 key 启动/热加载时从环境变量取, 不落盘。
// 日志: JSONL 追加写, 逐笔明细; 按本地日历天轮转, 跨天归档为
// <base>-<YYYY-MM-DD>.jsonl, -summary 与 -usage 都能跨归档读。
//
// 代码按职责分文件: main/config 入口与配置, errclass 报错分类, gate* 调度与锁,
// serve* 转发主链路, observe* 观测面, probe* 探测, log/usage 日志与用量, rate 吞吐口径,
// summary 汇总命令, tui* 状态台, tui_gate/tui_store/tui_settings*/tui_prio* 参数编辑器。
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const (
	// 客户端打在代理上的路径。
	clientPath = "/v1/chat/completions"
	// 上游路径: base_url 自带 /v1 前缀, 不能再拼一次。
	upstreamPath = "/chat/completions"
	// /v1/responses: 与 chat/completions 并列的第二条转发路径, 共用除路径外
	// 的全部链路。该接口没有 stream_options (不需要 include_usage 注入), 用量在
	// 响应体或 response.completed 事件里给出。
	clientPathResponses   = "/v1/responses"
	upstreamPathResponses = "/responses"
)

// defaultTuiStateURL 是 -tui-state 不带地址时观测的常驻服务 (与示例配置的 listen 一致)。
const defaultTuiStateURL = "http://127.0.0.1:8787"

// tuiStateArg 是 -tui-state 的值: 记住这个选项有没有出现过 (裸写经 withTuiStateDefault
// 补成默认地址), 好让 -tui 与它互斥。
type tuiStateArg struct {
	url string
	set bool
}

func (a *tuiStateArg) String() string { return a.url }

func (a *tuiStateArg) Set(s string) error {
	a.url, a.set = s, true
	return nil
}

// withTuiStateDefault 给只写 `-tui-state` (不带值) 的写法补上默认地址, 免得 flag 包把它
// 当成缺参数报错; `-tui-state <base>` 与 `-tui-state=<base>` 照常保留。`--` 之后是位置
// 参数, 不再改写。
func withTuiStateDefault(args []string, defURL string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "-tui-state" || a == "--tui-state" {
			next := ""
			if i+1 < len(args) {
				next = args[i+1]
			}
			if next == "" || strings.HasPrefix(next, "-") {
				a += "=" + defURL
			}
		}
		out = append(out, a)
	}
	return out
}

// tuiTarget 解析状态台的两种入口: -tui 同进程起代理, -tui-state [base] 看常驻服务
// (不带值用 defURL)。两个选项同时给算用法错误。
func tuiTarget(inProc, stateSet bool, stateURL, defURL string) (bool, string, error) {
	switch {
	case inProc && stateSet:
		return false, "", errors.New("-tui 与 -tui-state 不能同时使用")
	case inProc:
		return true, "", nil
	case stateSet:
		if stateURL == "" {
			stateURL = defURL
		}
		return true, stateURL, nil
	}
	return false, "", nil
}

func main() {
	cfgPath := flag.String("config", "config.local.json", "config file path")
	summary := flag.String("summary", "", "aggregate JSONL request log and exit")
	usage := flag.String("usage", "", "list token usage by upstream/model and window, then exit")
	windows := flag.String("windows", "3h,12h,24h", "windows for -usage, comma separated")
	check := flag.Bool("check", false, "validate config and exit")
	probe := flag.String("probe", "", "print the latency/probe report for a config file and exit")
	tui := flag.Bool("tui", false, "run the terminal dashboard with an in-process proxy (q to quit)")
	var tuiState tuiStateArg
	flag.Var(&tuiState, "tui-state", "run the terminal dashboard against a running proxy (default URL: "+
		defaultTuiStateURL+")")
	flag.CommandLine.Parse(withTuiStateDefault(os.Args[1:], defaultTuiStateURL))

	enabled, baseURL, err := tuiTarget(*tui, tuiState.set, tuiState.url, defaultTuiStateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "用法: -tui (同进程) 或 -tui-state [base] (看常驻服务)")
		os.Exit(2)
	}
	if enabled {
		runTUI(*cfgPath, baseURL)
		return
	}

	if *probe != "" {
		runProbeReport(*probe)
		return
	}

	if *summary != "" {
		runSummary(*summary)
		return
	}
	if *usage != "" {
		var ws []string
		for _, s := range strings.Split(*windows, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ws = append(ws, s)
			}
		}
		runUsage(*usage, ws)
		return
	}
	if *check {
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("config invalid: %v", err)
		}
		log.Printf("config OK: listen=%s log_path=%s upstreams=%d aliases=%d",
			cfg.Listen, cfg.LogPath, len(cfg.Upstreams), len(cfg.Aliases))
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	app, err := newApp(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	// SIGHUP 热加载别名与上游。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		for range ch {
			c, err := loadConfig(*cfgPath)
			if err != nil {
				log.Printf("reload failed: %v", err)
				continue
			}
			app.applyConfig(c)
			log.Printf("config reloaded from %s", *cfgPath)
		}
	}()

	log.Printf("llm-request-tollgate listening on %s (config %s)", cfg.Listen, *cfgPath)
	if err := http.ListenAndServe(cfg.Listen, app); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
