// gate_tps.go: 低性能锁 —— 窗口内「低吞吐 或 高延迟」任一命中即锁住该 上游/模型。

package main

import (
	"time"
)

// lowPerfReason 是低性能锁的原因标记 (落盘与观测面都按它认)。
const lowPerfReason = "lowperf"

// 判定样本: 至少 minPerfSamples 笔才判 (单笔可能是刚开流就断的短请求);
// 至多留 maxPerfSamples 笔只为防内存涨 (慢请求时远到不了这个数)。
const (
	minPerfSamples = 2
	maxPerfSamples = 256
)

// notePerf 记一笔成功响应的性能样本并判低性能锁。tp 非 nil = 有解码吞吐样本 (流式),
// ttfbMs > 0 = 有首字节时延样本 (任何成功响应)。窗口内样本够数且「低吞吐 (峰值与平均
// 都低于门槛) 或 高延迟 (平均首字节时延超过阈值)」任一命中时返回 (peak, avg, latAvg,
// true), 由调用方加锁并落盘。
// 触发后清空样本: 锁到期从新样本重判, 不至于被旧样本立刻再锁一次。
func (c *cand) notePerf(model string, tp *tokPoint, ttfbMs int64, g GateCfg) (peak, avg, latAvg float64, low bool) {
	now := time.Now()
	window := g.lowPerfWindow()
	c.mu.Lock()
	defer c.mu.Unlock()
	mg := c.gate(model)
	if l := mg.lock; l.active(now) && l.reason == lowPerfReason {
		return 0, 0, 0, false // 已经在低性能锁里: 等解锁后从新样本重判
	}
	if tp != nil {
		mg.tps = append(mg.tps, *tp)
		mg.tps = trimSamples(mg.tps, now, window)
	}
	if ttfbMs > 0 {
		mg.lat = append(mg.lat, latPoint{ts: now, ms: ttfbMs})
		mg.lat = trimLat(mg.lat, now, window)
	}
	// 低吞吐分支: 需能拆出解码段的流式成功样本, 且峰值与平均都低于门槛。
	if len(mg.tps) >= minPerfSamples {
		avg, peak, ok := tokRate(mg.tps, now, window)
		if ok && peak < float64(g.LowPerfPeakMax) && avg < float64(g.LowPerfAvgMax) {
			mg.tps, mg.lat = nil, nil
			return peak, avg, latAvg, true
		}
	}
	// 高延迟分支: 仅当配置了非零阈值, 窗口内平均首字节时延超过它才判。
	if thr := g.lowPerfTtfbMax(); thr > 0 && len(mg.lat) >= minPerfSamples {
		latAvg = avgLat(mg.lat)
		if latAvg > float64(thr.Milliseconds()) {
			mg.tps, mg.lat = nil, nil
			return peak, avg, latAvg, true
		}
	}
	return peak, avg, latAvg, false
}

// trimSamples 把解码吞吐样本裁剪到窗口内 (至多留 maxPerfSamples 笔)。
func trimSamples(pts []tokPoint, now time.Time, window time.Duration) []tokPoint {
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

// notePerfRequest 记一笔成功响应: 有解码吞吐则给吞吐样本, 有首字节时延则给延迟样本;
// 任一低性能命中即加低性能锁。探测类请求不走这里 (已删除探测功能)。
func (a *App) notePerfRequest(upID, model string, tok int, decodeMs, ttfbMs int64) {
	a.mu.RLock()
	c, g := a.cands[upID], a.gate
	a.mu.RUnlock()
	if c == nil {
		return
	}
	var tp *tokPoint
	if decodeMs > 0 && tok > 0 {
		tp = &tokPoint{ts: time.Now(), tok: tok, dur: decodeMs}
	}
	peak, avg, latAvg, low := c.notePerf(model, tp, ttfbMs, g)
	if low {
		a.addLockLowPerf(c, model, peak, avg, latAvg, g.lowPerfLock())
	}
}
