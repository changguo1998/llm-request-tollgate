// tui_settingsdraw.go: settings 页绘制与提示。

package main

import (
	"strconv"
	"strings"
	"time"
)

// drawSettings 画设置页: 三列并排 —— 第 1 列 全局参数 + 供应商, 第 2 / 3 列是模型
// 列表的两栏 (按逻辑名分组, 组与组之间用逻辑名隔开, 两栏连续铺排), 底部信息区
// (常驻告警 / 瞬时结果 / 临时输入)。各区标题中文, 焦点区用 [ 标题 ] 括起; 标题用 tuiTitle 色。
// 列宽按内容算, 模型两栏平分剩余宽度, 一直顶到终端右边界; 模型列顶到内容区底部 (信息区
// 上方), 不留空白。信息区要占几行已由 draw() 从 body 里扣掉, 没有信息时一行都不占。
// 供应商与模型列里的 provider 名沿用第 1 页上游表的专属色 (upNameAttr), 选中行反显保留。
func (m *tuiModel) drawSettings(c *tuiCanvas, body int) {
	e := m.prio
	if e == nil {
		c.line(tuiIndent + tuiYell + "设置页未装载" + tuiReset)
		return
	}
	e.vis = 1
	col1 := m.settingsParamsCol(e, body)
	col2, col3 := m.settingsModelCols(e, body)
	m.settingsGrid(c, col1, col2, col3, body)
	m.settingsInfo(c, e)
}

// settingsInfo 画底部信息区, 自上而下位置固定: 常驻告警 (红字, 逐条一行, 不消失) ->
// 瞬时结果 (10 秒后消失, 同一时刻最多一条, 失败红字 / 说明用前景色) -> 临时输入
// (模型列表的 / 过滤)。只占文字本身的宽度, 不整行反显, 也不补齐铺满。
func (m *tuiModel) settingsInfo(c *tuiCanvas, e *tuiEditor) {
	for _, s := range e.alerts() {
		c.line(tuiRed + tuiIndent + s + tuiReset)
	}
	if text, isErr, ok := e.noticeText(time.Now()); ok {
		col := ""
		if isErr {
			col = tuiRed
		}
		c.line(col + tuiIndent + text + tuiReset)
	}
	if e.hasBottomPrompt() {
		c.line(tuiIndent + e.prompt.label + " " + tuiRev + e.prompt.buffer + " " + tuiReset)
	}
}

// settingsParamsCol 画第 1 列: 全局参数 + 供应商。供应商高度 = 内容区 (信息区已扣除)
// 去掉参数区与区间的空行, 超出的不画; 配置读不到时只显示错误。
func (m *tuiModel) settingsParamsCol(e *tuiEditor, body int) []string {
	provH := 0
	if e.usable() {
		provH = min(len(e.providers), max(1, body-(1+len(gateLabels))-6))
	}
	col1 := m.settingsGateLines(e, body)
	col1 = append(col1, "")
	col1 = append(col1, settingsTitle("供应商", e.focus == 1))
	if !e.usable() {
		return append(col1, tuiYell+e.err+tuiReset)
	}
	for i, name := range e.providers {
		if i >= provH {
			break
		}
		col1 = append(col1, m.settingsProviderLine(e, i, name))
	}
	return col1
}

// settingsGateLines 画全局参数区: 标签左对齐; 冒号后补齐到固定数字列, 数字右对齐
// (小数点/最后一位对齐), 单位第一字母对齐; 焦点行反显。按 = 进入原位输入时, 该行只把
// 数值格换成输入缓冲 (反显当光标), 单位照旧跟在后面, 不再整行换底色。
func (m *tuiModel) settingsGateLines(e *tuiEditor, body int) []string {
	col1 := make([]string, 0, body)
	col1 = append(col1, settingsTitle("全局参数", e.focus == 2))
	labelW := 0
	for i := range gateLabels {
		if w := tuiWidth(gateCN[i]); w > labelW {
			labelW = w
		}
	}
	numW := 0
	for i := range gateLabels {
		n, _ := e.gateNumUnit(i)
		if w := tuiWidth(n); w > numW {
			numW = w
		}
	}
	for i := range gateLabels {
		n, u := e.gateNumUnit(i)
		pad := labelW - tuiWidth(gateCN[i])
		val, w := n, numW
		if e.editingGate(i) {
			val = tuiRev + e.prompt.buffer + tuiReset
			if e.prompt.buffer == "" {
				val = tuiRev + " " + tuiReset // 空缓冲也要看得见光标
			}
			if b := tuiWidth(e.prompt.buffer); b > w {
				w = b // 输入比原值长时把数字列撑开, 不截断
			}
		}
		text := tuiIndent + gateCN[i] + ": " + strings.Repeat(" ", pad) + tuiPadLeft(val, w) + " " + u
		if e.focus == 2 && i == e.gateSel && !e.editingGate(i) {
			text = tuiRev + text + tuiReset
		}
		col1 = append(col1, text)
		// 显示分组: 其他 | 锁时长 | 低性能设置 (含打断超时) | 主题, 区之间空一行
		if i == 5 || i == 9 || i == gateIdxAbort {
			col1 = append(col1, "")
		}
	}
	return col1
}

// settingsProviderLine 画一行供应商: 名字带专属色, 优先级与停用标记跟在后面。
func (m *tuiModel) settingsProviderLine(e *tuiEditor, i int, name string) string {
	attr := ""
	if e.focus == 1 && i == e.provSel {
		attr = tuiRev
	}
	prio := prioOf(e.provEntry(name))
	state := ""
	if prio < 0 {
		state = " (停用)"
	}
	return attr + tuiIndent + m.upNameAttr(name, attr) + "  priority=" + strconv.Itoa(prio) + state + tuiReset
}

// settingsModelCols 画第 2 / 3 列: 模型列表按逻辑名分组后横向分两栏, 连续铺排。
// 模型列独立铺满内容区: 标题 1 行, 其余行全部给模型 (可滚动), 底部信息区的行数
// 已在 draw() 里扣掉。
func (m *tuiModel) settingsModelCols(e *tuiEditor, body int) ([]string, []string) {
	cs := e.cands()
	rows := e.filtered()
	col2 := make([]string, 0, body)
	hidden := len(e.droplist())
	flt := e.filter
	if flt == "" {
		flt = "-"
	}
	t := "模型+组 (过滤: " + flt + ", " + strconv.Itoa(len(rows)+len(e.groupCands)) + " 行, 黑名单剔除 " + strconv.Itoa(hidden) + ")"
	col2 = append(col2, settingsTitle(t, e.focus == 0))
	lines, selLine := m.settingsModelLines(e, cs)
	window := settingsScrollWindow(e, len(lines), len(cs), body, selLine)
	// 两栏都从 e.scrollTop 开始连续铺: 左栏前 window 行, 右栏续后 window 行。
	llen := min(e.scrollTop+window, len(lines))
	rlen := min(e.scrollTop+2*window, len(lines))
	col2 = settingsPane(col2, lines, e.scrollTop, llen)
	col3 := settingsPane(make([]string, 0, body), lines, e.scrollTop+window, rlen)
	return col2, col3
}

// settingsLine 是模型列表的一显示行: row >= 0 是可选行 (cands 下标), -1 是分组标题。
type settingsLine struct {
	row  int
	text string
}

// settingsModelLines 把模型面板的可选行摊成显示行: 逻辑名变化时插一行分组标题
// (别名或组名); 组候选行只列成员逻辑名, 不带 M/P 权重 (组不是权重条目)。返回选中行的
// 显示下标。粘性偏好与锁状态不在配置页 (第 1 页管, 这里只管配置)。
func (m *tuiModel) settingsModelLines(e *tuiEditor, cs []tuiCand) ([]settingsLine, int) {
	lines := make([]settingsLine, 0, len(cs)+8)
	selLine := -1
	last := ""
	for i, c := range cs {
		if c.logical != last {
			lines = append(lines, settingsLine{row: -1, text: settingsLogicalHeader(c.isGroup, c.logical)})
			last = c.logical
		}
		attr := ""
		if e.focus == 0 && i == e.modelSel {
			attr = tuiRev
		}
		var text string
		if c.isGroup {
			// 组候选行: 只显示成员逻辑名, 不带 M/P 权重 (组不是权重条目)。
			text = "  " + c.member
		} else {
			rows := e.filtered()
			if c.row < len(rows) {
				row := rows[c.row]
				// M 后的模型优先级按 2 列右对齐 (负号也算 1 列), 与负数并排时不顶格。
				text = "  M" + tuiPadLeft(strconv.Itoa(prioOf(row.model)), 2) + " P" + strconv.Itoa(prioOf(row.entry)) +
					" " + m.upNameAttr(row.provider, attr) + "/" + modelID(row.model)
			}
		}
		lines = append(lines, settingsLine{row: i, text: attr + text + tuiReset})
		if i == e.modelSel {
			selLine = len(lines) - 1
		}
	}
	return lines, selLine
}

// settingsLogicalHeader 拼逻辑名分组标题: 组名黄色, 别名前景色 (不带「组」字区分,
// 靠颜色)。粘性倒计时在第 1 页模型表的分组标题上, 不在这里。
func settingsLogicalHeader(isGroup bool, logical string) string {
	if isGroup {
		return tuiYell + logical + tuiReset
	}
	return logical
}

// settingsScrollWindow 按显示行算滚动窗口: body 是已扣除底部信息区的行数,
// window = max(1, body-1) (模型列标题占 1 行), 两栏合计 2*window 个显示行; 选中行始终
// 钉在这屏里 (焦点在模型面板时); 返回每栏行数。
func settingsScrollWindow(e *tuiEditor, nLines, nRows, body, selLine int) int {
	window := max(1, body-1) // 标题占 1 行, 其余给模型
	if nLines > 0 {
		window = min(window, nLines)
	}
	e.vis = max(1, window)
	if e.focus == 0 && selLine >= 0 {
		e.modelSel = clampInt(e.modelSel, 0, max(0, nRows-1))
		span := 2 * window
		if selLine < e.scrollTop {
			e.scrollTop = selLine
		}
		if selLine >= e.scrollTop+span {
			e.scrollTop = selLine - span + 1
		}
	}
	if e.scrollTop > max(0, nLines-1) {
		e.scrollTop = max(0, nLines-1)
	}
	return window
}

// settingsPane 取显示行的一段拼成列内容: 分组标题用标题色, 模型行缩进。
func settingsPane(dst []string, lines []settingsLine, from, to int) []string {
	if from < 0 {
		from = 0
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from >= to { // 窗口完全落在显示行之外 (含空列表) 时没有内容
		return dst
	}
	for _, ln := range lines[from:to] {
		if ln.row < 0 {
			dst = append(dst, tuiIndent+tuiTitle+ln.text+tuiReset)
			continue
		}
		dst = append(dst, tuiIndent+ln.text)
	}
	return dst
}

// settingsGrid 三列并排 (模型列表占第 2 / 3 两列): 第 1 列按内容宽, 模型两栏平分
// 剩余宽度, 顶到终端右边界; 内容区不足的行补空, 让三列贴满整个内容高度 (body 已扣除
// 底部信息区占的行)。
func (m *tuiModel) settingsGrid(c *tuiCanvas, col1, col2, col3 []string, body int) {
	w1 := colWidth(col1)
	w2 := max(6, (m.cols-2-w1-3)/2)
	w3 := max(6, m.cols-2-w1-2-w2-1)
	gridH := max(1, body)
	for i := 0; i < gridH; i++ {
		c.line(tuiPad(atSlice(col1, i), w1) + " " + tuiPad(atSlice(col2, i), w2) + " " + tuiPad(atSlice(col3, i), w3))
	}
}

// atSlice 取切片第 i 项, 越界给空串 (各列行数不同时补空)。
func atSlice(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// colWidth 是切片里最宽一行的显示宽度。
func colWidth(s []string) int {
	w := 0
	for _, l := range s {
		if n := tuiWidth(l); n > w {
			w = n
		}
	}
	return w
}

// settingsTitle 拼区标题: 焦点时加方括号, 一律用标题色。
func settingsTitle(text string, focused bool) string {
	if focused {
		return tuiTitle + "[ " + text + " ]" + tuiReset
	}
	return tuiTitle + text + tuiReset
}
