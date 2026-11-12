// tui_tok.go: TUI 的吞吐采样窗口与路由事件类型。

package main

import (
	"time"
)

// tuiWindow 是默认回看窗口: 聚合只算这么久以内的日志 (与 -summary 的窗口口径一致)。
const (
	tuiWindow     = 3 * time.Hour
	tuiScanWindow = 24 * time.Hour
)

// perfPoint 是一笔速度参考样本 (r* 的原始输入: 输出 token / 解码毫秒 / 响应字节)。
// 性能列展示时按当前参数表 (perfIdx) 现算 r* = (c/d)*(c/b)^g, 参数重算后自动跟随。
type perfPoint struct {
	ts      time.Time
	c, d, b int64
}

// addPerf 追加一笔性能样本并裁掉超出 win 窗口的过期样本。
func addPerf(pts []perfPoint, p perfPoint, now time.Time, win time.Duration) []perfPoint {
	pts = append(pts, p)
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > win {
		start++
	}
	return pts[start:]
}

// ttfbPoint 是一笔首字节时延样本 (ms)。
type ttfbPoint struct {
	ts time.Time
	ms int64
}

// addTtfb 追加一笔首字节样本并裁掉超出 win 窗口的过期样本。
func addTtfb(pts []ttfbPoint, p ttfbPoint, now time.Time, win time.Duration) []ttfbPoint {
	pts = append(pts, p)
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > win {
		start++
	}
	return pts[start:]
}

// ttfbAvg 算 win 窗口内首字节时延的算术平均 (ms); 窗口内没有样本时 ok=false。
func ttfbAvg(pts []ttfbPoint, now time.Time, win time.Duration) (avg float64, ok bool) {
	var sum int64
	var n int
	for _, p := range pts {
		if now.Sub(p.ts) > win {
			continue
		}
		sum += p.ms
		n++
	}
	if n == 0 {
		return 0, false
	}
	return float64(sum) / float64(n), true
}

// ttfbMin 返回样本里最小的首字节时延 (谷值, ms); 样本为空时返回 0。
func ttfbMin(pts []ttfbPoint) int64 {
	var min int64
	for i, p := range pts {
		if i == 0 || p.ms < min {
			min = p.ms
		}
	}
	return min
}

// ttfbLast 返回样本里最近的一笔首字节时延 (ms); 样本为空时 ok=false。
func ttfbLast(pts []ttfbPoint) (ms int64, ok bool) {
	if len(pts) == 0 {
		return 0, false
	}
	l := pts[0]
	for _, p := range pts[1:] {
		if p.ts.After(l.ts) {
			l = p
		}
	}
	return l.ms, true
}

// routeRow 是「逻辑名 x 上游 x 真实模型」的一行聚合 (来自日志)。
type routeRow struct {
	Logical, Upstream, Model string
	Reqs, Errs, Fallbacks    int
	Prompt, Cached, Total    int
	TtfbPts                  []ttfbPoint // 窗口内各请求的首字节时延样本 (平均/谷值用)
	// 性能样本: 窗口内每笔流式请求的 (c, d, b), 展示时按参数表算 r* 的算术平均。
	PerfPts     []perfPoint
	First, Last time.Time
}

// routeEvent 是路由历史里值得单独看一眼的一条 (观测面事件或从日志逐行还原)。
type routeEvent struct {
	Ts       time.Time
	Req      uint64 // 请求编号 (0 = 不挂某笔请求的锁 / 探测 / 启动事件)
	Kind     string // stage | error | gate | queue | lock | unlock | probe | ok | start | reload
	Logical  string
	Upstream string
	Model    string
	Status   int
	Msg      string
}
