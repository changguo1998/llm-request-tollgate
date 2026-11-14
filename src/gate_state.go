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

// gatePerfRec 是落盘的历史性能值: N 是样本笔数, Sum 是各笔性能值 (r*, 或退化原始解码速率) 之和;
// 历史平均 = Sum/N (各笔性能值的算术平均)。
type gatePerfRec struct {
	Upstream string  `json:"upstream"`
	Model    string  `json:"model"`
	N        int64   `json:"n"`
	Sum      float64 `json:"sum"`
}

// gateState 是 gate-state.json 的内容: 未到期的定时锁 (组锁与模型锁) 与历史性能样本。
type gateState struct {
	UpdatedAt string                 `json:"updated_at"`
	Locks     map[string]gateLockRec `json:"locks"`
	Perf      map[string]gatePerfRec `json:"perf,omitempty"`
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
	dur = c.relockDur(model, dur, a.gate.perfWindow())
	cause := "peak " + ftoa(peak, 1) + " / avg " + ftoa(avg, 1)
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

// saveGateState 把未到期的定时锁与历史性能样本落盘 (先写临时文件再改名)。
// 锁的键只求唯一, 归属看记录自己的 upstream / group / model; 性能记录按 upstream/model
// 聚合, 用于重启后继续算低吞吐的历史平均。
func (a *App) saveGateState() {
	if a.gatePath == "" {
		return
	}
	a.gateSaveMu.Lock()
	defer a.gateSaveMu.Unlock()
	now := time.Now()
	st := gateState{
		UpdatedAt: now.Format(time.RFC3339Nano),
		Locks:     map[string]gateLockRec{},
		Perf:      map[string]gatePerfRec{},
	}
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
		for _, p := range c.perfStats() {
			st.Perf[p.Upstream+"/"+p.Model] = p
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

// perfStats 取该候选各模型的历史性能样本 (锁内快照), 供落盘。
func (c *cand) perfStats() []gatePerfRec {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]gatePerfRec, 0, len(c.modelGates))
	for model, g := range c.modelGates {
		if g.perfN > 0 {
			out = append(out, gatePerfRec{Upstream: c.id, Model: model, N: g.perfN, Sum: g.perfSum})
		}
	}
	return out
}

// perfSaveInterval 是历史性能样本的落盘节流: 成功请求很频繁, 不必每笔都重写状态文件。
const perfSaveInterval = 10 * time.Second

// savePerfState 按节流把历史性能样本写盘 (与锁共用 gate-state.json)。
func (a *App) savePerfState() {
	if a.gatePath == "" {
		return
	}
	a.perfMu.Lock()
	if time.Since(a.perfSaved) < perfSaveInterval {
		a.perfMu.Unlock()
		return
	}
	a.perfSaved = time.Now()
	a.perfMu.Unlock()
	a.saveGateState()
}

// loadGateState 启动时恢复未到期的定时锁与历史性能样本; 过期的锁直接丢掉。
func (a *App) loadGateState() {
	if a.gatePath == "" {
		return
	}
	b, err := os.ReadFile(a.gatePath)
	if err != nil {
		return
	}
	var st gateState
	if json.Unmarshal(b, &st) != nil {
		return
	}
	now := time.Now()
	a.mu.RLock()
	defer a.mu.RUnlock()
	restored := 0
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
	perf := 0
	for _, p := range st.Perf {
		if p.N <= 0 || p.Model == "" {
			continue
		}
		c, ok := a.cands[p.Upstream]
		if !ok {
			continue
		}
		c.mu.Lock()
		mg := c.gate(p.Model)
		mg.perfN, mg.perfSum = p.N, p.Sum
		c.mu.Unlock()
		perf++
	}
	if restored > 0 || perf > 0 {
		log.Printf("gate: restored %d timed locks and %d perf records from %s", restored, perf, a.gatePath)
	}
}
