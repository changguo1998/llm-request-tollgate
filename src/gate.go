// gate.go: 闸门类型与取资格流程 —— 信号量、流量队列、模型队列。

package main

import (
	"context"
	"sync"
	"time"
)

// minSoftCap 是并发上限的起点: 每次重启从 1 重新爬。
const minSoftCap = 1

// defaultGroup 是没写 billing 的模型所属的额度组: 键是空串, 事件文案里写作 def。
// 一个上游一个默认组: 不写 models 时别名引用的模型都归默认组, 额度锁覆盖该上游的全部模型。
const defaultGroup = ""

// groupLabel 把组名写成事件文案里的短词 (空串是内部键, 不能直出)。
func groupLabel(group string) string {
	if group == defaultGroup {
		return "def"
	}
	return group
}

type ticket struct {
	c     *cand
	model string    // 这一笔要用的真实模型 (它决定进哪个模型队列)
	group string    // 该模型所属的额度组 (它决定会撞上哪把额度锁)
	enq   time.Time // 入流量队列的时刻
	where queueKind // 此刻排在哪条队列里 (退出时按它摘)
}

// queueKind 是一张 ticket 当前所在的队列。
type queueKind int

const (
	queueNone  queueKind = iota // 不在任何队列里
	queueFlow                   // provider 的流量队列 (出队节拍)
	queueModel                  // 该模型的队列 (熔断锁 / 退避的等待口)
)

// cand 是每个上游的调度状态: 两层队列 + 自适应并发上限 + 四类锁。
// 退避、熔断计数与定时锁按模型分开记 (modelGates), 所以同家一个模型坏了不挡其它模型;
// 额度锁按额度组分开记 (groupLocks), 所以一个组额度用完不挡同家的其它组。
type cand struct {
	id string

	mu       sync.Mutex
	flow     []*ticket            // 流量队列: 只保证出队节拍, 不被任何模型的退避堵住
	modelQ   map[string][]*ticket // 模型队列: 出队即发, 只在熔断锁 / 退避时等
	inFlight int                  // 已拿到信号量的请求数 (含还在排队的)
	softCap  int                  // 自适应并发上限: 起点 1, 不设天花板
	probing  bool                 // 是否已有一笔试探在进行 (同一时刻只允许一笔)
	nextSend time.Time            // 下一笔最早可发送的时刻 (出队节拍)
	// 模型级状态: 键是真实模型 id。锁查询不建表, 记账 (失败/成功/加锁) 才建。
	modelGates map[string]*modelGate
	seen       map[string]bool // 进过队列的真实模型 (TUI 模型视角只列有过请求的)
	// 额度组锁: 键是组名 (默认组为空串)。额度是组级事实, 命中只锁该组。
	groupLocks   map[string]lockState
	concLock     lockState // 并发上限锁: 只锁超发试探
	recoverProbe bool      // 是否开启锁恢复探测 (锁到期进入恢复态, 探测通过才解锁)
	wake         chan struct{}
}

// modelGate 是一个 upstream/model 的失败状态与性能采样。
type modelGate struct {
	fails       int        // 连续错误计数, 成功清零
	pausedUntil time.Time  // 退避到什么时候: 退避期间该模型不出队
	lock        lockState  // 定时锁 (熔断 / 低性能): 只锁该 upstream/model
	tps         []perfPt   // 低性能锁的吞吐样本: 窗口内每笔流式成功请求的性能值 (r*, 或退化原始速率)
	lat         []latPoint // 低性能锁的延迟样本: 窗口内每笔成功响应的首字节时延
	// 历史性能值 (落盘): perfN 是样本笔数, perfSum 是各笔性能值 (r*, 或退化原始速率) 之和;
	// 历史平均 = perfSum/perfN, 供低吞吐的相对门槛比较。
	perfN   int64
	perfSum float64
	// 低性能锁的观察期 (纯内存, 不落盘): unlockAt 是最近一次恢复解锁时刻, recycle 是
	// 一个观察窗口 (perf_window) 内「解锁 -> 重锁」的轮数。轮数越大重锁越长,
	// 让没真恢复的候选慢慢退成长期死锁, 抖动自衰减; 超过窗口未重锁则归零。
	unlockAt time.Time
	recycle  int
}

// latPoint 是低性能锁的一笔首字节时延样本。
type latPoint struct {
	ts time.Time
	ms int64
}

func newCand(id string) *cand {
	return &cand{id: id, softCap: minSoftCap, seen: map[string]bool{},
		groupLocks: map[string]lockState{}, wake: make(chan struct{}, 1)}
}

// signal 唤醒一个等待者; 通道容量 1, 满了说明已经有人会醒来重查。
func (c *cand) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// slotKind 说明这一笔是正常占用信号量, 还是并发满时的超发试探。
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

// acquire 拿到发送资格: 取信号量 (a) → 过流量队列 (b) → 过模型队列 (b2)。
// 模型队列只有在该模型被熔断锁住、或它所属的额度组被额度锁住、或它正在退避时才等
// (正常情况下出队即发); waitMs 是排队两段之和, 只用于记日志。上层换候选用下一笔。
func (c *cand) acquire(ctx context.Context, g GateCfg, model, group string, onStage ...func(int)) (kind slotKind, why string, semMs, waitMs int64, ok bool) {
	// 展示用回调: 把这一笔此刻卡在流程哪一步报给 TUI。可变参数 = 老调用点不用改。
	report := func(st int) {
		if len(onStage) > 0 && onStage[0] != nil {
			onStage[0](st)
		}
	}
	report(stageSem)
	start := time.Now()
	kind, why, ok = c.acquireSlot(ctx, g, model, group)
	semMs = time.Since(start).Milliseconds()
	if !ok {
		return kind, why, semMs, 0, false
	}
	t := &ticket{c: c, model: model, group: group, enq: time.Now()}
	start = time.Now()
	if why, ok = c.waitFlow(ctx, g, t, report); !ok {
		c.finish(kind, false) // 没轮到就退出: 归还信号量 (试探不占, 无需归还)
		return kind, why, semMs, time.Since(start).Milliseconds(), false
	}
	if why, ok = c.waitModel(ctx, g, t, report); !ok {
		c.finish(kind, false)
		return kind, why, semMs, time.Since(start).Milliseconds(), false
	}
	return kind, "", semMs, time.Since(start).Milliseconds(), true
}

// acquireSlot 取信号量: 有余量直接占; 占满时先试探 (纯超发), 不允许试探才等 semWaitMs。
func (c *cand) acquireSlot(ctx context.Context, g GateCfg, model, group string) (slotKind, string, bool) {
	deadline := time.Now().Add(g.semWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if locked, _, _ := c.lockedIn(model, group, now); locked {
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

// waitFlow 在 provider 的流量队列里等自己的出队节拍:
// 出队即离开这条队列, 接着去模型队列 —— 别家模型的退避不会堵在这里。
func (c *cand) waitFlow(ctx context.Context, g GateCfg, t *ticket, report func(int)) (string, bool) {
	c.mu.Lock()
	c.enqueueLocked(t, queueFlow)
	c.mu.Unlock()
	c.signal()
	report(stageQueue)
	deadline := t.enq.Add(g.queueWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if locked, _, _ := c.lockedIn(t.model, t.group, now); locked {
			// 排队期间被锁 (该模型的熔断锁, 或它所属组的额度锁): 退出队列, 由上层换下一个
			// 候选 (整队迁移)。同家别的组不在这里被释放。
			c.removeTicketLocked(t)
			c.mu.Unlock()
			c.signal()
			return "locked", false
		}
		if c.flowHead(t) && !now.Before(c.nextSend) {
			c.dequeueLocked(t, queueFlow)
			c.nextSend = now.Add(g.interval())
			c.enqueueLocked(t, queueModel) // 出了流量队列就进模型队列: 出门即发
			c.mu.Unlock()
			c.signal()
			return "", true
		}
		next := c.nextSend
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

// waitModel 在模型队列里等熔断解锁 / 组额度解锁 / 退避结束; 正常情况 (没被熔断、组没被
// 额度锁、没在退避) 立刻放行, 不等节拍 —— 节拍已经在流量队列那一段付过了。
// 上界用 queue_wait_ms (与流量队列同一个预算口径): 退避 10/20/30s 比它长, 等到超时就换候选。
func (c *cand) waitModel(ctx context.Context, g GateCfg, t *ticket, report func(int)) (string, bool) {
	report(stageModelWait)
	deadline := time.Now().Add(g.queueWait())
	for {
		now := time.Now()
		c.mu.Lock()
		if locked, _, _ := c.lockedIn(t.model, t.group, now); locked {
			c.removeTicketLocked(t)
			c.mu.Unlock()
			c.signal()
			return "locked", false
		}
		paused := !now.Before(c.pauseOf(t.model))
		if paused && c.modelHead(t) {
			c.dequeueLocked(t, queueModel)
			c.mu.Unlock()
			c.signal()
			return "", true
		}
		if !paused {
			// 退避已经结束 (或没在退避): 不占着队列, 直接发。
			c.dequeueLocked(t, queueModel)
			c.mu.Unlock()
			c.signal()
			return "", true
		}
		pause := c.pauseOf(t.model)
		c.mu.Unlock()
		switch c.sleep(ctx, deadline, pause) {
		case wakeCanceled:
			c.dropTicket(t)
			return "canceled", false
		case wakeDeadline:
			c.dropTicket(t)
			return "backoff_timeout", false
		}
	}
}

// flowHead / modelHead 判断这一项是不是该队列的队首 (调用方持有 c.mu)。
func (c *cand) flowHead(t *ticket) bool {
	return len(c.flow) > 0 && c.flow[0] == t
}

func (c *cand) modelHead(t *ticket) bool {
	q := c.modelQ[t.model]
	return len(q) > 0 && q[0] == t
}
