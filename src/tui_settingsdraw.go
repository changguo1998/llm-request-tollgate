// tui_settingsdraw.go: settings 页绘制与提示。

package main

import (
	"strconv"
	"strings"
)

// drawSettings 画设置页: 三列并排 —— 第 1 列 全局参数 + 供应商, 第 2 列 模型, 第 3 列 预览,
// 底部消息行。各区标题中文, 焦点区用 [ 标题 ] 括起; 标题用 tuiTitle 色。列宽按内容算,
// 第 3 列吃掉剩余宽度, 一直顶到终端右边界; 模型列顶到内容区底部 (消息行上方), 不留空白。
// 供应商与模型列里的 provider 名沿用第 1 页上游表的专属色 (upNameAttr), 选中行反显保留。
func (m *tuiModel) drawSettings(c *tuiCanvas, body int) {
	e := m.prio
	if e == nil {
		c.line(tuiIndent + tuiYell + "设置页未装载" + tuiReset)
		return
	}
	gateLines := 1 + len(gateLabels)
	provH := 0
	if e.usable() {
		provH = min(len(e.providers), max(1, body-gateLines-7))
	}
	// 模型列独立铺满内容区: 标题 1 行 + 底部消息行 1 行, 其余行全部给模型 (可滚动),
	// 不再被第 1 列的高度公式压短。
	visible := max(1, body-2)
	rows := e.filtered()
	if len(rows) == 0 {
		visible = 0
	} else {
		visible = min(visible, len(rows))
	}
	e.vis = max(1, visible)
	if e.focus == 0 && len(rows) > 0 {
		e.modelSel = clampInt(e.modelSel, 0, len(rows)-1)
		if e.modelSel < e.top {
			e.top = e.modelSel
		}
		if e.modelSel >= e.top+visible {
			e.top = e.modelSel - visible + 1
		}
	}
	if e.top > max(0, len(rows)-1) {
		e.top = max(0, len(rows)-1)
	}

	// ---- 第 1 列: 全局参数 + 供应商 ----
	col1 := make([]string, 0, body)
	col1 = append(col1, settingsTitle("全局参数", e.focus == 2))
	// 标签左对齐; 冒号后补齐到固定数字列, 数字右对齐 (小数点/最后一位对齐), 单位第一字母对齐。
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
	}
	col1 = append(col1, "")
	col1 = append(col1, settingsTitle("供应商", e.focus == 1))
	if e.usable() {
		for i, name := range e.providers {
			if i >= provH {
				break
			}
			attr := ""
			if e.focus == 1 && i == e.provSel {
				attr = tuiRev
			}
			col1 = append(col1, attr+tuiIndent+m.upNameAttr(name, attr)+"  priority="+strconv.Itoa(prioOf(e.provEntry(name)))+tuiReset)
		}
	} else {
		col1 = append(col1, tuiYell+"store 读不到: "+e.err+tuiReset)
	}

	// ---- 第 2 列: 模型 ----
	col2 := make([]string, 0, body)
	hidden := len(e.droplist())
	flt := e.filter
	if flt == "" {
		flt = "-"
	}
	t := "模型 (过滤: " + flt + ", " + strconv.Itoa(len(rows)) + " 个, 黑名单剔除 " + strconv.Itoa(hidden) + ")"
	col2 = append(col2, settingsTitle(t, e.focus == 0))
	for i := e.top; i < min(len(rows), e.top+visible); i++ {
		row := rows[i]
		attr := ""
		if e.focus == 0 && i == e.modelSel {
			attr = tuiRev
		}
		// M 后的模型优先级按 2 列右对齐 (负号也算 1 列), 与负数并排时不顶格。
		text := tuiIndent + "M" + tuiPadLeft(strconv.Itoa(prioOf(row.model)), 2) + " P" + strconv.Itoa(prioOf(row.entry)) +
			" " + m.upNameAttr(row.provider, attr) + "/" + modelID(row.model)
		col2 = append(col2, attr+text+tuiReset)
	}

	// ---- 第 3 列: 预览 ----
	col3 := make([]string, 0, body)
	col3 = append(col3, settingsTitle("预览", false))
	if row := e.selected(); row != nil {
		cands := e.candsFor(row.alias)
		col3 = append(col3, tuiBold+tuiIndent+row.alias+" ("+strconv.Itoa(len(cands))+" 候选)"+tuiReset)
		for i, cand := range cands {
			attr := ""
			if cand.provider == row.provider && cand.modelID == modelID(row.model) {
				attr = tuiRev
			}
			text := tuiIndent + strconv.Itoa(i+1) + ". " + cand.provider + "/" + cand.modelID +
				"  M" + strconv.Itoa(cand.modelPrio) + " P" + strconv.Itoa(cand.provPrio)
			col3 = append(col3, attr+text+tuiReset)
		}
	}

	// 第 1 / 2 列按内容宽, 第 3 列吃掉剩余; 内容区不足的行补空, 让三列贴满整个内容高度
	// (中间空白), 消息行固定落在内容区最底一行 —— 不再让它浮在中间。
	w1 := colWidth(col1)
	w2 := colWidth(col2)
	w3 := max(6, m.cols-2-w1-w2-2)
	gridH := max(1, body-1)
	for i := 0; i < gridH; i++ {
		c.line(tuiPad(atSlice(col1, i), w1) + " " + tuiPad(atSlice(col2, i), w2) + " " + tuiPad(atSlice(col3, i), w3))
	}
	c.line(m.prioMessage(e))
}

// atSlice 取切片第 i 项, 越界给空串 (三列行数不同时补空)。
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
		text += " store 读不到: " + e.err
	}
	bg := tuiRev
	if e.prompt != nil {
		bg = tuiEditBg // = 编辑输入态: 整行换强调背景色
	}
	return bg + tuiPad(tuiIndent+text+" ", m.cols-1) + tuiReset
}
