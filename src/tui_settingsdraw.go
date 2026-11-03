// tui_settingsdraw.go: settings 页绘制与提示。

package main

import (
	"strconv"
)

// drawSettings 画设置页: 三列并排 —— 第 1 列 全局参数 + 供应商, 第 2 列 模型, 第 3 列 预览,
// 底部消息行。各区标题中文, 焦点区用 [ 标题 ] 括起; 标题用 tuiTitle 色。列宽按内容算,
// 第 3 列吃掉剩余宽度, 一直顶到终端右边界。
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
	visible := max(1, body-gateLines-provH-5)
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
	// 冒号紧跟文字后面; 值列右对齐到固定宽, 让数字 (与单位) 成列对齐。
	valueW := 0
	for i := range gateLabels {
		if w := tuiWidth(e.gateText(i)); w > valueW {
			valueW = w
		}
	}
	for i := range gateLabels {
		text := tuiIndent + gateCN[i] + ": " + tuiPadLeft(e.gateText(i), valueW)
		if e.focus == 2 && i == e.gateSel {
			text = tuiRev + text + tuiReset
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
			col1 = append(col1, attr+tuiIndent+name+"  priority="+strconv.Itoa(prioOf(e.provEntry(name)))+tuiReset)
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
		text := tuiIndent + "M" + strconv.Itoa(prioOf(row.model)) + " P" + strconv.Itoa(prioOf(row.entry)) +
			" " + row.provider + "/" + modelID(row.model)
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
	return tuiRev + tuiPad(tuiIndent+text+" ", m.cols-1) + tuiReset
}
