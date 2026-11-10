// gate_state.go: 定时锁落盘与重启恢复。

package main

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

// gateLockRec 是落盘的一条定时锁。Model 非空 = 该模型的定时锁 (熔断 / 低性能);
// 否则是额度组锁, Group 给出组名 (空 = 默认组)。Path 是触发锁用的上游路径, 恢复探测沿用。
type gateLockRec struct {
	UntilMs  int64  `json:"until_unix_ms"`
	Reason   string `json:"reason"`
	Upstream string `json:"upstream,omitempty"`
	Group    string `json:"group,omitempty"`
	Model    string `json:"model,omitempty"`
	Path     string `json:"path,omitempty"`
}

// gateState 是 gate-state.json 的内容: 只存未到期的定时锁 (组锁与模型锁)。
type gateState struct {
	UpdatedAt string                 `json:"updated_at"`
	Locks     map[string]gateLockRec `json:"locks"`
}

// recordGenericFailure 记一次通用兜底失败: 该模型连续错误计数 +1 并按 retry_backoff_ms
// 暂停它; 到阈值就熔断 (只锁该 upstream/model, 恢复时长优先用调用方解析出的恢复时间,
// 解析不到才用 breaker_lock_ms) 并清零计数。返回 true 表示该模型进了熔断锁, 这一笔该
// 换下一个候选; false 表示退避后重试同一候选。recoverDur 是调用方从错误响应 (body /
// 响应头) 解析出的恢复倒计时, 0 = 没解析到。
func (a *App) recordGenericFailure(c *cand, model string, g GateCfg, path string, recoverDur time.Duration) bool {
	if !c.recordOther(model, g) {
		a.evAdd("gate", "", c.id, model, "backoff")
		return false
	}
	dur := g.breakerLock()
	if recoverDur > 0 {
		dur = recoverDur
	}
	a.addLockModel(c, model, "breaker", dur, path)
	c.recordSuccess(model) // 熔断后重新计数, 锁到期从 0 开始
	return true
}

// addLockGroup 给额度组加锁并把锁状态落盘 —— 重启后仍在锁内。
// path 是触发锁的请求用的上游路径, 供恢复探测沿用。
func (a *App) addLockGroup(c *cand, group, reason string, dur time.Duration, path string) {
	a.evAdd("lock", "", c.id, "", "lock(quota "+groupLabel(group)+" "+durStr(dur)+")")
	c.lockGroup(group, reason, dur, path)
	if c.recoverProbe && recoverProbeable(reason) {
		a.armLockRecovery(recoverRef{c: c, isModel: false, group: group, reason: reason, path: path}, time.Now().Add(dur))
	}
	a.saveGateState()
}

// addLockModel 给 upstream/model 加熔断锁并落盘 (只锁该模型, 同家别的模型照常发送)。
// path 是触发锁的请求用的上游路径, 供恢复探测沿用。
func (a *App) addLockModel(c *cand, model, reason string, dur time.Duration, path string) {
	a.evAdd("lock", "", c.id, model, "lock(breaker "+durStr(dur)+")")
	c.lockModel(model, reason, dur, path)
	if c.recoverProbe && recoverProbeable(reason) {
		a.armLockRecovery(recoverRef{c: c, isModel: true, model: model, reason: reason, path: path}, time.Now().Add(dur))
	}
	a.saveGateState()
}

// addLockLowPerf 给 upstream/model 加低性能锁并落盘 (作用域同熔断锁: 只锁该模型)。
// peak/avg 是低吞吐分支的峰值与平均 (tokens/s), latAvg 是高延迟分支的平均首字节时延
// (ms); 哪个分支命中哪个非 0。path 是触发锁的请求用的上游路径, 供恢复探测沿用。
func (a *App) addLockLowPerf(c *cand, model string, peak, avg, latAvg float64, dur time.Duration, path string) {
	// 重锁时长按观察期递增 (解锁后窗口内又锁 => 翻倍, 封顶 8 倍): 持续低性能的候选
	// 慢慢退成长期死锁, 抖动自衰减; 超过一个窗口没再锁则归零回基础时长。
	dur = c.relockDur(model, dur, a.gate.lowPerfWindow())
	cause := "peak " + ftoa(peak, 1) + " / avg " + ftoa(avg, 1) + " t/s"
	if latAvg > 0 {
		cause = "ttfb " + durStr(time.Duration(latAvg*float64(time.Millisecond)))
	}
	a.evAdd("lock", "", c.id, model, "lock(lowperf "+cause+" "+durStr(dur)+")")
	c.lockModel(model, lowPerfReason, dur, path)
	if c.recoverProbe && recoverProbeable(lowPerfReason) {
		a.armLockRecovery(recoverRef{c: c, isModel: true, model: model, reason: lowPerfReason, path: path}, time.Now().Add(dur))
	}
	a.saveGateState()
}

// locks 取该候选所有未到期的定时锁 (锁内快照), 供落盘与观测面用。
func (c *cand) locks() (out []gateLockRec) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for group, l := range c.groupLocks {
		if left := l.left(now); left > 0 {
			out = append(out, gateLockRec{
				UntilMs: now.Add(left).UnixMilli(), Reason: l.reason, Upstream: c.id,
				Group: group, Path: l.path,
			})
		}
	}
	for model, g := range c.modelGates {
		if left := g.lock.left(now); left > 0 {
			out = append(out, gateLockRec{
				UntilMs: now.Add(left).UnixMilli(), Reason: g.lock.reason, Upstream: c.id,
				Model: model, Path: g.lock.path,
			})
		}
	}
	return out
}

// saveGateState 把未到期的定时锁落盘 (先写临时文件再改名)。
// 键只求唯一, 归属看记录自己的 upstream / group / model。
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
			key := c.id // 默认组的额度锁
			if rec.Model != "" {
				key = c.id + "/" + rec.Model // 单模型锁 (熔断)
			} else if rec.Group != "" {
				key = c.id + "/group:" + rec.Group // 有名额度组
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

// loadGateState 启动时恢复未到期的定时锁 (组锁与模型锁); 已过期的直接丢掉。
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
	for _, rec := range st.Locks {
		upID, group, model := rec.Upstream, rec.Group, rec.Model
		c, ok := a.cands[upID]
		if !ok {
			continue
		}
		until := time.UnixMilli(rec.UntilMs)
		if !until.After(now) {
			continue
		}
		c.mu.Lock()
		if model != "" {
			mg := c.gate(model)
			if until.After(mg.lock.until) {
				mg.lock = lockState{until: until, reason: rec.Reason, path: rec.Path}
			}
		} else if until.After(c.groupLocks[group].until) {
			c.groupLocks[group] = lockState{until: until, reason: rec.Reason, path: rec.Path}
		}
		c.mu.Unlock()
		// 活跃且可探测的锁: 重挂恢复定时器, 让到期后继续走恢复探测。
		if c.recoverProbe && recoverProbeable(rec.Reason) {
			a.armLockRecovery(recoverRef{c: c, isModel: model != "", model: model, group: group, reason: rec.Reason, path: rec.Path}, until)
		}
		restored++
	}
	if restored > 0 {
		log.Printf("gate: restored %d timed locks from %s", restored, a.gatePath)
	}
}
