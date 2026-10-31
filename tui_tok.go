// tui_tok.go: token 速率采样与路由事件类型。

package main

import (
	"time"
)

// tuiWindow 是默认回看窗口: 聚合只算这么久以内的日志 (与 -summary 的窗口口径一致)。
const (
	tuiWindow     = 6 * time.Hour
	tuiScanWindow = 24 * time.Hour
)

// 模型/路由表的吞吐: 最近 1h 窗口内的总输出 token 除以这些请求的耗时之和 (空闲不计)。
const tuiTokWindow = time.Hour

// tokPoint 是一笔请求的输出 token 与耗时采样。
type tokPoint struct {
	ts  time.Time
	tok int
	dur int64 // 请求耗时 (毫秒)
}

// tokRate 算 1h 窗口内的吞吐 (tokens/s): 平均 = 窗口内总输出 token 除以这些请求的
// 耗时之和 (空闲不计), 峰值 = 单笔请求里最高的那笔 (同一口径: 该笔 token / 该笔耗时)。
// 窗口内没有可用样本时 ok=false。
func tokRate(pts []tokPoint, now time.Time) (avg, peak float64, ok bool) {
	tok, dur := 0, int64(0)
	for _, p := range pts {
		if now.Sub(p.ts) > tuiTokWindow || p.dur <= 0 {
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

// addTok 追加一个采样, 并裁掉超出 1h 窗口的过期样本。
func addTok(pts []tokPoint, p tokPoint, now time.Time) []tokPoint {
	pts = append(pts, p)
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > tuiTokWindow {
		start++
	}
	return pts[start:]
}

// routeRow 是「逻辑名 x 上游 x 真实模型」的一行聚合 (来自日志)。
type routeRow struct {
	Logical, Upstream, Model string
	Reqs, Errs, Fallbacks    int
	Prompt, Cached, Total    int
	TtfbSum                  int64
	TtfbN                    int
	Tok                      []tokPoint
	First, Last              time.Time
}

// routeEvent 是路由历史里值得单独看一眼的一条 (从日志逐行还原)。
type routeEvent struct {
	Ts       time.Time
	Kind     string // error | fallback | gate | slow | ok
	Logical  string
	Upstream string
	Model    string
	Status   int
	Msg      string
}
