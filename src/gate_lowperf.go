// gate_tps.go: 低性能锁 —— 窗口内「低吞吐 或 高延迟」任一命中即锁住该 上游/模型。

package main

import (
	"time"
)

// lowPerfReason 是低性能锁的原因标记 (落盘与观测面都按它认)。
const lowPerfReason = "lowperf"

// lowPerfExempt 是否该模型在低性能锁豁免名单里 (按真实模型 id 匹配): 名单内的模型
// 禁止触发低性能锁。名单来自全局扁平键 lowperf_exempt, 热加载即生效。
func (g GateCfg) lowPerfExempt(model string) bool {
	for _, m := range g.LowPerfExempt {
		if m == model {
			return true
		}
	}
	return false
}

// 判定样本: 至少 minPerfSamples 笔才判 (单笔可能是刚开流就断的短请求);
// 至多留 maxPerfSamples 笔只为防内存涨 (慢请求时远到不了这个数)。
const (
	minPerfSamples = 2
	maxPerfSamples = 256
)

// notePerf 记一笔成功响应的性能样本并判低性能锁。perf > 0 = 有吞吐样本 (该请求的 r*, 或
// 退化原始解码速率), ttfbMs > 0 = 有首字节时延样本 (任何成功响应)。低吞吐 = 窗口内性能值的
// 峰值与平均都低于绝对门槛, 且当前平均 / 历史平均低于 lowperf_rel_max; 历史样本不足时不判
// 低吞吐。高延迟 = 窗口内平均首字节时延超过阈值; 两个分支任一命中即返回 low=true, 由调用方
// 加锁。sampled 表示这笔是否记入了历史性能值 (供落盘节流)。触发后清空窗口样本。
func (c *cand) notePerf(model string, perf float64, ttfbMs int64, g GateCfg) (peak, avg, latAvg float64, low, sampled bool) {
	// 豁免名单 (lowperf_exempt): 名单内的模型禁止触发低性能锁, 也不收集样本。
	if g.lowPerfExempt(model) {
		return 0, 0, 0, false, false
	}
	now := time.Now()
	window := g.perfWindow()
	c.mu.Lock()
	defer c.mu.Unlock()
	mg := c.gate(model)
	if l := mg.lock; l.active(now) && l.reason == lowPerfReason {
		return 0, 0, 0, false, false // 已经在低性能锁里: 等解锁后从新样本重判
	}
	if perf > 0 {
		mg.tps = append(mg.tps, perfPt{ts: now, v: perf})
		mg.tps = trimPerf(mg.tps, now, window)
		mg.perfN++
		mg.perfSum += perf
		sampled = true
	}
	if ttfbMs > 0 {
		mg.lat = append(mg.lat, latPoint{ts: now, ms: ttfbMs})
		mg.lat = trimLat(mg.lat, now, window)
	}
	// 低吞吐分支: 绝对门槛 (性能峰值与平均都低于门槛) 且相对历史平均也明显变慢。
	// 历史样本不足时不判: 新模型 / 重启后要先积累历史。
	if len(mg.tps) >= minPerfSamples {
		avg, peak = perfStats(mg.tps)
		if peak < float64(g.LowPerfPeakMax) && avg < float64(g.LowPerfAvgMax) &&
			mg.perfN >= minPerfSamples && mg.perfSum > 0 {
			histAvg := mg.perfSum / float64(mg.perfN)
			if avg/histAvg < g.LowPerfRelMax {
				mg.tps, mg.lat = nil, nil
				return peak, avg, latAvg, true, sampled
			}
		}
	}
	// 高延迟分支: 仅当配置了非零阈值, 窗口内平均首字节时延超过它才判。
	if thr := g.lowPerfTtfbMax(); thr > 0 && len(mg.lat) >= minPerfSamples {
		latAvg = avgLat(mg.lat)
		if latAvg > float64(thr.Milliseconds()) {
			mg.tps, mg.lat = nil, nil
			return peak, avg, latAvg, true, sampled
		}
	}
	return peak, avg, latAvg, false, sampled
}

// relockDur 是低性能锁的重锁时长: 距上次恢复解锁不足一个观察窗口就又锁上, 说明性能
// 没真恢复, 锁定时长翻倍 (最多 base 的 8 倍); 超过窗口才归零回 base。轮数记在
// modelGate 上 (纯内存), 重启后从头计。window <= 0 = 全当首锁 (测试直发时长时用)。
func (c *cand) relockDur(model string, base, window time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	mg := c.gate(model)
	now := time.Now()
	if mg.unlockAt.IsZero() || window <= 0 || now.Sub(mg.unlockAt) >= window {
		mg.recycle = 0
	} else {
		mg.recycle++
	}
	mult := 1
	for i := 0; i < mg.recycle && i < 3; i++ {
		mult *= 2
	}
	return base * time.Duration(mult)
}

// perfPt 是低性能锁的一笔吞吐样本: 该请求的性能值 (r*, 或退化原始解码速率)。
type perfPt struct {
	ts time.Time
	v  float64
}

// perfStats 是窗口内样本的性能平均与峰值 (窗口裁剪由 trimPerf 完成)。
func perfStats(pts []perfPt) (avg, peak float64) {
	if len(pts) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, p := range pts {
		if p.v > peak {
			peak = p.v
		}
		sum += p.v
	}
	return sum / float64(len(pts)), peak
}

// trimPerf 把性能样本裁剪到窗口内 (至多留 maxPerfSamples 笔)。
func trimPerf(pts []perfPt, now time.Time, window time.Duration) []perfPt {
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > window {
		start++
	}
	pts = pts[start:]
	if n := len(pts); n > maxPerfSamples {
		pts = pts[n-maxPerfSamples:]
	}
	return pts
}

// trimLat 把首字节时延样本裁剪到窗口内 (至多留 maxPerfSamples 笔)。
func trimLat(pts []latPoint, now time.Time, window time.Duration) []latPoint {
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > window {
		start++
	}
	pts = pts[start:]
	if n := len(pts); n > maxPerfSamples {
		pts = pts[n-maxPerfSamples:]
	}
	return pts
}

// avgLat 是窗口内首字节时延的平均值 (毫秒)。
func avgLat(pts []latPoint) float64 {
	if len(pts) == 0 {
		return 0
	}
	var sum int64
	for _, p := range pts {
		sum += p.ms
	}
	return float64(sum) / float64(len(pts))
}

// notePerfRequest 记一笔成功响应: 有解码吞吐则给吞吐样本 (该请求的性能值 r*; 无响应字节或
// 缺参数时退化原始解码速率), 有首字节时延则给延迟样本; 任一低性能命中即加低性能锁。path 是
// 这笔请求用的上游路径, 供恢复探测沿用。
func (a *App) notePerfRequest(upID, model string, tok int, decodeMs, ttfbMs, bytesOut int64, path string) {
	a.mu.RLock()
	c, g := a.cands[upID], a.gate
	a.mu.RUnlock()
	if c == nil {
		return
	}
	// 每请求性能值: 有解码段 (流式) 才算; 响应字节齐全时按当前参数表算 r* = (c/d)*(c/b)^g,
	// 缺字节或缺参数退原始解码速率。
	var perf float64
	if tok > 0 && decodeMs > 0 {
		if bytesOut > 0 {
			perf = a.perfIndex(upID, model, int64(tok), decodeMs, bytesOut)
		} else {
			perf = float64(tok) / float64(decodeMs) * 1000
		}
	}
	peak, avg, latAvg, low, sampled := c.notePerf(model, perf, ttfbMs, g)
	if low {
		a.addLockLowPerf(c, model, peak, avg, latAvg, g.lowPerfLock(), path)
	} else if sampled {
		a.savePerfState()
	}
}
