// 峰值 = 单笔最高的那笔 (同一口径)。调度器的低性能锁用 tokRate (体积加权) 与 avgRate。

package main

import (
	"time"
)

// msCeil 把时长向上取整为毫秒 (>0 才计, 不计负值或 0)。
func msCeil(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

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

// avgRate 是各样本速率 (tok/dur) 的算术平均, 与历史平均同口径; 无有效样本返回 0。
func avgRate(pts []tokPoint) float64 {
	var sum float64
	var n int
	for _, p := range pts {
		if p.dur <= 0 {
			continue
		}
		sum += float64(p.tok) / (float64(p.dur) / 1000)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
