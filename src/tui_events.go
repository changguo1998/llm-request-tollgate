// tui_events.go: events 页与路由历史汇总。

package main

import (
	"math"
	"regexp"
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
// 标识列一套格式对应三类主体: 请求 编号/逻辑名, 模型 供应商/模型, 供应商 供应商。
// 同一秒内同一请求的阶段切换在显示时合成一条链; 锁 / 探测这类不挂请求的事件 (req=0)
// 原样一行。正文用终端的前景色, 只给语义词上色: 失败结局 fail(原因) 红字, 异常过渡词
// switch / retry 黄字, 成功收尾词 ok(200) 绿字; 标识列的逻辑名与正文里的供应商名另带
// 专属色 (见 logicalName / colorEventBody)。
func (m *tuiModel) drawEvents(c *tuiCanvas, body int) {
	rows := m.mergedEventRows()
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.Ts.Format("15:04:05"),
			m.eventWho(r),
			r.Msg,
		})
	}
	cols := []tuiCol{
		{"时间", 8, false, false, 4}, {"编号/逻辑名", 12, true, false, 2},
		{"事件", 40, true, false, 0},
	}
	cols, vis := tableLayout(cols, out, c.cols)
	tableTop(c, m.eventsTitle(len(rows)), cols, vis)
	if len(rows) == 0 {
		m.drawEventsEmpty(c)
		return
	}
	limit := max(1, body-4)
	// 事件列放不下时折行: 续行以 "→ " 开头 (断点只在箭头之前), 并相对该条第一行的
	// 事件列开头再缩进 eventWrapGap 列; 首段仍走表格对齐, 续行按正文宽度自己排版。
	const evCol = 2
	lines := make([][]string, len(rows))
	cont := strings.Repeat(" ", colOffset(cols, vis, evCol)+eventWrapGap)
	total := 0
	for i := range rows {
		lines[i] = wrapEventMsg(m.colorEventBody(out[i][evCol]), cols[evCol].w)
		total += len(lines[i])
		out[i][evCol] = lines[i][0]
	}
	top := m.scrollWindow(len(rows), limit)
	// 滚动仍按条目 (一段链 = 一条), 窗口却按显示行数装: 装不下下一条就停, 至少装一条。
	end, used := top, 0
	for end < len(rows) {
		n := len(lines[end])
		if end > top && used+n > limit {
			break
		}
		used += n
		end++
	}
	// 折行让条目占的行数不一: 到了表尾还没占满窗口时把窗口往上撑, 别在底部留一片空白。
	for top > 0 && end == len(rows) {
		n := len(lines[top-1])
		if used+n > limit {
			break
		}
		top, used = top-1, used+n
	}
	tablePosLine(c, len(rows), top, end, total > limit)
	for i := top; i < end; i++ {
		c.line(tuiIndent + strings.Join(alignRow(cols, out[i], vis), tuiColSep))
		for _, s := range lines[i][1:] {
			c.line(cont + s)
		}
	}
}

// eventWrapGap 是事件条目续行相对该条第一行事件列开头的缩进 (字符)。
const eventWrapGap = 2

// wrapEventMsg 把事件正文按列宽 (width) 折行: 宽度不够时断在箭头之前 (续行以 "→ "
// 开头), 但换候选 / 同候选重试的转折词 (switch / retry) 放在箭头之后另起一行,
// 让回退的那一步自成一行。续行比首行少 eventWrapGap 列 (缩进); 贪心地往一行里接阶段词,
// 单词本身超过可用宽度时截断 (末尾留 ~, 与同列其余截断同一写法), 不另找断点。
func wrapEventMsg(msg string, width int) []string {
	toks := strings.Split(msg, stageSep)
	if len(toks) == 1 {
		return []string{tuiTrunc(msg, width)}
	}
	// 断在箭头前: 上一行以 " →" 收尾; 断在箭头后: 续行以 "→ " 开头 (两者都是 2 列)。
	trail, lead := stageSep[:len(stageSep)-1], stageSep[1:]
	avail := width
	cur := toks[0]
	if tuiWidth(cur) > avail {
		cur = tuiTrunc(cur, avail)
	}
	out := make([]string, 0, 2)
	for _, tok := range toks[1:] {
		if cand := cur + stageSep + tok; tuiWidth(cand) <= avail {
			cur = cand
			continue
		}
		prev := avail
		avail = max(1, width-eventWrapGap)
		if isTurnWord(tok) && tuiWidth(cur+trail) <= prev {
			out = append(out, cur+trail)
			cur = tok
		} else {
			out = append(out, cur)
			cur = lead + tok
		}
		if tuiWidth(cur) > avail {
			cur = tuiTrunc(cur, avail)
		}
	}
	return append(out, cur)
}

// isTurnWord 判断阶段词是不是回退的转折词 (换候选 switch / 同候选重试 retry)。
func isTurnWord(tok string) bool {
	return strings.HasPrefix(tok, "switch") || strings.HasPrefix(tok, "retry")
}

// eventMergeKey 是显示合并的键: 秒 + 请求编号 + 逻辑名。
type eventMergeKey struct {
	sec     time.Time
	req     uint64
	logical string
}

// mergedEventRows 把原始事件流折成显示行。规则: 同一秒内同一请求 (req>0) 的阶段切换
// 连成一条链 (跨秒的链自然拆成多条, 下一秒从断点接着出); req=0 的事件 (锁 / 探测 /
// 退避 / 启动) 不合并, 一行一条, 仍按时间倒序。
// 时间范围筛选生效时数据换成日志回读的结果 (ev.rows, 不受 256 条上限约束); 文本筛选
// 叠在合并后的链上 (链里任一段命中就保留), 编号 / 逻辑名按原值匹配。
func (m *tuiModel) mergedEventRows() []routeEvent {
	evs := m.events
	if !m.ev.from.IsZero() && m.logPath != "" {
		evs = m.ev.rows
	}
	var keys []eventMergeKey
	buckets := map[eventMergeKey][]routeEvent{}
	for _, e := range evs {
		if !m.ev.inRange(e.Ts) {
			continue
		}
		k := eventMergeKey{sec: e.Ts.Truncate(time.Second), req: e.Req, logical: e.Logical}
		if _, ok := buckets[k]; !ok {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], e)
	}
	rows := make([]routeEvent, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		if k.req == 0 || len(b) == 1 {
			rows = append(rows, b...)
			continue
		}
		msgs := make([]string, len(b))
		for i, e := range b {
			msgs[i] = e.Msg
		}
		last := b[len(b)-1]
		rows = append(rows, routeEvent{Ts: k.sec, Req: k.req, Logical: k.logical,
			Kind: last.Kind, Msg: mergeEventChain(msgs)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Ts.After(rows[j].Ts) })
	if m.ev.q == "" {
		return rows
	}
	keep := rows[:0]
	for _, r := range rows {
		if m.ev.matchText(r) {
			keep = append(keep, r)
		}
	}
	return keep
}

// mergeEventChain 把同一请求同一秒内的事件正文连成一条过渡链。相邻条目里同一个阶段
// 既是前一条的尾也是后一条的头时只留一个; 前后写法不同 (如 sem 与 sem(u1)) 取信息量
// 更多的那个; 真正不同的过渡词 (fail / switch / new / reject …) 直接接在后面。
func mergeEventChain(msgs []string) string {
	toks := make([]string, 0, 16)
	for _, msg := range msgs {
		for _, p := range strings.Split(msg, stageSep) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if n := len(toks); n > 0 && stageBase(toks[n-1]) == stageBase(p) {
				if stageInfo(p) > stageInfo(toks[n-1]) {
					toks[n-1] = p
				}
				continue
			}
			toks = append(toks, p)
		}
	}
	return strings.Join(toks, stageSep)
}

// stageBase 取过渡词括号前的部分 (new / sem / fail …), 判断两个表达是不是同一个阶段。
func stageBase(tok string) string {
	if i := strings.IndexByte(tok, '('); i >= 0 {
		return tok[:i]
	}
	return tok
}

// stageInfo 是过渡词的信息量: 带参数的比裸词多一层信息。
func stageInfo(tok string) int {
	if strings.Contains(tok, "(") {
		return 1
	}
	return 0
}

// eventWho 是标识格: 请求写 编号/逻辑名, 模型写 供应商/模型, 供应商写 供应商;
// 锁 / 探测 / 启动这类不挂请求的事件没有编号, 也不再显示逻辑名。
// 逻辑名与供应商名各自带专属色 (供应商部分由 upModel 负责)。
func (m *tuiModel) eventWho(e routeEvent) string {
	switch {
	case e.Req > 0:
		if e.Logical != "" {
			return strconv.FormatUint(e.Req, 10) + "/" + m.logicalName(e.Logical)
		}
		return strconv.FormatUint(e.Req, 10)
	case e.Logical != "":
		return "-/" + m.logicalName(e.Logical) // 日志回放的事件: 没有请求编号
	case e.Upstream != "" || e.Model != "":
		return m.upModel(e.Upstream, e.Model)
	}
	return "-"
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

// logicalName 给逻辑名上它的专属色, 再叠一层加粗: 标识列的逻辑名是粗体, 正文里的
// 供应商名是细体, 同屏不会看混。配置里没有的名字 (如已删掉的别名) 原样返回。
func (m *tuiModel) logicalName(name string) string {
	if name == "" {
		return ""
	}
	if c := m.logicalColors[name]; c != "" {
		return tuiBold + c + name + tuiReset
	}
	return name
}

// eventCandRe 匹配正文里带候选参数的阶段词: sem(up) / freq(up) / queue(up/mod) /
// switch(up/mod) / retry(up/mod); 参数里第一个 "/" 之前是供应商, 之后是真实模型。
var eventCandRe = regexp.MustCompile(`(sem|freq|queue|switch|retry)\(([^)/]+)(/[^)]+)?\)`)

// eventAbnRe 匹配异常过渡词 retry / switch: 换候选与同候选重试都算异常分支, 整个词上
// 黄字 (参数里的供应商名仍带专属色, 由 paintOver 在 reset 之后补回黄色)。
var eventAbnRe = regexp.MustCompile(`\b(retry|switch)(\([^)]*\))?`)

// eventOKRe 匹配正文里的成功收尾词 ok(200): 只给这一个词上绿字。
var eventOKRe = regexp.MustCompile(`\b(ok)(\([^)]*\))`)

// eventFailRe 匹配失败结局 fail(原因): 整个词上红字。
var eventFailRe = regexp.MustCompile(`\bfail\([^)]*\)`)

// paintOver 把一层语义色叠到已经带专属色的片段上: 片段内嵌的 reset 会清掉颜色,
// 这里在每个 reset 之后补回语义色, 末尾再收一次。
func paintOver(color, s string) string {
	return color + strings.ReplaceAll(s, tuiReset, tuiReset+color) + tuiReset
}

// colorEventBody 给事件正文上色: 失败结局 fail(原因) 红字, 异常过渡词 switch / retry
// 黄字, 成功收尾词 ok(200) 绿字; 阶段词参数里的供应商名用各页同款专属色 (模型部分、
// new(逻辑名) 与 reject 的参数不动; 逻辑名只在标识列上色), 其余文字保持终端前景色。
func (m *tuiModel) colorEventBody(s string) string {
	if len(m.upColors) > 0 {
		s = eventCandRe.ReplaceAllStringFunc(s, func(tok string) string {
			open := strings.IndexByte(tok, '(')
			param := tok[open+1 : len(tok)-1]
			up := param
			if i := strings.IndexByte(param, '/'); i >= 0 {
				up = param[:i]
			}
			return tok[:open+1] + m.upName(up) + param[len(up):] + ")"
		})
	}
	s = eventAbnRe.ReplaceAllStringFunc(s, func(tok string) string { return paintOver(tuiYell, tok) })
	s = eventFailRe.ReplaceAllStringFunc(s, func(tok string) string { return paintOver(tuiRed, tok) })
	return eventOKRe.ReplaceAllString(s, tuiGreen+"$1$2"+tuiReset)
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

// ttfbCell 首字节汇总: 最近一次/平均 (平均 = rateWin 内算术平均)。
func (m *tuiModel) ttfbCell(a routeAgg) string {
	if len(a.ttfbPts) == 0 {
		return "-"
	}
	if avg, ok := ttfbAvg(a.ttfbPts, time.Now(), m.rateWin); ok {
		if last, ok2 := ttfbLast(a.ttfbPts); ok2 {
			return durStr(time.Duration(last)*time.Millisecond) + "/" +
				durStr(time.Duration(avg)*time.Millisecond)
		}
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
