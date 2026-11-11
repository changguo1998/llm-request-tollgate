// tui_flowdraw.go: active 页表格绘制与窗口滚动。

package main

import (
	"strings"
)

// drawFlowChainCol 是阶段链所在列 (最后列, 与事件页的事件列一样独占剩余宽度)。
const drawFlowChainCol = 3

// drawFlow 是第 1 页: 一张请求表 (每笔请求按尝试一行, 最新请求在最上面)。阶段链与事件页
// 一样在宽度不够时折行 (断在箭头之前, 续行相对链首缩进 eventWrapGap), 表头固定在页签
// 下方, 表体与下面的上游状态 / 模型路由聚合两张表一起滚动。
func (m *tuiModel) drawFlow(c *tuiCanvas, body int) {
	rows := m.flowRows()
	cols, vis := tableLayout(activeCols, rows, c.cols)
	c.line("") // 表头上方空一行, 不顶在页签下面
	c.line(tableHead(cols, vis))
	cont := strings.Repeat(" ", colOffset(cols, vis, drawFlowChainCol)+eventWrapGap)
	full := &tuiCanvas{cols: c.cols}
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
	m.drawModelsBody(full) // 上游状态 + 模型/路由聚合, 接在请求表下方
	m.drawWindow(c, full, max(1, body-2))
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

// pickRows 按 colKeep 的结果裁行; 与 pickCols 用同一份 keep, 下标保持一致。
func pickRows(keep []bool, rows [][]string) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		cell := make([]string, 0, len(keep))
		for i := 0; i < len(keep) && i < len(r); i++ {
			if keep[i] {
				cell = append(cell, r[i])
			}
		}
		out = append(out, cell)
	}
	return out
}

// dropEmptyCols 去掉整列都是空串的列 (例如这批请求都还没选中上游时)。
// 单张表用这一对函数就够了; 一页多块共用一份表头时, 直接用 colKeep / pickRows。
func dropEmptyCols(cols []tuiCol, rows [][]string) ([]tuiCol, [][]string) {
	keep := colKeep(cols, rows)
	return pickCols(cols, keep), pickRows(keep, rows)
}

// drawWindow 把画布按滚动位置裁一屏贴到 c (第 1 页: 请求表 + 两张模型表一起滚)。
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
