// tui_flowdraw.go: active 页表格绘制与窗口滚动。

package main

import (
	"strings"
)

// drawFlowChainCol 是阶段链所在列 (最后列, 与事件页的事件列一样独占剩余宽度)。
const drawFlowChainCol = 3

// drawFlow 是第 1 页: 自上而下依次是 上游状态表 / 模型-路由聚合表 / 请求表 (每笔请求
// 按尝试一行, 最新请求在最上面)。三张表一起滚动, 页面上没有固定表头; 请求表的阶段链与
// 事件页一样在宽度不够时折行 (断在箭头之前, 续行相对链首缩进 eventWrapGap)。上面两张表
// 带行光标 (h/j/k/l 移动, u/Enter/c 操作), 光标动过时窗口跟到光标行。
func (m *tuiModel) drawFlow(c *tuiCanvas, body int) {
	full := &tuiCanvas{cols: c.cols}
	curLine := m.drawModelsBody(full) // 上游状态 + 模型/路由聚合, 排在最前
	rows := m.flowRows()
	cols, vis := tableLayout(activeCols, rows, c.cols)
	cont := strings.Repeat(" ", colOffset(cols, vis, drawFlowChainCol)+eventWrapGap)
	full.line("") // 与上面的模型表空一行
	full.line(tableHead(cols, vis))
	for _, r := range rows {
		segs := wrapEventMsg(r[drawFlowChainCol], cols[drawFlowChainCol].w)
		first := append(append([]string(nil), r[:drawFlowChainCol]...), segs[0])
		full.line(tuiIndent + strings.Join(alignRow(cols, first, vis), tuiColSep))
		for _, s := range segs[1:] {
			full.line(cont + s)
		}
	}
	if len(rows) == 0 {
		full.line(tuiDim + "  (没有进行中的请求)" + tuiReset)
	}
	m.followCursor(curLine, body)
	m.drawWindow(c, full, max(1, body))
}

// followCursor 记下光标行; 光标刚移动过时把窗口滚到它 (用户用方向键 / PgUp / PgDn 自己
// 滚开时不抢回 —— 只有光标动过才跟)。
func (m *tuiModel) followCursor(line, body int) {
	m.curLine = line
	if !m.curFollow {
		return
	}
	m.curFollow = false
	if line < 0 || body < 1 {
		return
	}
	top := m.scroll[tabActive]
	switch {
	case line < top:
		m.scroll[tabActive] = line
	case line >= top+body:
		m.scroll[tabActive] = line - body + 1
	}
}

// colKeep 标出哪些列在 rows 里至少有一个"有值"的格: 空串、0、- 都算空 ——
// 于是整列全是 0 (如排队 / 连错都为零) 或全是 - 的列 (缺字段的速率) 会被整列收掉。
func colKeep(cols []tuiCol, rows [][]string) []bool {
	keep := make([]bool, len(cols))
	for i := range keep {
		for _, r := range rows {
			if i < len(r) {
				s := strings.TrimSpace(r[i])
				if s != "" && s != "0" && s != "-" {
					keep[i] = true
					break
				}
			}
		}
	}
	return keep
}

// pickCols 按 colKeep 的结果裁列。
func pickCols(cols []tuiCol, keep []bool) []tuiCol {
	out := make([]tuiCol, 0, len(cols))
	for i, col := range cols {
		if i < len(keep) && keep[i] {
			out = append(out, col)
		}
	}
	return out
}

// pickRow 按 colKeep 的结果裁一行; pickRows 的单行版。
func pickRow(keep []bool, r []string) []string {
	out := make([]string, 0, len(keep))
	for i := 0; i < len(keep) && i < len(r); i++ {
		if keep[i] {
			out = append(out, r[i])
		}
	}
	return out
}

// pickRows 按 colKeep 的结果裁行; 与 pickCols 用同一份 keep, 下标保持一致。
func pickRows(keep []bool, rows [][]string) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, pickRow(keep, r))
	}
	return out
}

// drawWindow 把画布按滚动位置裁一屏贴到 c (第 1 页: 三张表一起滚)。
// 当前窗口写在键位提示行里。
func (m *tuiModel) drawWindow(c *tuiCanvas, full *tuiCanvas, body int) {
	lines := full.splitLines()
	m.pageTotal = len(lines)
	top := m.scrollWindow(len(lines), body)
	m.pageTop, m.pageWin = top, body
	for _, ln := range lines[top:min(top+body, len(lines))] {
		c.raw(ln)
	}
}
