// tui_settingsdraw.go: settings 页绘制与提示。

package main

import (
	"strconv"
	"strings"
	"time"
)

// drawSettings 画设置页: 三列并排 —— 第 1 列 全局参数 + 供应商, 第 2 / 3 列是模型
// 列表的两栏 (按逻辑名分组, 与 sticky 页同款: 组与组之间用逻辑名隔开, 两栏连续铺排),
// 底部消息行。各区标题中文, 焦点区用 [ 标题 ] 括起; 标题用 tuiTitle 色。列宽按内容算,
// 模型两栏平分剩余宽度, 一直顶到终端右边界; 模型列顶到内容区底部 (消息行上方), 不留空白。
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
	c.line(m.prioMessage(e))
}

// settingsParamsCol 画第 1 列: 全局参数 + 供应商。供应商高度 = 内容区去掉参数区与
// 固定留白, 超出的不画; 配置读不到时只显示错误。
func (m *tuiModel) settingsParamsCol(e *tuiEditor, body int) []string {
	provH := 0
	if e.usable() {
		provH = min(len(e.providers), max(1, body-(1+len(gateLabels))-7))
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
// (小数点/最后一位对齐), 单位第一字母对齐; 焦点行反显, 正在输入时换编辑底色。
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
		text := tuiIndent + gateCN[i] + ": " + strings.Repeat(" ", pad) + tuiPadLeft(n, numW) + " " + u
		if e.focus == 2 && i == e.gateSel {
			if e.prompt != nil {
				text = tuiEditBg + text + tuiReset // 正在 = 编辑: 蓝底强调
			} else {
				text = tuiRev + text + tuiReset
			}
		}
		col1 = append(col1, text)
		if i == 5 || i == 9 { // 显示分组: 其他 | 锁时长 | 低性能设置, 区之间空一行
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
// 模型列独立铺满内容区: 标题 1 行 + 底部消息行 1 行, 其余行全部给模型 (可滚动),
// 不再被第 1 列的高度公式压短。
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

// settingsModelLines 把模型面板的可选行摊成显示行: 逻辑名变化时插一行加粗分组标题
// (别名或组名); 每行固定 1 列标记位, 是该逻辑名当前粘性偏好目标时标 *。组候选行不带
// M/P 权重 (只置偏好), 与别名行视觉上分开。返回选中行的显示下标。
func (m *tuiModel) settingsModelLines(e *tuiEditor, cs []tuiCand) ([]settingsLine, int) {
	lines := make([]settingsLine, 0, len(cs)+8)
	selLine := -1
	last := ""
	for i, c := range cs {
		if c.logical != last {
			lines = append(lines, settingsLine{row: -1, text: m.settingsLogicalHeader(c.isGroup, c.logical)})
			last = c.logical
		}
		attr := ""
		if e.focus == 0 && i == e.modelSel {
			attr = tuiRev
		}
		mark := " "
		if p, ok := m.pulse.Sticky[c.logical]; ok && stickyCandHit(c, p) {
			mark = "*"
		}
		var text string
		if c.isGroup {
			// 组候选行: 只显示成员逻辑名, 不带 M/P 权重 (组不是权重条目, 只置偏好)。
			text = "  " + mark + " " + c.member
		} else {
			rows := e.filtered()
			if c.row < len(rows) {
				row := rows[c.row]
				// M 后的模型优先级按 2 列右对齐 (负号也算 1 列), 与负数并排时不顶格。
				text = "  " + mark + " M" + tuiPadLeft(strconv.Itoa(prioOf(row.model)), 2) + " P" + strconv.Itoa(prioOf(row.entry)) +
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

// settingsLogicalHeader 拼逻辑名分组标题: 别名加粗 / 组名加粗换黄色 (不带「组」字
// 区分, 靠颜色): 该逻辑名有粘性偏好时标题后带剩余时长 (与 sticky 页同款倒计时列)。
func (m *tuiModel) settingsLogicalHeader(isGroup bool, logical string) string {
	attr := tuiBold
	if isGroup {
		attr = tuiBold + tuiYell
	}
	hdr := attr + logical + tuiReset
	if p, ok := m.pulse.Sticky[logical]; ok {
		if rem := time.Duration(p.RemainingMs) * time.Millisecond; rem > 0 {
			hdr += "  " + tuiDim + durStr(rem) + tuiReset
		}
	}
	return hdr
}

// stickyCandHit 判断某候选行是否是当前偏好目标: 别名行精确匹配 (上游/模型),
// 组行匹配成员候选链里的任一项 (组偏好可能迁到成员的非首候选)。
func stickyCandHit(c tuiCand, p stickyView) bool {
	if c.isGroup {
		for _, tgt := range c.chain {
			if tgt[0] == p.Upstream && tgt[1] == p.Model {
				return true
			}
		}
		return false
	}
	return p.Upstream == c.provider && p.Model == c.model
}

// settingsScrollWindow 按显示行算滚动窗口: 模型列表横向分两栏, 一屏 = 2*window 个
// 显示行 (左栏 + 右栏), 选中行始终钉在这屏里 (焦点在模型面板时); 返回每栏行数。
func settingsScrollWindow(e *tuiEditor, nLines, nRows, body, selLine int) int {
	window := max(1, body-2)
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
// 剩余宽度, 顶到终端右边界; 内容区不足的行补空, 让三列贴满整个内容高度。
func (m *tuiModel) settingsGrid(c *tuiCanvas, col1, col2, col3 []string, body int) {
	w1 := colWidth(col1)
	w2 := max(6, (m.cols-2-w1-3)/2)
	w3 := max(6, m.cols-2-w1-2-w2-1)
	gridH := max(1, body-1)
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

// prioMessage 拼底部消息行 (输入态、未保存提醒、错误都在这一行)。
func (m *tuiModel) prioMessage(e *tuiEditor) string {
	text := e.message
	if e.prompt != nil {
		text = e.prompt.label + " " + e.prompt.buffer
	}
	if e.dirty {
		text += " [优先级未保存]"
	}
	if e.gateDirty {
		text += " [调度参数未保存]"
	}
	if e.gateErr != "" {
		text += " 配置读不到: " + e.gateErr
	}
	if e.err != "" {
		text += " " + e.err
	}
	bg := tuiRev
	if e.prompt != nil {
		bg = tuiEditBg // = 编辑输入态: 整行换强调背景色
	}
	return bg + tuiPad(tuiIndent+text+" ", m.cols-1) + tuiReset
}
