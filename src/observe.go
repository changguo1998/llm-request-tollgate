// observe.go: 只读观测面的数据结构与阶段名。

package main

import (
	"strings"
	"time"
)

// 服务只暴露「此刻正在发生什么」: 进行中的请求与每个候选的队列/锁/退避。历史一律由 TUI
// 从日志里读, 服务不为 TUI 记账、不写状态文件。

const (
	viewVer     = "tollgate-state/2" // 观测面版本 (TUI 用它判断对端是不是这个接口)
	viewEvents  = 2048               // 事件环容量 (阶段事件每笔数条, 环要大些才接得住一轮)
	viewActives = 512                // 进行中请求的上限 (防泄漏兜底, 正常远低于此)
)

// 请求所处阶段: 与调度器的真实步骤一一对应 (TUI 第 1 页按它逐列展示)。
// 顺序即流程顺序, 别随意调换。
const (
	stageParse     = iota // 已解析出逻辑名, 还没进候选链
	stageSem              // 取信号量 (含并发满时的超发试探)
	stageQueue            // provider 流量队列: 等出队节拍
	stageModelWait        // 模型队列: 等退避结束 / 熔断解锁
	stageSend             // 已发出, 等响应头 (upstream 已填)
	stageRecv             // 响应头已到, 正在读 body / 流
	stageFailed           // 候选链走完仍未发出去 (合成 429 / 透传错误)
	stageCount            // 阶段总数 (各阶段累计耗时的数组长度)
)

// flow 是一笔进行中的请求 (只用于展示, 不参与调度判断)。字段导出, 因为它要按原样
// 进 /state 的 JSON。
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
	Attempt   int       `json:"attempt,omitempty"`    // 已试过的候选数 (含当前)
	Gate      string    `json:"gate,omitempty"`       // 在等什么 (屏幕上那句话): sent / interval … / backoff … / quota …
	RecvTok   int       `json:"recv_tok,omitempty"`   // 接收中: 已收到的输出 token 数 (流式估算)
	RecvBytes int64     `json:"recv_bytes,omitempty"` // 接收中: 已转发的响应字节数 (流式)
	RecvStart int64     `json:"recv_start,omitempty"` // 接收中: 首字节到达时刻 (Unix 毫秒)
	// Tries 是这笔请求的每一次尝试 (换候选 / 同候选重试就多一段): 第 1 页一段一行。
	Tries []flowTry `json:"tries,omitempty"`

	stageAt time.Time // 进入当前阶段的时刻 (当前一段的该阶段耗时现算用, 不出 JSON)
}

// flowTry 是一次尝试: 到过各阶段的已结算耗时 (秒, 下标 = stage*), 当前阶段与其进入
// 时刻 (TUI 现算这一格), 以及结束时的结局词。Cur < 0 = 这一段已结束。
type flowTry struct {
	Up    string    `json:"upstream,omitempty"`
	Model string    `json:"backend,omitempty"`
	Segs  []float64 `json:"segs,omitempty"`
	Cur   int       `json:"cur"`
	CurAt int64     `json:"cur_at,omitempty"`
	Stop  int       `json:"stop,omitempty"` // 停在哪个阶段 (Fail 非空时有意义)
	Fail  string    `json:"fail,omitempty"` // 结局词: fail(502) / reject(quota 05:00)
}

// viewEvent 是事件流的一条: 调度器动作 + 每笔请求的阶段与结局 (由引擎侧发出, 不经日志回读)。
// Req = 请求编号 (flow.ID, 0 = 这条事件不挂在某笔请求上, 如锁 / 探测 / 启动);
// 有 Req 的事件同时带 Logical, 没有 Req 但有 Upstream/Model 的事件按 供应商/模型 显示。
type viewEvent struct {
	Seq      uint64 `json:"seq"`
	Ts       string `json:"ts"`
	Kind     string `json:"kind"` // stage | start | lock | unlock | gate | error | ok | probe | reload
	Req      uint64 `json:"req,omitempty"`
	Logical  string `json:"logical,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Model    string `json:"model,omitempty"`
	Msg      string `json:"msg"`
}

// candModelLock 是某个 upstream/model 头上的定时锁 (熔断 / 低性能), 供状态台显示。
type candModelLock struct {
	Model    string `json:"model"`
	Queue    int    `json:"queue"`     // 该真实模型的队列长度
	Fails    int    `json:"fails"`     // 连续错误数
	PausedMs int64  `json:"paused_ms"` // > 0 = 退避中, 剩这么久
	LeftMs   int64  `json:"left_ms"`   // > 0 = 模型定时锁, 剩这么久
	Why      string `json:"why"`       // breaker | lowperf
}

// candGroup 是某上游一个记账组的瞬时状态 (额度锁剩余)。组名空串 = 默认组。
type candGroup struct {
	Name   string `json:"name"`
	LockMs int64  `json:"lock_ms,omitempty"` // > 0 = 该组在额度锁内, 剩这么久
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
	LockMs   int64           `json:"lock_ms"`          // > 0 = 至少一个额度组在锁中, 取最久的
	LockWhy  string          `json:"lock_why"`         // quota
	Groups   []candGroup     `json:"groups,omitempty"` // 该上游声明的额度组 (按配置顺序)
	Models   []candModelLock `json:"models,omitempty"` // 每个真实模型的队列 / 退避 / 熔断锁
	ConcMs   int64           `json:"conc_lock_ms"`     // > 0 = 并发锁 (只锁试探)
	NextMs   int64           `json:"next_ms"`          // 距下次出队节拍
	LastTps  float64         `json:"-"`                // 保留: TPS 从日志读, 不在这里
}

// stickyView 是某个逻辑名当前的粘性偏好候选 (观测面只读, 参与第 1 页的 * 标记与
// 模型表分组标题倒计时)。RemainingMs 是该偏好还能保持多久 (ms, 0 = 已过失效窗口)。
type stickyView struct {
	Upstream    string `json:"upstream"`
	Model       string `json:"model"`
	RemainingMs int64  `json:"remaining_ms,omitempty"`
}

// pulse 是 GET /state (以及 -tui 时的 var/tui-pulse.json) 的载荷。
type pulse struct {
	Ver       string      `json:"ver"` // 观测面版本: 用来认出"对端不是这个接口"
	Listen    string      `json:"listen"`
	Now       string      `json:"now"`
	Active    []flow      `json:"active"`
	Cands     []candView  `json:"cands"`
	Events    []viewEvent `json:"events"`  // 序号 > 请求里的 since 的那些
	MaxSeq    uint64      `json:"max_seq"` // 下次轮询带它作 since
	MissingAt string      `json:"missing_at,omitempty"`
	// Sticky 是各逻辑名当前的粘性偏好候选 (纯内存, 无偏好时缺省不出现)。
	Sticky map[string]stickyView `json:"sticky,omitempty"`
}

// isGateDetail 判断 Gate 是不是值得写在链尾的细节: quota / breaker / lowperf / backoff
// 是「为什么卡住」的原因; interval (在队列里必然在等) 与 sent (发送阶段本身已说明) 不写。
func isGateDetail(g string) bool {
	for _, p := range []string{"quota ", "breaker ", "lowperf ", "backoff "} {
		if strings.HasPrefix(g, p) {
			return true
		}
	}
	return false
}
