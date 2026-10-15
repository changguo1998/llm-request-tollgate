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
//     三类; 额度锁整个上游 10min (额度是 key 级共享的), 其他按 10/20/30s 退避、连续
//     4 次熔断, 熔断只锁出错的 上游/模型 5min (同家别的模型不受影响),
//     锁与退避状态落 var/gate-state.json (重启仍生效)。服务不记账, 只管写日志。
//  6. 响应整形: 非流 / 流式 chunk 里的 model 字段改回请求的逻辑名 (O9)。
//  7. 对外面: GET /v1/models 返回逻辑名清单 (O5); 日志记 source 标识 (O3);
//     -summary 汇总命令 (O2); -check 配置自检 + deploy/ 常驻与自检脚本 (O1)。
//  8. 集中 fallback: 候选按序尝试, 首字节前 429/5xx/传输失败换下一个, 全败透传最后错误。
//  9. 定时锁: 熔断只锁出错的 上游/模型, 额度锁整个上游; 锁内不真发, 排队中的请求整队
//     迁移到下一个候选。
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

// ErrRule 是一条错误分类规则: 状态码在 Status 里 (缺省 = 任意状态码), 且匹配文本里出现
// Contains 里任一子串, 两者都满足才算命中 (docs/providers-api.md §1.2)。
// 状态码只用来收敛候选集, 定类必须落到 body 的结构化字段或关键词 —— 多家的 429 是
// 「限流与额度共用」的同一个码, 只看状态码就锁候选会误判。
type ErrRule struct {
	Status   []int    `json:"status,omitempty"`
	Contains []string `json:"contains,omitempty"`
}

// ErrRules 是每个上游一份的错误分类规则, 形状见 docs/providers-api.md §3.1。
// 只保留两档会触发特殊动作的类: 额度 (锁候选 10min) 与并发 (锁超发试探 1h);
// 调研文档里的可选 `rate` 档动作与通用兜底一致, 所以不单独建类 (配了也不生效)。
type ErrRules struct {
	Quota       []ErrRule `json:"quota,omitempty"`
	Concurrency []ErrRule `json:"concurrency,omitempty"`
}

// errClass 是三类特殊限制加一类通用兜底 (设计 §5.5.2)。
type errClass int

const (
	classOther       errClass = iota // 通用兜底: 退避与熔断
	classConcurrency                 // 并发限制: 锁掉超发试探
	classQuota                       // 额度限制: 锁整个候选
)

// errMatchText 拼出关键词匹配用的文本: error.type + error.code + error.message 转小写;
// body 不是 JSON, 或没有 error 对象 (有的上游回纯文本) 时退回整段 body
// (docs/providers-api.md §1.2 第 2/3 步 —— 不同上游的关键词落在不同字段上)。
func errMatchText(body []byte) string {
	if len(body) > 8*1024 {
		body = body[:8*1024] // 只用于匹配, 截断到 8KB
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil &&
		(env.Error.Message != "" || env.Error.Type != "" || len(env.Error.Code) > 0) {
		// code 有的是字符串 ("QuotaExceeded.X"), 有的是数字; 去掉引号后当文本拼进去。
		code := strings.Trim(string(env.Error.Code), `"`)
		return strings.ToLower(strings.Join(
			[]string{env.Type, env.Error.Type, code, env.Error.Message}, " "))
	}
	return strings.ToLower(string(body))
}

// match 判定一条规则是否命中 (status 与 contains 是与关系); 空规则恒不命中。
func (r ErrRule) match(status int, text string) bool {
	if len(r.Status) == 0 && len(r.Contains) == 0 {
		return false // 空规则: 别当成"命中一切"
	}
	if len(r.Status) > 0 {
		hit := false
		for _, s := range r.Status {
			if s == status {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(r.Contains) == 0 {
		return true // 只写状态码的规则: 语义明确, 但额度类不该这么配 (§1.3)
	}
	for _, kw := range r.Contains {
		if kw != "" && strings.Contains(text, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// classify 按该上游自己的规则给报错归类 (设计 §5.5.2); 没配规则就是通用兜底。
// 额度类先判: 它不可重试, 必须先于并发/限流类 (docs/providers-api.md §3.2 第 1 条)。
func (u *UpstreamCfg) classify(status int, body []byte) errClass {
	if u.Errors != nil {
		text := errMatchText(body)
		for _, r := range u.Errors.Quota {
			if r.match(status, text) {
				return classQuota
			}
		}
		for _, r := range u.Errors.Concurrency {
			if r.match(status, text) {
				return classConcurrency
			}
		}
	}
	return classOther
}

// bodySnippet 取 body 前 n 个字符用于日志, 去掉换行与回车免得把日志行撑破。
func bodySnippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// minSoftCap 是并发上限的起点: 每次重启从 1 重新爬 (设计 §5.5.2)。
const minSoftCap = 1

// lockAll 是定时锁里的"整家"作用域 (键): 额度是 key 级事实, 命中就锁整个上游;
// 熔断按 upstream/model 粒度锁, 键写具体的真实模型 id (设计 §5.5.3)。
const lockAll = "*"

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
// 退避、熔断计数与熔断锁按模型分开记 (modelGates), 所以同家一个模型坏了不挡其它模型。
type cand struct {
	id string

	mu       sync.Mutex
	queue    []*ticket
	inFlight int       // 已拿到信号量的请求数 (含还在排队的)
	softCap  int       // 自适应并发上限: 起点 1, 不设天花板
	probing  bool      // 是否已有一笔试探在进行 (同一时刻只允许一笔)
	nextSend time.Time // 下一笔最早可发送的时刻 (出队节拍)
	// 模型级状态: 键是真实模型 id。锁查询不建表, 记账 (失败/成功/加锁) 才建。
	modelGates map[string]*modelGate
	concLock   lockState // 并发上限锁: 只锁超发试探
	wake       chan struct{}
}

// modelGate 是一个 upstream/model 的失败状态 (设计 §5.5.2 通用兜底 / §5.5.3 熔断锁)。
type modelGate struct {
	fails       int                  // 连续错误计数, 成功清零
	pausedUntil time.Time            // 退避到什么时候: 退避期间该模型不出队
	locks       map[string]lockState // 键 lockAll = 整家 (额度 10min); 其它 = 该模型的熔断锁 5min
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
func (c *cand) acquire(ctx context.Context, g GateCfg, model string) (kind slotKind, why string, semMs, queueMs int64, ok bool) {
	start := time.Now()
	kind, why, ok = c.acquireSlot(ctx, g, model)
	semMs = time.Since(start).Milliseconds()
	if !ok {
		return kind, why, semMs, 0, false
	}
	start = time.Now()
	if why, ok = c.waitTurn(ctx, g, model); !ok {
		c.finish(kind, false) // 没轮到就退出: 归还信号量 (试探不占, 无需归还)
		return kind, why, semMs, time.Since(start).Milliseconds(), false
	}
	return kind, "", semMs, time.Since(start).Milliseconds(), true
}

// acquireSlot 取信号量: 有余量直接占; 占满时先试探 (纯超发), 不允许试探才等 semWaitMs
// (设计 §5.5.1 的 a 与 §5.5.2 的并发限制)。
func (c *cand) acquireSlot(ctx context.Context, g GateCfg, model string) (slotKind, string, bool) {
	deadline := time.Now().Add(g.semWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if locked, _, _ := c.lockedIn(model, now); locked {
			c.mu.Unlock()
			return slotCapped, "locked", false
		}
		if c.inFlight < c.softCap {
			c.inFlight++
			c.mu.Unlock()
			return slotCapped, "", true
		}
		if !c.concLock.active(now) && !c.probing {
			// 并发满, 但试探没被锁、也没有别的试探在进行: 这一笔超发, 不占信号量。
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
func (c *cand) waitTurn(ctx context.Context, g GateCfg, model string) (string, bool) {
	t := &ticket{c: c, enq: time.Now()}
	c.mu.Lock()
	c.queue = append(c.queue, t)
	c.mu.Unlock()
	c.signal()
	deadline := t.enq.Add(g.queueWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if locked, _, _ := c.lockedIn(model, now); locked {
			// 排队期间该候选被锁: 退出队列, 由上层换下一个候选 (整队迁移, §5.5.3)。
			c.removeLocked(t)
			c.mu.Unlock()
			c.signal()
			return "locked", false
		}
		if len(c.queue) > 0 && c.queue[0] == t &&
			!now.Before(c.nextSend) && !now.Before(c.pauseOf(model)) {
			c.queue = c.queue[1:]
			c.nextSend = now.Add(g.interval())
			c.mu.Unlock()
			c.signal()
			return "", true
		}
		next := c.nextSend
		if pause := c.pauseOf(model); pause.After(next) {
			next = pause // 退避暂停: 队首也要等到暂停结束
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

// gate 拿某个模型的失败状态 (没有就建); 调用方持有 c.mu。
func (c *cand) gate(model string) *modelGate {
	if c.modelGates == nil {
		c.modelGates = map[string]*modelGate{}
	}
	g := c.modelGates[model]
	if g == nil {
		g = &modelGate{locks: map[string]lockState{}}
		c.modelGates[model] = g
	}
	return g
}

// peekGate 只读地拿某个模型的失败状态 (没有就返回 nil); 调用方持有 c.mu。
func (c *cand) peekGate(model string) *modelGate {
	return c.modelGates[model]
}

// pauseOf 是该模型的退避截止时刻 (从没退避过就是零值); 调用方持有 c.mu。
func (c *cand) pauseOf(model string) time.Time {
	if g := c.peekGate(model); g != nil {
		return g.pausedUntil
	}
	return time.Time{}
}

// lockedIn 是锁查询的加锁版实现: 先看锁在它自己头上的熔断锁, 再看整家锁 (额度 10min)。
// 两把同时在时以模型锁为准 —— 它更具体, 而且还剩多久与整家锁无关。调用方必须持有 c.mu
// (locked 与队列里的检查都走它)。
func (c *cand) lockedIn(model string, now time.Time) (bool, time.Duration, string) {
	if g := c.peekGate(model); g != nil {
		if l := g.locks[model]; l.active(now) {
			return true, l.left(now), l.reason
		}
	}
	if g := c.peekGate(lockAll); g != nil {
		if l := g.locks[lockAll]; l.active(now) {
			return true, l.left(now), l.reason
		}
	}
	return false, 0, ""
}

// locked 报告该模型现在能不能真发 (自己加锁)。
func (c *cand) locked(model string) (bool, time.Duration, string) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lockedIn(model, now)
}

// lock 加一把定时锁并唤醒排队者, 让它们退出队列换下一个候选。scope 是 lockAll
// (额度: 锁整个上游) 或真实模型 id (熔断: 只锁该模型); 同一作用域里取更晚的到期时间
// (设计 §5.5.3)。
func (c *cand) lock(scope, reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	g := c.gate(scope)
	if until.After(g.locks[scope].until) {
		g.locks[scope] = lockState{until: until, reason: reason}
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

// recordSuccess 清掉该模型的连续错误计数 (设计 §5.5.2 通用兜底): 同家别的模型的
// 成功不该把坏模型的计数洗掉, 所以计数按模型分开。
func (c *cand) recordSuccess(model string) {
	c.mu.Lock()
	if g := c.peekGate(model); g != nil {
		g.fails = 0
	}
	c.mu.Unlock()
}

// recordOther 记一次通用兜底失败: 该模型连续计数 +1, 并按 retry_backoff_ms 暂停它;
// 计数超过退避序列长度时报告该熔断 (设计 §5.5.2)。退避也是模型级的: 一个模型退避时
// 同家的其它模型照常出队。
func (c *cand) recordOther(model string, g GateCfg) (breaker bool) {
	c.mu.Lock()
	mg := c.gate(model)
	mg.fails++
	n := mg.fails
	if pause := g.backoff(n); pause > 0 {
		if until := time.Now().Add(pause); until.After(mg.pausedUntil) {
			mg.pausedUntil = until
		}
	}
	c.mu.Unlock()
	c.signal()
	return n > len(g.RetryBackoffMs)
}

// ---------- v2 锁状态落盘 (设计 §5.5.3) ----------

// gateLockRec 是落盘的一条定时锁。Model 空 = 整家锁 (额度); 否则只锁该模型 (熔断)。
type gateLockRec struct {
	UntilMs  int64  `json:"until_unix_ms"`
	Reason   string `json:"reason"`
	Upstream string `json:"upstream,omitempty"`
	Model    string `json:"model,omitempty"`
}

// gateState 是 gate-state.json 的内容: 只存未到期的定时锁 (整家锁与模型锁)。
type gateState struct {
	UpdatedAt string                 `json:"updated_at"`
	Locks     map[string]gateLockRec `json:"locks"`
}

// recordGenericFailure 记一次通用兜底失败: 该模型连续错误计数 +1 并按 retry_backoff_ms
// 暂停它; 到阈值就熔断 (只锁该 upstream/model 5min) 并清零计数。返回 true 表示该模型进了
// 熔断锁, 这一笔该换下一个候选; false 表示退避后重试同一候选 (设计 §5.5.2)。
func (a *App) recordGenericFailure(c *cand, model string, g GateCfg) bool {
	if !c.recordOther(model, g) {
		a.evAdd("gate", "", c.id, model, "上游报错, 先退避再试")
		return false
	}
	a.addLock(c, model, "breaker", g.breakerLock())
	c.recordSuccess(model) // 熔断后重新计数, 锁到期从 0 开始
	return true
}

// addLock 加一把定时锁并把锁状态落盘 —— 重启后仍在锁内 (设计 §5.5.3)。scope 是 lockAll
// (额度: 锁整个上游) 或真实模型 id (熔断: 只锁该模型)。
func (a *App) addLock(c *cand, scope, reason string, dur time.Duration) {
	why := "额度限制"
	if reason == "breaker" {
		why = "连续错误到阈值, 熔断"
	}
	a.evAdd("lock", "", c.id, scope, "加锁 "+why+" "+durStr(dur))
	c.lock(scope, reason, dur)
	a.saveGateState()
}

// locks 取该候选所有未到期的定时锁 (锁内快照), 供落盘与观测面用。Model 空 = 整家锁。
func (c *cand) locks() (out []gateLockRec) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.modelGates {
		for scope, l := range g.locks {
			if left := l.left(now); left > 0 {
				rec := gateLockRec{
					UntilMs: now.Add(left).UnixMilli(), Reason: l.reason, Upstream: c.id,
				}
				if scope != lockAll {
					rec.Model = scope
				}
				out = append(out, rec)
			}
		}
	}
	return out
}

// saveGateState 把未到期的定时锁落盘 (先写临时文件再改名, 与探测状态同一套路)。
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
		for _, rec := range c.locks() {
			key := c.id + "/" + lockAll // 整家锁 (额度)
			if rec.Model != "" {
				key = c.id + "/" + rec.Model // 单模型锁 (熔断)
			}
			st.Locks[key] = rec
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

// loadGateState 启动时恢复未到期的定时锁 (整家锁与模型锁); 已过期的直接丢掉。
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
	for key, rec := range st.Locks {
		// 锁的归属: 先信记录自己的 upstream / model; 老版本的记录没有这两个字段, 退回按
		// map 键取 ("<上游>" = 整家锁, "<上游>/<模型>" = 模型锁)。
		scope := rec.Model // 空 = 整家锁 (额度 / 旧记录的供应商锁)
		upID := rec.Upstream
		if upID == "" || scope == "" {
			parts := strings.SplitN(key, "/", 2)
			if upID == "" {
				upID = parts[0]
			}
			if scope == "" && len(parts) == 2 && parts[1] != lockAll {
				scope = parts[1]
			}
		}
		if scope == "" {
			scope = lockAll
		}
		c, ok := a.cands[upID]
		if !ok {
			continue
		}
		until := time.UnixMilli(rec.UntilMs)
		if !until.After(now) {
			continue
		}
		c.mu.Lock()
		mg := c.gate(scope)
		if until.After(mg.locks[scope].until) {
			mg.locks[scope] = lockState{until: until, reason: rec.Reason}
		}
		c.mu.Unlock()
		restored++
	}
	if restored > 0 {
		log.Printf("gate: restored %d timed locks from %s", restored, a.gatePath)
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

	gatePath string // v2 定时锁状态文件 (var/gate-state.json)

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
	logDay string // 当前日志文件所属的本地日期 (按天轮转用)

	probeMu   sync.Mutex
	probe     map[string]*probeStat // "上游|模型" -> 滚动统计 (主动探测 + 被动观测)
	probePath string                // 指标状态文件 (var/probe.json)

	// 只读观测面 (TUI): 进行中的请求、最近事件、事件序号。纯内存、不落盘、不参与记账,
	// 服务重启即清空 —— 明细的事实来源仍是日志 (D12)。
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
}

// ---------- 只读观测面 (TUI) ----------
//
// 服务只暴露「此刻正在发生什么」: 进行中的请求与每个候选的队列/锁/退避。历史一律由 TUI
// 从日志 (D12 的唯一事实来源) 里读, 服务不为 TUI 记账、不写状态文件。

const (
	viewVer     = "tollgate-state/1" // 观测面版本 (TUI 用它判断对端是不是这个接口)
	viewEvents  = 256                // 事件环容量
	viewActives = 512                // 进行中请求的上限 (防泄漏兜底, 正常远低于此)
)

// 请求所处阶段: 上游调度器那一侧细分到「等锁 / 排队 / 等信号量」, 见 flowStage。
const (
	stageParse  = iota // 已解析出逻辑名, 还没进候选链
	stageSend          // 正对某候选发送或已发出 (upstream 已填)
	stageFailed        // 候选链走完仍未发出去 (合成 429 / 透传错误)
)

// flow 是一笔进行中的请求 (只用于展示, 不参与调度判断)。字段导出, 因为它要按原样
// 进 /v1/state 的 JSON。
type flow struct {
	ID        uint64    `json:"id"`
	Logical   string    `json:"logical"`
	Source    string    `json:"source,omitempty"`
	Path      string    `json:"path"`
	Stream    bool      `json:"stream"`
	StartTime time.Time `json:"start"`
	Up        string    `json:"upstream,omitempty"` // 当前候选上游
	Model     string    `json:"backend,omitempty"`  // 该候选的真实模型 id
	Stage     int       `json:"stage"`
	Attempt   int       `json:"attempt,omitempty"` // 已试过的候选数 (含当前)
	Gate      string    `json:"gate,omitempty"`    // 在等什么 (屏幕上那句话): 已发出 / 等节拍 … / 退避 … / 额度锁 …
}

// viewEvent 是事件流的一条: 调度器动作 + 每笔请求的结局 (由引擎侧发出, 不经日志回读)。
type viewEvent struct {
	Seq      uint64 `json:"seq"`
	Ts       string `json:"ts"`
	Kind     string `json:"kind"` // start | lock | unlock | fallback | gate | error | slow | ok | reload
	Logical  string `json:"logical,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Model    string `json:"model,omitempty"`
	Msg      string `json:"msg"`
}

// candModelLock 是某个 upstream/model 头上的定时锁 (熔断), 供状态台显示。
type candModelLock struct {
	Model  string `json:"model"`
	LeftMs int64  `json:"left_ms"`
	Why    string `json:"why"` // breaker
}

// candView 是某候选上游的瞬时调度状态。
type candView struct {
	ID       string          `json:"id"`
	Queue    int             `json:"queue"`
	InFlight int             `json:"in_flight"`
	SoftCap  int             `json:"soft_cap"`
	Probing  bool            `json:"probing"`
	ErrStrk  int             `json:"err_streak"`       // 该上游下最大的连续错误数
	PausedMs int64           `json:"paused_ms"`        // > 0 = 至少一个模型在退避, 取最久的
	LockMs   int64           `json:"lock_ms"`          // > 0 = 整家锁中 (额度), 剩这么久
	LockWhy  string          `json:"lock_why"`         // quota
	Models   []candModelLock `json:"models,omitempty"` // 各模型头上的熔断锁
	ConcMs   int64           `json:"conc_lock_ms"`     // > 0 = 并发锁 (只锁试探)
	NextMs   int64           `json:"next_ms"`          // 距下次出队节拍
	LastTps  float64         `json:"-"`                // 保留: TPS 从日志读, 不在这里
}

// pulse 是 GET /v1/state (以及 -tui 时的 var/tui-pulse.json) 的载荷。
type pulse struct {
	Ver       string      `json:"ver"` // 观测面版本: 用来认出"对端不是这个接口" (老二进制)
	Listen    string      `json:"listen"`
	Now       string      `json:"now"`
	Active    []flow      `json:"active"`
	Cands     []candView  `json:"cands"`
	Events    []viewEvent `json:"events"`  // 序号 > 请求里的 since 的那些
	MaxSeq    uint64      `json:"max_seq"` // 下次轮询带它作 since
	SlowMs    int64       `json:"slow_ms"` // 慢请求告警线 (TUI 用同一口径)
	MissingAt string      `json:"missing_at,omitempty"`
}

// stageName 是屏幕上「状态」那一列: 只写变化的部分 (在等什么 / 卡在哪一步)。
// 不加"等待"前缀 —— 表里每行都在等, 前缀只是噪音。
func stageName(f flow) string {
	switch f.Stage {
	case stageParse:
		return "解析中"
	case stageFailed:
		return "已失败"
	}
	if f.Gate != "" {
		return f.Gate
	}
	return "已发出"
}

func (a *App) evAdd(kind, logical, upstream, model, msg string) {
	e := viewEvent{
		Ts: time.Now().Format(time.RFC3339Nano), Kind: kind, Logical: logical,
		Upstream: upstream, Model: model, Msg: msg,
	}
	a.evMu.Lock()
	a.evNext++
	e.Seq = a.evNext
	a.events[int(e.Seq)%viewEvents] = e
	a.evMu.Unlock()
}

// eventsSince 取序号 > since 的事件 (最多整环), 按序号升序。
func (a *App) eventsSince(since uint64) ([]viewEvent, uint64) {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	out := make([]viewEvent, 0, 16)
	for _, e := range a.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, a.evNext
}

func (a *App) flowStart(logical, source, path string, stream bool) uint64 {
	now := time.Now()
	a.flowMu.Lock()
	a.nextFlow++
	id := a.nextFlow
	if len(a.flows) >= viewActives { // 兜底: 异常堆积时丢最老的, 不让它无界长
		oldest, ok := uint64(0), false
		for k := range a.flows {
			if !ok || k < oldest {
				oldest, ok = k, true
			}
		}
		delete(a.flows, oldest)
	}
	a.flows[id] = &flow{ID: id, Logical: logical, Source: source, Path: path,
		Stream: stream, StartTime: now, Stage: stageParse}
	a.flowMu.Unlock()
	return id
}

// flowStage 更新一笔进行中请求的展示状态; up/mod 为空表示不改这两列。
// gate 由调用方先算好 (读 cand 的锁/节拍), 这里只写展示字段, 不碰调度。
func (a *App) flowStage(id uint64, up, mod string, stage int, gate string, attempt int) {
	a.flowMu.Lock()
	if f := a.flows[id]; f != nil {
		if up != "" {
			f.Up = up
		}
		if mod != "" {
			f.Model = mod
		}
		f.Stage, f.Gate, f.Attempt = stage, gate, attempt
	}
	a.flowMu.Unlock()
}

// flowObs 把一笔请求的展示状态与事件喂给观测面 (TUI)。只读地看调度器, 不改调度:
// 它只读 cand 的锁/队列快照, 不做任何等待或记账。
type flowObs struct {
	a       *App
	id      uint64
	logical string

	up      string // 当前候选 (换候选时记一条 fallback 事件)
	model   string // 当前候选的真实模型 id (换候选时一起更新)
	attempt int
	slowMs  int64 // 首字节超过它就另记一条 slow (0 = 不记)
	done    bool
}

// mark 刷新展示状态。status: 0 = 还没结论, -1 = 传输层失败, 其余为上游状态码。
// 换到新候选时记一条 fallback 事件 (这条候选链的走向就是 TUI 要看的路由过程)。
func (o *flowObs) mark(status int, msg string) {
	if status != 0 {
		o.a.evAdd("fallback", o.logical, o.up, o.model, firstLine(msg, status))
	}
	if status < 0 {
		o.a.flowStage(o.id, o.up, o.model, stageFailed, "", o.attempt)
		return
	}
	o.a.flowStage(o.id, o.up, o.model, stageSend, o.gateNow(), o.attempt)
}

// gateNow 是「这一笔现在卡在哪一步」: 模型锁 / 整家锁 / 退避 / 出队节拍 / 已发出。
func (o *flowObs) gateNow() string {
	c := o.a.candOf(o.up)
	if c == nil {
		return "已发出"
	}
	now := time.Now()
	if locked, left, reason := c.locked(o.model); locked {
		why := "额度"
		if reason == "breaker" {
			why = "熔断"
		}
		return why + "锁 " + durStr(left)
	}
	c.mu.Lock()
	paused, next := c.pauseOf(o.model), c.nextSend
	c.mu.Unlock()
	if paused.After(now) {
		return "退避 " + durStr(paused.Sub(now))
	}
	if next.After(now) {
		return "等节拍 " + durStr(next.Sub(now))
	}
	return "已发出"
}

// firstLine 拼事件正文 (状态码优先, 没有就截断错误串)。
func firstLine(msg string, status int) string {
	if status > 0 {
		return "状态码 " + strconv.Itoa(status)
	}
	if msg == "" {
		return "连接失败"
	}
	return bodySnippet([]byte(msg), 120)
}

func (a *App) candOf(id string) *cand {
	a.mu.RLock()
	c := a.cands[id]
	a.mu.RUnlock()
	return c
}

// finish 是这笔请求的结局: 进事件流 (2xx 记 ok, 其余记 error; 慢请求另记一条)。
func (o *flowObs) finish(status int, ttfbMs int64) {
	if o.done {
		return
	}
	o.done = true
	if status == 0 {
		status = 200 // 正常收尾 (含 4xx 透传之外的路径) 都算成功
	}
	if status >= 400 {
		o.a.evAdd("error", o.logical, o.up, "", "状态码 "+strconv.Itoa(status))
		return
	}
	o.a.evAdd("ok", o.logical, o.up, "", "状态码 "+strconv.Itoa(status))
	if o.slowMs > 0 && ttfbMs >= o.slowMs {
		o.a.evAdd("slow", o.logical, o.up, "", "首字节 "+durStr(time.Duration(ttfbMs)*time.Millisecond))
	}
}

// durStr 把时长写成屏幕上好读的形态 (时间段, 不是时刻): 前导的零段省掉 ——
// 有小时写 hh:mm:ss, 只有分秒写 mm:ss, 只有秒写 12s / 0.8s (带单位, 不然看不出是秒)。
func durStr(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	case m > 0:
		return fmt.Sprintf("%02d:%02d", m, s)
	}
	return strconv.Itoa(s) + "s"
}

func (a *App) flowEnd(id uint64) {
	a.flowMu.Lock()
	delete(a.flows, id)
	a.flowMu.Unlock()
}

// flowActives 拷贝当前进行中的请求 (按开始时间升序)。
func (a *App) flowActives() []flow {
	a.flowMu.Lock()
	out := make([]flow, 0, len(a.flows))
	for _, f := range a.flows {
		out = append(out, *f)
	}
	a.flowMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.Before(out[j].StartTime) })
	return out
}

// candViews 读每个候选上游的瞬时调度状态 (逐个短锁, 不嵌套)。
func (a *App) candViews() []candView {
	a.mu.RLock()
	cands := make([]*cand, 0, len(a.cands))
	for _, c := range a.cands {
		cands = append(cands, c)
	}
	a.mu.RUnlock()
	now := time.Now()
	out := make([]candView, 0, len(cands))
	for _, c := range cands {
		c.mu.Lock()
		v := candView{
			ID: c.id, Queue: len(c.queue), InFlight: c.inFlight, SoftCap: c.softCap,
			Probing: c.probing,
		}
		for model, g := range c.modelGates {
			if g.fails > v.ErrStrk {
				v.ErrStrk = g.fails
			}
			if left := g.pausedUntil.Sub(now); left > 0 && msCeil(left) > v.PausedMs {
				v.PausedMs = msCeil(left)
			}
			if left := g.locks[lockAll].left(now); left > 0 {
				v.LockMs, v.LockWhy = msCeil(left), g.locks[lockAll].reason
			}
			if model != lockAll {
				if left := g.locks[model].left(now); left > 0 {
					v.Models = append(v.Models, candModelLock{
						Model: model, LeftMs: msCeil(left), Why: g.locks[model].reason,
					})
				}
			}
		}
		sort.Slice(v.Models, func(i, j int) bool { return v.Models[i].Model < v.Models[j].Model })
		if left := c.concLock.left(now); left > 0 {
			v.ConcMs = msCeil(left)
		}
		if left := c.nextSend.Sub(now); left > 0 {
			v.NextMs = msCeil(left)
		}
		c.mu.Unlock()
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (a *App) listenAddr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cfg == nil {
		return ""
	}
	return a.cfg.Listen
}

// snapshot 组装一份观测载荷 (since = TUI 上次拿到的 MaxSeq)。
func (a *App) snapshot(since uint64) pulse {
	evs, max := a.eventsSince(since)
	return pulse{
		Ver:    viewVer,
		Listen: a.listenAddr(), Now: time.Now().Format(time.RFC3339Nano),
		Active: a.flowActives(), Cands: a.candViews(), Events: evs, MaxSeq: max,
		SlowMs: 30000,
	}
}

// serveState 是只读观测面 (GET /v1/state): 进行中的请求 + 候选调度状态 + 新增事件。
// 与 /v1/probe 同级: 不给客户端用, 给运维面 (TUI) 看。
func (a *App) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET /v1/state")
		return
	}
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(a.snapshot(since))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.Write(b)
}

// startPulseLoop 每秒把观测载荷写到日志同目录的 tui-pulse.json: -tui 同进程时
// TUI 直接读文件, 不起 HTTP 轮询。原子替换 (临时文件 + rename), 读侧永远看到完整 JSON。
func (a *App) startPulseLoop(path string) {
	if path == "" {
		return
	}
	go func() {
		var last uint64
		seen := map[string]bool{} // 候选 -> 上一拍是否处于锁/退避中 (检测恢复用)
		for {
			p := a.snapshot(last)
			if b, err := json.Marshal(p); err == nil {
				tmp := path + ".tmp"
				if os.WriteFile(tmp, b, 0o644) == nil {
					os.Rename(tmp, path)
				}
			}
			// 锁/退避到期是"没有请求也可能发生"的状态变化, 只能靠轮询发现。
			// seen 里存的是"上一拍是否处于锁/退避中", 从有到无=恢复。
			for _, c := range p.Cands {
				busy := c.LockMs > 0 || c.PausedMs > 0
				if prev, ok := seen[c.ID]; ok && prev && !busy {
					a.evAdd("unlock", "", c.ID, "", "恢复: 锁/退避已到期")
				}
				seen[c.ID] = busy
			}
			last = p.MaxSeq
			time.Sleep(time.Second)
		}
	}()
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
	kind, why, _, _, ok := cd.acquire(gateCtx, gate, model)
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
	if r.URL.Path == "/v1/state" {
		a.serveState(w, r)
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
	// 观测面 (TUI): 这笔请求进"进行中"表, 每换候选/进出闸门刷一次展示状态, 结束时出表。
	source := r.Header.Get("X-Tollgate-Source")
	flowID := a.flowStart(logical, source, r.URL.Path, stream)
	obs := &flowObs{a: a, id: flowID, logical: logical, slowMs: 30000}
	var finalStatus int
	var finalTtfb int64
	defer func() {
		// 退出顺序: 先按实际结局刷新展示状态, 再把这笔从"进行中"表里摘掉。
		obs.finish(finalStatus, finalTtfb)
		a.flowEnd(flowID)
	}()
	// 每个 return 前面都要有它: 收尾状态 (429/502/透传错误码) 靠它带出。
	setFinal := func(status int, ttfb int64) { finalStatus, finalTtfb = status, ttfb }

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
		// 进入这个候选: 展示状态跟着走 (换候选时 mark 收到失败码, 顺手记一条 fallback 事件)。
		obs.attempt++
		obs.up, obs.model = at.up.ID, at.mod
		obs.mark(0, "")
		if at.cd == nil {
			lastErr = fmt.Errorf("upstream %s has no scheduler", at.up.ID)
			lastUpID, lastM = at.up.ID, at.mod
			failed++
			continue
		}
		if locked, left, reason := at.cd.locked(at.mod); locked {
			// 锁内不真发 (该模型的熔断锁 5min / 整家的额度锁 10min), 直接换下一个候选。
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
			kind, why, sem, qms, ok := at.cd.acquire(r.Context(), gate, at.mod)
			semMs += sem
			queueMs += qms
			if !ok {
				gateReject = why
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				if why == "locked" {
					lockedSkips++
					if _, left, _ := at.cd.locked(at.mod); left > gateLeft {
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
				a.recordGenericFailure(at.cd, at.mod, gate)
				lastErr = err
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				obs.mark(-1, err.Error())
				continue
			}
			if shouldFallback(resp.StatusCode) {
				b, rerr := io.ReadAll(resp.Body)
				resp.Body.Close()
				at.cd.finish(kind, false)
				obs.mark(resp.StatusCode, "")
				moveOn := true
				switch at.up.classify(resp.StatusCode, b) {
				case classQuota:
					// 额度限制: 锁整个上游 (额度是 key 级共享的) 10min, 排队的请求随之迁移。
					a.addLock(at.cd, lockAll, "quota", gate.quotaLock())
				case classConcurrency:
					// 并发限制: 锁掉超发试探 1h (设计 §5.5.2)。试探撞上就锁完直接换下一个
					// 候选; 普通请求锁完后本笔按普通错误走 —— 退避后重试同一候选
					// (providers-api.md §1.2 第 5 条)。
					at.cd.lockConcurrency("concurrency", gate.concurrencyLock())
					if kind != slotProbe {
						moveOn = a.recordGenericFailure(at.cd, at.mod, gate)
					}
				default:
					// 规则表没覆盖的样本: 把上游 + 状态码 + body 片段记进服务日志, 便于按
					// 真实文案补规则 (docs/providers-api.md §1.2 / §3.2)。
					log.Printf("unclassified upstream error: upstream=%s status=%d body=%s",
						at.up.ID, resp.StatusCode, bodySnippet(b, 200))
					moveOn = a.recordGenericFailure(at.cd, at.mod, gate)
				}
				lastErr = fmt.Errorf("upstream %s returned %d", at.up.ID, resp.StatusCode)
				lastStatus, lastHdr, lastBody = resp.StatusCode, resp.Header.Clone(), b
				lastUpID, lastM = at.up.ID, at.mod
				failed++
				if rerr != nil {
					lastErr = rerr
				}
				if !moveOn {
					// e2: 退避后回到出队, 重试同一个候选。退避是模型级的, 记这次失败时
					// 已经把它自己暂停过; 重试时队首会等到暂停结束, 等不到就排队超时换候选。
					continue
				}
				break
			}
			at.cd.recordSuccess(at.mod)
			if kind == slotProbe {
				at.cd.finish(slotProbe, true) // 试探成功: 上限 +1 (不设天花板)
			} else {
				heldCand, heldKind = at.cd, kind // 响应发完才归还槽位
			}
			servedUp, servedMod = at.up, at.mod
			obs.mark(0, "")
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
		setFinal(http.StatusTooManyRequests, 0)
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
			setFinal(lastStatus, 0)
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
		// 候选全在锁里: 回 429 + Retry-After 让 agent 退避 (不是我们的闸门,
		// 而是上游限制的传导), 比 502 更贴近真实原因。
		if lockedSkips > 0 && lastStatus == 0 {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(gateLeft.Seconds())))))
			writeError(w, http.StatusTooManyRequests, "upstream_locked",
				"all candidates for "+logical+" are locked after upstream limits")
			setFinal(http.StatusTooManyRequests, 0)
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
		setFinal(http.StatusBadGateway, 0)
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

	setFinal(status, ttfbMs)
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
	tui := flag.Bool("tui", false, "run the terminal dashboard on top of the proxy (q to quit)")
	tuiState := flag.String("tui-state", "", "TUI: observe a running proxy at this base URL (default: in-process)")
	flag.Parse()

	if *tui {
		runTUI(*cfgPath, *tuiState)
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
