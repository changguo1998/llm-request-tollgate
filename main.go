// llm-request-tollgate: 自研单文件 OpenAI 兼容代理 (设计文档 docs/design.md)。
//
// 单进程内含九件事 (设计 §4.1):
//  1. 别名表 (白名单 + 路由): 逻辑模型名 -> [(上游, 真实模型 id), ...],
//     未命中的模型名直接返 404, 不猜上游 (D2)。
//  2. 按上游分组 (v1 一个上游一把 key, "上游+凭据" 等价于上游记录) 的
//     令牌桶 (速率 + 突发) 与信号量 (并发), 桶空排队, 超 queue_wait_ms 返
//     429 带 Retry-After (D3, F3)。
//  3. 手动转发: 认证头替换 (D1), 删 Accept-Encoding 让 Transport 协商并
//     透明解压 (设计 §4.9.1), 不加 X-Forwarded-For (F9)。
//  4. 用量提取: 强写 stream_options.include_usage, 流末 usage chunk / 非流式
//     响应体 usage; 缺 usage 按 F7 字符口径兜底并计数。
//  5. 预算自适应 (D12/D13): 定时扫日志重建窗口 token 累计; 不拦截请求, 而是把上游
//     的 429 当限额信号: 该上游 (provider+key) 进冷却, 跨候选切下一个模型, 并据
//     撞墙时的窗口累计反推折算系数 (落 var/budget-units.json)。
//  6. 响应整形: 非流 / 流式 chunk 里的 model 字段改回请求的逻辑名 (O9)。
//  7. 对外面: GET /v1/models 返回逻辑名清单 (O5); 日志记 source 标识 (O3);
//     -summary 汇总命令 (O2); -check 配置自检 + deploy/ 常驻与自检脚本 (O1)。
//  8. 集中 fallback: 候选按序尝试, 首字节前 429/5xx/传输失败换下一个, 全败透传最后错误。
//  9. 熔断: 连续失败达阈值短暂跳过该上游 (冷却期内不真发), 冷却后自动恢复。
//  10. 探测与延迟观测 (D14, 只观测不接入): 真实请求被动记 ttfb_ms/tps; 开启 probe 后
//     只主动探测 fallback 顺序里的下一个候选 (scope=next), 结果进 GET /v1/probe 与
//     var/probe.json; 不改候选顺序、不动冷却/熔断/预算, 已建立的流不迁移。
//
// 配置: config.local.json (gitignored, 软链到 ~/fff/config), SIGHUP 热加载别名与上游 (F1);
// key: 支持 key_env 字段只存环境变量名, 真 key 启动/热加载时从环境变量取, 不落盘。
// 日志: JSONL 追加写, 逐笔明细 (F5, F8); 按本地日历天轮转, 跨天归档为
// <base>-<YYYY-MM-DD>.jsonl, 预算聚合与 -summary 都能跨归档读。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// 客户端打在代理上的路径 (D4)。
	clientPath = "/v1/chat/completions"
	// 上游路径: base_url 自带 /v1 前缀 (设计 §4.8), 不能再拼一次。
	upstreamPath = "/chat/completions"
	// /v1/responses (O5): 与 chat/completions 并列的第二条转发路径, 共用除路径外
	// 的全部链路。该接口没有 stream_options (不需要 include_usage 注入), 用量在
	// 响应体或 response.completed 事件里给出。
	clientPathResponses   = "/v1/responses"
	upstreamPathResponses = "/responses"
)

// ---------- 配置 ----------

type Timeouts struct {
	ConnectMs        int `json:"connect_ms"`
	ResponseHeaderMs int `json:"response_header_ms"`
	StreamIdleMs     int `json:"stream_idle_ms"`
	QueueWaitMs      int `json:"queue_wait_ms"`
}

type Limits struct {
	RatePerSec  float64 `json:"rate_per_sec"`
	Burst       float64 `json:"burst"`
	Concurrency int     `json:"concurrency"`
}

type UpstreamCfg struct {
	ID             string            `json:"id"`
	BaseURL        string            `json:"base_url"`
	AuthHeader     string            `json:"auth_header"`
	Key            string            `json:"key"`
	KeyEnv         string            `json:"key_env,omitempty"`
	KeyPrefix      *string           `json:"key_prefix,omitempty"`
	Headers        map[string]string `json:"headers"`
	IncludeUsage   bool              `json:"include_usage"`
	BreakerFail    int               `json:"breaker_fail,omitempty"`
	BreakerCooldMs int               `json:"breaker_cooldown_ms,omitempty"`
	Limits         Limits            `json:"limits"`
	Budget         *BudgetCfg        `json:"budget,omitempty"`
}

type Config struct {
	Listen    string                 `json:"listen"`
	LogPath   string                 `json:"log_path"`
	Timeouts  Timeouts               `json:"timeouts"`
	Upstreams []UpstreamCfg          `json:"upstreams"`
	Aliases   map[string][][2]string `json:"aliases"`
	Probe     *ProbeCfg              `json:"probe,omitempty"`
}

// ---------- 限速: 令牌桶 + 信号量, 按上游分组 ----------

type Limiter struct {
	rate, burst, tokens float64
	last                time.Time
	sem                 chan struct{}
	cfg                 Limits // 建桶时的配置, 供热加载比对 (改了不会自动生效)
	mu                  sync.Mutex
}

// normLimits 按 newLimiter 的取值规则补齐参数, 供建桶与热加载比对共用。
func normLimits(l Limits) Limits {
	if l.Burst < 1 {
		l.Burst = 1
	}
	if l.Concurrency < 1 {
		l.Concurrency = 1
	}
	l.RatePerSec = math.Max(0, l.RatePerSec)
	return l
}

func newLimiter(l Limits) *Limiter {
	l = normLimits(l)
	return &Limiter{
		rate:   l.RatePerSec,
		burst:  l.Burst,
		tokens: l.Burst,
		sem:    make(chan struct{}, l.Concurrency),
		cfg:    l,
	}
}

// takeToken 在 ctx 超时内拿到一个令牌, 拿不到返回 false。
func (l *Limiter) takeToken(ctx context.Context) bool {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.last.IsZero() {
			l.last = now
		}
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return true
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(need)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

func (l *Limiter) acquireSem(ctx context.Context) bool {
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *Limiter) releaseSem() { <-l.sem }

// ---------- 熔断 ----------

// breaker 是按上游的熔断器: 连续失败达阈值打开, 冷却期内不再请求该上游
// (fallback 直接跳过), 冷却期满自动恢复; 成功后复位。
type breaker struct {
	mu       sync.Mutex
	fails    int
	openAt   time.Time // 零值 = 关闭
	failTh   int
	cooldown time.Duration
}

// normBreakerDur 按 newBreaker 的取值规则补齐参数, 供建熔断器与热加载比对共用。
func normBreakerDur(failTh int, cooldown time.Duration) (int, time.Duration) {
	if failTh < 1 {
		failTh = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return failTh, cooldown
}

func newBreaker(failTh int, cooldown time.Duration) *breaker {
	failTh, cooldown = normBreakerDur(failTh, cooldown)
	return &breaker{failTh: failTh, cooldown: cooldown}
}

// allow 返回此刻是否允许尝试该上游。打开且未过冷却期 -> 拒绝; 其余放行
// (冷却期满即关闭, 下一次尝试当作探测)。
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.openAt.IsZero() {
		if time.Since(b.openAt) < b.cooldown {
			return false
		}
		b.openAt = time.Time{}
		b.fails = 0
	}
	return true
}

// recordFail 记一次失败; 连续失败达阈值即打开熔断。
func (b *breaker) recordFail() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails >= b.failTh {
		b.openAt = time.Now()
		b.fails = 0
	}
}

// recordSuccess 成功选型: 复位失败计数并关闭熔断。
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = 0
	b.openAt = time.Time{}
}

// ---------- 用量 ----------

type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	// Cached 是命中缓存的输入 token (各家字段名不同, 见 openaiUsage);
	// 平台按缓存价计费 (约为输入价 1/10), 系数标定要用到。
	Cached int `json:"cached,omitempty"`
}

// openaiUsage 是上游响应里的原始字段名 (OpenAI 兼容口径)。
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Responses API (/v1/responses) 口径: 同名语义用 input/output_tokens 表达。
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// 四种缓存口径: OpenAI/vLLM 的 prompt_tokens_details.cached_tokens,
	// DeepSeek 的 prompt_cache_hit_tokens, Anthropic 风格的 cache_read_input_tokens,
	// Responses API 的 input_tokens_details.cached_tokens。
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

func toUsage(u *openaiUsage) *Usage {
	if u == nil {
		return nil
	}
	// chat/completions 用 prompt/completion_tokens, Responses API 用 input/output_tokens;
	// 两者归一成同一份 Usage, 下游记账与汇总不必区分接口。
	prompt, completion := u.PromptTokens, u.CompletionTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	if completion == 0 {
		completion = u.OutputTokens
	}
	cached := u.PromptCacheHitTokens
	if cached == 0 {
		cached = u.CacheReadInputTokens
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		cached = u.InputTokensDetails.CachedTokens
	}
	return &Usage{
		Prompt: prompt, Completion: completion,
		Total: u.TotalTokens, Cached: cached,
	}
}

// estimateTokens 按 F7 口径粗估: 非 ASCII 1 token/字符, 其余 4 字符 1 token。
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return other + (ascii+3)/4
}

func extractUsage(buf []byte) *Usage {
	var resp struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return toUsage(resp.Usage)
}

// estimateUsageFromBody 取响应体生成文本, 用于缺 usage 时的 F7 兜底估算。
func estimateUsageFromBody(buf []byte) int {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		// /v1/responses: 生成文本在 output[].content[].text。
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return 0
	}
	n := 0
	for _, c := range resp.Choices {
		n += estimateTokens(c.Message.Content)
	}
	for _, o := range resp.Output {
		for _, c := range o.Content {
			n += estimateTokens(c.Text)
		}
	}
	return n
}

// replaceModelValue 把 data 里值为 raw 的 "model" 字段换成 enc, 其余字节原样保留。
func replaceModelValue(data, raw, enc []byte) []byte {
	key := []byte(`"model":`)
	for from := 0; ; {
		i := bytes.Index(data[from:], key)
		if i < 0 {
			return data
		}
		i += from
		j := i + len(key)
		for j < len(data) && (data[j] == ' ' || data[j] == '\t') {
			j++
		}
		if j+len(raw) <= len(data) && bytes.Equal(data[j:j+len(raw)], raw) {
			out := make([]byte, 0, len(data)+(len(enc)-len(raw)))
			out = append(out, data[:j]...)
			out = append(out, enc...)
			out = append(out, data[j+len(raw):]...)
			return out
		}
		from = i + 1
	}
}

// rewriteModel 把 JSON 里顶层 "model" 字段的值改成 logical, 其余字节原样保留 (O9)。
// 仅处理值为不含量转义的简单字符串, 否则原样返回。
func rewriteModel(data []byte, logical string) []byte {
	if logical == "" {
		return data
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	raw, ok := m["model"]
	if !ok || len(raw) < 2 || raw[0] != '"' || bytes.IndexByte(raw, '\\') >= 0 {
		return data
	}
	var cur string
	if json.Unmarshal(raw, &cur) != nil || cur == logical {
		return data
	}
	enc, _ := json.Marshal(logical)
	return replaceModelValue(data, raw, enc)
}

// rewriteModelKnown 同 rewriteModel, 但用于 /v1/responses 事件里嵌在 response
// 对象下的 model (顶层没有 model 字段可解析): 真实 id 由路由已知, 直接按字面量换。
func rewriteModelKnown(data []byte, real, logical string) []byte {
	if real == "" || logical == "" || real == logical || strings.ContainsAny(real, `\"`) {
		return data
	}
	raw, err := json.Marshal(real)
	if err != nil {
		return data
	}
	enc, _ := json.Marshal(logical)
	return replaceModelValue(data, raw, enc)
}

// ---------- 日志 (JSONL) ----------

type logRec struct {
	Ts        string  `json:"ts"`
	Logical   string  `json:"logical"`
	Upstream  string  `json:"upstream"`
	Model     string  `json:"model"`
	Path      string  `json:"path"`
	Stream    bool    `json:"stream"`
	Status    int     `json:"status"`
	Usage     *Usage  `json:"usage,omitempty"`
	Missing   bool    `json:"missing_usage"`
	Queued    bool    `json:"queued"`
	QueueMs   int64   `json:"queue_ms"`
	Client    string  `json:"client"`
	UA        string  `json:"ua,omitempty"`
	Source    string  `json:"source,omitempty"`
	Fallbacks int     `json:"fallbacks,omitempty"`
	Err       string  `json:"err,omitempty"`
	Gate      string  `json:"gate,omitempty"` // 被代理闸门拒绝时: concurrency | rate
	BudgetHit bool    `json:"budget_hit,omitempty"`
	CoolSkips int     `json:"cooldown_skips,omitempty"`
	TtfbMs    int64   `json:"ttfb_ms,omitempty"` // 首字节时延 (被动采样)
	Tps       float64 `json:"tps,omitempty"`     // 解码吞吐 tokens/s (流式才有)
}

// ---------- 预算闸门 (D12): 定时从日志重建窗口累计 ----------

// BudgetCfg 预算闸门 (D12): 定时从请求日志重建各窗口累计, 满额拒新请求。
// Windows: "3h" -> 限额, ...; Unit 是 token 折算平台额度的系数 (默认 1);
// Units 按真实模型 id 覆盖 Unit (平台各模型扣额倍率不同, 系数用 -usage 标定);
// CooldownMs 是撞 429 后该上游 (provider+key) 的冷却时长 (默认 5 分钟);
// RefreshSec 是定时扫日志的间隔秒数 (默认 60)。
type BudgetCfg struct {
	Windows    map[string]float64 `json:"windows"`
	Unit       float64            `json:"unit,omitempty"`
	Units      map[string]float64 `json:"units,omitempty"`
	CooldownMs int                `json:"cooldown_ms,omitempty"`
	RefreshSec int                `json:"refresh_sec,omitempty"`
}

// ---------- 主动探测 + 被动时延采样 (D14: 只观测, 不接入路由) ----------

// ProbeCfg 探测配置。默认关闭; 开启后只做两件事: ①对 fallback 顺序里的下一个候选
// 发小请求 (scope=next), ②把真实请求的首字节时延/吞吐记成被动样本。
// 结果只进报告 (GET /v1/probe) 与状态文件 (var/probe.json) —— 不改候选顺序,
// 不动冷却/熔断/预算, 也不打断已建立的流 (切换只可能发生在下次请求开始时)。
type ProbeCfg struct {
	Enabled      bool     `json:"enabled,omitempty"`
	Scope        string   `json:"scope,omitempty"`          // next (默认) | all
	IntervalSec  int      `json:"interval_sec,omitempty"`   // 默认 900
	MaxTokens    int      `json:"max_tokens,omitempty"`     // 每次探测的生成上限, 默认 8
	TimeoutMs    int      `json:"timeout_ms,omitempty"`     // 单次探测超时, 默认 30000
	Window       int      `json:"window,omitempty"`         // 每候选保留样本数, 默认 5
	Targets      []string `json:"targets,omitempty"`        // 只探这些别名; 空 = 全部多候选别名
	TargetTtfbMs int      `json:"target_ttfb_ms,omitempty"` // 标准: 首字节时延上限 (仅报告判定)
	TargetTps    float64  `json:"target_tps,omitempty"`     // 标准: 吞吐下限 (仅报告判定)
	SwitchMargin float64  `json:"switch_margin,omitempty"`  // 明显优势阈值 (仅报告判定)
}

// probeSample 一次采样: 主动探测 (Passive=false) 或真实请求 (Passive=true)。
type probeSample struct {
	Ts      string  `json:"ts"`
	TtfbMs  int64   `json:"ttfb_ms"`
	Tps     float64 `json:"tps,omitempty"`
	Tokens  int     `json:"tokens,omitempty"`
	OK      bool    `json:"ok"`
	Err     string  `json:"err,omitempty"`
	Passive bool    `json:"passive,omitempty"`
}

// probeStat 某 (上游, 模型) 的滚动统计: EWMA (α=0.3) + 最近 Window 个样本。
type probeStat struct {
	Upstream   string        `json:"upstream"`
	Model      string        `json:"model"`
	Samples    []probeSample `json:"samples"`
	EwmaTtfbMs float64       `json:"ewma_ttfb_ms"`
	EwmaTps    float64       `json:"ewma_tps"`
	OK         int           `json:"ok"`
	Fail       int           `json:"fail"`
	Active     int           `json:"active"`
	Passive    int           `json:"passive"`
	UpdatedAt  string        `json:"updated_at"`
}

// probeState 状态文件 (与 budget-units.json 同目录, 重启不丢)。
type probeState struct {
	Stats     map[string]probeStat `json:"stats"`
	UpdatedAt string               `json:"updated_at"`
}

// relayOut relayStream 的产出: 用量 + 时延 (首字节与解码段)。
type relayOut struct {
	Usage    *Usage
	Missing  bool
	TtfbMs   int64
	DecodeMs int64
}

// budgetState 持有上次扫描重建的窗口累计: upstreamID -> 窗口key -> 已用(×unit)。
type App struct {
	mu        sync.RWMutex
	cfg       *Config
	upstreams map[string]*UpstreamCfg
	aliases   map[string][]aliasTarget
	limiters  map[string]*Limiter
	breakers  map[string]*breaker
	transport *http.Transport
	streamIdl time.Duration

	budgetMu   sync.Mutex
	budgetRaw  map[string]map[string]float64 // 上游 -> 窗口 -> 原始 token 累计
	budgetCoef map[string]float64            // 上游 -> 撞墙反推的折算系数
	coolUntil  map[string]time.Time          // 上游 -> 冷却截止时间
	statePath  string                        // 系数/冷却状态文件 (var/budget-units.json)

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
	logDay string // 当前日志文件所属的本地日期 (按天轮转用)

	probeMu   sync.Mutex
	probe     map[string]*probeStat // "上游|模型" -> 滚动统计 (主动探测 + 被动观测)
	probePath string                // 指标状态文件 (var/probe.json)
}

// parseBudgetDur 解析 "3h"/"12h"/"90m"/"7d" 这类窗口时长。
func parseBudgetDur(s string) (time.Duration, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch s[i:] {
	case "s":
		return time.Duration(n) * time.Second, true
	case "m", "min":
		return time.Duration(n) * time.Minute, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// refreshBudget 扫一遍请求日志, 为每个配了预算的上游重建各窗口原始 token 累计 (D12)。
// 只统计有 usage 的记录; 冷却已过期的条目顺手清掉。
func (a *App) refreshBudget() {
	a.mu.RLock()
	targets := map[string]*BudgetCfg{}
	for _, u := range a.upstreams {
		if u.Budget != nil && len(u.Budget.Windows) > 0 {
			targets[u.ID] = u.Budget
		}
	}
	logPath := a.cfg.LogPath
	a.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	now := time.Now()
	raw := make(map[string]map[string]float64, len(targets))
	maxWindow := time.Duration(0)
	for id, b := range targets {
		m := make(map[string]float64, len(b.Windows))
		for w := range b.Windows {
			m[w] = 0
			if d, ok := parseBudgetDur(w); ok && d > maxWindow {
				maxWindow = d
			}
		}
		raw[id] = m
	}

	// 跨天时窗口会跨越归档边界, 因此当前日志与最近归档一起读。
	for _, fp := range logFilesForScan(logPath, maxWindow) {
		f, err := os.Open(fp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec logRec
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Usage == nil {
				continue
			}
			if _, ok := targets[rec.Upstream]; !ok {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, rec.Ts)
			if err != nil || t.After(now) {
				continue
			}
			for w := range raw[rec.Upstream] {
				d, ok := parseBudgetDur(w)
				if !ok || now.Sub(t) > d {
					continue
				}
				raw[rec.Upstream][w] += float64(rec.Usage.Total)
			}
		}
		f.Close()
	}

	a.budgetMu.Lock()
	a.budgetRaw = raw
	for id, until := range a.coolUntil {
		if now.After(until) {
			delete(a.coolUntil, id)
		}
	}
	a.budgetMu.Unlock()
}

// startBudgetLoop 按最短刷新间隔启动定时扫描; 没有任何上游配预算则不开。
func (a *App) startBudgetLoop() {
	refresh := 60
	hasBudget := false
	a.mu.RLock()
	for _, u := range a.upstreams {
		if u.Budget != nil && len(u.Budget.Windows) > 0 {
			hasBudget = true
			if u.Budget.RefreshSec > 0 && u.Budget.RefreshSec < refresh {
				refresh = u.Budget.RefreshSec
			}
		}
	}
	a.mu.RUnlock()
	if !hasBudget {
		return
	}
	a.refreshBudget()
	t := time.NewTicker(time.Duration(refresh) * time.Second)
	go func() {
		for range t.C {
			a.refreshBudget()
		}
	}()
}

const defaultBudgetCooldown = 5 * time.Minute

// quotaHints / rateHints 用来区分「额度撞墙」与「限流」两类 429: 只有前者才反推系数
// (限流时的窗口累计与额度无关, 拿它标定会算出荒唐的值); 冷却两类都进。
var (
	quotaHints = []string{"budget", "quota", "credit", "balance", "insufficient", "额度", "余额"}
	rateHints  = []string{"rate limit", "ratelimit", "rate_limit", "rpm", "tpm", "too many requests", "限流", "频率"}
)

func looksLikeQuotaError(body []byte) bool {
	text := strings.ToLower(string(body))
	for _, hint := range rateHints {
		if strings.Contains(text, hint) {
			return false
		}
	}
	for _, hint := range quotaHints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

// cooling 报告某上游是否在冷却, 以及剩余时长。
func (a *App) cooling(upID string) (bool, time.Duration) {
	a.budgetMu.Lock()
	until, ok := a.coolUntil[upID]
	a.budgetMu.Unlock()
	if !ok {
		return false, 0
	}
	left := time.Until(until)
	if left <= 0 {
		return false, 0
	}
	return true, left
}

// markLimitHit 处理上游的 429 (D13): 该上游 (provider+key) 进冷却;
// body 像额度类错误时, 用撞墙时刻的窗口累计反推折算系数 (取各窗口最紧的下界:
// 撞满的窗口给出 coefficient >= max/已用, 取最大值以免低估)。
func (a *App) markLimitHit(u *UpstreamCfg, model string, body []byte) {
	if u.Budget == nil {
		return
	}
	cooldown := time.Duration(u.Budget.CooldownMs) * time.Millisecond
	if cooldown <= 0 {
		cooldown = defaultBudgetCooldown
	}
	quotaShaped := looksLikeQuotaError(body)
	a.budgetMu.Lock()
	a.coolUntil[u.ID] = time.Now().Add(cooldown)
	coef := 0.0
	if quotaShaped {
		for w, max := range u.Budget.Windows {
			used := a.budgetRaw[u.ID][w]
			if used > 0 && max > 0 {
				if c := max / used; c > coef {
					coef = c
				}
			}
		}
		if coef > 0 {
			a.budgetCoef[u.ID] = coef
		}
	}
	rawCopy := map[string]float64{}
	for w, v := range a.budgetRaw[u.ID] {
		rawCopy[w] = v
	}
	a.budgetMu.Unlock()

	switch {
	case coef > 0:
		log.Printf("budget limit hit on %s (model %s): cooldown %s, calibrated unit %.9g (raw tokens %v)",
			u.ID, model, cooldown, coef, rawCopy)
	case quotaShaped:
		log.Printf("budget limit hit on %s (model %s): cooldown %s (no window data, unit kept)",
			u.ID, model, cooldown)
	default:
		log.Printf("limit hit on %s (model %s): cooldown %s (not quota-shaped, unit kept)",
			u.ID, model, cooldown)
	}
	a.saveBudgetState()
}

// ---------- 系数/冷却状态文件 ----------

type budgetState struct {
	Units     map[string]float64 `json:"units,omitempty"`
	Cooldown  map[string]string  `json:"cooldown_until,omitempty"`
	UpdatedAt string             `json:"updated_at,omitempty"`
}

// loadBudgetState 读回上次标定的系数与冷却 (进程重启不丢); 过期的冷却直接丢弃。
func (a *App) loadBudgetState() {
	path := a.statePath
	if path == "" {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st budgetState
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("budget state %s unreadable: %v", path, err)
		return
	}
	now := time.Now()
	a.budgetMu.Lock()
	for id, v := range st.Units {
		if v > 0 {
			a.budgetCoef[id] = v
		}
	}
	for id, ts := range st.Cooldown {
		if until, err := time.Parse(time.RFC3339Nano, ts); err == nil && until.After(now) {
			a.coolUntil[id] = until
		}
	}
	a.budgetMu.Unlock()
}

// saveBudgetState 原子写状态文件 (临时文件 + rename)。
func (a *App) saveBudgetState() {
	path := a.statePath
	if path == "" {
		return
	}
	a.budgetMu.Lock()
	st := budgetState{
		Units:     map[string]float64{},
		Cooldown:  map[string]string{},
		UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	for id, v := range a.budgetCoef {
		st.Units[id] = v
	}
	for id, until := range a.coolUntil {
		if until.After(time.Now()) {
			st.Cooldown[id] = until.Format(time.RFC3339Nano)
		}
	}
	a.budgetMu.Unlock()

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		log.Printf("budget state write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("budget state rename failed: %v", err)
	}
}

// coefficientOf 取某模型的折算系数: 配置的 units 优先, 其次配置的 unit,
// 再次撞墙标定值, 最后 1。
func (a *App) coefficientOf(u *UpstreamCfg, model string) float64 {
	if u.Budget == nil {
		return 1
	}
	if v, ok := u.Budget.Units[model]; ok && v > 0 {
		return v
	}
	if u.Budget.Unit > 0 {
		return u.Budget.Unit
	}
	a.budgetMu.Lock()
	c := a.budgetCoef[u.ID]
	a.budgetMu.Unlock()
	if c > 0 {
		return c
	}
	return 1
}

type aliasTarget struct {
	Upstream string
	Model    string
}

// ---------- 探测实现 (D14) ----------

// probeDefaults 填默认值: 只探 fallback 顺序的下一个候选, 间隔 900s,
// 每次生成上限 8 token, 单次超时 30s, 样本窗 5。
func probeDefaults(c *ProbeCfg) *ProbeCfg {
	out := *c
	if out.Scope == "" {
		out.Scope = "next"
	}
	if out.IntervalSec <= 0 {
		out.IntervalSec = 900
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 8
	}
	if out.TimeoutMs <= 0 {
		out.TimeoutMs = 30000
	}
	if out.Window <= 0 {
		out.Window = 5
	}
	return &out
}

func probeKey(upID, model string) string { return upID + "|" + model }

// msCeil 毫秒向上取整: 亚毫秒的响应记成 1ms, 不记 0 (0 会被当成"没测到")。
func msCeil(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// probePathFor 指标状态文件与 budget-units.json 同目录 (var/)。
func probePathFor(c *Config) string {
	if c == nil || c.LogPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.LogPath), "probe.json")
}

// probeCfg 取当前 (已填默认值的) 探测配置; 未配置或未开启时返回 nil。
func (a *App) probeCfg() *ProbeCfg {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cfg == nil || a.cfg.Probe == nil {
		return nil
	}
	return probeDefaults(a.cfg.Probe)
}

// recordProbeSample 追加样本并更新 EWMA (α=0.3); 只失败也记, 便于看可用率。
func (a *App) recordProbeSample(upID, model string, s probeSample, window int) {
	if a.probe == nil {
		return
	}
	if window <= 0 {
		window = 5
	}
	k := probeKey(upID, model)
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	st := a.probe[k]
	if st == nil {
		st = &probeStat{Upstream: upID, Model: model}
		a.probe[k] = st
	}
	if s.Passive {
		st.Passive++
	} else {
		st.Active++
	}
	if s.OK {
		st.OK++
		if st.EwmaTtfbMs <= 0 {
			st.EwmaTtfbMs = float64(s.TtfbMs)
		} else {
			st.EwmaTtfbMs = 0.7*st.EwmaTtfbMs + 0.3*float64(s.TtfbMs)
		}
		if s.Tps > 0 {
			if st.EwmaTps <= 0 {
				st.EwmaTps = s.Tps
			} else {
				st.EwmaTps = 0.7*st.EwmaTps + 0.3*s.Tps
			}
		}
	} else {
		st.Fail++
	}
	st.UpdatedAt = s.Ts
	st.Samples = append(st.Samples, s)
	if len(st.Samples) > window {
		st.Samples = st.Samples[len(st.Samples)-window:]
	}
}

func (a *App) loadProbeState() {
	if a.probePath == "" {
		return
	}
	b, err := os.ReadFile(a.probePath)
	if err != nil {
		return
	}
	var st probeState
	if json.Unmarshal(b, &st) != nil || st.Stats == nil {
		return
	}
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	for k := range st.Stats {
		s := st.Stats[k]
		a.probe[k] = &s
	}
	log.Printf("probe: loaded %d candidate stats from %s", len(st.Stats), a.probePath)
}

// saveProbeState 原子写 (临时文件 + rename), 与预算状态同一套路。
func (a *App) saveProbeState() {
	path := a.probePath
	if path == "" {
		return
	}
	a.probeMu.Lock()
	st := probeState{Stats: map[string]probeStat{}, UpdatedAt: time.Now().Format(time.RFC3339Nano)}
	for k, v := range a.probe {
		st.Stats[k] = *v
	}
	a.probeMu.Unlock()

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		log.Printf("probe state write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("probe state rename failed: %v", err)
	}
}

// startProbeLoop 起探测循环; 未开启时什么都不做 (默认关闭)。
// 探测参数启动时固化, 改配置要重启 (与 limits / timeouts 一致)。
func (a *App) startProbeLoop() {
	c := a.probeCfg()
	if c == nil || !c.Enabled {
		return
	}
	log.Printf("probe: enabled scope=%s interval=%ds max_tokens=%d targets=%d window=%d",
		c.Scope, c.IntervalSec, c.MaxTokens, len(c.Targets), c.Window)
	go func() {
		a.probeOnce(c)
		t := time.NewTicker(time.Duration(c.IntervalSec) * time.Second)
		defer t.Stop()
		for range t.C {
			a.probeOnce(c)
		}
	}()
}

// probeOnce 跑一轮: 对每个目标候选发一次小请求并落状态。
func (a *App) probeOnce(c *ProbeCfg) {
	targets := a.probeTargets(c)
	for _, t := range targets {
		a.probeCandidate(t.alias, t.up, t.model, c)
	}
	if len(targets) > 0 {
		a.saveProbeState()
	}
}

type probeTarget struct {
	alias string
	up    *UpstreamCfg
	model string
}

// probeTargets 列出要探测的候选: scope=next 时只取 fallback 顺序里的第二个
// (第一个候选由真实流量被动采样), 单候选别名不探。
func (a *App) probeTargets(c *ProbeCfg) []probeTarget {
	a.mu.RLock()
	defer a.mu.RUnlock()
	filter := map[string]bool{}
	for _, t := range c.Targets {
		filter[t] = true
	}
	names := make([]string, 0, len(a.aliases))
	for name := range a.aliases {
		if len(filter) > 0 && !filter[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var out []probeTarget
	for _, name := range names {
		cands := a.aliases[name]
		pick := cands
		if c.Scope != "all" {
			if len(cands) < 2 {
				continue
			}
			pick = cands[1:2] // 只探下一个候选
		}
		for _, cand := range pick {
			if u, ok := a.upstreams[cand.Upstream]; ok {
				out = append(out, probeTarget{alias: name, up: u, model: cand.Model})
			}
		}
	}
	return out
}

// probeCandidate 发一次探测: 走该上游的限速闸 (不与用户流量抢队), 流式读首字节
// 与流末 usage, 得到 ttfb_ms 与 tps; 全程不碰冷却/熔断/预算标记。
func (a *App) probeCandidate(alias string, up *UpstreamCfg, model string, c *ProbeCfg) {
	sample := probeSample{Ts: time.Now().Format(time.RFC3339Nano)}
	payload := map[string]any{
		"model":          model,
		"messages":       []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens":     c.MaxTokens,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	lim := a.limiters[up.ID]
	if lim != nil {
		// 探测自己排队, 最多等 5s; 排不上就跳过这一轮, 绝不占用用户请求的排队预算。
		gateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		okSem := lim.acquireSem(gateCtx)
		okTok := okSem && lim.takeToken(gateCtx)
		cancel()
		if !okTok {
			if okSem {
				lim.releaseSem()
			}
			sample.Err = "limiter busy"
			a.recordProbeSample(up.ID, model, sample, c.Window)
			return
		}
		defer lim.releaseSem()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutMs)*time.Millisecond)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		sample.Err = err.Error()
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	req.ContentLength = int64(len(body))
	prepareOutboundHeaders(req.Header, http.Header{}, up)
	resp, err := a.transport.RoundTrip(req)
	if err != nil {
		sample.Err = err.Error()
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		sample.Err = fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}

	// 流式读: 首字节时刻 = ttfb, 流末时刻 - 首字节 = 解码段。
	var firstAt, lastAt time.Time
	var usage *Usage
	acc := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			now := time.Now()
			if firstAt.IsZero() {
				firstAt = now
			}
			lastAt = now
			if len(acc) < 64*1024 { // 探测响应很小, 截断只为兜底
				acc = append(acc, buf[:n]...)
			}
		}
		if rerr != nil {
			break
		}
	}
	if firstAt.IsZero() {
		sample.Err = "no response body"
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	if u := extractUsage(acc); u != nil {
		usage = u
	}
	sample.OK = true
	sample.TtfbMs = msCeil(firstAt.Sub(start))
	if usage != nil {
		sample.Tokens = usage.Completion
		if d := lastAt.Sub(firstAt); d > 0 && usage.Completion > 0 {
			sample.Tps = float64(usage.Completion) / d.Seconds()
		}
	}
	a.recordProbeSample(up.ID, model, sample, c.Window)
	// 探测流量如实写请求日志 (source=probe): 它真的消耗配额, 预算窗口照实计入。
	a.logReq(logRec{
		Ts: time.Now().Format(time.RFC3339Nano), Logical: alias, Upstream: up.ID, Model: model,
		Path: "/v1/probe", Stream: true, Status: http.StatusOK, Usage: usage,
		Missing: usage == nil, Source: "probe", TtfbMs: sample.TtfbMs, Tps: sample.Tps,
	})
}

// probeRow 报告里的一行: 某别名的某个候选及其指标与建议。
type probeRow struct {
	Alias     string  `json:"alias"`
	Role      string  `json:"role"` // primary | next | candidate
	Upstream  string  `json:"upstream"`
	Model     string  `json:"model"`
	TtfbMs    float64 `json:"ttfb_ms"`
	Tps       float64 `json:"tps"`
	Samples   int     `json:"samples"`
	OK        int     `json:"ok"`
	Fail      int     `json:"fail"`
	Active    int     `json:"active"`
	Passive   int     `json:"passive"`
	Meets     bool    `json:"meets_targets"`
	Advantage bool    `json:"advantage"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// probeReport 生成只读报告: 每别名按 fallback 顺序列出候选指标, 并给出
// "是否达到标准" 与 "是否相对主候选有明显优势" 的建议值 —— 仅供决策, 不切换。
func (a *App) probeReport() map[string]any {
	c := a.probeCfg()
	if c == nil {
		c = probeDefaults(&ProbeCfg{})
	}
	a.mu.RLock()
	names := make([]string, 0, len(a.aliases))
	for name := range a.aliases {
		names = append(names, name)
	}
	aliases := make(map[string][]aliasTarget, len(a.aliases))
	for k, v := range a.aliases {
		aliases[k] = v
	}
	ups := make(map[string]*UpstreamCfg, len(a.upstreams))
	for k, v := range a.upstreams {
		ups[k] = v
	}
	a.mu.RUnlock()
	sort.Strings(names)

	a.probeMu.Lock()
	stats := make(map[string]probeStat, len(a.probe))
	for k, v := range a.probe {
		stats[k] = *v
	}
	a.probeMu.Unlock()

	meets := func(st probeStat) bool {
		if st.OK == 0 {
			return false
		}
		if c.TargetTtfbMs > 0 && st.EwmaTtfbMs > float64(c.TargetTtfbMs) {
			return false
		}
		if c.TargetTps > 0 && st.EwmaTps < c.TargetTps {
			return false
		}
		return c.TargetTtfbMs > 0 || c.TargetTps > 0
	}
	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339Nano),
		"standards": map[string]any{
			"target_ttfb_ms": c.TargetTtfbMs,
			"target_tps":     c.TargetTps,
			"switch_margin":  c.SwitchMargin,
		},
		"probe": map[string]any{
			"enabled": c.Enabled, "scope": c.Scope, "interval_sec": c.IntervalSec,
		},
	}
	rows := map[string][]probeRow{}
	for _, name := range names {
		cands := aliases[name]
		list := make([]probeRow, 0, len(cands))
		for i, cand := range cands {
			role := "candidate"
			switch {
			case i == 0:
				role = "primary"
			case i == 1:
				role = "next"
			}
			row := probeRow{Alias: name, Role: role, Upstream: cand.Upstream, Model: cand.Model}
			if st, ok := stats[probeKey(cand.Upstream, cand.Model)]; ok {
				row.TtfbMs, row.Tps = st.EwmaTtfbMs, st.EwmaTps
				row.Samples, row.OK, row.Fail = len(st.Samples), st.OK, st.Fail
				row.Active, row.Passive, row.UpdatedAt = st.Active, st.Passive, st.UpdatedAt
				row.Meets = meets(st)
			}
			// 建议: 下一个候选达标、且相对主候选有 margin 以上的优势 (两个标准任一即可)。
			if row.Role == "next" && row.Meets && len(list) > 0 {
				p := list[0]
				if p.TtfbMs > 0 && row.TtfbMs > 0 {
					if float64(row.TtfbMs) <= float64(p.TtfbMs)*(1-c.SwitchMargin) {
						row.Advantage = true
					}
				}
				if !row.Advantage && p.Tps > 0 && row.Tps > 0 {
					if row.Tps >= p.Tps*(1+c.SwitchMargin) {
						row.Advantage = true
					}
				}
			}
			list = append(list, row)
		}
		rows[name] = list
	}
	out["aliases"] = rows
	return out
}

// serveProbe GET /v1/probe: 只读报告 (标准判定仅建议; 不改变任何路由行为)。
func (a *App) serveProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(a.probeReport())
}

// runProbeReport -probe: 从配置文件与状态文件离线打印同一个报告。
func runProbeReport(cfgPath string) {
	cfg, err := loadConfigOpt(cfgPath, true)
	if err != nil {
		log.Fatalf("config invalid: %v", err)
	}
	a := &App{
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		limiters:  map[string]*Limiter{},
		breakers:  map[string]*breaker{},
		probe:     map[string]*probeStat{},
	}
	a.applyConfig(cfg)
	a.probePath = probePathFor(cfg)
	a.loadProbeState()
	rep := a.probeReport()
	std := rep["standards"].(map[string]any)
	fmt.Printf("标准: ttfb<=%v ms, tps>=%v, margin=%v\n", std["target_ttfb_ms"], std["target_tps"], std["switch_margin"])
	rows := rep["aliases"].(map[string][]probeRow)
	names := make([]string, 0, len(rows))
	for k := range rows {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		list := rows[name]
		if len(list) < 2 {
			continue
		}
		fmt.Printf("%s\n", name)
		for _, row := range list {
			flag := " "
			if row.Advantage {
				flag = "*" // * = 建议切换 (仅建议)
			}
			fmt.Printf("  %s %-8s %-6s/%-28s ttfb=%6.0fms tps=%6.1f 样本=%d(主%v/探%v) 失败=%d 达标=%v\n",
				flag, row.Role, row.Upstream, row.Model, row.TtfbMs, row.Tps,
				row.Samples, row.Passive, row.Active, row.Fail, row.Meets)
		}
	}
}

func newApp(c *Config) (*App, error) {
	a := &App{
		upstreams:  map[string]*UpstreamCfg{},
		aliases:    map[string][]aliasTarget{},
		limiters:   map[string]*Limiter{},
		breakers:   map[string]*breaker{},
		budgetRaw:  map[string]map[string]float64{},
		budgetCoef: map[string]float64{},
		coolUntil:  map[string]time.Time{},
		probe:      map[string]*probeStat{},
	}
	a.applyConfig(c)
	if err := a.openLog(); err != nil {
		return nil, err
	}
	// 系数与冷却状态放在日志同目录 (var/), 不进仓库。
	if c.LogPath != "" {
		a.statePath = filepath.Join(filepath.Dir(c.LogPath), "budget-units.json")
	}
	a.loadBudgetState()
	a.startBudgetLoop()
	// 探测状态与预算状态同目录; 未配置 probe 时 probePath 为空, 一切探测都停用。
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
	upstreams := map[string]*UpstreamCfg{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		upstreams[u.ID] = u
		// 限速器与熔断器按上游 id 保留不重建: 状态有意跨热加载延续 (排队中的请求、
		// 令牌水位、熔断计数都不该被 hup 清掉)。代价是参数改了不会自动生效, 所以
		// 这里显式告警 —— 否则 hup 之后是静默沿用旧值。
		if l, ok := a.limiters[u.ID]; !ok {
			a.limiters[u.ID] = newLimiter(u.Limits)
		} else if want := normLimits(u.Limits); want != l.cfg {
			log.Printf("upstream %s: limits 已改 %+v -> %+v, 限速器保留旧状态, 需重启才生效",
				u.ID, l.cfg, want)
		}
		if b, ok := a.breakers[u.ID]; !ok {
			a.breakers[u.ID] = newBreaker(u.BreakerFail,
				time.Duration(u.BreakerCooldMs)*time.Millisecond)
		} else if ft, cd := normBreakerDur(u.BreakerFail,
			time.Duration(u.BreakerCooldMs)*time.Millisecond); ft != b.failTh || cd != b.cooldown {
			log.Printf("upstream %s: breaker 参数已改 -> fail=%d cooldown=%s, 需重启才生效",
				u.ID, ft, cd)
		}
	}
	a.upstreams = upstreams
	aliases := map[string][]aliasTarget{}
	for name, list := range c.Aliases {
		for _, pair := range list {
			aliases[name] = append(aliases[name], aliasTarget{pair[0], pair[1]})
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
				// 每次读都重设 idle 死线, 实现流空闲超时 (F2)。
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

// logDayOf 返回时间所在本地日历天 (轮转边界按本地日期算)。
func logDayOf(t time.Time) string { return t.Format("2006-01-02") }

// archivePath 给出归档文件名: var/requests.jsonl -> var/requests-2026-10-12.jsonl。
// 目标已存在时追加 -1/-2 序号, 不覆盖既有归档。
func archivePath(path, day string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	if _, err := os.Stat(base + "-" + day + ext); err != nil {
		return base + "-" + day + ext
	}
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s-%s-%d%s", base, day, i, ext)
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}

// rotateLog 把当前日志归档为带日期的文件, 再新开一份当前日志。
// 调用方须持有 logMu。prevDay 是这段日志所属的日期。
func (a *App) rotateLog(prevDay string) {
	path := a.cfg.LogPath
	if a.logW != nil {
		a.logW.Close()
	}
	a.logW, a.logger = nil, nil
	if st, err := os.Stat(path); err == nil {
		if st.Size() > 0 {
			if err := os.Rename(path, archivePath(path, prevDay)); err != nil {
				log.Printf("log rotate failed: %v", err)
			}
		} else {
			os.Remove(path) // 空文件不留归档
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("log reopen failed: %v", err)
		return
	}
	a.logW = f
	a.logger = log.New(f, "", 0)
}

// logFilesForScan 返回参与聚合的日志文件: 当前日志 + 最近归档 (覆盖 maxWindow 跨度)。
// 归档的 mtime 就是它最后被写入的时间, 据此判断是否还在窗口内 (多留一天余量)。
func logFilesForScan(path string, maxWindow time.Duration) []string {
	var files []string
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		files = append(files, path)
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	ms, _ := filepath.Glob(base + "-*" + ext)
	cutoff := time.Now().Add(-maxWindow - 24*time.Hour)
	for _, m := range ms {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(cutoff) {
			files = append(files, m)
		}
	}
	return files
}

func (a *App) openLog() error {
	path := a.cfg.LogPath
	if path == "" {
		return nil
	}
	if i := strings.LastIndex(path, "/"); i >= 0 && i > 0 {
		if err := os.MkdirAll(path[:i], 0o755); err != nil {
			return err
		}
	}
	// 隔天重启: 上一段日志先归档, 当前日志从空开始。
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		if old := logDayOf(st.ModTime()); old != logDayOf(time.Now()) {
			if err := os.Rename(path, archivePath(path, old)); err != nil {
				log.Printf("log rotate on start failed: %v", err)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	a.logW = f
	a.logger = log.New(f, "", 0)
	a.logDay = logDayOf(time.Now())
	return nil
}

func (a *App) logReq(rec logRec) {
	if a.logger == nil {
		return
	}
	b, _ := json.Marshal(rec)
	a.logMu.Lock()
	defer a.logMu.Unlock()
	// 跨过本地日期边界就轮转 (长期常驻时按天归档)。
	if day := logDayOf(time.Now()); day != a.logDay {
		a.rotateLog(a.logDay)
		a.logDay = day
		if a.logger == nil {
			return
		}
	}
	a.logger.Println(string(b))
}

// ---------- HTTP 处理 ----------

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// 客户端可能带来的认证头一律删除, 换成上游配置的凭据 (D1)。
var clientAuthHeaders = []string{
	"Authorization", "Api-Key", "X-Api-Key", "api-key", "X-Goog-Api-Key", "Proxy-Authorization",
}

// shouldFallback: 该状态码值得换下一个候选重试 (首字节前)。429 与 5xx 可重试;
// 其余 4xx (模型不存在 / 参数错 / 鉴权错) 不重试, 直接透传。
func shouldFallback(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// buildAttemptPayload 按候选上游生成转发请求体: 真实模型 id + 按上游开关注入
// include_usage。每次从原始 payload 复制, 不跨尝试共享可变状态。
func buildAttemptPayload(payload map[string]any, model string, includeUsage, stream bool) ([]byte, error) {
	p := make(map[string]any, len(payload))
	for k, v := range payload {
		p[k] = v
	}
	if stream {
		var so map[string]any
		if orig, ok := p["stream_options"].(map[string]any); ok {
			so = make(map[string]any, len(orig)+1)
			for k, v := range orig {
				so[k] = v
			}
		} else if includeUsage {
			so = map[string]any{}
		}
		if so != nil {
			if includeUsage {
				so["include_usage"] = true
			}
			p["stream_options"] = so
		}
	}
	p["model"] = model
	return json.Marshal(p)
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/probe" {
		a.serveProbe(w, r)
		return
	}
	if r.URL.Path == "/v1/models" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"only GET /v1/models")
			return
		}
		a.serveModels(w)
		return
	}
	isResponses := r.URL.Path == clientPathResponses
	if r.Method != http.MethodPost || (!isResponses && r.URL.Path != clientPath) {
		writeError(w, http.StatusNotFound, "not_found",
			"unsupported path/method: "+r.Method+" "+r.URL.Path)
		return
	}
	// /v1/responses 与 /v1/chat/completions 共用除路径外的全部链路 (闸门/记账/fallback)。
	upPath := upstreamPath
	if isResponses {
		upPath = upstreamPathResponses
	}
	// 请求体整体缓冲, 才能改写 model 与 stream_options (设计 §4.9.4)。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"cannot read request body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"request body is not valid JSON")
		return
	}
	logical, _ := payload["model"].(string)
	if logical == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing model")
		return
	}
	stream := false
	if s, ok := payload["stream"].(bool); ok {
		stream = s
	}

	// 集中 fallback 的候选表: 上游 + 真实模型 + 该候选自己的限速器。一把 RLock 拿齐,
	// 循环里不再读 a.upstreams / a.limiters (热加载会改这两张表)。
	type attempt struct {
		up  *UpstreamCfg
		mod string
		lim *Limiter
	}
	a.mu.RLock()
	alias, aliasOK := a.aliases[logical]
	var chosen *UpstreamCfg
	realModel := ""
	attempts := make([]attempt, 0, len(alias))
	for _, cand := range alias {
		u, ok := a.upstreams[cand.Upstream]
		if !ok {
			continue
		}
		if chosen == nil {
			chosen, realModel = u, cand.Model
		}
		attempts = append(attempts, attempt{u, cand.Model, a.limiters[u.ID]})
	}
	queueWait := time.Duration(a.cfg.Timeouts.QueueWaitMs) * time.Millisecond
	a.mu.RUnlock()

	if !aliasOK {
		writeError(w, http.StatusNotFound, "model_not_found", "unknown model: "+logical)
		return
	}
	if chosen == nil {
		writeError(w, http.StatusNotFound, "model_not_found",
			"model "+logical+" has no configured upstream")
		return
	}

	// 预算不再提前拦截 (D13): 判定改为「上游 429 = 限额信号」, 见尝试循环里的
	// markLimitHit (该上游进冷却 + 跨候选切换 + 反推折算系数)。
	//
	// 限速闸 (F3/F4): 信号量 + 令牌桶由**当前候选**承担, 在尝试循环里逐个过 ——
	// fallback 换到另一个上游/模型, 就要重新排那个候选自己的队, 不能拿着前一个候选
	// 的槽位直接发。整条请求共用一份 queue_wait 预算, 所以换候选不会把等待上限叠成
	// N 倍 (总时长与单候选时相同, 仍在客户端 5 分钟静默线以内, §2 第 3 条)。
	// 关键: 换候选前必须归还上一个候选的槽位, 否则会同时占着两个上游互相等 (死锁)。
	gateCtx, cancel := context.WithTimeout(r.Context(), queueWait)
	defer cancel()
	var (
		queueMs    int64    // 各候选排队时长之和
		gateReject string   // 非空表示被闸门拒了: concurrency | rate
		held       *Limiter // 已拿到槽位、正在服务的那个候选
	)
	defer func() {
		if held != nil {
			held.releaseSem()
		}
	}()

	var (
		resp            *http.Response
		lastErr         error
		lastStatus      int
		lastBody        []byte
		lastHdr         http.Header
		lastUpID, lastM string
		failed          int
		servedUp        *UpstreamCfg
		servedMod       string
		attemptStart    time.Time // 被动时延采样: 该候选开始等待的时刻
	)
	coolSkips := 0
	budgetHit := false
	// 集中 fallback: 按候选顺序尝试, 只在还没给客户端写任何字节前 (传输失败 /
	// 上游 429 或 5xx) 换下一个候选; 全部失败保留最近一次错误原样透传。
	for _, at := range attempts {
		if cool, left := a.cooling(at.up.ID); cool {
			// 该上游 (provider+key) 撞过限额, 冷却期内不真发, 直接换下一个候选。
			lastErr = fmt.Errorf("upstream %s cooling for %s", at.up.ID, left.Round(time.Second))
			lastUpID, lastM = at.up.ID, at.mod
			coolSkips++
			failed++
			continue
		}
		br := a.breakers[at.up.ID]
		if !br.allow() {
			// 熔断打开: 不真发, 直接视作该候选失败换下一个。
			lastErr = fmt.Errorf("upstream %s circuit open", at.up.ID)
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		// /v1/responses 没有 stream_options, 不做 include_usage 注入 (用量本就在响应里)。
		body, err := buildAttemptPayload(payload, at.mod, at.up.IncludeUsage && !isResponses, stream)
		if err != nil {
			lastErr = err
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method,
			at.up.BaseURL+upPath, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		outReq.ContentLength = int64(len(body))
		prepareOutboundHeaders(outReq.Header, r.Header, at.up)

		// 这个候选自己的闸门: 排队等它的槽位与令牌, 共用整条请求的 queue_wait 预算。
		// 放在请求体准备之后, 这样构造失败不白占配额。
		lim := at.lim
		if lim == nil {
			lastErr = fmt.Errorf("upstream %s has no limiter", at.up.ID)
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		atGateStart := time.Now()
		okSem := lim.acquireSem(gateCtx)
		okTok := okSem && lim.takeToken(gateCtx)
		queueMs += time.Since(atGateStart).Milliseconds() // 成功或失败都记排了多久
		if !okSem {
			gateReject = "concurrency"
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			break
		}
		if !okTok {
			lim.releaseSem()
			gateReject = "rate"
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			break
		}

		attemptStart = time.Now()
		resp, err = a.transport.RoundTrip(outReq)
		if err != nil {
			lim.releaseSem()
			br.recordFail()
			lastErr = err
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		if shouldFallback(resp.StatusCode) {
			lim.releaseSem()
			br.recordFail()
			b, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests {
				// 上游 429: 当作限额信号处理 (冷却 + 可能的系数标定), 再切下一个候选。
				budgetHit = true
				a.markLimitHit(at.up, at.mod, b)
			}
			lastErr = fmt.Errorf("upstream %s returned %d", at.up.ID, resp.StatusCode)
			lastStatus, lastHdr, lastBody = resp.StatusCode, resp.Header.Clone(), b
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			if rerr != nil {
				lastErr = rerr
			}
			continue
		}
		br.recordSuccess()
		servedUp, servedMod = at.up, at.mod
		held = lim
		break
	}
	queued := queueMs >= 5

	if servedUp == nil && gateReject != "" {
		// 闸门排队超预算: 整条请求的等待额度已用尽, 不再试后面的候选 (gateCtx 已到期,
		// 再试也只是立刻失败)。这就是 F3 的排队上限。
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(queueWait/time.Second))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"upstream busy: queue wait exceeded "+queueWait.String())
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
			Status: http.StatusTooManyRequests, Missing: true,
			Queued: queued, QueueMs: queueMs,
			Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
			Source: r.Header.Get("X-Tollgate-Source"),
			Err:    "rate_limited", Gate: gateReject, Fallbacks: failed,
			BudgetHit: budgetHit, CoolSkips: coolSkips,
		})
		return
	}

	if servedUp == nil {
		// 全部候选失败: 最近一次是 HTTP 错误就保留状态码/头/体原样透传, 否则 502。
		if lastStatus != 0 && lastBody != nil {
			dst := w.Header()
			for k, vv := range lastHdr {
				if isHopHeader(k) {
					continue
				}
				for _, v := range vv {
					dst.Add(k, v)
				}
			}
			w.WriteHeader(lastStatus)
			w.Write(lastBody)
			a.logReq(logRec{
				Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
				Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
				Status: lastStatus, Missing: true, Queued: queued, QueueMs: queueMs,
				Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
				Source: r.Header.Get("X-Tollgate-Source"),
				Err:    lastErr.Error(), Fallbacks: failed, BudgetHit: budgetHit, CoolSkips: coolSkips,
			})
			return
		}
		// 候选全在限额冷却里: 回 429 + Retry-After 让 agent 退避 (不是我们的闸门,
		// 而是上游限额的传导), 比 502 更贴近真实原因。
		if coolSkips > 0 && lastStatus == 0 {
			left := time.Duration(0)
			for _, at := range attempts {
				if cool, l := a.cooling(at.up.ID); cool && l > left {
					left = l
				}
			}
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(left.Seconds())))))
			writeError(w, http.StatusTooManyRequests, "upstream_cooling",
				"all candidates for "+logical+" are cooling down after upstream limit hits")
			a.logReq(logRec{
				Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
				Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
				Status: http.StatusTooManyRequests, Missing: true, Queued: queued, QueueMs: queueMs,
				Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
				Source: r.Header.Get("X-Tollgate-Source"),
				Err:    "upstream_cooling", Fallbacks: failed, BudgetHit: budgetHit, CoolSkips: coolSkips,
			})
			return
		}
		msg := "all upstreams failed"
		if lastErr != nil {
			msg = lastErr.Error()
		}
		writeError(w, http.StatusBadGateway, "upstream_error", msg)
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
			Status: http.StatusBadGateway, Missing: true, Queued: queued, QueueMs: queueMs,
			Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
			Source: r.Header.Get("X-Tollgate-Source"),
			Err:    msg, Fallbacks: failed, BudgetHit: budgetHit, CoolSkips: coolSkips,
		})
		return
	}
	defer resp.Body.Close()
	chosen = servedUp
	realModel = servedMod

	// 转发响应头 (F9): 除 hop-by-hop 外原样透传。
	dst := w.Header()
	for k, vv := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	var usage *Usage
	missing := true
	status := resp.StatusCode
	var ttfbMs, decodeMs int64
	if stream {
		w.WriteHeader(status)
		dst.Del("Content-Length")
		ro := relayStream(w, resp.Body, logical, realModel, attemptStart, isResponses)
		usage, missing, ttfbMs, decodeMs = ro.Usage, ro.Missing, ro.TtfbMs, ro.DecodeMs
	} else {
		buf, rerr := io.ReadAll(resp.Body)
		// 非流式: 首字节与整体等待几乎同时 (上游把响应写全才回头), tps 无从分解。
		ttfbMs = msCeil(time.Since(attemptStart))
		if rerr != nil {
			status = http.StatusBadGateway
			buf = []byte(`{"error":{"message":` + strconv.Quote(rerr.Error()) + `}}`)
			dst.Set("Content-Type", "application/json")
		}
		buf = rewriteModel(buf, logical)
		usage = extractUsage(buf)
		missing = usage == nil
		if missing {
			est := estimateUsageFromBody(buf)
			usage = &Usage{Completion: est, Total: est}
		}
		dst.Set("Content-Length", strconv.Itoa(len(buf)))
		w.WriteHeader(status)
		w.Write(buf)
	}

	// 被动时延采样 (D14): 真实流量本身就是主候选的观测, 不额外发探测请求。
	// 只有流式能拆出解码段, 所以 tps 仅流式有值。
	tps := 0.0
	completion := 0
	if usage != nil {
		completion = usage.Completion
	}
	if stream && decodeMs > 0 && completion > 0 {
		tps = float64(completion) / (float64(decodeMs) / 1000)
	}
	if ttfbMs > 0 {
		window := 5
		if pc := a.probeCfg(); pc != nil {
			window = pc.Window
		}
		a.recordProbeSample(chosen.ID, realModel, probeSample{
			Ts: time.Now().Format(time.RFC3339Nano), TtfbMs: ttfbMs, Tps: tps,
			Tokens: completion, OK: status < 400, Passive: true,
		}, window)
	}

	a.logReq(logRec{
		Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
		Upstream: chosen.ID, Model: realModel, Path: r.URL.Path, Stream: stream,
		Status: status, Usage: usage, Missing: missing, Queued: queued, QueueMs: queueMs,
		Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"), Source: r.Header.Get("X-Tollgate-Source"),
		Fallbacks: failed, BudgetHit: budgetHit, CoolSkips: coolSkips,
		TtfbMs: ttfbMs, Tps: tps,
	})
}

// relayStream 按行透传 SSE, 期间提取流末 usage (设计 §4.9.2)。
// 流未以 [DONE] 正常结束时, 发一个 SSE error 事件后关流, 不伪造正常结束 (F6)。
// 返回提取到的 usage 与是否缺失 (配合 F7 兜底)。
func relayStream(w http.ResponseWriter, body io.Reader, logical, realModel string, start time.Time, isResponses bool) relayOut {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var usage *Usage
	est := 0
	done := false
	var firstAt, lastAt time.Time // 首字节 / 末字节时刻 (被动时延采样)

	write := func(s string) {
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	for sc.Scan() {
		now := time.Now()
		if firstAt.IsZero() {
			firstAt = now
		}
		lastAt = now
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				done = true
			} else if data != "" {
				var chunk struct {
					Usage *openaiUsage `json:"usage"`
					Type  string       `json:"type"`
					// /v1/responses 的事件把 usage 与 model 都装在 response 对象里。
					Response *struct {
						Usage *openaiUsage `json:"usage"`
					} `json:"response"`
					Delta   json.RawMessage `json:"delta"`
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if usage == nil {
						switch {
						case chunk.Usage != nil:
							usage = toUsage(chunk.Usage)
						case chunk.Response != nil && chunk.Response.Usage != nil:
							usage = toUsage(chunk.Response.Usage)
						}
					}
					if isResponses {
						// Responses API 走语义事件流, 终态是 response.completed (不保证发
						// [DONE]); failed/incomplete 是上游自己报的终态, 也算正常收尾 ——
						// 否则每条正常流都会被补一个 error 事件 (F6 误报)。
						switch chunk.Type {
						case "response.completed", "response.failed", "response.incomplete":
							done = true
						case "response.output_text.delta":
							var s string
							if json.Unmarshal(chunk.Delta, &s) == nil {
								est += estimateTokens(s)
							}
						}
					}
					for _, c := range chunk.Choices {
						est += estimateTokens(c.Delta.Content)
					}
					// O9: 块内 model 改回逻辑名 (只改该字段, 其余字节原样)。
					data2 := rewriteModel([]byte(data), logical)
					if isResponses {
						// 事件里的 model 嵌在 response 对象下, 顶层改写够不着。
						data2 = rewriteModelKnown(data2, realModel, logical)
					}
					if string(data2) != data {
						line = "data: " + string(data2)
					}
				}
			}
		}
		write(line + "\n")
	}
	if !done {
		// 流未正常结束: 用流内约定形态表达错误 (设计 §2.2 / F6)。
		write("event: error\n")
		write(`data: {"error":{"message":"upstream stream ended unexpectedly","type":"upstream_error"}}` + "\n\n")
	}
	missing := usage == nil
	if missing {
		usage = &Usage{Completion: est, Total: est}
	}
	out := relayOut{Usage: usage, Missing: missing}
	if !firstAt.IsZero() {
		out.TtfbMs = msCeil(firstAt.Sub(start))
		if !lastAt.IsZero() {
			out.DecodeMs = msCeil(lastAt.Sub(firstAt))
		}
	}
	return out
}

// keyPrefix 给出认证头里的 scheme: 显式配置优先; 缺省 Authorization 用 "Bearer ",
// 其余头 (api-key / x-api-key 这类) 直接放裸 key。
func (u *UpstreamCfg) keyPrefix() string {
	if u.KeyPrefix != nil {
		return *u.KeyPrefix
	}
	if strings.EqualFold(u.AuthHeader, "Authorization") {
		return "Bearer "
	}
	return ""
}

// prepareOutboundHeaders 拷贝客户端头, 替换认证, 删 hop-by-hop 与 Accept-Encoding。
func prepareOutboundHeaders(dst, src http.Header, up *UpstreamCfg) {
	for k, vv := range src {
		dst[k] = append([]string(nil), vv...)
	}
	for _, k := range clientAuthHeaders {
		dst.Del(k)
	}
	for _, k := range hopHeaders {
		dst.Del(k)
	}
	dst.Del("Accept-Encoding")
	if up.AuthHeader != "" {
		dst.Set(up.AuthHeader, up.keyPrefix()+up.Key)
	}
	for k, v := range up.Headers {
		dst.Set(k, v)
	}
}

func isHopHeader(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": code, "code": code},
	})
}

// serveModels 返回别名表里的逻辑模型清单 (O5)。只列对外名字, 不暴露上游真实 id。
func (a *App) serveModels(w http.ResponseWriter) {
	a.mu.RLock()
	names := make([]string, 0, len(a.aliases))
	for n := range a.aliases {
		names = append(names, n)
	}
	a.mu.RUnlock()
	sort.Strings(names)
	data := make([]map[string]string, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]string{"id": n, "object": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// ---------- 入口 ----------

func loadConfig(path string) (*Config, error) {
	return loadConfigOpt(path, false)
}

// loadConfigOpt 读配置。lenient=true 时缺 key 环境变量不报错 (只给离线报告用:
// -probe 只关心上游 id 与别名顺序, 不发请求, 所以不需要真 key)。
func loadConfigOpt(path string, lenient bool) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.LogPath == "" {
		c.LogPath = "var/requests.jsonl"
	}
	// key_env: 配置只存环境变量名, 真 key 启动/热加载时从环境取, 不落盘。
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.KeyEnv != "" {
			v, ok := os.LookupEnv(u.KeyEnv)
			if !ok || v == "" {
				if lenient {
					continue
				}
				return nil, fmt.Errorf("upstream %s: env %s not set", u.ID, u.KeyEnv)
			}
			u.Key = v
		}
	}
	return &c, nil
}

// sumRow 是 -summary 的每行聚合 (O2)。
type sumRow struct {
	Key     string
	Up      string
	Reqs    int
	Errs    int
	Queued  int
	Missing int
	P, C, T int
}

// runSummary 汇总 JSONL 请求日志 (O2): 按 逻辑名+来源 分组, 来源取 source, 缺省用 ua。
// path 可以是单个日志文件, 也可以是一个目录 (汇总其中全部 *.jsonl, 用于跨天归档)。
func runSummary(path string) {
	files := []string{path}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		ms, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
		if err != nil || len(ms) == 0 {
			log.Fatalf("summary: no *.jsonl under %s", path)
		}
		sort.Strings(ms)
		files = ms
	}
	rows := map[string]*sumRow{}
	var order []string
	for _, fp := range files {
		f, err := os.Open(fp)
		if err != nil {
			log.Fatalf("summary: %v", err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec logRec
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Logical == "" {
				continue
			}
			src := rec.Source
			if src == "" {
				src = rec.UA
			}
			key := rec.Logical + "\x00" + src
			r, ok := rows[key]
			if !ok {
				r = &sumRow{Key: rec.Logical + " / " + src}
				rows[key] = r
				order = append(order, key)
			}
			if r.Up == "" {
				r.Up = rec.Upstream
			}
			r.Reqs++
			if rec.Status >= 400 {
				r.Errs++
			}
			if rec.Queued {
				r.Queued++
			}
			if rec.Missing {
				r.Missing++
			}
			if rec.Usage != nil {
				r.P += rec.Usage.Prompt
				r.C += rec.Usage.Completion
				r.T += rec.Usage.Total
			}
		}
		if err := sc.Err(); err != nil {
			f.Close()
			log.Fatalf("summary: %v", err)
		}
		f.Close()
	}
	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].T > rows[order[j]].T
	})
	fmt.Printf("%-34s %-10s %6s %5s %5s %5s %8s %8s %8s\n",
		"logical/source", "upstream", "reqs", "err", "queued", "miss", "prompt", "completion", "total")
	tot := &sumRow{Key: "TOTAL"}
	for _, k := range order {
		r := rows[k]
		fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
			r.Key, r.Up, r.Reqs, r.Errs, r.Queued, r.Missing, r.P, r.C, r.T)
		tot.Reqs += r.Reqs
		tot.Errs += r.Errs
		tot.Queued += r.Queued
		tot.Missing += r.Missing
		tot.P += r.P
		tot.C += r.C
		tot.T += r.T
	}
	fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
		tot.Key, "", tot.Reqs, tot.Errs, tot.Queued, tot.Missing, tot.P, tot.C, tot.T)
}

// logFilesFor 列出要读的日志文件: 传目录则目录下全部 *.jsonl (按名排序),
// 传文件则它本身加最近归档 (覆盖 maxWindow 跨度)。
func logFilesFor(path string, maxWindow time.Duration) []string {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		ms, _ := filepath.Glob(filepath.Join(path, "*.jsonl"))
		sort.Strings(ms)
		return ms
	}
	return logFilesForScan(path, maxWindow)
}

// usageRow 是 -usage 的每行: 上游 + 真实模型 的 token 分口径累计。
type usageRow struct {
	Key                               string
	Reqs, Miss                        int
	Prompt, Completion, Cached, Total int
}

// runUsage 按 上游+真实模型 列出各窗口的 token 用量, 用于标定预算折算系数:
// 平台已用额度 / 该模型窗口内 total = 该模型的系数 (填进 budget.units)。
func runUsage(path string, windows []string) {
	maxWindow := time.Duration(0)
	for _, w := range windows {
		if d, ok := parseBudgetDur(w); ok && d > maxWindow {
			maxWindow = d
		}
	}
	type entry struct {
		ts    time.Time
		up    string
		model string
		usage Usage
		miss  bool
	}
	var entries []entry
	for _, fp := range logFilesFor(path, maxWindow) {
		f, err := os.Open(fp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec logRec
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Usage == nil {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, rec.Ts)
			if err != nil {
				continue
			}
			entries = append(entries, entry{ts: t, up: rec.Upstream, model: rec.Model, usage: *rec.Usage, miss: rec.Missing})
		}
		f.Close()
	}

	now := time.Now()
	groups := append(append([]string{}, windows...), "")
	for _, w := range groups {
		label, span := "全部", time.Duration(0)
		if w != "" {
			label = w
			span, _ = parseBudgetDur(w)
		}
		rows := map[string]*usageRow{}
		var order []string
		for _, e := range entries {
			if w != "" && now.Sub(e.ts) > span {
				continue
			}
			key := e.up + "/" + e.model
			r, ok := rows[key]
			if !ok {
				r = &usageRow{Key: key}
				rows[key] = r
				order = append(order, key)
			}
			r.Reqs++
			if e.miss {
				r.Miss++
			}
			r.Prompt += e.usage.Prompt
			r.Completion += e.usage.Completion
			r.Cached += e.usage.Cached
			r.Total += e.usage.Total
		}
		sort.Slice(order, func(i, j int) bool { return rows[order[i]].Total > rows[order[j]].Total })
		fmt.Printf("== 窗口 %s (仅含有 usage 的记录)\n", label)
		fmt.Printf("%-40s %6s %6s %9s %10s %9s %9s\n",
			"upstream/model", "reqs", "miss", "prompt", "completion", "cached", "total")
		for _, k := range order {
			r := rows[k]
			fmt.Printf("%-40s %6d %6d %9d %10d %9d %9d\n",
				r.Key, r.Reqs, r.Miss, r.Prompt, r.Completion, r.Cached, r.Total)
		}
	}
	fmt.Println("提示: 折算系数 = 平台已用额度 / 该模型窗口内 total; 按真实模型 id 填进对应上游的 budget.units")
}

func main() {
	cfgPath := flag.String("config", "config.local.json", "config file path")
	summary := flag.String("summary", "", "aggregate JSONL request log and exit")
	usage := flag.String("usage", "", "list token usage by upstream/model and window, then exit")
	windows := flag.String("windows", "3h,12h,24h", "windows for -usage, comma separated")
	check := flag.Bool("check", false, "validate config and exit")
	probe := flag.String("probe", "", "print the latency/probe report for a config file and exit")
	flag.Parse()

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

	// SIGHUP 热加载别名与上游 (F1)。
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
