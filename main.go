// llm-request-tollgate: 自研单文件 OpenAI 兼容代理 (设计文档 docs/design.md)。
//
// 单进程内含九件事 (设计 §4.1):
//  1. 别名表 (白名单 + 路由): 逻辑模型名 -> [(上游, 真实模型 id), ...],
//     未命中的模型名直接返 404, 不猜上游 (D2)。
//  2. 按上游分组的 v2 调度 (设计 §5.5, 一个上游一把 key, "上游+凭据" 等价于上游记录):
//     每个上游一条 FIFO 队列 + 自适应并发上限 (起点 1, 试探成功 +1), 出队节拍
//     interval_ms; 被挡下而没发出去的请求返 429 带 Retry-After (D3)。
//  3. 手动转发: 认证头替换 (D1), 删 Accept-Encoding 让 Transport 协商并
//     透明解压 (设计 §4.9.1), 不加 X-Forwarded-For (F9)。
//  4. 用量提取: 强写 stream_options.include_usage, 流末 usage chunk / 非流式
//     响应体 usage; 缺 usage 按 F7 字符口径兜底并计数。
//  5. 限制反应 (设计 §5.5.2): 上游报错按每个上游自己的规则分成并发 / 额度 / 其他
//     三类; 额度锁该候选 10min, 其他按 10/20/30s 退避、连续 4 次熔断锁 5min,
//     锁与退避状态落 var/gate-state.json (重启仍生效)。服务不记账, 只管写日志。
//  6. 响应整形: 非流 / 流式 chunk 里的 model 字段改回请求的逻辑名 (O9)。
//  7. 对外面: GET /v1/models 返回逻辑名清单 (O5); 日志记 source 标识 (O3);
//     -summary 汇总命令 (O2); -check 配置自检 + deploy/ 常驻与自检脚本 (O1)。
//  8. 集中 fallback: 候选按序尝试, 首字节前 429/5xx/传输失败换下一个, 全败透传最后错误。
//  9. 供应商锁: 锁内不真发, 排队中的请求整队迁移到下一个候选。
//  10. 探测与延迟观测 (D14, 只观测不接入): 真实请求被动记 ttfb_ms/tps; 开启 probe 后
//     只主动探测 fallback 顺序里的下一个候选 (scope=next), 结果进 GET /v1/probe 与
//     var/probe.json; 不改候选顺序、不动失败计数与锁, 已建立的流不迁移。
//
// 配置: config.local.json (gitignored, 软链到 ~/fff/config), SIGHUP 热加载别名与上游 (F1);
// key: 支持 key_env 字段只存环境变量名, 真 key 启动/热加载时从环境变量取, 不落盘。
// 日志: JSONL 追加写, 逐笔明细 (F5, F8); 按本地日历天轮转, 跨天归档为
// <base>-<YYYY-MM-DD>.jsonl, -summary 与 -usage 都能跨归档读。
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
}

type UpstreamCfg struct {
	ID           string            `json:"id"`
	BaseURL      string            `json:"base_url"`
	AuthHeader   string            `json:"auth_header"`
	Key          string            `json:"key"`
	KeyEnv       string            `json:"key_env,omitempty"`
	KeyPrefix    *string           `json:"key_prefix,omitempty"`
	Headers      map[string]string `json:"headers"`
	IncludeUsage bool              `json:"include_usage"`
	// Errors 是 v2 的报错分类规则 (设计 §4.8): 每个上游一份「状态码 + body 关键词」,
	// 用来把报错分成并发 / 额度 / 其他三类; 缺省空表 = 全部走通用兜底。
	Errors *ErrRules `json:"errors,omitempty"`
}

type Config struct {
	Listen    string                 `json:"listen"`
	LogPath   string                 `json:"log_path"`
	Timeouts  Timeouts               `json:"timeouts"`
	Gate      GateCfg                `json:"gate"`
	Upstreams []UpstreamCfg          `json:"upstreams"`
	Aliases   map[string][][2]string `json:"aliases"`
	Probe     *ProbeCfg              `json:"probe,omitempty"`
}

// ---------- v2 门控: 每上游一条队列 + 自适应并发上限 (设计 §5.5) ----------
//
// 这一段是 v2 的调度核心: 排队与出队节拍、自适应并发上限与超发试探、两把锁、报错归类。
// 转发主路径的接入与 v1 限速的删除在后续轮次做, 所以现在还没有调用方。

// GateCfg 是所有上游共用的门控参数 (设计 §5.5.4)。
type GateCfg struct {
	IntervalMs        int   `json:"interval_ms"`
	SemWaitMs         int   `json:"sem_wait_ms"`
	QueueWaitMs       int   `json:"queue_wait_ms"`
	ConcurrencyLockMs int   `json:"concurrency_lock_ms"`
	QuotaLockMs       int   `json:"quota_lock_ms"`
	BreakerLockMs     int   `json:"breaker_lock_ms"`
	RetryBackoffMs    []int `json:"retry_backoff_ms"`
}

// gateIntervalMs 是出队节拍的缺省值 (设计 §5.5.4: 固定 500ms, 不按上游配)。
const gateIntervalMs = 500

// normGate 按设计 §5.5.4 的缺省值补齐门控参数。
func normGate(g GateCfg) GateCfg {
	if g.IntervalMs <= 0 {
		g.IntervalMs = gateIntervalMs
	}
	if g.SemWaitMs <= 0 {
		g.SemWaitMs = 20000
	}
	if g.QueueWaitMs <= 0 {
		g.QueueWaitMs = 10000
	}
	if g.ConcurrencyLockMs <= 0 {
		g.ConcurrencyLockMs = 3600000
	}
	if g.QuotaLockMs <= 0 {
		g.QuotaLockMs = 600000
	}
	if g.BreakerLockMs <= 0 {
		g.BreakerLockMs = 300000
	}
	if len(g.RetryBackoffMs) == 0 {
		g.RetryBackoffMs = []int{10000, 20000, 30000}
	}
	return g
}

// 配置里写的是毫秒, 调度里用 Duration。
func (g GateCfg) interval() time.Duration { return time.Duration(g.IntervalMs) * time.Millisecond }
func (g GateCfg) semWait() time.Duration  { return time.Duration(g.SemWaitMs) * time.Millisecond }
func (g GateCfg) queueWait() time.Duration {
	return time.Duration(g.QueueWaitMs) * time.Millisecond
}
func (g GateCfg) concurrencyLock() time.Duration {
	return time.Duration(g.ConcurrencyLockMs) * time.Millisecond
}
func (g GateCfg) quotaLock() time.Duration {
	return time.Duration(g.QuotaLockMs) * time.Millisecond
}
func (g GateCfg) breakerLock() time.Duration {
	return time.Duration(g.BreakerLockMs) * time.Millisecond
}

// backoff 返回第 n 次连续失败该暂停多久; 超出序列长度返回 0 (那一次该进熔断)。
func (g GateCfg) backoff(n int) time.Duration {
	if n < 1 || n > len(g.RetryBackoffMs) {
		return 0
	}
	return time.Duration(g.RetryBackoffMs[n-1]) * time.Millisecond
}

// ErrRule 是一条错误特征: 状态码命中, 或 body 里出现任一关键词, 即算命中 (OR)。
type ErrRule struct {
	Status   []int    `json:"status,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
}

// ErrRules 是每个上游一份的错误分类规则 (设计 §4.8)。
type ErrRules struct {
	Concurrency *ErrRule `json:"concurrency,omitempty"`
	Quota       *ErrRule `json:"quota,omitempty"`
}

// errClass 是三类特殊限制加一类通用兜底 (设计 §5.5.2)。
type errClass int

const (
	classOther       errClass = iota // 通用兜底: 退避与熔断
	classConcurrency                 // 并发限制: 锁掉超发试探
	classQuota                       // 额度限制: 锁整个候选
)

// match 判定一条规则是否命中; 规则缺失时恒不命中。
func (r *ErrRule) match(status int, body []byte) bool {
	if r == nil {
		return false
	}
	for _, s := range r.Status {
		if s == status {
			return true
		}
	}
	if len(r.Keywords) == 0 {
		return false
	}
	low := strings.ToLower(string(body))
	for _, kw := range r.Keywords {
		if kw != "" && strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// classify 按该上游自己的规则给报错归类 (设计 §5.5.2); 没配规则就是通用兜底。
func (u *UpstreamCfg) classify(status int, body []byte) errClass {
	if u.Errors != nil {
		if u.Errors.Concurrency.match(status, body) {
			return classConcurrency
		}
		if u.Errors.Quota.match(status, body) {
			return classQuota
		}
	}
	return classOther
}

// minSoftCap 是并发上限的起点: 每次重启从 1 重新爬 (设计 §5.5.2)。
const minSoftCap = 1

// lockState 是一个到期时间戳加一个原因标记 (设计 §5.5.3)。
type lockState struct {
	until  time.Time
	reason string
}

func (l lockState) active(now time.Time) bool { return now.Before(l.until) }

func (l lockState) left(now time.Time) time.Duration {
	if !l.active(now) {
		return 0
	}
	return l.until.Sub(now)
}

// ticket 是候选队列里的一项; enq 记入队时刻, 用来算排队超时。
type ticket struct {
	c   *cand
	enq time.Time
}

// cand 是每个上游的调度状态: 一条 FIFO 队列 + 自适应并发上限 + 两把锁 (设计 §5.5.2/§5.5.3)。
type cand struct {
	id string

	mu       sync.Mutex
	queue    []*ticket
	inFlight int       // 已拿到信号量的请求数 (含还在排队的)
	softCap  int       // 自适应并发上限: 起点 1, 不设天花板
	probing  bool      // 是否已有试探在飞 (同一时刻只允许一笔)
	nextSend time.Time // 下一笔最早可发送的时刻 (出队节拍)
	// 通用兜底 (设计 §5.5.2): 退避是候选级的, 不是每笔请求各自退避; 退避期间新请求照常
	// 进队列, 但队首在暂停结束前不会被放行 —— 排队上限只有 10s, 所以 20s/30s 的退避
	// 基本等不到, 请求会换下一个候选。
	pausedUntil time.Time
	errStreak   int       // 连续错误计数, 成功清零
	provLock    lockState // 定时供应商锁: 额度 10min / 熔断 5min
	concLock    lockState // 并发上限锁: 只锁超发试探
	wake        chan struct{}
}

func newCand(id string) *cand {
	return &cand{id: id, softCap: minSoftCap, wake: make(chan struct{}, 1)}
}

// signal 唤醒一个等待者; 通道容量 1, 满了说明已经有人会醒来重查。
func (c *cand) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// slotKind 说明这一笔是正常占用信号量, 还是并发满时的超发试探 (设计 §5.5.2)。
type slotKind int

const (
	slotCapped slotKind = iota // 正常占用信号量, 结束时归还
	slotProbe                  // 试探: 不占信号量, 失败无需释放
)

// wakeReason 是一次等待的结束原因。
type wakeReason int

const (
	wakeNotify   wakeReason = iota // 状态可能变了, 重查
	wakeDeadline                   // 等到了期限
	wakeCanceled                   // ctx 结束 (客户端断开或取消)
)

// sleep 等到 deadline 或 until (谁先到) 或一次唤醒; ctx 结束直接返回。
func (c *cand) sleep(ctx context.Context, deadline, until time.Time) wakeReason {
	now := time.Now()
	if !now.Before(deadline) {
		return wakeDeadline
	}
	d := deadline.Sub(now)
	if !until.IsZero() {
		if til := until.Sub(now); til < d {
			d = til
		}
	}
	if d <= 0 {
		d = time.Millisecond // 已经该动的情况: 短重查一次, 不空转
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return wakeCanceled
	case <-c.wake:
		return wakeNotify
	case <-t.C:
		if !time.Now().Before(deadline) {
			return wakeDeadline
		}
		return wakeNotify
	}
}

// acquire 按设计 §5.5.1 拿到发送资格: 先取信号量 (a), 再排队等轮到自己 (b)。
// ok=false 时 why 说明原因 (locked / sem_timeout / queue_timeout / canceled), 上层换下一个候选;
// semMs / queueMs 是两段等待的毫秒数, 只用于记日志。
func (c *cand) acquire(ctx context.Context, g GateCfg) (kind slotKind, why string, semMs, queueMs int64, ok bool) {
	start := time.Now()
	kind, why, ok = c.acquireSlot(ctx, g)
	semMs = time.Since(start).Milliseconds()
	if !ok {
		return kind, why, semMs, 0, false
	}
	start = time.Now()
	if why, ok = c.waitTurn(ctx, g); !ok {
		c.finish(kind, false) // 没轮到就退出: 归还信号量 (试探不占, 无需归还)
		return kind, why, semMs, time.Since(start).Milliseconds(), false
	}
	return kind, "", semMs, time.Since(start).Milliseconds(), true
}

// acquireSlot 取信号量: 有余量直接占; 占满时先试探 (纯超发), 不允许试探才等 semWaitMs
// (设计 §5.5.1 的 a 与 §5.5.2 的并发限制)。
func (c *cand) acquireSlot(ctx context.Context, g GateCfg) (slotKind, string, bool) {
	deadline := time.Now().Add(g.semWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if c.provLock.active(now) {
			c.mu.Unlock()
			return slotCapped, "locked", false
		}
		if c.inFlight < c.softCap {
			c.inFlight++
			c.mu.Unlock()
			return slotCapped, "", true
		}
		if !c.concLock.active(now) && !c.probing {
			// 并发满, 但试探没被锁也没有别的试探在飞: 这一笔超发, 不占信号量。
			c.probing = true
			c.mu.Unlock()
			return slotProbe, "", true
		}
		c.mu.Unlock()
		switch c.sleep(ctx, deadline, time.Time{}) {
		case wakeCanceled:
			return slotCapped, "canceled", false
		case wakeDeadline:
			return slotCapped, "sem_timeout", false
		}
	}
}

// waitTurn 入队等轮到自己, 并保证出队节拍 (设计 §5.5.1 的 b / b1, §5.5.2 的流量限制)。
func (c *cand) waitTurn(ctx context.Context, g GateCfg) (string, bool) {
	t := &ticket{c: c, enq: time.Now()}
	c.mu.Lock()
	c.queue = append(c.queue, t)
	c.mu.Unlock()
	c.signal()
	deadline := t.enq.Add(g.queueWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if c.provLock.active(now) {
			// 排队期间该候选被锁: 退出队列, 由上层换下一个候选 (整队迁移, §5.5.3)。
			c.removeLocked(t)
			c.mu.Unlock()
			c.signal()
			return "locked", false
		}
		if len(c.queue) > 0 && c.queue[0] == t &&
			!now.Before(c.nextSend) && !now.Before(c.pausedUntil) {
			c.queue = c.queue[1:]
			c.nextSend = now.Add(g.interval())
			c.mu.Unlock()
			c.signal()
			return "", true
		}
		next := c.nextSend
		if c.pausedUntil.After(next) {
			next = c.pausedUntil // 退避暂停: 队首也要等到暂停结束
		}
		c.mu.Unlock()
		switch c.sleep(ctx, deadline, next) {
		case wakeCanceled:
			c.dropTicket(t)
			return "canceled", false
		case wakeDeadline:
			c.dropTicket(t)
			return "queue_timeout", false
		}
	}
}

// dropTicket 把一项从队列里摘掉 (客户端断开或排队超时)。
func (c *cand) dropTicket(t *ticket) {
	c.mu.Lock()
	c.removeLocked(t)
	c.mu.Unlock()
	c.signal()
}

// removeLocked 摘队列项; 调用方持有 c.mu。
func (c *cand) removeLocked(t *ticket) {
	for i, x := range c.queue {
		if x == t {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return
		}
	}
}

// finish 结束一笔发送: 正常占用的归还槽位, 试探按结果放宽上限 (设计 §5.5.2)。
func (c *cand) finish(kind slotKind, ok bool) {
	c.mu.Lock()
	if kind == slotProbe {
		c.probing = false
		if ok {
			c.softCap++ // 试探成功 → 上限 +1, 不设天花板也不落盘
		}
	} else {
		c.inFlight--
	}
	c.mu.Unlock()
	c.signal()
}

// lockProvider 加定时供应商锁: 额度与熔断共用这一机制, 只是时长不同, 取更晚的到期时间
// (设计 §5.5.3); 唤醒排队者, 让它们退出队列换下一个候选。
func (c *cand) lockProvider(reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	if until.After(c.provLock.until) {
		c.provLock = lockState{until: until, reason: reason}
	}
	c.mu.Unlock()
	c.signal()
}

// lockConcurrency 锁掉超发试探: 候选本身照常用 (设计 §5.5.3)。
func (c *cand) lockConcurrency(reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	if until.After(c.concLock.until) {
		c.concLock = lockState{until: until, reason: reason}
	}
	c.mu.Unlock()
}

// locked 报告该候选是否在供应商锁内, 以及还剩多久、因为什么。
func (c *cand) locked() (bool, time.Duration, string) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.provLock.active(now), c.provLock.left(now), c.provLock.reason
}

// recordSuccess 清掉连续错误计数 (设计 §5.5.2 通用兜底)。
func (c *cand) recordSuccess() {
	c.mu.Lock()
	c.errStreak = 0
	c.mu.Unlock()
}

// recordOther 记一次通用兜底失败: 连续计数 +1, 并按 retry_backoff_ms 暂停该候选;
// 计数超过退避序列长度时报告该熔断 (设计 §5.5.2)。
func (c *cand) recordOther(g GateCfg) (breaker bool) {
	c.mu.Lock()
	c.errStreak++
	n := c.errStreak
	if pause := g.backoff(n); pause > 0 {
		if until := time.Now().Add(pause); until.After(c.pausedUntil) {
			c.pausedUntil = until
		}
	}
	c.mu.Unlock()
	c.signal()
	return n > len(g.RetryBackoffMs)
}

// ---------- v2 锁状态落盘 (设计 §5.5.3) ----------

// gateLockRec 是落盘的一条定时供应商锁。
type gateLockRec struct {
	UntilMs int64  `json:"until_unix_ms"`
	Reason  string `json:"reason"`
}

// gateState 是 gate-state.json 的内容: 只存未到期的定时供应商锁。
type gateState struct {
	UpdatedAt string                 `json:"updated_at"`
	Locks     map[string]gateLockRec `json:"locks"`
}

// lockProvider 给候选加定时供应商锁, 并把锁状态落盘 —— 重启后仍在锁内 (设计 §5.5.3)。
func (a *App) lockProvider(c *cand, reason string, dur time.Duration) {
	c.lockProvider(reason, dur)
	a.saveGateState()
}

// saveGateState 把未到期的供应商锁写盘 (先写临时文件再改名, 与探测状态同一套路)。
func (a *App) saveGateState() {
	if a.gatePath == "" {
		return
	}
	now := time.Now()
	st := gateState{UpdatedAt: now.Format(time.RFC3339Nano), Locks: map[string]gateLockRec{}}
	a.mu.RLock()
	cands := make([]*cand, 0, len(a.cands))
	for _, c := range a.cands {
		cands = append(cands, c)
	}
	a.mu.RUnlock()
	for _, c := range cands {
		if locked, left, reason := c.locked(); locked {
			st.Locks[c.id] = gateLockRec{UntilMs: now.Add(left).UnixMilli(), Reason: reason}
		}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := a.gatePath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		log.Printf("gate state write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, a.gatePath); err != nil {
		log.Printf("gate state rename failed: %v", err)
	}
}

// loadGateState 启动时恢复未到期的供应商锁; 已过期的直接丢掉。
func (a *App) loadGateState() {
	if a.gatePath == "" {
		return
	}
	b, err := os.ReadFile(a.gatePath)
	if err != nil {
		return
	}
	var st gateState
	if json.Unmarshal(b, &st) != nil || st.Locks == nil {
		return
	}
	now := time.Now()
	restored := 0
	a.mu.RLock()
	defer a.mu.RUnlock()
	for id, rec := range st.Locks {
		c, ok := a.cands[id]
		if !ok {
			continue
		}
		until := time.UnixMilli(rec.UntilMs)
		if !until.After(now) {
			continue
		}
		c.mu.Lock()
		c.provLock = lockState{until: until, reason: rec.Reason}
		c.mu.Unlock()
		restored++
	}
	if restored > 0 {
		log.Printf("gate: restored %d provider locks from %s", restored, a.gatePath)
	}
}

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
	SemMs     int64   `json:"sem_ms,omitempty"` // 等并发信号量的时长 (v2)
	Client    string  `json:"client"`
	UA        string  `json:"ua,omitempty"`
	Source    string  `json:"source,omitempty"`
	Fallbacks int     `json:"fallbacks,omitempty"`
	Err       string  `json:"err,omitempty"`
	Gate      string  `json:"gate,omitempty"` // 被闸门挡下没发出去时: locked | sem_timeout | queue_timeout | canceled
	LockSkips int     `json:"lock_skips,omitempty"`
	TtfbMs    int64   `json:"ttfb_ms,omitempty"` // 首字节时延 (被动采样)
	Tps       float64 `json:"tps,omitempty"`     // 解码吞吐 tokens/s (流式才有)
}

// 不动失败计数与锁, 也不打断已建立的流 (切换只可能发生在下次请求开始时)。
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

// probeState 状态文件 (与 gate-state.json 同目录, 重启不丢)。
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

type App struct {
	mu        sync.RWMutex
	cfg       *Config
	upstreams map[string]*UpstreamCfg
	aliases   map[string][]aliasTarget
	// v2 调度: 每个上游一份队列与自适应并发上限, 门控参数对所有上游共用。
	cands     map[string]*cand
	gate      GateCfg
	transport *http.Transport
	streamIdl time.Duration

	gatePath string // v2 供应商锁状态文件 (var/gate-state.json)

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
	logDay string // 当前日志文件所属的本地日期 (按天轮转用)

	probeMu   sync.Mutex
	probe     map[string]*probeStat // "上游|模型" -> 滚动统计 (主动探测 + 被动观测)
	probePath string                // 指标状态文件 (var/probe.json)
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

// probePathFor 指标状态文件与 gate-state.json 同目录 (var/)。
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

// saveProbeState 原子写 (临时文件 + rename), 与锁状态同一套路。
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
// 与流末 usage, 得到 ttfb_ms 与 tps; 全程不碰失败计数与锁。
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
	a.mu.RLock()
	cd, gate := a.cands[up.ID], a.gate
	a.mu.RUnlock()
	if cd == nil {
		sample.Err = "no scheduler"
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	// 探测自己排队, 最多等 5s; 排不上就跳过这一轮, 不占用户请求的信号量与排队预算。
	gateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	kind, why, _, _, ok := cd.acquire(gateCtx, gate)
	cancel()
	if !ok {
		sample.Err = "gate busy: " + why
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	probeOK := false
	defer func() { cd.finish(kind, probeOK) }()

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
	probeOK = true
	sample.TtfbMs = msCeil(firstAt.Sub(start))
	if usage != nil {
		sample.Tokens = usage.Completion
		if d := lastAt.Sub(firstAt); d > 0 && usage.Completion > 0 {
			sample.Tps = float64(usage.Completion) / d.Seconds()
		}
	}
	a.recordProbeSample(up.ID, model, sample, c.Window)
	// 探测流量如实写请求日志 (source=probe): 它真的消耗配额, 用量交给外部程序分析。
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
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		cands:     map[string]*cand{},
		probe:     map[string]*probeStat{},
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
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		upstreams[u.ID] = u
		// 调度状态按上游 id 保留不重建: 队列里排着的请求、爬到的并发上限、退避与锁
		// 都该跨热加载延续 (上限有意不落盘, 但 hup 不该把它清掉)。
		if _, ok := a.cands[u.ID]; !ok {
			a.cands[u.ID] = newCand(u.ID)
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

	// 集中 fallback 的候选表: 上游 + 真实模型 + 该候选自己的 v2 调度状态。一把 RLock 拿齐,
	// 循环里不再读 a.upstreams / a.cands (热加载会改这两张表)。
	type attempt struct {
		up  *UpstreamCfg
		mod string
		cd  *cand
	}
	a.mu.RLock()
	alias, aliasOK := a.aliases[logical]
	gate := a.gate
	var chosen *UpstreamCfg
	realModel := ""
	attempts := make([]attempt, 0, len(alias))
	for _, tgt := range alias {
		u, ok := a.upstreams[tgt.Upstream]
		if !ok {
			continue
		}
		if chosen == nil {
			chosen, realModel = u, tgt.Model
		}
		attempts = append(attempts, attempt{u, tgt.Model, a.cands[u.ID]})
	}
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

	// v2 调度 (设计 §5.5): 闸门由**当前候选**承担 —— 换候选就重新取信号量、重新排队,
	// 每换一个候选重新给 sem_wait_ms + queue_wait_ms (没有整请求总预算)。关键: 换候选前
	// 必须归还上一个候选的槽位, 否则会同时占着两个上游互相等 (死锁)。
	var (
		semMs      int64         // 各候选等信号量时长之和
		queueMs    int64         // 各候选排队时长之和
		gateReject string        // 非空 = 被闸门挡下没发出去: locked | sem_timeout | queue_timeout | canceled
		gateLeft   time.Duration // 被挡下时最长的锁剩余时间 (Retry-After 用)
		heldCand   *cand         // 已拿到资格、正在服务的那个候选
		heldKind   slotKind
	)
	defer func() {
		if heldCand != nil {
			heldCand.finish(heldKind, false) // 响应发完才归还槽位 (ok 只对试探有意义)
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
	lockedSkips := 0
	// 集中 fallback: 按候选顺序尝试, 只在还没给客户端写任何字节前 (传输失败 /
	// 上游 429 或 5xx) 换下一个候选; 全部失败保留最近一次错误原样透传。
	for _, at := range attempts {
		if at.cd == nil {
			lastErr = fmt.Errorf("upstream %s has no scheduler", at.up.ID)
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		if locked, left, reason := at.cd.locked(); locked {
			// 供应商锁内不真发 (额度 10min / 熔断 5min), 直接换下一个候选。
			lastErr = fmt.Errorf("upstream %s locked for %s (%s)",
				at.up.ID, left.Round(time.Second), reason)
			lastUpID, lastM = at.up.ID, at.mod
			if left > gateLeft {
				gateLeft = left
			}
			lockedSkips++
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
		// 候选内的重试: e2 的「退避后回到出队」就是重新排队 —— 退避 (10/20/30s) 超过
		// 排队上限 (10s) 时自然变成 queue_timeout, 于是换下一个候选 (设计 §5.5.1/§5.5.2)。
		for {
			// 每轮都重建出站请求: 上一轮的 body 已经被读过, 复用同一个 Request 会带空体。
			outReq, err := http.NewRequestWithContext(r.Context(), r.Method,
				at.up.BaseURL+upPath, bytes.NewReader(body))
			if err != nil {
				lastErr = err
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				break
			}
			outReq.ContentLength = int64(len(body))
			prepareOutboundHeaders(outReq.Header, r.Header, at.up)
			// 取资格放在请求体准备之后, 构造失败就不白占槽位。
			kind, why, sem, qms, ok := at.cd.acquire(r.Context(), gate)
			semMs += sem
			queueMs += qms
			if !ok {
				gateReject = why
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				if why == "locked" {
					lockedSkips++
					if _, left, _ := at.cd.locked(); left > gateLeft {
						gateLeft = left
					}
				}
				break // 换下一个候选
			}
			attemptStart = time.Now()
			resp, err = a.transport.RoundTrip(outReq)
			if err != nil {
				// 传输层失败: 没有状态码与 body 可判, 按通用兜底走 (设计 §5.5.2)。
				at.cd.finish(kind, false)
				if at.cd.recordOther(gate) {
					a.lockProvider(at.cd, "breaker", gate.breakerLock())
					at.cd.recordSuccess()
				}
				lastErr = err
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				continue
			}
			if shouldFallback(resp.StatusCode) {
				b, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				at.cd.finish(kind, false)
				moveOn := true
				switch at.up.classify(resp.StatusCode, b) {
				case classQuota:
					// 额度限制: 锁整个候选 10min, 排队的请求随之迁移 (设计 §5.5.2)。
					a.lockProvider(at.cd, "quota", gate.quotaLock())
				case classConcurrency:
					// 并发限制: 锁掉超发试探 1h, 不等那 20s, 直接换下一个候选。
					at.cd.lockConcurrency("concurrency", gate.concurrencyLock())
				default:
					// 通用兜底: 连续计数 +1 并按序列退避; 到阈值熔断 (锁 5min)。
					moveOn = false
					if at.cd.recordOther(gate) {
						a.lockProvider(at.cd, "breaker", gate.breakerLock())
						at.cd.recordSuccess()
						moveOn = true
					}
				}
				lastErr = fmt.Errorf("upstream %s returned %d", at.up.ID, resp.StatusCode)
				lastStatus, lastHdr, lastBody = resp.StatusCode, resp.Header.Clone(), b
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				if rerr != nil {
					lastErr = rerr
				}
				if !moveOn {
					continue // e2: 退避后回到出队, 重试同一个候选
				}
				break
			}
			at.cd.recordSuccess()
			if kind == slotProbe {
				at.cd.finish(slotProbe, true) // 试探成功: 上限 +1 (不设天花板)
			} else {
				heldCand, heldKind = at.cd, kind // 响应发完才归还槽位
			}
			servedUp, servedMod = at.up, at.mod
			break
		}
		if servedUp != nil {
			break
		}
	}
	queued := queueMs >= 5

	if servedUp == nil && gateReject != "" && lastStatus == 0 {
		// 候选链试完仍没拿到发送资格 (被锁 / 等不到信号量 / 排队超时): 合成 429 说明原因;
		// 请求根本没发出去, 所以不能透传上游的错误。
		retry := gate.queueWait()
		if gateLeft > 0 {
			retry = gateLeft
		}
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"upstream busy ("+gateReject+"): no candidate for "+logical+" gave a send slot")
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
			Status: http.StatusTooManyRequests, Missing: true,
			Queued: queued, QueueMs: queueMs, SemMs: semMs,
			Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
			Source: r.Header.Get("X-Tollgate-Source"),
			Err:    "rate_limited", Gate: gateReject, Fallbacks: failed,
			LockSkips: lockedSkips,
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
				Status: lastStatus, Missing: true, Queued: queued, QueueMs: queueMs, SemMs: semMs,
				Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
				Source: r.Header.Get("X-Tollgate-Source"),
				Err:    lastErr.Error(), Fallbacks: failed, LockSkips: lockedSkips,
			})
			return
		}
		// 候选全在供应商锁里: 回 429 + Retry-After 让 agent 退避 (不是我们的闸门,
		// 而是上游限制的传导), 比 502 更贴近真实原因。
		if lockedSkips > 0 && lastStatus == 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(gateLeft.Seconds())))))
			writeError(w, http.StatusTooManyRequests, "upstream_locked",
				"all candidates for "+logical+" are locked after upstream limits")
			a.logReq(logRec{
				Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
				Upstream: lastUpID, Model: lastM, Path: r.URL.Path, Stream: stream,
				Status: http.StatusTooManyRequests, Missing: true,
				Queued: queued, QueueMs: queueMs, SemMs: semMs,
				Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
				Source: r.Header.Get("X-Tollgate-Source"),
				Err:    "upstream_locked", Fallbacks: failed, LockSkips: lockedSkips,
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
			Status: http.StatusBadGateway, Missing: true, Queued: queued, QueueMs: queueMs, SemMs: semMs,
			Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
			Source: r.Header.Get("X-Tollgate-Source"),
			Err:    msg, Fallbacks: failed, LockSkips: lockedSkips,
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
		SemMs:  semMs,
		Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"), Source: r.Header.Get("X-Tollgate-Source"),
		Fallbacks: failed, LockSkips: lockedSkips,
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

// parseDur 解析 "3h" / "12h" / "90m" / "7d" 这类窗口时长 (-usage 的窗口参数用)。
func parseDur(s string) (time.Duration, bool) {
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

// runUsage 按 上游+真实模型 列出各窗口的 token 用量 (服务只写日志, 分析在服务之外)。
func runUsage(path string, windows []string) {
	maxWindow := time.Duration(0)
	for _, w := range windows {
		if d, ok := parseDur(w); ok && d > maxWindow {
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
			span, _ = parseDur(w)
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
	fmt.Println("提示: 服务只写日志, 不做预算与折算; 额度分析交给独立的分析程序 (设计 O10, 尚未实现)")
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
