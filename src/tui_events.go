// tui_events.go: events 页与路由历史汇总。

package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// raw 贴一行已经画好的内容 (整页滚动取窗口用): 行尾的清除序列由内容自己带着。
func (c *tuiCanvas) raw(line string) {
	c.b.WriteString(line)
	c.b.WriteString("\r\n")
}

// splitLines 把画布切成一行行 (每行含行尾的清除序列), 供整页滚动取窗口。
func (c *tuiCanvas) splitLines() []string {
	s := strings.TrimSuffix(c.b.String(), "\r\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\r\n")
}

// drawEvents: 事件流 (最新在前); 连续同类事件折成一行 xN, 免得刷屏。
func (m *tuiModel) drawEvents(c *tuiCanvas, body int) {
	evs := make([]routeEvent, 0, len(m.events))
	evs = append(evs, m.events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].Ts.After(evs[j].Ts) })
	rows := make([][]string, 0, len(evs))
	for i := 0; i < len(evs); {
		e := evs[i]
		reps := 1
		for i+reps < len(evs) && sameEvent(e, evs[i+reps]) {
			reps++
		}
		msg := e.Msg
		if reps > 1 {
			msg += "  x" + strconv.Itoa(reps)
		}
		rows = append(rows, []string{
			e.Ts.Format("15:04:05"),
			tuiEventTag(e.Kind),
			e.Logical,
			m.upModel(e.Upstream, e.Model),
			msg,
		})
		i += reps
	}
	drawTable(c, "事件流 (最新在前)", []tuiCol{
		{"时间", 8, false, false, 0}, {"类型", 10, false, false, 0}, {"逻辑名", 20, true, false, 1},
		{"上游/模型", 30, true, false, 2}, {"说明", 60, true, false, 0},
	}, rows, m.scrollWindow(len(rows), max(1, body-4)), max(1, body-4))
}

// upName 给上游名上它的专属色; 空串原样返回。
func (m *tuiModel) upName(up string) string {
	if up == "" {
		return ""
	}
	if c := m.upColors[up]; c != "" {
		return c + up + tuiReset
	}
	return up
}

// upModel 把上游与真实模型拼成一格 (`上游/模型`, 与活跃连接表和路由表同一写法);
// 上游部分用专属色, 有一边为空时不留那个斜杠。
func (m *tuiModel) upModel(up, model string) string {
	switch {
	case up == "":
		return model
	case model == "":
		return m.upName(up)
	}
	return m.upName(up) + "/" + model
}

// sameEvent 判断两条事件能不能折叠成一行 (同类 + 同路由 + 同文案)。
func sameEvent(a, b routeEvent) bool {
	return a.Kind == b.Kind && a.Logical == b.Logical && a.Upstream == b.Upstream &&
		a.Model == b.Model && a.Msg == b.Msg
}

// tuiEventKinds 把内部事件名换成屏幕上给的中文 (内部名字保持英文, 便于对日志)。
var tuiEventKinds = map[string]string{
	"start": "启动", "reload": "重载", "ok": "正常", "error": "错误",
	"fallback": "换候选", "gate": "闸门", "queue": "排队", "slow": "慢",
	"lock": "加锁", "unlock": "解锁", "breaker": "熔断", "probe": "探测",
}

// tuiEventTag 给事件类型上色; 窄终端不依赖颜色也能读。
func tuiEventTag(kind string) string {
	name, ok := tuiEventKinds[kind]
	if !ok {
		name = kind
	}
	switch kind {
	case "error", "gate", "lock", "breaker":
		return tuiRed + name + tuiReset
	case "fallback", "slow", "queue":
		return tuiYell + name + tuiReset
	case "ok", "unlock":
		return tuiGreen + name + tuiReset
	}
	return tuiDim + name + tuiReset
}

// routeRows 按最后使用时间降序 (最近用过的排最上面); 同一时刻的按请求数兜底。
func (m *tuiModel) routeRows() []*routeRow {
	out := make([]*routeRow, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Reqs > out[j].Reqs
	})
	return out
}

func (m *tuiModel) routeSummary(rows []*routeRow) string {
	var reqs, errs, fb, total, cached, ttfbN int
	var ttfbSum int64
	var first, last time.Time
	for _, r := range rows {
		reqs += r.Reqs
		errs += r.Errs
		fb += r.Fallbacks
		total += r.Total
		cached += r.Cached
		ttfbSum += r.TtfbSum
		ttfbN += r.TtfbN
		if r.First.IsZero() {
			continue
		}
		if first.IsZero() || r.First.Before(first) {
			first = r.First
		}
		if last.IsZero() || r.Last.After(last) {
			last = r.Last
		}
	}
	span := "n/a"
	if !first.IsZero() {
		span = durStr(last.Sub(first))
	}
	cache := "-"
	if total > 0 {
		cache = ftoa(float64(cached)*100/float64(total), 0) + "%"
	}
	ttfb := "-"
	if ttfbN > 0 {
		ttfb = durStr(time.Duration(ttfbSum/int64(ttfbN)) * time.Millisecond)
	}
	return "窗口 " + durStr(m.window) + " (实际 " + span + ")   " + strconv.Itoa(len(rows)) + " 条路由   " +
		strconv.Itoa(reqs) + " 请求   " + strconv.Itoa(errs) + " 错误   " +
		strconv.Itoa(fb) + " 次换候选   " + humanTokens(total) + " token (缓存 " + cache +
		")   首字节平均 " + ttfb
}

// mages 是"最近一次"列的写法: 距现在多久 (与其它时长同一口径)。
func mages(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return durStr(time.Since(t))
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return ftoa(float64(n)/1e6, 1) + "M"
	case n >= 1_000:
		return ftoa(float64(n)/1e3, 1) + "k"
	}
	return strconv.Itoa(n)
}

func ftoa(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

// humanRate 把字节速率写成带单位的字符串 (1024 进制): 1.2 KB/s / 3.4 MB/s。
func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return ftoa(bps/(1<<20), 1) + " MB/s"
	case bps >= 1<<10:
		return ftoa(bps/(1<<10), 1) + " KB/s"
	}
	return itoa64(int64(bps)) + " B/s"
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
