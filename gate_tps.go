// gate_tps.go: 低流量锁 —— 窗口内解码吞吐的峰值与平均都低于阈值时锁住该模型。

package main

import (
	"time"
)

// lowTpsReason 是低流量锁的原因标记 (落盘与观测面都按它认)。
const lowTpsReason = "lowtps"

// 判定样本: 至少 minTpsSamples 笔才判 (单笔可能是刚开流就断的短请求);
// 至多留 maxTpsSamples 笔只为防内存涨 (低吞吐时远到不了这个数)。
const (
	minTpsSamples = 2
	maxTpsSamples = 256
)

// noteTps 记一笔解码吞吐并判定低流量锁: 窗口内的峰值与平均都低于阈值时返回
// (peak, avg, true), 由调用方加锁并落盘。样本只来自能测到解码段的流式成功请求。
// 触发后清空样本: 锁到期从新样本重新判, 不至于被旧样本立刻再锁一次。
func (c *cand) noteTps(model string, p tokPoint, g GateCfg) (peak, avg float64, low bool) {
	now := time.Now()
	window := g.lowTpsWindow()
	c.mu.Lock()
	defer c.mu.Unlock()
	mg := c.gate(model)
	if l := mg.lock; l.active(now) && l.reason == lowTpsReason {
		return 0, 0, false // 已经在低流量锁里: 等解锁后从新样本重判
	}
	mg.tps = append(mg.tps, p)
	start := 0
	for start < len(mg.tps) && now.Sub(mg.tps[start].ts) > window {
		start++
	}
	mg.tps = mg.tps[start:]
	if n := len(mg.tps); n > maxTpsSamples {
		mg.tps = mg.tps[n-maxTpsSamples:]
	}
	avg, peak, ok := tokRate(mg.tps, now, window)
	if !ok || len(mg.tps) < minTpsSamples {
		return 0, 0, false
	}
	if peak >= float64(g.LowTpsPeakMax) || avg >= float64(g.LowTpsAvgMax) {
		return peak, avg, false
	}
	mg.tps = nil
	return peak, avg, true
}

// noteDecodeTps 记一笔真实请求的解码吞吐 (只有流式能拆出解码段), 触发时加低流量锁。
func (a *App) noteDecodeTps(upID, model string, tok int, decodeMs int64) {
	a.mu.RLock()
	c, g := a.cands[upID], a.gate
	a.mu.RUnlock()
	if c == nil {
		return
	}
	peak, avg, low := c.noteTps(model, tokPoint{ts: time.Now(), tok: tok, dur: decodeMs}, g)
	if low {
		a.addLockLowTps(c, model, peak, avg, g.lowTpsLock())
	}
}
