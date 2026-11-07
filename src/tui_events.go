// tui_events.go: events 页与路由历史汇总。

package main

import (
	"math"
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

// upNameAttr 给上游名上第 1 页同款专属色, 并保留行尾反显: 名字串里的 reset 会把
// 选中行的反显也清掉, 上完色再补回 attr, 后面的 priority= 还能继续反显。
func (m *tuiModel) upNameAttr(up, attr string) string {
	if up == "" {
		return ""
	}
	if c := m.upColors[up]; c != "" {
		return c + up + tuiReset + attr
	}
	return up + attr
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

// routeAgg 是窗口内所有路由聚合后的计数, 顶栏的实时行与统计行共用这一次遍历。
type routeAgg struct {
	reqs, errs, fb, total int
	ttfbPts               []ttfbPoint
	cache                 string // 缓存占比 ("-" = 窗口内没有请求)
}

// aggRoutes 遍历路由行, 聚合请求计数、token 与首字节样本 (顶栏统计行用)。
func aggRoutes(rows []*routeRow) routeAgg {
	var a routeAgg
	var cached int
	for _, r := range rows {
		a.reqs += r.Reqs
		a.errs += r.Errs
		a.fb += r.Fallbacks
		a.total += r.Total
		cached += r.Cached
		a.ttfbPts = append(a.ttfbPts, r.TtfbPts...)
	}
	a.cache = "-"
	if a.total > 0 {
		a.cache = ftoa(float64(cached)*100/float64(a.total), 0) + "%"
	}
	return a
}

// ttfbCell 首字节汇总: 窗口内平均/谷值 (与模型表列同一口径)。
func (m *tuiModel) ttfbCell(a routeAgg) string {
	if len(a.ttfbPts) == 0 {
		return "-"
	}
	if avg, ok := ttfbAvg(a.ttfbPts, time.Now(), m.rateWin); ok {
		return durStr(time.Duration(avg)*time.Millisecond) + "/" +
			durStr(time.Duration(ttfbMin(a.ttfbPts))*time.Millisecond)
	}
	return "-"
}

// mages 是"最近一次"列的写法: 距现在多久 (与其它时长同一口径)。
func mages(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return durStr(time.Since(t))
}

// unit4 把带单位的数字右对齐到固定 4 字符 (如 "1K" -> "  1K", "100K" 不变), 表格列内
// 数字对齐靠它 + 列自身的右对齐; 超过 4 字符的极端大值原样返回, 不截断。
func unit4(s string) string {
	if len(s) >= 4 {
		return s
	}
	return strings.Repeat(" ", 4-len(s)) + s
}

// humanTokens 把 token 数写成短形式 (1000 进制): 千以上不保留小数, 四舍五入成整数 +
// 单位 (K/M), 整体右对齐到 4 字符; 千以内的整数原样输出。
func humanTokens(n int) string {
	if n < 1_000 {
		return strconv.Itoa(n)
	}
	if k := int(math.Round(float64(n) / 1e3)); k < 1_000 {
		return unit4(strconv.Itoa(k) + "K")
	}
	return unit4(strconv.Itoa(int(math.Round(float64(n)/1e6))) + "M")
}

func ftoa(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

// humanRate 把字节速率写成带单位的字符串 (1024 进制): 4 MB/s / 31 KB/s / 969 B/s,
// 大单位不保留小数 (四舍五入成整数)。
func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return strconv.Itoa(int(math.Round(bps/(1<<20)))) + " MB/s"
	case bps >= 1<<10:
		return strconv.Itoa(int(math.Round(bps/(1<<10)))) + " KB/s"
	}
	return strconv.Itoa(int(math.Round(bps))) + " B/s"
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// humanByte 把字节写成最简形式 (1024 进制, 不带 /s): 969B / 8K / 3M。K/M 分支不保留
// 小数 (四舍五入成整数), 数字+单位整体右对齐到 4 字符。
func humanByte(v float64) string {
	switch {
	case v >= 1<<20:
		m := int(math.Round(v / (1 << 20)))
		return unit4(strconv.Itoa(m) + "M")
	case v >= 1<<10:
		k := int(math.Round(v / (1 << 10)))
		if k >= 1<<10 { // 四舍五入到 1024 就进位成 M
			return unit4(strconv.Itoa(int(math.Round(v/(1<<20)))) + "M")
		}
		return unit4(strconv.Itoa(k) + "K")
	}
	return unit4(itoa64(int64(v)) + "B")
}

// humanTPS 把吞吐 (tokens/s) 写成短形式: 千/百万进 K/M 且不保留小数 (四舍五入成整数,
// 整体右对齐到 4 字符), 千以内的小值保留 1 位小数 (含输入的端到端速率动辄几万,
// 不缩写会把列撑破)。
func humanTPS(v float64) string {
	switch {
	case v >= 1_000_000:
		return unit4(strconv.Itoa(int(math.Round(v/1e6))) + "M")
	case v >= 1_000:
		k := int(math.Round(v / 1e3))
		if k >= 1_000 { // 四舍五入到 1000 就进位成 M
			return unit4(strconv.Itoa(int(math.Round(v/1e6))) + "M")
		}
		return unit4(strconv.Itoa(k) + "K")
	}
	return ftoa(v, 1)
}
