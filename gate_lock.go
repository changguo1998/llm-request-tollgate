// gate_lock.go: 三类锁 (模型熔断 / 记账组额度 / 超发试探) 与失败计数。

package main

import (
	"time"
)

// lockState 是一个到期时间戳加一个原因标记。
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
		if l := g.lock; l.active(now) {
			return true, l.left(now), l.reason
		}
	}
	if l, ok := c.groupLocks[group]; ok && l.active(now) {
		return true, l.left(now), l.reason
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
