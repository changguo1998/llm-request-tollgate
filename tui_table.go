// tui_table.go: 表格列宽、对齐与滚动。

package main

import (
	"sort"
	"strconv"
	"strings"
)

// tuiCol 是表格的一列: 标题 + 基准宽 + 是否按内容涨 (+ 右对齐)。
// w 是基准宽: 内容伸缩与压缩都以它为下限。
// grow 为真时列宽跟着内容涨, 上限是终端右边界 (不写死); 为假 = 定宽列, 不随内容涨。
// hide 是被牺牲的先后 (只统计可见列): 0 = 最后才砍, 越大越先砍; 4 = 永不砍。
type tuiCol struct {
	title string
	w     int
	grow  bool
	right bool
	hide  int
}

// tableLayout 按终端宽度定好一张表的列宽与可见列。一页只有一份表头时用它:
// 列只算一次, 后面每块都用同一套列, 列才不会错位。
func tableLayout(cols []tuiCol, rows [][]string, width int) ([]tuiCol, []bool) {
	cols = autoCols(cols, rows, width)
	return cols, fitCols(cols, width)
}

// tableHead 是表头那一行 (列名, 按可见列对齐)。
func tableHead(cols []tuiCol, vis []bool) string {
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = tuiTrunc(col.title, col.w)
	}
	return tuiIndent + tuiBold + strings.Join(alignRow(cols, head, vis), tuiColSep) + tuiReset
}

// drawTable 画一张表: 标题行 + 表头 + 行 + 滚动位置 + 单位行 (标题与单位是灰字, 空表也画)。
func drawTable(c *tuiCanvas, title string, cols []tuiCol, rows [][]string, scroll, limit int) {
	cols, vis := tableLayout(cols, rows, c.cols)
	if title != "" {
		c.line(tuiDim + tuiIndent + title + tuiReset)
	}
	c.line(tableHead(cols, vis))
	if len(rows) == 0 {
		c.line(tuiDim + "  (空)" + tuiReset)
		return
	}
	top := clampScroll(scroll, len(rows), limit)
	end := min(top+limit, len(rows))
	// 位置行画在行之前: 内容高过屏幕时画布会裁尾部, 放最后就会被裁掉 (行比位置重要,
	// 所以裁的是行, 不是它)。
	if len(rows) > limit {
		c.line(tuiDim + "  " + strconv.Itoa(len(rows)) + " 行, 当前 " +
			strconv.Itoa(top+1) + "-" + strconv.Itoa(end) + tuiReset)
	}
	for i := top; i < end; i++ {
		c.line(tuiIndent + strings.Join(alignRow(cols, rows[i], vis), tuiColSep))
	}
}

// clampScroll 把滚动位置夹进 [0, rows-limit]: 空表与 G 键 (设成很大) 都靠它兜底,
// 否则会画出一片空白。整页滚动与逐表滚动共用这一处口径。
func clampScroll(top, rows, limit int) int {
	if limit < 1 {
		limit = 1
	}
	if maxTop := rows - limit; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// fitCols 在窄终端上按 hide 档位丢列 (0 = 最后丢, 4 = 永不丢), 至少留 3 列。
func fitCols(cols []tuiCol, width int) []bool {
	vis := make([]bool, len(cols))
	for i := range vis {
		vis[i] = true
	}
	total := func() int {
		n, shown := len(tuiIndent), 0
		for i, col := range cols {
			if !vis[i] {
				continue
			}
			n += col.w + tuiColGap
			shown++
		}
		if shown > 1 {
			n -= tuiColGap
		}
		return n
	}
	for drop := 1; drop <= 3; drop++ {
		for i, col := range cols {
			if total() <= width {
				return vis
			}
			if col.hide == drop {
				vis[i] = false
			}
		}
	}
	return vis
}

// tableWidth 是一行表格的显示宽度: 缩进 + 各列列宽 + 列间距 (tuiColGap)。
func tableWidth(cols []tuiCol) int {
	n := len(tuiIndent)
	for i, col := range cols {
		if i > 0 {
			n += tuiColGap
		}
		n += col.w
	}
	return n
}

// cutRank 是列被牺牲的先后: 越小越先被牺牲 (hide 4 = 永不砍, 排最前)。
func cutRank(hide int) int {
	if hide >= 4 {
		return -1
	}
	return hide
}

// autoCols 按内容与可用宽度分配列宽: 每列先占住基准宽 (下限), 余量按"要得少的先补齐"
// 分给各列 (差得一样多时重要的先), 一直补到该列的内容宽为止。列宽不写死上限, 上限就是
// 终端右边界: 内容长的列一路顶到边界, 顶满之后还有的内容才截断。这样能补齐的列最多,
// 贪吃的那列 (通常是模型名或耗时) 吃剩下的余量; 连基准宽都放不下才原样返回,
// 交给 fitCols 按老规矩丢列。
func autoCols(cols []tuiCol, rows [][]string, width int) []tuiCol {
	out := append([]tuiCol(nil), cols...)
	base := tableWidth(out)
	if base > width {
		return out
	}
	need := make([]int, len(cols))
	for i, col := range cols {
		need[i] = col.w // 定宽列的需求就是基准宽
		if !col.grow {
			continue
		}
		n := tuiWidth(col.title)
		for _, r := range rows {
			if i < len(r) && tuiWidth(r[i]) > n {
				n = tuiWidth(r[i])
			}
		}
		need[i] = max(n, col.w)
	}
	order := make([]int, len(cols))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		da, db := need[ia]-out[ia].w, need[ib]-out[ib].w
		if da != db {
			return da < db
		}
		return cutRank(cols[ia].hide) < cutRank(cols[ib].hide)
	})
	free := width - base
	for _, i := range order {
		if free <= 0 {
			break
		}
		give := min(free, need[i]-out[i].w)
		out[i].w += give
		free -= give
	}
	return out
}

// alignRow 只输出 vis 为真的列 (与 fitCols 一起决定窄终端的表宽)。
func alignRow(cols []tuiCol, cells []string, vis []bool) []string {
	out := make([]string, 0, len(cols))
	for i, col := range cols {
		if !vis[i] {
			continue
		}
		s := ""
		if i < len(cells) {
			s = cells[i]
		}
		s = tuiTrunc(s, col.w)
		if col.right {
			out = append(out, strings.Repeat(" ", max(0, col.w-tuiWidth(s)))+s)
		} else {
			out = append(out, tuiPad(s, col.w))
		}
	}
	return out
}
