// tui_settingsdraw.go: settings 页绘制与提示。

package main

import (
	"strconv"
)

// drawSettings 画设置页: 左栏调度参数 (gate) + provider 面板 + 模型列表, 右栏候选顺序
// 预览, 底部消息行。列宽按内容算, 右栏一直顶到终端右边界; 不补装饰空行, 短的一栏就
// 让右侧继续显示。
func (m *tuiModel) drawSettings(c *tuiCanvas, body int) {
	e := m.prio
	if e == nil {
		c.line(tuiIndent + tuiYell + "设置页未装载" + tuiReset)
		return
	}
	gateLines := 1 + len(gateLabels)
	provH := 0
	if e.usable() {
		// 上游列表优先显示全 (通常就 4-6 个), 剩下的行给模型列表。
		provH = min(len(e.providers), max(1, body-gateLines-7))
	}
	// 段与段之间留一个空行: gate / Providers / 模型。
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

	left := make([]string, 0, body)
	focusGate := ""
	if e.focus == 2 {
		focusGate = " (焦点)"
	}
	left = append(left, tuiBold+"调度参数 (gate)"+focusGate+tuiReset)
	for i := range gateLabels {
		text := tuiIndent + gateLabels[i] + "  " + e.gateText(i)
		if e.focus == 2 && i == e.gateSel {
			text = tuiRev + text + tuiReset
		}
		left = append(left, text)
	}
	left = append(left, "")
	if !e.usable() {
		left = append(left, tuiYell+"store 读不到: "+e.err+tuiReset)
	} else {
		left = append(left, tuiBold+e.provTitle()+tuiReset)
		for i, name := range e.providers {
			if i >= provH {
				break
			}
			attr := ""
			if e.focus == 1 && i == e.provSel {
				attr = tuiRev
			}
			left = append(left, attr+tuiIndent+name+"  priority="+strconv.Itoa(prioOf(e.provEntry(name)))+tuiReset)
		}
		left = append(left, "")
		hidden := len(e.droplist())
		left = append(left, tuiBold+e.listTitle(len(rows), hidden)+tuiReset)
		for i := e.top; i < min(len(rows), e.top+visible); i++ {
			row := rows[i]
			attr := ""
			if e.focus == 0 && i == e.modelSel {
				attr = tuiRev
			}
			text := tuiIndent + "M" + strconv.Itoa(prioOf(row.model)) + " P" + strconv.Itoa(prioOf(row.entry)) +
				" " + row.provider + "/" + modelID(row.model)
			left = append(left, attr+text+tuiReset)
		}
	}

	right := make([]string, 0, body)
	right = append(right, tuiBold+tuiIndent+"候选顺序预览"+tuiReset)
	if row := e.selected(); row != nil {
		cands := e.candsFor(row.alias)
		right = append(right, tuiBold+tuiIndent+row.alias+" ("+strconv.Itoa(len(cands))+" 候选)"+tuiReset)
		for i, cand := range cands {
			attr := ""
			if cand.provider == row.provider && cand.modelID == modelID(row.model) {
				attr = tuiRev
			}
			text := tuiIndent + strconv.Itoa(i+1) + ". " + cand.provider + "/" + cand.modelID +
				"  M" + strconv.Itoa(cand.modelPrio) + " P" + strconv.Itoa(cand.provPrio)
			right = append(right, attr+text+tuiReset)
		}
	}

	// 左右宽度都按内容算; 右栏吃掉剩余宽度, 一直顶到终端右边界。
	leftW := 32
	for _, s := range left {
		leftW = max(leftW, tuiWidth(s)+1)
	}
	leftW = min(leftW, max(24, m.cols-16))
	rightW := max(8, m.cols-leftW-1)

	n := max(len(left), len(right))
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		c.line(tuiPad(l, leftW) + tuiPad(r, rightW))
	}
	c.line(m.prioMessage(e))
}

func (e *tuiEditor) provTitle() string {
	focus := ""
	if e.focus == 1 {
		focus = " (焦点)"
	}
	return tuiIndent + "Providers" + focus
}

func (e *tuiEditor) listTitle(shown, hidden int) string {
	focus := ""
	if e.focus == 0 {
		focus = " (焦点)"
	}
	filter := e.filter
	if filter == "" {
		filter = "-"
	}
	return tuiIndent + "模型 (过滤: " + filter + ", " + strconv.Itoa(shown) + " 个, 黑名单剔除 " +
		strconv.Itoa(hidden) + ")" + focus
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
