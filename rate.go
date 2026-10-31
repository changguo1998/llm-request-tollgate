// rate.go: 令牌速率口径 —— 平均 = 窗口内总输出 token 除以这些请求的耗时之和 (空闲不计),
// 峰值 = 单笔最高的那笔 (同一口径)。TUI 的聚合表与调度器的低流量锁共用这一份算法。

package main

import (
	"time"
)

// tokPoint 是一笔采样: 输出 token 与当分母的耗时 (毫秒)。
// TUI 用整笔请求耗时 (非空闲吞吐), 调度器用解码段耗时 (解码吞吐)。
type tokPoint struct {
	ts  time.Time
	tok int
	dur int64
}

// tokRate 算 window 窗口内的吞吐 (tokens/s), 超窗或没有耗时的样本不计;
// 窗口内没有可用样本时 ok=false。
func tokRate(pts []tokPoint, now time.Time, window time.Duration) (avg, peak float64, ok bool) {
	tok, dur := 0, int64(0)
	for _, p := range pts {
		if now.Sub(p.ts) > window || p.dur <= 0 {
			continue
		}
		tok += p.tok
		dur += p.dur
		if v := float64(p.tok) / (float64(p.dur) / 1000); v > peak {
			peak = v
		}
	}
	if tok <= 0 || dur <= 0 {
		return 0, 0, false
	}
	return float64(tok) / (float64(dur) / 1000), peak, true
}
