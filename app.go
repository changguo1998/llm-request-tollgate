// app.go: App 结构与装配 (热加载、连接池)。

package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type App struct {
	mu        sync.RWMutex
	cfg       *Config
	upstreams map[string]*UpstreamCfg
	// groups 是「上游 -> 真实模型 -> 额度组名」的索引 (applyConfig 重建), 探测等热路径
	// 据此把 (上游, 模型) 解析成组, 不必每次扫 models 表。
	groups map[string]map[string]string
	// groupOrder 是「上游 -> 组名列表」, 按配置里首次出现的顺序 (含默认组的空串),
	// 状态台用它铺记账列。
	groupOrder map[string][]string
	aliases    map[string][]aliasTarget
	// 调度: 每个上游一份队列与自适应并发上限, 门控参数对所有上游共用。
	cands     map[string]*cand
	gate      GateCfg
	transport *http.Transport
	streamIdl time.Duration

	gatePath string // 定时锁状态文件 (var/gate-state.json)

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
	logDay string // 当前日志文件所属的本地日期 (按天轮转用)

	probeMu   sync.Mutex
	probe     map[string]*probeStat // "上游|模型" -> 滚动统计 (主动探测 + 被动观测)
	probePath string                // 指标状态文件 (var/probe.json)

	// 只读观测面 (TUI): 进行中的请求、最近事件、事件序号。纯内存、不落盘、不参与记账,
	// 服务重启即清空 —— 明细的事实来源仍是日志。
	flowMu   sync.Mutex
	flows    map[uint64]*flow
	nextFlow uint64
	evMu     sync.Mutex
	events   [viewEvents]viewEvent
	evNext   uint64 // 累计事件序号 (TUI 拿它当起点, 只取新增的那部分)
}

type aliasTarget struct {
	Upstream string
	Model    string
	Group    string // 该模型在上游 models 里声明的额度组 (空 = 默认组)
}

func newApp(c *Config) (*App, error) {
	a := &App{
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		cands:     map[string]*cand{},
		probe:     map[string]*probeStat{},
		flows:     map[uint64]*flow{},
	}
	a.applyConfig(c)
	if err := a.openLog(); err != nil {
		return nil, err
	}
	// 锁状态放在日志同目录 (var/), 不进仓库。
	if c.LogPath != "" {
		a.gatePath = filepath.Join(filepath.Dir(c.LogPath), "gate-state.json")
	}
	a.loadGateState()
	// 探测状态与锁状态同目录; 未配置 probe 时 probePath 为空, 一切探测都停用。
	a.probePath = probePathFor(c)
	a.loadProbeState()
	a.startProbeLoop()
	return a, nil
}

// applyConfig 重建上游表与别名表; 限速器按上游 id 保留, 不因热加载被重置。
func (a *App) applyConfig(c *Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = c
	a.streamIdl = time.Duration(c.Timeouts.StreamIdleMs) * time.Millisecond
	// 门控参数是全局的, 热加载直接生效; 队列与自适应上限按上游 id 保留 (爬了半天的
	// 上限不该被 hup 清掉, 排队中的请求也不该被丢)。
	a.gate = normGate(c.Gate)
	// 只做报告的入口 (如 -probe) 会自己拼一个 App, 这里的表可能还没建。
	if a.cands == nil {
		a.cands = map[string]*cand{}
	}
	upstreams := map[string]*UpstreamCfg{}
	groups := map[string]map[string]string{} // 上游 -> 真实模型 -> 额度组名
	groupSeq := map[string][]string{}        // 上游 -> 组名 (按配置首次出现, 含默认组)
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		upstreams[u.ID] = u
		byModel := make(map[string]string, len(u.Models))
		var names []string
		seenGroup := map[string]bool{}
		for _, m := range u.Models {
			byModel[m.ID] = m.BillingGroup
			if !seenGroup[m.BillingGroup] {
				seenGroup[m.BillingGroup] = true
				names = append(names, m.BillingGroup)
			}
		}
		if len(names) == 0 {
			names = []string{defaultGroup} // 不写 models 的上游: 只有一个默认组
		}
		groupSeq[u.ID] = names
		groups[u.ID] = byModel
		// 调度状态按上游 id 保留不重建: 队列里排着的请求、爬到的并发上限、退避与锁
		// 都该跨热加载延续 (上限有意不落盘, 但 hup 不该把它清掉)。
		if _, ok := a.cands[u.ID]; !ok {
			a.cands[u.ID] = newCand(u.ID)
		}
	}
	a.upstreams = upstreams
	a.groups = groups
	a.groupOrder = groupSeq
	aliases := map[string][]aliasTarget{}
	for name, list := range c.Aliases {
		for _, pair := range list {
			aliases[name] = append(aliases[name], aliasTarget{
				Upstream: pair[0], Model: pair[1], Group: groups[pair[0]][pair[1]],
			})
		}
	}
	a.aliases = aliases
	// 超时参数启动时一次性固化为 Transport (热加载不重建连接池)。
	if a.transport == nil {
		connect := time.Duration(c.Timeouts.ConnectMs) * time.Millisecond
		a.transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := net.Dialer{Timeout: connect}
				conn, err := d.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				// 每次读都重设 idle 死线, 实现流空闲超时。
				return &idleConn{Conn: conn, idle: a.streamIdl}, nil
			},
			ResponseHeaderTimeout: time.Duration(c.Timeouts.ResponseHeaderMs) * time.Millisecond,
		}
	}
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}
