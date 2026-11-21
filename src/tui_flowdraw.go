// tui_flowdraw.go: active 页表格绘制与窗口滚动。

package main

import (
	"strconv"
	"strings"
)

// drawFlowChainCol 是阶段链所在列 (最后列, 与事件页的事件列一样独占剩余宽度)。
const drawFlowChainCol = 3

// drawFlow 是第 1 页: 自上而下依次是 上游状态表 / 模型-路由聚合表 / 请求表 (每笔请求
// 按尝试一行, 最新请求在最上面)。三张表一起滚动, 页面上没有固定表头; 请求表的阶段链
// 列按「阶段网格」渲染: 每阶段一格 (序号在前, 格宽全局统一), 各请求的第 n 阶段纵向对齐,
// 格子间用 " → " 分隔; 列放不下时折行, 折出来的新显示行重新从 1 计数。
func (m *tuiModel) drawFlow(c *tuiCanvas, body int) {
	full := &tuiCanvas{cols: c.cols}
	curLine := m.drawModelsBody(full) // 上游状态 + 模型/路由聚合, 排在最前
	lines := m.flowLines()
	posW := flowPosWidth(lines)
	// 列宽按「补齐后的整行网格」算: 阶段位各自补宽后才是网格实宽, 否则长格一被补宽,
	// 原本一排能放下的请求也会被误折行。
	fitRows := make([][]string, 0, len(lines))
	for _, ln := range lines {
		fitRows = append(fitRows, append(append([]string(nil), ln.head...),
			paddedFlowChain(ln.stages, ln.tail, posW)))
	}
	cols, vis := tableLayout(activeCols, fitRows, c.cols)
	colW := cols[drawFlowChainCol].w
	blocks := flowBlocks(posW, colW)
	// 折行行 (无标识列) 顶到阶段列起点: 这样所有显示行的第 1 阶段都从同一列起。
	cont := strings.Repeat(" ", colOffset(cols, vis, drawFlowChainCol))
	full.line("") // 与上面的模型表空一行
	full.line(tableHead(cols, vis))
	for _, ln := range lines {
		grid := flowGrid(ln.stages, ln.tail, posW, blocks, colW)
		first := append(append([]string(nil), ln.head...), grid[0])
		full.line(tuiIndent + strings.Join(alignRow(cols, first, vis), tuiColSep))
		for _, g := range grid[1:] {
			full.line(cont + g)
		}
	}
	if len(lines) == 0 {
		full.line(tuiGray + "  (没有进行中的请求)" + tuiReset)
	}
	m.followCursor(curLine, body)
	m.drawWindow(c, full, max(1, body))
}

// flowRowsFromLines 把结构化行摊成表格行 (阶段链 = 阶段格用 " → " 连起 + 链尾)。
func flowRowsFromLines(lines []flowLine) [][]string {
	out := make([][]string, 0, len(lines))
	for _, ln := range lines {
		chain := strings.Join(ln.stages, stageSep)
		if len(ln.tail) > 0 {
			if chain == "" {
				chain = strings.Join(ln.tail, " ")
			} else {
				chain += " " + strings.Join(ln.tail, " ")
			}
		}
		out = append(out, append(append([]string(nil), ln.head...), chain))
	}
	return out
}

// flowSepW 是阶段分隔符 " → " 的显示宽 (stageSep 由三部分组成: 空格 + 箭头 + 空格)。
const flowSepW = 3

// flowPosWidth 算每个阶段位 (0 基, 排满所有显示行) 的格宽: 序数 "N " + 该位最宽内容。
// 位宽按位取最大, 各显示行的第 n 个阶段就能从同一列起。
func flowPosWidth(lines []flowLine) []int {
	maxPos := 0
	for _, ln := range lines {
		if len(ln.stages) > maxPos {
			maxPos = len(ln.stages)
		}
	}
	w := make([]int, maxPos)
	for _, ln := range lines {
		for i, s := range ln.stages {
			if n := tuiWidth(s) + 2; n > w[i] {
				w[i] = n
			}
		}
	}
	return w
}

// flowBlocks 把阶段位按累计格宽分块 (每个 [起点, 个数], 0 基): 每块总宽 (位宽 + 分隔)
// 不超过阶段列宽; 一块就是一条显示行, 块内序数重新从 1 起。
func flowBlocks(w []int, colW int) [][2]int {
	var out [][2]int
	i := 0
	for i < len(w) {
		start, width := i, 0
		for i < len(w) {
			add := w[i]
			if i > start {
				add += flowSepW
			}
			if width+add > colW && i > start {
				break
			}
			width += add
			i++
		}
		out = append(out, [2]int{start, i - start})
	}
	return out
}

// paddedFlowChain 把一条阶段链按位宽补齐成一整行 (不做折行, 编号 1..P): 供 tableLayout
// 算阶段列宽用 —— 网格的实宽是各阶段位补宽后的总和, 不是未补齐的内容宽, 否则长格一被
// 补宽, 原本一排能放下的请求也会被误折行。
func paddedFlowChain(stages, tail []string, posW []int) string {
	if len(stages) == 0 {
		if len(tail) == 0 {
			return ""
		}
		return strings.Join(tail, " ")
	}
	parts := make([]string, 0, len(stages))
	for i, s := range stages {
		cell := tuiTrunc(strconv.Itoa(i+1)+" "+s, posW[i])
		parts = append(parts, tuiPad(cell, posW[i]))
	}
	line := strings.Join(parts, stageSep)
	if len(tail) > 0 {
		line += " " + strings.Join(tail, " ")
	}
	return line
}

// flowGrid 把一条阶段链排成对齐的显示行: 每阶段一格 (序数前置), 格宽按位统一, 各显示行
// 的第 n 个阶段的起点落在同一列; 位排不下时折行, 折出的新显示行重新从 1 计数。链尾挂在
// 最后一行末尾 (超宽截断)。返回每显示行的字符串 (不含前几列)。
func flowGrid(stages, tail []string, w []int, blocks [][2]int, colW int) []string {
	if len(stages) == 0 {
		if len(tail) == 0 {
			return []string{""}
		}
		return []string{tuiTrunc(strings.Join(tail, " "), colW)}
	}
	var out []string
	for _, blk := range blocks {
		start, n := blk[0], blk[1]
		if start >= len(stages) {
			break // 这一行阶段已经排完
		}
		last := start+n >= len(stages) // 这条链的最后一格落在这块
		parts := make([]string, 0, n)
		for p := start; p < start+n && p < len(stages); p++ {
			cell := tuiTrunc(strconv.Itoa(p-start+1)+" "+stages[p], w[p])
			parts = append(parts, tuiPad(cell, w[p]))
		}
		line := strings.Join(parts, stageSep)
		if last && len(tail) > 0 {
			line += " " + strings.Join(tail, " ")
		}
		out = append(out, tuiTrunc(line, colW))
	}
	return out
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
