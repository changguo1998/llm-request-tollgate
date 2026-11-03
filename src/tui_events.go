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

// drawEvents: 事件流 (最新在前), 三列: 时间 / 编号/逻辑名 / 事件。
// 标识列一套格式对应三类主体: 请求 编号/逻辑名, 模型 供应商/模型, 供应商 供应商;
// 连续相同事件折成一行 xN, 免得刷屏。
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
		if col := eventColor(e.Kind); col != "" {
			msg = col + msg + tuiReset
		}
		rows = append(rows, []string{
			e.Ts.Format("15:04:05"),
			m.eventWho(e),
			msg,
		})
		i += reps
	}
	drawTable(c, "事件流 (最新在前)", []tuiCol{
		{"时间", 8, false, false, 4}, {"编号/逻辑名", 20, true, false, 2},
		{"事件", 40, true, false, 0},
	}, rows, m.scrollWindow(len(rows), max(1, body-4)), max(1, body-4))
}

// eventWho 是标识格: 请求写 编号/逻辑名, 模型写 供应商/模型, 供应商写 供应商;
// 锁 / 探测 / 启动这类不挂请求的事件没有编号, 也不再显示逻辑名。
func (m *tuiModel) eventWho(e routeEvent) string {
	switch {
	case e.Req > 0:
		if e.Logical != "" {
			return strconv.FormatUint(e.Req, 10) + "/" + e.Logical
		}
		return strconv.FormatUint(e.Req, 10)
	case e.Logical != "":
		return "-/" + e.Logical // 日志回放的事件: 没有请求编号
	case e.Upstream != "" || e.Model != "":
		return m.upModel(e.Upstream, e.Model)
	}
	return "-"
}

// eventColor 给事件正文上色 (类型列已去掉, 靠颜色保住扫读时的层次):
// 阶段事件不上色 (量最大, 保持素净); 锁 / 错误红, 等待类黄, 解锁 / 成功绿, 其余暗色。
func eventColor(kind string) string {
	switch kind {
	case "error", "lock":
		return tuiRed
	case "fallback", "queue", "gate":
		return tuiYell
	case "ok", "unlock":
		return tuiGreen
	case "stage":
		return ""
	}
	return tuiDim
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

// sameEvent 判断两条事件能不能折叠成一行 (同请求 + 同类 + 同路由 + 同文案)。
func sameEvent(a, b routeEvent) bool {
	return a.Req == b.Req && a.Kind == b.Kind && a.Logical == b.Logical &&
		a.Upstream == b.Upstream && a.Model == b.Model && a.Msg == b.Msg
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

// humanByte 把字节写成最简形式 (1024 进制, 不带 /s): 969B / 8.5K / 3.4M。
func humanByte(v float64) string {
	switch {
	case v >= 1<<20:
		return ftoa(v/(1<<20), 1) + "M"
	case v >= 1<<10:
		return ftoa(v/(1<<10), 1) + "K"
	}
	return itoa64(int64(v)) + "B"
}

// humanTPS 把吞吐 (tokens/s) 写成短形式: 小值 1 位小数, 千/百万进 K/M
// (含输入的端到端速率动辄几万, 不缩写会把列撑破)。
func humanTPS(v float64) string {
	switch {
	case v >= 1_000_000:
		return ftoa(v/1e6, 1) + "M"
	case v >= 1_000:
		return ftoa(v/1e3, 1) + "K"
	}
	return ftoa(v, 1)
}
