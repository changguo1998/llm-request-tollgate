// gate_lock.go: 三类锁 (模型熔断 / 记账组额度 / 超发试探) 与失败计数, 以及锁恢复探测。

package main

import (
	"time"
)

// lockState 是一个到期时间戳加一个原因标记, 外加恢复探测的两段瞬态。
// recover = 锁已到期, 进入恢复探测 (仍视为锁定, 直到探测通过才解锁);
// probing = 正有一笔探测在飞 (去责备用, 同一目标同时只允许一笔, 不落盘)。
type lockState struct {
	until   time.Time
	reason  string
	recover bool
	probing bool
}

func (l lockState) active(now time.Time) bool { return now.Before(l.until) }

func (l lockState) left(now time.Time) time.Duration {
	if !l.active(now) {
		return 0
	}
	return l.until.Sub(now)
}

// locked 判断该锁此刻是否挡路: 到期内、或处于恢复态; 已到期但该目标开着恢复探测时也
// 视为锁定 (待探测通过才放行, 免得真实请求直接冲进可能仍坏的模型)。
func (l lockState) locked(now time.Time, recoverProbe bool) bool {
	if l.active(now) || l.recover {
		return true
	}
	return recoverProbe && recoverProbeable(l.reason)
}

// lockedLeft 返回关闭该锁还恨不得多久 (恢复态已到期, 剩余 0)。
func (l lockState) lockedLeft(now time.Time) time.Duration {
	if l.active(now) {
		return l.left(now)
	}
	return 0
}

// recoverProbeable 是哪些原因会进入恢复探测; 并发试探锁 (concurrency) 不在此列。
func recoverProbeable(reason string) bool {
	switch reason {
	case "quota", "breaker", lowPerfReason:
		return true
	}
	return false
}

// gate 拿某个模型的失败状态 (没有就建); 调用方持有 c.mu。
// ticket 是排队里的一项; enq 记入队时刻, 用来算各段的等待预算。
func (c *cand) gate(model string) *modelGate {
	if c.modelGates == nil {
		c.modelGates = map[string]*modelGate{}
	}
	g := c.modelGates[model]
	if g == nil {
		g = &modelGate{}
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

// paused 报告该模型是否在退避中, 以及还剩多久。
func (c *cand) paused(model string) (bool, time.Duration) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := c.pauseOf(model).Sub(now); left > 0 {
		return true, left
	}
	return false, 0
}

// lockedIn 是锁查询的加锁版实现: 先看锁在它自己头上的熔断锁, 再看它所属额度组的
// 额度锁。两把同时在时以模型锁为准 —— 它更具体, 而且还剩多久与组锁无关。调用方必须
// 持有 c.mu (locked 与队列里的检查都走它)。
func (c *cand) lockedIn(model, group string, now time.Time) (bool, time.Duration, string) {
	if g := c.peekGate(model); g != nil {
		if l := g.lock; l.locked(now, c.recoverProbe) {
			return true, l.lockedLeft(now), l.reason
		}
	}
	if l, ok := c.groupLocks[group]; ok && l.locked(now, c.recoverProbe) {
		return true, l.lockedLeft(now), l.reason
	}
	return false, 0, ""
}

// locked 报告该模型现在能不能真发 (自己加锁)。
func (c *cand) locked(model, group string) (bool, time.Duration, string) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lockedIn(model, group, now)
}

// lockGroup 给一个额度组加锁并唤醒排队者, 让组内的请求退出队列换下一个候选;
// 同组里取更晚的到期时间。
func (c *cand) lockGroup(group, reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	if until.After(c.groupLocks[group].until) {
		c.groupLocks[group] = lockState{until: until, reason: reason}
	}
	c.mu.Unlock()
	c.signal()
}

// lockModel 给一个真实模型加熔断锁 (只锁该 upstream/model), 同一模型取更晚的到期时间。
func (c *cand) lockModel(model, reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	g := c.gate(model)
	if until.After(g.lock.until) {
		g.lock = lockState{until: until, reason: reason}
	}
	c.mu.Unlock()
	c.signal()
}

// lockConcurrency 锁掉超发试探: 候选本身照常用。
func (c *cand) lockConcurrency(reason string, dur time.Duration) {
	until := time.Now().Add(dur)
	c.mu.Lock()
	if until.After(c.concLock.until) {
		c.concLock = lockState{until: until, reason: reason}
	}
	c.mu.Unlock()
}

// recordSuccess 清掉该模型的连续错误计数: 同家别的模型的
// 成功不该把坏模型的计数洗掉, 所以计数按模型分开。
func (c *cand) recordSuccess(model string) {
	c.mu.Lock()
	if g := c.peekGate(model); g != nil {
		g.fails = 0
	}
	c.mu.Unlock()
}

// recordOther 记一次通用兜底失败: 该模型连续计数 +1, 并按 retry_backoff_ms 暂停它;
// 计数超过退避序列长度时报告该熔断。退避也是模型级的: 一个模型退避时
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

// --- 锁恢复探测 ---

// recoverRef 指向一处锁状态的值槽 (模型锁或额度组锁), 恢复探测经它读写。
type recoverRef struct {
	c       *cand
	isModel bool
	group   string // isModel=false 时: 额度组 (默认组为空串)
	model   string // isModel=true 时: 真实模型
	reason  string
}

func (r recoverRef) read() lockState {
	if r.isModel {
		if g := r.c.peekGate(r.model); g != nil {
			return g.lock
		}
		return lockState{}
	}
	return r.c.groupLocks[r.group]
}

func (r recoverRef) write(l lockState) {
	if r.isModel {
		g := r.c.gate(r.model)
		g.lock = l
		return
	}
	r.c.groupLocks[r.group] = l
}

// beginRecovery 进入恢复并发起探测认领 (定时器与请求两条触发都走这里):
//   - 目标不在恢复候选 (锁未到期, 或原因不可探测) → 认领不到
//   - 已有一笔探测在飞 → claimed=false, 别处正在探
//   - 否则标记 recover+probing=true, 返回 claimed=true, 由调用方发探测
//
// 调用方须已开启恢复探测 (c.recoverProbe)。
func (c *cand) beginRecovery(model, group string) (recoverRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.recoverProbe {
		return recoverRef{}, false
	}
	r, ok := c.findRecoverLocked(model, group)
	if !ok {
		return recoverRef{}, false
	}
	l := r.read()
	if l.probing {
		return r, false
	}
	l.recover, l.probing = true, true
	r.write(l)
	c.signal()
	return r, true
}

// findRecoverLocked 找该模型/组上处于「已到期待恢复」或「已过期且原因可探测」的锁槽
// (需持 c.mu)。
func (c *cand) findRecoverLocked(model, group string) (recoverRef, bool) {
	if mg := c.peekGate(model); mg != nil {
		if l := mg.lock; l.recover || (recoverProbeable(l.reason) && !l.active(time.Now())) {
			return recoverRef{c: c, isModel: true, model: model, reason: l.reason}, true
		}
	}
	if l, ok := c.groupLocks[group]; ok {
		if l.recover || (recoverProbeable(l.reason) && !l.active(time.Now())) {
			return recoverRef{c: c, isModel: false, group: group, reason: l.reason}, true
		}
	}
	return recoverRef{}, false
}

// settleRecovery 结束一笔恢复探测: 成功清锁解锁 (返回零 until); 失败按原原因对应时长
// 重新活跃加锁 (返回新的 until, 供上层重挂定时器)。recover/probing 都重置。
func (c *cand) settleRecovery(r recoverRef, ok bool, g GateCfg) time.Time {
	now := time.Now()
	c.mu.Lock()
	if ok {
		r.write(lockState{})
		c.mu.Unlock()
		c.signal()
		return time.Time{}
	}
	until := now.Add(recoverLockDur(r.reason, g))
	r.write(lockState{until: until, reason: r.reason})
	c.mu.Unlock()
	c.signal()
	return until
}

// recoverLockDur 是探测失败后重新加锁的时长, 按原因取各锁自己的配置。
func recoverLockDur(reason string, g GateCfg) time.Duration {
	switch reason {
	case "quota":
		return g.quotaLock()
	case lowPerfReason:
		return g.lowPerfLock()
	default: // breaker
		return g.breakerLock()
	}
}

// denyRecovery 关闭恢复探测时清除所有恢复态, 让已到期的锁恢复成普通时间锁 (热加载调用)。
func (c *cand) denyRecovery() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for group, l := range c.groupLocks {
		l.recover, l.probing = false, false
		c.groupLocks[group] = l
	}
	for _, g := range c.modelGates {
		g.lock.recover, g.lock.probing = false, false
	}
}

// clearLocks 强制解锁 (调试用): 清掉该候选的锁与失败计数。
//   - model 非空: 只清该模型的锁 (group 非空时连带清该额度组锁);
//   - model 为空: 清全部组锁 + 全部模型锁 + 并发试探锁。
//
// 同时清掉恢复探测标记与连续错误计数 (避免刚解开又立刻被旧计数再熔断)。返回清掉的锁数。
func (c *cand) clearLocks(model, group string) int {
	c.mu.Lock()
	n := 0
	if model != "" {
		if g := c.peekGate(model); g != nil {
			if g.lock.reason != "" {
				n++
			}
			g.lock = lockState{}
			g.fails = 0
			g.pausedUntil = time.Time{}
		}
		if group != "" {
			if l, ok := c.groupLocks[group]; ok {
				if l.reason != "" {
					n++
				}
				delete(c.groupLocks, group)
			}
		}
	} else {
		n += len(c.groupLocks)
		c.groupLocks = map[string]lockState{}
		for _, g := range c.modelGates {
			if g.lock.reason != "" {
				n++
			}
			g.lock = lockState{}
			g.fails = 0
			g.pausedUntil = time.Time{}
		}
		if c.concLock.reason != "" {
			n++
		}
		c.concLock = lockState{}
	}
	c.probing = false
	c.mu.Unlock()
	c.signal()
	return n
}
