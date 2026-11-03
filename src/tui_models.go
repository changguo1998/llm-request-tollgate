// tui_models.go: models 页 (候选与锁状态)。

package main

import (
	"os"
	"sort"
	"strconv"
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

// provRanks 缓存排序键: 只有 store 或配置的文件时间变了才重读, 读不到就缓存空表,
// 免得每帧反复解析。
func (m *tuiModel) provRanks() map[string][2]int {
	sm, serr := os.Stat(storePathFor(m.cfgPath))
	cm, cerr := os.Stat(m.cfgPath)
	if serr != nil || cerr != nil {
		return nil
	}
	key := [2]time.Time{sm.ModTime(), cm.ModTime()}
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

// groupCell 画一个记账组: 未锁 = 绿色组名 (默认组写 -), 锁内 = 红色倒计时。
func groupCell(g candGroup, isDefault bool) string {
	if g.LockMs > 0 {
		return colorDur(g.LockMs, tuiRed)
	}
	if isDefault {
		return "-"
	}
	return tuiGreen + g.Name + tuiReset
}

// drawModelsBody 把模型视角的两张表接在第 1 页活动表下方, 与分块一起滚动:
// 先上游状态 (并发 / 排队 / 错误 / 记账组锁), 再列有过请求的 上游/模型 队列与路由聚合。
// 两张表不画标题 (列名行保留), 表与表之间空一行; 只出现实际上游/模型, 不按逻辑名或
// 配置候选铺开。
func (m *tuiModel) drawModelsBody(full *tuiCanvas) {
	cands := m.sortedCands()
	// 记账列: 第一列固定是「记账」(默认组), 后面按各行自己声明的组铺开, 列标题留空;
	// 列数取各行组数的最大值, 组少的行后面留空。行与行之间互不占位。
	maxNamed := 0
	for _, cand := range cands {
		if n := namedGroupCount(cand.Groups); n > maxNamed {
			maxNamed = n
		}
	}
	var urows [][]string
	for _, cand := range cands {
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
		urows = append(urows, row)
	}
	cols := []tuiCol{
		{"上游", tuiWidth("上游"), true, false, 4},
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
	full.line("") // 与上面的分块空一行
	drawTable(full, "", cols, urows, 0, len(urows))
	full.line("") // 两张表之间空一行

	// 合并表: 队列状态 (观测面) + 聚合统计 (日志), 行 = 有过请求的 上游/真实模型。
	var mrows [][]string
	for _, r := range m.modelRows() {
		// 队列的锁都画在队列表里: 退避 = 黄 (等待), 熔断锁 = 红 (锁)。
		paused := colorDur(r.PausedMs, tuiYell)
		lock := colorDur(r.LockMs, tuiRed)
		errs := colorCount(r.Errs)
		ttfb := "-"
		if r.TtfbN > 0 {
			ttfb = durStr(time.Duration(r.TtfbSum/int64(r.TtfbN)) * time.Millisecond)
		}
		// 三种速度都写成 平均/峰值 (同一份 tokRate 口径: 平均=Σ体积/Σ时间, 峰值=单笔最高);
		// 缺字段的格子显示 "-"(2B), 整列全空由 dropEmptyCols 收掉。
		tps := func(pts []tokPoint) string {
			if avg, peak, ok := tokRate(pts, time.Now(), m.rateWin); ok {
				return humanTPS(avg) + "/" + humanTPS(peak)
			}
			return "-"
		}
		bsp := func(pts []tokPoint) string {
			if avg, peak, ok := tokRate(pts, time.Now(), m.rateWin); ok {
				return humanByte(avg) + "/" + humanByte(peak)
			}
			return "-"
		}
		// 与 modelCols 的顺序一一对应 (dropEmptyCols 按下标裁列)。
		mrows = append(mrows, []string{
			m.upModel(r.up, r.model),
			strconv.Itoa(r.Queue), paused, lock, colorCount(r.Fails),
			strconv.Itoa(r.Reqs), errs, strconv.Itoa(r.Fallbacks),
			humanTokens(r.Total), humanTokens(r.Cached), ttfb,
			tps(r.DecodeRate), tps(r.TotalRate), bsp(r.ByteRate),
			mages(r.Last),
		})
	}
	mcols, mrows := dropEmptyCols(modelCols(), mrows)
	drawTable(full, "", mcols, mrows, 0, len(mrows))
}

// modelCols 是第 1 页模型/路由聚合表的列: 按类别从左到右排, 同类相邻 ——
// 调度状态 (排队 / 退避 / 熔断锁 / 连续错误), 历史计数 (请求 / 错误 / 换候选),
// 流量 (总 token / 缓存命中 / 首字节 / 三种速度: 解码=completion/decode,
// 总=(prompt+completion)/duration, 字节=bytes/duration), 时间 (最近一次)。
// 三种速度各自一列, 都写 平均/峰值。
// 全部按内容成长 (grow=true), 基准宽只贴标题/内容的典型宽度, 不留大补白 ——
// 这样与第 1 页和其它表一样, 列与列之间视觉上只隔 1 格。
func modelCols() []tuiCol {
	return []tuiCol{
		{"上游/模型", 20, true, false, 4},
		{"排队", 2, true, true, 0}, {"退避", 4, true, true, 4},
		{"熔断", 5, true, true, 4}, {"连错", 2, true, true, 2},
		{"请求", 4, true, true, 0}, {"错误", 2, true, true, 0}, {"换候选", 4, true, true, 1},
		{"token", 6, true, true, 3}, {"缓存", 5, true, true, 1},
		{"首字节", 6, true, true, 3},
		{"解码速率", 11, true, true, 1}, {"总速率", 11, true, true, 0}, {"下载速率", 16, true, true, 1},
		{"最近", 6, true, true, 2},
	}
}

// modelRow 是第 1 页模型/路由聚合表的一行: 队列状态 (观测面) + 聚合统计 (日志, 按上游/模型求和,
// 不区分逻辑名)。
type modelRow struct {
	up, model                       string
	Queue, Fails                    int
	PausedMs, LockMs                int64
	Reqs                            int
	Errs                            int
	Fallbacks                       int
	Total, Cached                   int
	TtfbSum                         int64
	TtfbN                           int
	DecodeRate, TotalRate, ByteRate []tokPoint
	Last                            time.Time
}

// blank 判断这一行是否全无数据: 队列 / 退避 / 熔断 / 连错 / 请求 / 错误 / 换候选 /
// token / 缓存 / 首字节 / 吞吐都是 0 或 -, 且没有最近请求时间。
func (r *modelRow) blank() bool {
	return r.Queue == 0 && r.Fails == 0 && r.PausedMs == 0 && r.LockMs == 0 &&
		r.Reqs == 0 && r.Errs == 0 && r.Fallbacks == 0 &&
		r.Total == 0 && r.Cached == 0 && r.TtfbN == 0 &&
		len(r.DecodeRate) == 0 && len(r.TotalRate) == 0 && len(r.ByteRate) == 0 &&
		r.Last.IsZero()
}

// modelRows 合并观测面里的模型队列与日志聚合: 只保留真正有过请求的 上游/模型;
// 所有数据都是 0 / - 的行 (配置里有、但从没请求过) 不进表。
func (m *tuiModel) modelRows() []*modelRow {
	by := map[string]*modelRow{}
	get := func(up, model string) *modelRow {
		k := up + "\x00" + model
		r := by[k]
		if r == nil {
			r = &modelRow{up: up, model: model}
			by[k] = r
		}
		return r
	}
	for _, cand := range m.pulse.Cands {
		for _, md := range cand.Models {
			r := get(cand.ID, md.Model)
			r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
		}
	}
	for _, rr := range m.routes {
		if rr.Upstream == "" && rr.Model == "" {
			continue // 没路由到任何上游的记录 (如 unknown model) 不属于模型视角
		}
		r := get(rr.Upstream, rr.Model)
		r.Reqs += rr.Reqs
		r.Errs += rr.Errs
		r.Fallbacks += rr.Fallbacks
		r.Total += rr.Total
		r.Cached += rr.Cached
		r.TtfbSum += rr.TtfbSum
		r.TtfbN += rr.TtfbN
		r.DecodeRate = append(r.DecodeRate, rr.DecodeRate...)
		r.TotalRate = append(r.TotalRate, rr.TotalRate...)
		r.ByteRate = append(r.ByteRate, rr.ByteRate...)
		if rr.Last.After(r.Last) {
			r.Last = rr.Last
		}
	}
	out := make([]*modelRow, 0, len(by))
	for _, r := range by {
		if r.blank() {
			continue // 全是 0 / - 的行没有信息量 (只是配置里有这个模型), 不占版面
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
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
	return out
}

// scrollWindow 把该页的滚动位置夹进实际行数, 返回窗口第一行 —— 空表与 G 键 (设成很大)
// 都靠它兜底, 免得画出一片空白。
func (m *tuiModel) scrollWindow(rows, limit int) int {
	top := clampScroll(m.scroll[m.tab], rows, limit)
	m.scroll[m.tab] = top
	return top
}
