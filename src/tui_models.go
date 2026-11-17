// tui_models.go: models 页 (候选与锁状态)。

package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sortedCands 是上游表的顺序: provider 优先级降序, 同优先级保持配置顺序
// (与 settings 页的 provider 面板同口径)。排序键按 store / 配置的 mtime 缓存, 不每帧
// 读文件; store 读不到时保持观测面原序。
func (m *tuiModel) sortedCands() []candView {
	cands := append([]candView(nil), m.pulse.Cands...)
	rank := m.provRanks()
	if len(rank) == 0 {
		return cands
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ri, iok := rank[cands[i].ID]
		rj, jok := rank[cands[j].ID]
		if iok != jok {
			return iok // 配置里有的排前面
		}
		if ri[0] != rj[0] {
			return ri[0] > rj[0]
		}
		return ri[1] < rj[1]
	})
	return cands
}

// provRanks 缓存排序键: 只有配置的文件时间变了才重读 (权重在配置文件里), 读不到就缓存空表,
// 免得每帧反复解析。
func (m *tuiModel) provRanks() map[string][2]int {
	cm, cerr := os.Stat(m.cfgPath)
	if cerr != nil {
		return nil
	}
	key := cm.ModTime()
	if m.provRank == nil || key != m.provRankMod {
		rank := provRank(m.cfgPath)
		if rank == nil {
			rank = map[string][2]int{}
		}
		m.provRank, m.provRankMod = rank, key
	}
	return m.provRank
}

// namedGroupCount 数一行里有名记账组的个数 (默认组不算, 它固定在「记账」列)。
func namedGroupCount(groups []candGroup) int {
	n := 0
	for _, g := range groups {
		if g.Name != defaultGroup {
			n++
		}
	}
	return n
}

// groupCell 画一个记账组: 未锁 = 绿色组名 (默认组写 -), 额度锁内 = 组名后接同色倒计时
// (先说明是哪个组, 再写还要等多久; 名字与倒计时都用额度锁的浅红)。默认组没有要写出的
// 名字, 锁上时只写倒计时。
func groupCell(g candGroup, isDefault bool) string {
	if g.LockMs > 0 {
		if isDefault {
			return colorDur(g.LockMs, tuiRedBright)
		}
		return tuiRedBright + g.Name + " " +
			durStr(time.Duration(g.LockMs)*time.Millisecond) + tuiReset
	}
	if isDefault {
		return "-"
	}
	return tuiGreen + g.Name + tuiReset
}

// drawModelsBody 把模型视角的两张表画在第 1 页最上面, 与下面的请求表一起滚动: 先上游
// 状态 (并发 / 排队 / 错误 / 记账组锁), 再列 上游/模型 的队列与路由聚合。两张表
// 不画标题 (列名行保留), 表与表之间空一行; 上游表只出现实际上游, 模型表在逻辑名
// 最近用过时把它名下配置的全部候选都铺开 (未用行全 0 / - 也占位); 第二张
// 表每个逻辑名先来一行标题 (逻辑名 + 粘性倒计时), 只列最近用过的逻辑名。返回光标所在的
// 显示行 (从内容区第一行算起, -1 = 没有光标)。
func (m *tuiModel) drawModelsBody(full *tuiCanvas) int {
	cands := m.sortedCands()
	rows := m.modelRows()
	cur, has := flowNavFrom(cands, rows).resolve(m.cur)
	if has {
		m.cur = cur
	}
	full.line("") // 页顶空一行, 不顶在页签下面
	line := m.drawUpstreamTable(full, cands, cur, has)
	full.line("") // 两张表之间空一行
	if l := m.drawModelTable(full, rows, cur, has); l >= 0 {
		line = l
	}
	return line
}

// drawUpstreamTable 画第 1 张表: 上游状态 (并发 / 排队 / 错误 / 记账组锁)。
// 记账列: 第一列固定是「记账」(默认组), 后面按各行自己声明的组铺开, 列标题留空;
// 列数取各行组数的最大值, 组少的行后面留空。光标格反显, 返回它的显示行 (本表在画布里
// 的绝对行号, -1 = 光标不在这张表)。
func (m *tuiModel) drawUpstreamTable(c *tuiCanvas, cands []candView, cur flowCursor, has bool) int {
	maxNamed := 0
	for _, cand := range cands {
		if n := namedGroupCount(cand.Groups); n > maxNamed {
			maxNamed = n
		}
	}
	rows := make([][]string, 0, len(cands))
	for _, cand := range cands {
		rows = append(rows, m.upstreamRow(cand, maxNamed))
	}
	cols, vis := tableLayout(upstreamCols(maxNamed), rows, c.cols)
	head := c.countLines()
	c.line(tableHead(cols, vis))
	if len(rows) == 0 {
		c.line(tuiDim + "  (空)" + tuiReset)
	}
	curLine := -1
	for i, r := range rows {
		if has && cur.table == 0 && cur.up == cands[i].ID {
			r = markCell(r, upstreamCell(cur, cands[i]))
			curLine = head + 1 + i
		}
		c.line(tuiIndent + strings.Join(alignRow(cols, r, vis), tuiColSep))
	}
	return curLine
}

// upstreamCell 是光标在上游行里的格下标: 0 = 上游名, 4 = 默认组, 5+ = 命名的记账组
// (中间三列并发 / 排队 / 错误不可选)。
func upstreamCell(c flowCursor, cand candView) int {
	if c.name {
		return 0
	}
	if c.group == defaultGroup {
		return 4
	}
	i := 1
	for _, g := range cand.Groups {
		if g.Name == defaultGroup {
			continue
		}
		if g.Name == c.group {
			return 4 + i
		}
		i++
	}
	return 0
}

// markCell 把第 i 格换成反显 (保留格内颜色: 每个 reset 后面补一次反显, 不然格内的
// 颜色收尾会把反显一起关掉); 其他格保持原样, i 越界时原样返回。
func markCell(row []string, i int) []string {
	if i < 0 || i >= len(row) {
		return row
	}
	out := append([]string(nil), row...)
	out[i] = tuiRev + strings.ReplaceAll(row[i], tuiReset, tuiReset+tuiRev) + tuiReset
	return out
}

// upstreamRow 拼上游表的一行: 并发 (试探另标 +1) / 排队 (带下一个发车时刻) /
// 错误 / 默认组与命名记账组的锁; 组少时补空, 让各行列数一致。
func (m *tuiModel) upstreamRow(cand candView, maxNamed int) []string {
	conc := strconv.Itoa(cand.InFlight)
	if cand.Probing {
		conc += tuiYell + "(+1)" + tuiReset // 试探不占槽位, 单独标出来
	}
	conc += "/" + strconv.Itoa(cand.SoftCap)
	queue := strconv.Itoa(cand.Queue)
	if cand.Queue > 0 && cand.NextMs > 0 {
		// 排队的下一个发车时刻直接跟在队数后面 (1/1.0s), 不加方括号: 省两格且列宽稳定。
		queue += "/" + durStr(time.Duration(cand.NextMs)*time.Millisecond)
	}
	var def candGroup
	var named []candGroup
	for _, g := range cand.Groups {
		if g.Name == defaultGroup {
			def = g
		} else {
			named = append(named, g)
		}
	}
	row := []string{m.upName(cand.ID), conc, queue, colorCount(cand.ErrStrk), groupCell(def, true)}
	for _, g := range named {
		row = append(row, groupCell(g, false))
	}
	for len(row) < 5+maxNamed {
		row = append(row, "")
	}
	return row
}

// upstreamCols 是上游表的列: 前 5 列固定, 命名记账组各占一列 (空标题)。
// 这张表按机构 (供应商) 聚合状态, 首列写供应商 id。
func upstreamCols(maxNamed int) []tuiCol {
	cols := []tuiCol{
		{"供应商", tuiWidth("供应商"), true, false, 4},
		// 并发列基准 8 格: 容得下 "3(+1)/10" 这种带超发试探标记的写法。
		{"并发", 8, true, true, 2},
		// 排队列最小 6 格: 容得下 "1/1.0s" 这种带等节拍的写法, 不被压成半截。
		{"排队", 6, true, true, 1},
		{"错误", tuiWidth("错误"), true, true, 2},
		{"记账", tuiWidth("记账"), true, false, 3},
	}
	for i := 0; i < maxNamed; i++ {
		cols = append(cols, tuiCol{"", 0, true, false, 3})
	}
	return cols
}

// modelLine 是模型/路由聚合表的一显示行: header 非空 = 逻辑名标题行 (整行已上色),
// 否则是数据行 (cells 与 modelCols 一一对应)。
type modelLine struct {
	header             string
	logical, up, model string
	cells              []string
}

// neverUsedTime 是没有用过的模型行的「最近」占位: mm:ss 形态, 一眼像时间而不是横线,
// 也压不塌列 (colKeep 不会把整列当空裁掉)。
const neverUsedTime = "--:--"

// modelRouteLines 把模型队列与聚合统计拼成第 2 张表的显示行: 每个逻辑名先来一行标题
// (只列最近用过的逻辑名; 用过逻辑名名下没进过队列的候选也占行, 见 modelRows), 再列
// 该组的 上游/模型 行 (不加逻辑名列)。
func (m *tuiModel) modelRouteLines(rows []*modelRow) []modelLine {
	lines := make([]modelLine, 0, len(rows)+4)
	prev := ""
	now := time.Now()
	// 「最近」格的高亮与置 0: groupBest 是每个逻辑名组内最近一次服务的时刻 (Last 最大),
	// 那行的「最近」格上绿; inflight 是当前有进行中请求在走的 (逻辑名/上游/模型) 键,
	// 命中行值固定写 0s (最近一次还没结束), 同样上绿 (拼装见下方 age)。
	groupBest := map[string]time.Time{}
	for _, r := range rows {
		if !r.Last.IsZero() && r.Last.After(groupBest[r.logical]) {
			groupBest[r.logical] = r.Last
		}
	}
	inflight := m.inflightModels()
	for i, r := range rows {
		// 队列的锁都画在队列表里: 退避 = 黄 (等待), 锁按原因分色 —— 熔断锁 = 红 (错误),
		// 低性能锁 = 黄 (慢到降级, 与退避同为等待/降级一类, 但分属不同列)。
		paused := colorDur(r.PausedMs, tuiYell)
		lockColor := tuiRed
		if r.LockWhy == lowPerfReason {
			lockColor = tuiYell
		}
		lock := colorDur(r.LockMs, lockColor)
		// 粘性偏好标记: 该逻辑名当前粘性选中的候选在 上游/模型 前标 *。所有行统一
		// 占一个标记位 (非偏好行补空格), 模型名从同一列开始, 不让 * 把对齐顶歪。
		marker := " "
		if p, ok := m.pulse.Sticky[r.logical]; ok && p.Upstream == r.up && p.Model == r.model {
			marker = "*"
		}
		if i == 0 || r.logical != prev {
			lines = append(lines, modelLine{header: m.modelGroupHeader(r.logical)})
			prev = r.logical
		}
		// 没有用过的行 (窗口内没有完成请求, Last 为零): 统计格留空 (表格按列宽补空格
		// 对齐), 「最近」占位写 --:--, 不画 0 / --- 这些假数据; 队列 / 退避 / 锁照旧。
		age := mages(r.Last)
		token := humanTokens(r.Cached) + "/" + humanTokens(r.Total)
		ttfb := ttfbCell(r.TtfbPts, now, m.rateWin, m.window)
		perf := perfCell(r.PerfPts, now, m.rateWin, m.window, r.up, r.model, m.perfIdx)
		hot := false
		if r.Last.IsZero() {
			age, token, ttfb, perf = neverUsedTime, "", "", ""
		} else if r.Last.Equal(groupBest[r.logical]) {
			hot = true // 组内最近一次服务的上游: 「最近」格上绿
		}
		if inflight[r.logical+"\x00"+r.up+"\x00"+r.model] {
			age, hot = durStr(0), true // 进行中: 最近一次还没结束, 固定为 0, 同样上绿
		}
		if hot {
			age = tuiGreen + age + tuiReset
		}
		// 与 modelCols 的顺序一一对应 (colKeep 按下标裁列)。
		cells := []string{
			marker + m.upModel(r.up, r.model),
			strconv.Itoa(r.Queue), paused, lock,
			// 缓存命中与总 token 合成一格 (cached/total)。
			token, ttfb, perf, age,
		}
		lines = append(lines, modelLine{logical: r.logical, up: r.up, model: r.model, cells: cells})
	}
	return lines
}

// inflightModels 返回当前有进行中请求正在走的 (逻辑名/上游/模型) 键: 取每笔进行中请求
// 的当前候选 (flow.Up/Model, 排队/退避阶段也算; 已换走的旧候选不算)。模型表用它把命中
// 行的「最近」固定为 0 (见 modelRouteLines)。
func (m *tuiModel) inflightModels() map[string]bool {
	out := make(map[string]bool, len(m.pulse.Active))
	for _, f := range m.pulse.Active {
		if f.Up == "" || f.Model == "" {
			continue // 还没落到候选 (parse 阶段): 没有对应的 上游/模型
		}
		out[f.Logical+"\x00"+f.Up+"\x00"+f.Model] = true
	}
	return out
}

// modelGroupHeader 是模型表的分组标题: 逻辑名 (专属色加粗) + 有粘性偏好时的剩余倒计时
// (mm:ss, 不带文字); 配置里找不到归属的行写 (未归属)。
func (m *tuiModel) modelGroupHeader(logical string) string {
	if logical == "" {
		return tuiDim + "(未归属)" + tuiReset
	}
	hdr := m.logicalName(logical)
	if p, ok := m.pulse.Sticky[logical]; ok {
		if rem := time.Duration(p.RemainingMs) * time.Millisecond; rem > 0 {
			hdr += "  " + tuiDim + durStr(rem) + tuiReset
		}
	}
	return hdr
}

// modelHang 是模型表表体比组名多缩进的宽度: 组名用 tuiIndent (向外悬挂), 列名行与数据行
// 用 modelIndent, 分组一眼能看出哪几行属于谁。多占的宽度先从表宽里扣, 免得顶出屏幕。
const (
	modelHang   = 2
	modelIndent = tuiIndent + "  "
)

// modelBar 是模型表统计四列 (Token / 首字节 / 性能 / 最近) 之间的边框竖线。
// tuiWidth 按 1 列算 (U+2502 不在全角区), 主流终端同宽, 不会顶歪列对齐。
const modelBar = "│"

// modelStatsSep 判断分隔点 (前一个可见列在 modelCols 里的下标) 是否该用边框竖线:
// Token (4) / 首字节 (5) / 性能 (6) 之后, 即统计四列两两之间。
func modelStatsSep(prevCol int) bool {
	return prevCol == 4 || prevCol == 5 || prevCol == 6
}

// modelKeptColIdx 是 colKeep 保住的原列下标 (每个 kept 列在 modelCols 里的位置),
// 与 pickCols 的返回一一对应。
func modelKeptColIdx(keep []bool) []int {
	out := make([]int, 0, len(keep))
	for i, k := range keep {
		if k {
			out = append(out, i)
		}
	}
	return out
}

// modelVisibleColIdx 把 kept 列的原下标按可见性滤一遍, 得到与 alignRow 输出一一
// 对应的原列下标序列 (竖线定位用)。
func modelVisibleColIdx(cols []tuiCol, vis []bool, full []int) []int {
	var out []int
	for i := range cols {
		if vis[i] {
			out = append(out, full[i])
		}
	}
	return out
}

// modelJoin 按列布局把已对齐格拼成一行正文 (不含行首缩进): Token / 首字节 / 性能 /
// 最近 四列之间用边框竖线, 其余列之间照旧空格; 表头与数据行共用, 竖线位次一致。
func modelJoin(cols []tuiCol, cells []string, vis []bool, vfull []int) string {
	aligned := alignRow(cols, cells, vis)
	var b strings.Builder
	for i, s := range aligned {
		if i > 0 {
			if modelStatsSep(vfull[i-1]) {
				b.WriteString(modelBar)
			} else {
				b.WriteString(tuiColSep)
			}
		}
		b.WriteString(s)
	}
	return b.String()
}

// modelBarPos 返回统计四列之间的边框竖线在数据行内容里的字符位 (不含行首缩进),
// 与 modelJoin 的拼法逐列一致; 数据行与组名行共用同一列布局, 位次相同。
func modelBarPos(cols []tuiCol, vis []bool, vfull []int) []int {
	var pos []int
	x := 0
	vi := -1
	for i, col := range cols {
		if !vis[i] {
			continue
		}
		vi++
		if vi > 0 {
			if modelStatsSep(vfull[vi-1]) {
				pos = append(pos, x)
				x++ // 竖线占 1 列
			} else {
				x += tuiColGap
			}
		}
		x += col.w
	}
	return pos
}

// modelHeadLine 拼模型表列名行 (含缩进): 统计四列之间同样画边框竖线, 与数据行
// 竖线位次一致。
func modelHeadLine(indent string, cols []tuiCol, vis []bool, vfull []int) string {
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = tuiTrunc(col.title, col.w)
	}
	return indent + tuiBold + modelJoin(cols, head, vis, vfull) + tuiReset
}

// modelGroupLine 拼组名显示行 (含左缩进): 组名文本在最左 (tuiIndent 起, 悬挂在表体
// 外), 统计四列的边框竖线补到与数据行同列 (数据行从 modelIndent 起, 差 modelHang
// 格), 让竖线贯穿组名行不断。
func modelGroupLine(header string, cols []tuiCol, vis []bool, vfull []int) string {
	pos := modelBarPos(cols, vis, vfull)
	var b strings.Builder
	b.WriteString(tuiIndent)
	w := len(tuiIndent)
	b.WriteString(header)
	w += tuiWidth(header)
	for _, p := range pos {
		target := len(modelIndent) + p // 数据行的竖线屏幕列位 (组名行的内容只是更靠左)
		if w < target {
			b.WriteString(strings.Repeat(" ", target-w))
			w = target
		}
		b.WriteString(modelBar)
		w++
	}
	return b.String()
}

// drawModelTable 画第 2 张表 (模型/路由聚合): 逻辑名标题行 + 该组的 上游/模型 行;
// 光标行反显 上游/模型 那一格, 返回它的显示行 (绝对行号, -1 = 光标不在这张表)。
func (m *tuiModel) drawModelTable(c *tuiCanvas, rows []*modelRow, cur flowCursor, has bool) int {
	lines := m.modelRouteLines(rows)
	data := make([][]string, 0, len(lines))
	for _, ln := range lines {
		if ln.header == "" {
			data = append(data, ln.cells)
		}
	}
	keep := colKeep(modelCols(), data)
	cols := pickCols(modelCols(), keep)
	cols, vis := tableLayout(cols, pickRows(keep, data), c.cols-modelHang)
	full := modelKeptColIdx(keep)
	vfull := modelVisibleColIdx(cols, vis, full)
	head := c.countLines()
	c.line(modelHeadLine(modelIndent, cols, vis, vfull))
	if len(lines) == 0 {
		c.line(tuiDim + modelIndent + "(空)" + tuiReset)
	}
	curLine := -1
	for i, ln := range lines {
		if ln.header != "" {
			c.line(modelGroupLine(ln.header, cols, vis, vfull))
			continue
		}
		cells := pickRow(keep, ln.cells)
		if has && cur.table == 1 && cur.logical == ln.logical && cur.up == ln.up && cur.model == ln.model {
			cells = markCell(cells, 0)
			curLine = head + 1 + i
		}
		c.line(modelIndent + modelJoin(cols, cells, vis, vfull))
	}
	return curLine
}

// cellDash 是四段统计里「这一段没有样本」的占位: 与性能数字段一样占 4 格 (两侧空格),
// 四段用空格分隔, 横线不会被数字顶歪。
const cellDash = " -- "

// ttfbDash 是首字节四段里「这一段没有样本」的占位: 与首字节的每个数字同宽 3 格,
// 四段用空格分隔, 横线不会错位。
const ttfbDash = "---"

// ttfbNum 是首字节列里每个数字的写法: 单位恒为秒, 3 位有效数字, 整数部分用正常前景色,
// 小数部分 (灰色) 最多 3 位到毫秒精度。整数部分位数不同, 小数的位数随之变化, 凑满 3 格:
//   - 不足 1 秒 (整数位 0): 3 位灰色小数, 全灰 (补 0 到毫秒), 如 123ms 写成 灰"123";
//   - 1 到 9.99 秒: 1 位整数 + 2 位灰色小数, 如 1.05s 写成 1 + 灰"05";
//   - 10 到 99.9 秒: 2 位整数 + 1 位灰色小数, 如 12.3s 写成 12 + 灰"3";
//   - 100 秒以上: 整数秒, 无小数, 超过 3 格时自然撑宽 (不截断)。
func ttfbNum(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	switch sec := ms / 1000; {
	case sec >= 100:
		return strconv.FormatInt(sec, 10)
	case sec >= 10:
		return strconv.FormatInt(sec, 10) + tuiDim + strconv.FormatInt((ms%1000)/100, 10) + tuiReset
	case sec >= 1:
		return strconv.FormatInt(sec, 10) + tuiDim + fmt.Sprintf("%02d", (ms%1000)/10) + tuiReset
	default:
		return tuiDim + fmt.Sprintf("%03d", ms) + tuiReset
	}
}

// ttfbCell 拼首字节格 (四段用空格分隔): 最近一次 / 短时平均 (perfWin 性能窗) / 长时平均
// (统计窗 3h) / 谷值 (整窗最低); 整窗没样本与各段窗口内没样本时都写 ttfbDash。
func ttfbCell(pts []ttfbPoint, now time.Time, shortWin, longWin time.Duration) string {
	if len(pts) == 0 {
		return ttfbDash
	}
	shortSum, shortN := int64(0), 0
	longSum, longN := int64(0), 0
	for _, p := range pts {
		if now.Sub(p.ts) <= shortWin {
			shortSum += p.ms
			shortN++
		}
		if now.Sub(p.ts) <= longWin {
			longSum += p.ms
			longN++
		}
	}
	shortAvg, longAvg := int64(0), int64(0)
	shortOK, longOK := shortN > 0, longN > 0
	if shortOK {
		shortAvg = shortSum / int64(shortN)
	}
	if longOK {
		longAvg = longSum / int64(longN)
	}
	last, lok := ttfbLast(pts)
	part := func(ms int64, ok bool) string {
		if !ok {
			return ttfbDash
		}
		return ttfbNum(ms)
	}
	return strings.Join([]string{
		part(last, lok), part(shortAvg, shortOK),
		part(longAvg, longOK), part(ttfbMin(pts), true),
	}, " ")
}

// perfCell 拼性能格 (四段用空格分隔): 最近一次 / 短时平均 (perfWin 性能窗) / 长时平均
// (统计窗 3h) / 峰值 (整窗最高); 整窗没样本与各段窗口内没样本时都写 cellDash。perfIdx
// 为空 (远端模式无参数表) 时退化为原始速率 c/d。
func perfCell(pts []perfPoint, now time.Time, shortWin, longWin time.Duration, up, model string, perfIdx func(string, string, int64, int64, int64) float64) string {
	if len(pts) == 0 {
		return cellDash
	}
	val := func(p perfPoint) float64 {
		if perfIdx == nil {
			return float64(p.c) / float64(p.d) * 1000
		}
		return perfIdx(up, model, p.c, p.d, p.b)
	}
	var shortSum, longSum float64
	var shortN, longN int
	var lastT time.Time
	var lastV, peak float64
	for _, p := range pts {
		v := val(p)
		if v > peak {
			peak = v
		}
		if p.ts.After(lastT) {
			lastT, lastV = p.ts, v
		}
		if now.Sub(p.ts) <= shortWin {
			shortSum += v
			shortN++
		}
		if now.Sub(p.ts) <= longWin {
			longSum += v
			longN++
		}
	}
	part := func(v float64, ok bool) string {
		if !ok {
			return cellDash
		}
		return humanTPS(v)
	}
	shortAvg, longAvg := 0.0, 0.0
	if shortN > 0 {
		shortAvg = shortSum / float64(shortN)
	}
	if longN > 0 {
		longAvg = longSum / float64(longN)
	}
	return strings.Join([]string{
		part(lastV, true), part(shortAvg, shortN > 0),
		part(longAvg, longN > 0), part(peak, true),
	}, " ")
}

// modelCols 是第 1 页模型/路由聚合表的列: 按类别从左到右排, 同类相邻 ——
// 调度状态 (排队 / 退避 / 熔断锁), 流量 (Token = cached/total / 首字节 / 性能),
// 时间 (最近一次)。性能列是窗口内每笔请求 r* = m*(c/d)*(c/b)^(-g) 的算术平均 (单值)。
// 全部按内容成长 (grow=true), 基准宽只贴标题/内容的典型宽度, 不留大补白 ——
// 这样与第 1 页和其它表一样, 列与列之间视觉上只隔 1 格。
func modelCols() []tuiCol {
	return []tuiCol{
		// 首列是「上游」= 供应商 + 模型 (一行一个上游, 列内写 供应商/真模型 id)。
		{"上游", 20, true, false, 4},
		{"排队", 2, true, true, 0}, {"退避", 4, true, true, 4}, {"熔断", 5, true, true, 4},
		// Token 一格写 cached/total (两段各 4 字符); 首字节写 最近/短均/长均/谷 四段
		// (短 = perfWin 性能窗, 长 = 统计窗 3h), 每段固定 3 字符 (整数 + 灰色小数到毫秒),
		// 四段用空格分隔, 3 个空格 = 15。
		{"Token", 9, true, true, 3}, {"首字节", 15, true, true, 3},
		// 性能列写 最近/短均/长均/峰 四段 (每笔请求 r*), 每段固定 4 字符, 四段用空格分隔
		// (3 个空格) = 19。
		{"性能", 19, true, true, 4},
		{"最近", 6, true, true, 2},
	}
}

// modelRow 是第 1 页模型/路由聚合表的一行: 队列状态 (观测面) + 聚合统计 (日志,
// 按 逻辑名/上游/模型 拆分, 同一上游/模型被多个逻辑名引用时各归各的组)。
type modelRow struct {
	logical, up, model string
	Queue, Fails       int
	PausedMs, LockMs   int64
	LockWhy            string // 模型锁原因 (breaker / lowperf): 倒计时按它分色
	Reqs               int
	Errs               int
	Fallbacks          int
	Total, Cached      int
	TtfbPts            []ttfbPoint // 窗口内各请求的首字节时延样本 (平均/谷值用)
	PerfPts            []perfPoint // 窗口内各请求的性能样本 (r* 算术平均用)
	Last               time.Time
	// forced 表示这行是「最近用过的逻辑名的配置候选」强制带进来的: 全 0 / - 也显示,
	// 让从没进过队列的 上游/模型 也能被光标选中 (置粘性偏好) 与标记。
	forced bool
}

// blank 判断这一行是否全无数据: 队列 / 退避 / 熔断 / 请求 / 错误 / 换候选 /
// Token / 首字节 / 吞吐都是 0 或 -, 且没有最近请求时间。连错计数 (fails) 不算显示理由:
// 它只在成功服务时清零, 可能只是残留几天前的一次失败, 表里也没有它的列。
func (r *modelRow) blank() bool {
	return r.Queue == 0 && r.PausedMs == 0 && r.LockMs == 0 &&
		r.Reqs == 0 && r.Errs == 0 && r.Fallbacks == 0 &&
		r.Total == 0 && r.Cached == 0 && len(r.TtfbPts) == 0 &&
		len(r.PerfPts) == 0 &&
		r.Last.IsZero()
}

// sweepRoutes 删掉最后一行日志已超出回看窗口 (m.window) 的路由聚合, 让超过 3h 的行
// 在下一拍自动消失。聚合状态只增不减: 一行再没有新日志时, 增量路径不会裁它的旧样本,
// 只有这里的定期清扫 (每个刷新周期跑一次, 见 refresh) 会按行移除。
// Last 是该行最新一笔已消费日志的时间 (消费时已按窗口过滤过), 它超窗即整行数据都在窗口外。
func (m *tuiModel) sweepRoutes() {
	now := time.Now()
	for k, r := range m.routes {
		if !r.Last.IsZero() && now.Sub(r.Last) > m.window {
			delete(m.routes, k)
		}
	}
}

// modelRows 合并观测面里的模型队列与日志聚合, 按 逻辑名 分组 (同一逻辑名的候选相邻):
// 保留真正有过请求的 上游/模型; 逻辑名最近用过 (有非空行) 时, 名下配置的全部候选
// 也带进来 (从没请求过的强制显示, 供光标选中与粘性标记); 观测面的队列 / 锁状态没有
// 逻辑名维度, 按 上游/模型 补到引用它的每组 — 引用关系先查配置 aliases: 只要某逻辑名
// 的候选链包含该 (上游/模型), 就归到那个组 (即使窗口内没日志); 配置里也找不到的才落
// 空逻辑名组 (排最末); 全 0 / - 且没被用过逻辑名点名的行不进表。组内行按配置候选链
// 的优先级排序 (与 settings 页面板同口径), 组间仍按最近请求时间。
func (m *tuiModel) modelRows() []*modelRow {
	by := modelRowIndex{}
	m.mergeRouteRows(by)
	ix := m.loadAliasIndex()
	m.mergeCandRows(by, ix.owners)
	used := usedLogicalNames(by)
	m.forceUsedCandidates(by, ix.owners, used)
	out := make([]*modelRow, 0, len(by))
	for _, r := range by {
		if r.blank() && !r.forced {
			continue // 全 0 / - 且没被用过逻辑名点名: 只是配置里有这个模型, 不占版面
		}
		out = append(out, r)
	}
	sortModelRows(out, ix.rank)
	return out
}

// usedLogicalNames 返回索引里至少有一个非空行的逻辑名: 窗口内有它的请求或锁 / 队列
// 状态才算「最近用过」; 空逻辑名 (只观测面) 不算。
func usedLogicalNames(by modelRowIndex) map[string]bool {
	used := map[string]bool{}
	for _, r := range by {
		if r.logical != "" && !r.blank() {
			used[r.logical] = true
		}
	}
	return used
}

// forceUsedCandidates 把用过逻辑名名下配置的全部候选补进索引并标 forced: 已存在的行
// 只顺手标记, 没有的建空行 (blank 也显示)。展开只认配置候选链 (owners 的 key 是
// "上游\x00模型", 值是该 上游/模型 的引用逻辑名们), 不看观测面有没有报过它; 配置读
// 不到时 owners 为空, 这一步自然什么都不做。
func (m *tuiModel) forceUsedCandidates(by modelRowIndex, owners map[string][]string, used map[string]bool) {
	for key, logicals := range owners {
		up, model, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		for _, logical := range logicals {
			if used[logical] {
				by.get(logical, up, model).forced = true
			}
		}
	}
}

// modelRowIndex 是 逻辑名/上游/模型 -> 聚合行 的索引, get 按需建行。
type modelRowIndex map[string]*modelRow

func (ix modelRowIndex) get(logical, up, model string) *modelRow {
	k := logical + "\x00" + up + "\x00" + model
	r := ix[k]
	if r == nil {
		r = &modelRow{logical: logical, up: up, model: model}
		ix[k] = r
	}
	return r
}

// mergeRouteRows 把日志聚合累加进索引: 同一 逻辑名/上游/模型 的分片合并, 样本按切片
// 拼接, 最近时间取极值。没路由到任何上游的记录 (如 unknown model) 不属于模型视角。
func (m *tuiModel) mergeRouteRows(by modelRowIndex) {
	for _, rr := range m.routes {
		if rr.Upstream == "" && rr.Model == "" {
			continue
		}
		r := by.get(rr.Logical, rr.Upstream, rr.Model)
		r.Reqs += rr.Reqs
		r.Errs += rr.Errs
		r.Fallbacks += rr.Fallbacks
		r.Total += rr.Total
		r.Cached += rr.Cached
		r.TtfbPts = append(r.TtfbPts, rr.TtfbPts...)
		r.PerfPts = append(r.PerfPts, rr.PerfPts...)
		if rr.Last.After(r.Last) {
			r.Last = rr.Last
		}
	}
}

// aliasIndex 是一次配置读取得到的候选索引: owners 是 (上游, 真实模型) -> 引用它的
// 逻辑名们 (mergeCandRows / forceUsedCandidates 用), rank 是 逻辑名 -> (上游, 模型)
// 在候选链里的下标 (0 = 最高优先级, 与 settings 页面板 / 运行时候选链同口径;
// sortModelRows 用)。
type aliasIndex struct {
	owners map[string][]string
	rank   map[string]map[[2]string]int
}

// loadAliasIndex 读配置展开候选链 (组展开后组名也算逻辑名), 建 owners 与 rank;
// 配置读不到 (如 -tui 时 cfgPath 缺失) 两表均为空, 调用方按无配置处理。
func (m *tuiModel) loadAliasIndex() aliasIndex {
	var ix aliasIndex
	c, err := loadConfigOpt(m.cfgPath, true)
	if err != nil {
		return ix
	}
	ix.owners = map[string][]string{}
	ix.rank = map[string]map[[2]string]int{}
	for logical, list := range c.expandedAliases() {
		idx := make(map[[2]string]int, len(list))
		for i, pair := range list {
			idx[pair] = i
			key := pair[0] + "\x00" + pair[1]
			ix.owners[key] = append(ix.owners[key], logical)
		}
		ix.rank[logical] = idx
	}
	return ix
}

// mergeCandRows 把观测面的队列 / 锁状态补进索引, 已有同 上游/模型 的行 (任意逻辑名)
// 一起更新; 窗口内没有日志行时按配置归属补到每个引用者; 配置里也没有的 (上游/模型)
// 落空逻辑名组, 只带观测面状态、垫在最末。
func (m *tuiModel) mergeCandRows(by modelRowIndex, owners map[string][]string) {
	for _, cand := range m.pulse.Cands {
		for _, md := range cand.Models {
			found := false
			for _, r := range by {
				if r.up == cand.ID && r.model == md.Model {
					applyCandState(r, md)
					found = true
				}
			}
			if found {
				continue
			}
			logs := owners[cand.ID+"\x00"+md.Model]
			if len(logs) == 0 {
				logs = []string{""}
			}
			for _, logical := range logs {
				applyCandState(by.get(logical, cand.ID, md.Model), md)
			}
		}
	}
}

// applyCandState 把一条模型锁状态写进聚合行。
func applyCandState(r *modelRow, md candModelLock) {
	r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
	r.LockWhy = md.Why
}

// sortModelRows 组间: 有逻辑名的按组内最近请求时间降序 (常用的在前), 空逻辑名 (只观测面)
// 垫底; 组内: 按配置候选链的优先级排 (rank 里的下标, 0 = 首选, 与 settings 页面板
// 同口径) — 没进过候选链的行 (配置读不到 / 候选集热加载变过) 排在被点名行之后,
// 再按最近时间 / 请求数 / 上游 / 模型兜底。
func sortModelRows(out []*modelRow, rank map[string]map[[2]string]int) {
	groupLast := map[string]time.Time{}
	for _, r := range out {
		if r.logical != "" && r.Last.After(groupLast[r.logical]) {
			groupLast[r.logical] = r.Last
		}
	}
	sort.Slice(out, func(i, j int) bool {
		switch {
		case out[i].logical != out[j].logical:
			if out[i].logical == "" {
				return false
			}
			if out[j].logical == "" {
				return true
			}
			if !groupLast[out[i].logical].Equal(groupLast[out[j].logical]) {
				return groupLast[out[i].logical].After(groupLast[out[j].logical])
			}
			return out[i].logical < out[j].logical
		}
		ri, iok := rank[out[i].logical][[2]string{out[i].up, out[i].model}]
		rj, jok := rank[out[j].logical][[2]string{out[j].up, out[j].model}]
		if iok != jok {
			return iok // 配置候选链里的排前面
		}
		if iok && ri != rj {
			return ri < rj
		}
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		if out[i].Reqs != out[j].Reqs {
			return out[i].Reqs > out[j].Reqs
		}
		if out[i].up != out[j].up {
			return out[i].up < out[j].up
		}
		return out[i].model < out[j].model
	})
}

// scrollWindow 把该页的滚动位置夹进实际行数, 返回窗口第一行 —— 空表与 G 键 (设成很大)
// 都靠它兜底, 免得画出一片空白。
func (m *tuiModel) scrollWindow(rows, limit int) int {
	top := clampScroll(m.scroll[m.tab], rows, limit)
	m.scroll[m.tab] = top
	return top
}
