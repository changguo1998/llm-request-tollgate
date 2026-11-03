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

// 模型/路由表的吞吐窗口: 最近 1h (算法见 rate.go, 分母是整笔请求耗时)。
const tuiTokWindow = time.Hour

// addTok 追加一个采样, 并裁掉超出 win 窗口的过期样本。
func addTok(pts []tokPoint, p tokPoint, now time.Time, win time.Duration) []tokPoint {
	pts = append(pts, p)
	start := 0
	for start < len(pts) && now.Sub(pts[start].ts) > win {
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
	// 三种速度的样本 (tokRate 平均/峰值): 分子/分母各不相同。
	DecodeRate  []tokPoint // completion / decode_ms  (缺 decode 不记)
	TotalRate   []tokPoint // (prompt+completion) / duration_ms
	ByteRate    []tokPoint // response bytes / duration_ms
	First, Last time.Time
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
