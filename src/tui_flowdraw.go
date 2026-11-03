// tui_flowdraw.go: active 页表格绘制与窗口滚动。

package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// flowCells 是一笔活动连接的表格行, 列序与 activeCols 一致 (编号 / 逻辑名 / 流式 /
// 上游 / 耗时 / 状态)。
func (m *tuiModel) flowCells(f flow) []string {
	tag := "否"
	if f.Stream {
		tag = "是"
	}
	status := stageName(f)
	if f.Stage == stageRecv && f.RecvStart > 0 && (f.RecvTok > 0 || f.RecvBytes > 0) {
		// 接收中: 状态列按模型/路由表的同三个速度写这笔的实时吞吐, 不加前缀 ——
		// 解码 = 已收到的输出 token / 首字节到达至今 (解码段),
		// 总   = 已收到的输出 token / 请求开始至今 (整笔, 此时还拿不到 prompt token),
		// 下载 = 已转发的响应字节 / 首字节到达至今。
		if el := time.Since(time.UnixMilli(f.RecvStart)).Seconds(); el > 0 {
			var parts []string
			if f.RecvTok > 0 {
				parts = append(parts, humanTPS(float64(f.RecvTok)/el)+" t/s")
				if tot := time.Since(f.StartTime).Seconds(); tot > 0 {
					parts = append(parts, humanTPS(float64(f.RecvTok)/tot)+" t/s")
				}
			}
			if f.RecvBytes > 0 {
				parts = append(parts, humanRate(float64(f.RecvBytes)/el))
			}
			status = strings.Join(parts, " ")
		}
	}
	return []string{
		flowIDCell(f.ID),
		f.Logical,
		tag,
		m.upModel(f.Up, f.Model),
		m.durCell(f),
		status,
	}
}

// flowIDCell 是编号格: 整十的编号前加 "*", 让每十条有个肉眼可数的锚点。
func flowIDCell(id uint64) string {
	s := strconv.FormatUint(id, 10)
	if id > 0 && id%10 == 0 {
		return "*" + s
	}
	return s
}

// drawFlow 是第 1 页: 活动连接按流程阶段分块, 每块内按编号倒序 (大的在上)。列名固定在
// 页签下方, 不随内容滚走;
// 分块内容单独滚动 (up/down 只动分块), 块与块之间不再空行 (标题自带分隔); 每块的数据
// 行区高度按窗口统计定参考、再用此刻笔数校准 (flowMinRows): 笔数多按实际留, 空块最少
// 1 行, 有空行时也不超过 6 行, 于是各块位置相对稳定。
// 列按全部请求一起算: 表头只有一份, 各块必须同列同宽。
// 分块下面是上游状态与模型/路由聚合两张表 (drawModelsBody), 与分块一起滚动。
func (m *tuiModel) drawFlow(c *tuiCanvas, body int) {
	grouped := make([][][]string, len(flowBlocks))
	var all [][]string
	for i, b := range flowBlocks {
		// 块内按编号从大到小: 编号大的是后进来的请求, 排在块的上方 (与日志尾部同向)。
		block := make([]flow, 0, len(m.pulse.Active))
		for _, f := range m.pulse.Active {
			if stageIn(f.Stage, b.stages) {
				block = append(block, f)
			}
		}
		sort.Slice(block, func(x, y int) bool { return block[x].ID > block[y].ID })
		for _, f := range block {
			grouped[i] = append(grouped[i], m.flowCells(f))
		}
		all = append(all, grouped[i]...)
	}
	// 整列都空的列 (这批请求都还没选中上游) 一起丢掉, 各块才能对齐。
	keep := colKeep(activeCols, all)
	// 状态列恒保留: 阶段已由块标题说明, 状态列多为细节 (退避 / 熔断锁 / 额度锁), 整列
	// 同时为空时不能像 上游/模型 那样丢列, 否则列会闪跳、各块对不齐。
	for i, c := range activeCols {
		if c.title == "状态" {
			keep[i] = true
		}
	}
	if len(all) == 0 {
		// 没有进行中的请求时不能按"整列为空"收列: 那样列名也会一起消失。
		// 列名是一直要显示的, 空表照常画全部列 (窄屏再由 fitCols 按档位丢)。
		for i := range keep {
			keep[i] = true
		}
	}
	// 行也按同一份 keep 对齐 (pickRows): 列丢过之后下标已重排, 再拿原始行
	// 给 autoCols 算内容宽会读错 cell, 后面的列停在基准宽, 内容被提前截断。
	picked := pickRows(keep, all)
	cols, vis := tableLayout(pickCols(activeCols, keep), picked, c.cols)

	// 列名画在固定区 (页签下方空一行), 分块画在可滚动区。
	c.line("") // 表头上一行空着, 不顶在页签下面
	c.line(tableHead(cols, vis))
	full := &tuiCanvas{cols: c.cols}
	minRows := m.flowMinRows() // 各块数据行区高度: 窗口统计定参考, 此刻笔数校准
	for i, b := range flowBlocks {
		rows := pickRows(keep, grouped[i])
		full.line(tuiDim + tuiIndent + b.title + " (" + strconv.Itoa(len(rows)) + ")" + tuiReset)
		for _, r := range rows {
			full.line(tuiIndent + strings.Join(alignRow(cols, r, vis), tuiColSep))
		}
		for n := len(rows); n < minRows[i]; n++ {
			full.line("") // 不满就补空行, 撑住这一块的高度
		}
	}
	m.drawModelsBody(full) // 上游状态 + 模型/路由聚合, 接在分块下方
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

// drawWindow 把画布按滚动位置裁一屏贴到 c (第 1 页: 分块 + 两张模型表一起滚)。
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
