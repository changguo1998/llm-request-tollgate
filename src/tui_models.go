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

// groupCell 画一个记账组: 未锁 = 绿色组名 (默认组写 -), 额度锁内 = 浅红倒计时。
func groupCell(g candGroup, isDefault bool) string {
	if g.LockMs > 0 {
		return colorDur(g.LockMs, tuiRedBright)
	}
	if isDefault {
		return "-"
	}
	return tuiGreen + g.Name + tuiReset
}

// drawModelsBody 把模型视角的两张表接在第 1 页活动表下方, 与请求表一起滚动:
// 先上游状态 (并发 / 排队 / 错误 / 记账组锁), 再列有过请求的 上游/模型 队列与路由聚合。
// 两张表不画标题 (列名行保留), 表与表之间空一行; 只出现实际上游/模型, 不按配置候选铺开;
// 第二张表的行按逻辑名分组, 组与组之间空一行。
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
	full.line("") // 与上面的请求表空一行
	drawTable(full, "", cols, urows, 0, len(urows))
	full.line("") // 两张表之间空一行

	// 合并表: 队列状态 (观测面) + 聚合统计 (日志), 行 = 逻辑名的 上游/真实模型,
	// 按逻辑名分组: 同一逻辑名的候选排在一起, 组与组之间空一行。不加逻辑名列。
	var mrows [][]string
	prevLogical := ""
	first := true
	for _, r := range m.modelRows() {
		// 队列的锁都画在队列表里: 退避 = 黄 (等待), 锁按原因分色 —— 熔断锁 = 红 (错误),
		// 低性能锁 = 黄 (慢到降级, 与退避同为等待/降级一类, 但分属不同列)。
		paused := colorDur(r.PausedMs, tuiYell)
		lockColor := tuiRed
		if r.LockWhy == lowPerfReason {
			lockColor = tuiYell
		}
		lock := colorDur(r.LockMs, lockColor)
		// 首字节列写 平均/谷值 (与速率列的 平均/峰值 同一口径): 平均 = rateWin 内
		// 算术平均, 谷值 = 回看窗口内最低的一次时延。
		ttfb := "-"
		if len(r.TtfbPts) > 0 {
			if avg, ok := ttfbAvg(r.TtfbPts, time.Now(), m.rateWin); ok {
				ttfb = durStr(time.Duration(avg)*time.Millisecond) + "/" +
					durStr(time.Duration(ttfbMin(r.TtfbPts))*time.Millisecond)
			}
		}
		// 三种速度都写成 平均/峰值 (rateShow: 平均 = rateWin 内算术平均, 峰值 = 回看窗口
		// 内单笔最高); 缺字段的格子显示 "-"(2B), 整列全空由 dropEmptyCols 收掉。
		tps := func(pts []tokPoint) string {
			if avg, peak, ok := rateShow(pts, time.Now(), m.rateWin, m.window); ok {
				return humanTPS(avg) + "/" + humanTPS(peak)
			}
			return "-"
		}
		bsp := func(pts []tokPoint) string {
			if avg, peak, ok := rateShow(pts, time.Now(), m.rateWin, m.window); ok {
				return humanByte(avg) + "/" + humanByte(peak)
			}
			return "-"
		}
		// 粘性偏好标记: 该逻辑名当前粘性选中的候选在 上游/模型 前标 *。所有行统一
		// 占一个标记位 (非偏好行补空格), 模型名从同一列开始, 不让 * 把对齐顶歪。
		marker := " "
		if p, ok := m.pulse.Sticky[r.logical]; ok && p.Upstream == r.up && p.Model == r.model {
			marker = "*"
		}
		cell := marker + m.upModel(r.up, r.model)
		// 与 modelCols 的顺序一一对应 (dropEmptyCols 按下标裁列)。
		if !first && r.logical != prevLogical {
			mrows = append(mrows, nil) // 组间隔一空行 (视觉分组, 不占列)
		}
		first = false
		prevLogical = r.logical
		mrows = append(mrows, []string{
			cell,
			strconv.Itoa(r.Queue), paused, lock,
			// 缓存命中与总 token 合成一格 (cached/total)。
			humanTokens(r.Cached) + "/" + humanTokens(r.Total), ttfb,
			tps(r.DecodeRate), tps(r.TotalRate), bsp(r.ByteRate),
			mages(r.Last),
		})
	}
	mcols, mrows := dropEmptyCols(modelCols(), mrows)
	drawTable(full, "", mcols, mrows, 0, len(mrows))
}

// modelCols 是第 1 页模型/路由聚合表的列: 按类别从左到右排, 同类相邻 ——
// 调度状态 (排队 / 退避 / 熔断锁), 流量 (Token = cached/total / 首字节 / 三种速度:
// 解码=completion/decode, 总=(prompt+completion)/duration, 字节=bytes/duration), 时间 (最近一次)。
// 三种速度各自一列, 都写 平均/峰值; 首字节写 平均/谷值 (同一套窗口口径)。
// 全部按内容成长 (grow=true), 基准宽只贴标题/内容的典型宽度, 不留大补白 ——
// 这样与第 1 页和其它表一样, 列与列之间视觉上只隔 1 格。
func modelCols() []tuiCol {
	return []tuiCol{
		{"上游/模型", 20, true, false, 4},
		{"排队", 2, true, true, 0}, {"退避", 4, true, true, 4}, {"熔断", 5, true, true, 4},
		// 首字节列写 最后一次/平均, 两个时长值加斜杠, 下限 9 不被压窄。
		{"Token", 9, true, true, 3}, {"首字节", 9, true, true, 3},
		// 三种速度各一列, 都写 平均/峰值: 带单位的数字整体 4 字符, 两个值加斜杠正好 9,
		// 列宽下限 9 保证 "100K/101K" 这类格不被压窄。窄终端丢列时解码速率保住 (hide=4),
		// 总速率与下载速率先丢 (hide=1)。
		{"解码速率", 9, true, true, 4}, {"总速率", 9, true, true, 1}, {"下载速率", 9, true, true, 1},
		{"最近", 6, true, true, 2},
	}
}

// modelRow 是第 1 页模型/路由聚合表的一行: 队列状态 (观测面) + 聚合统计 (日志,
// 按 逻辑名/上游/模型 拆分, 同一上游/模型被多个逻辑名引用时各归各的组)。
type modelRow struct {
	logical, up, model              string
	Queue, Fails                    int
	PausedMs, LockMs                int64
	LockWhy                         string // 模型锁原因 (breaker / lowperf): 倒计时按它分色
	Reqs                            int
	Errs                            int
	Fallbacks                       int
	Total, Cached                   int
	TtfbPts                         []ttfbPoint // 窗口内各请求的首字节时延样本 (平均/谷值用)
	DecodeRate, TotalRate, ByteRate []tokPoint
	Last                            time.Time
}

// blank 判断这一行是否全无数据: 队列 / 退避 / 熔断 / 请求 / 错误 / 换候选 /
// Token / 首字节 / 吞吐都是 0 或 -, 且没有最近请求时间。连错计数 (fails) 不算显示理由:
// 它只在成功服务时清零, 可能只是残留几天前的一次失败, 表里也没有它的列。
func (r *modelRow) blank() bool {
	return r.Queue == 0 && r.PausedMs == 0 && r.LockMs == 0 &&
		r.Reqs == 0 && r.Errs == 0 && r.Fallbacks == 0 &&
		r.Total == 0 && r.Cached == 0 && len(r.TtfbPts) == 0 &&
		len(r.DecodeRate) == 0 && len(r.TotalRate) == 0 && len(r.ByteRate) == 0 &&
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
// 只保留真正有过请求的 上游/模型; 所有数据都是 0 / - 的行 (配置里有、但从没请求过)
// 不进表。观测面的队列 / 锁状态没有逻辑名维度, 按 上游/模型 补到引用它的每组 —
// 引用关系先查配置 aliases: 只要某逻辑名的候选链包含该 (上游/模型), 就归到那个组
// (即使窗口内没日志); 配置里也找不到的才落空逻辑名组 (排最末)。
func (m *tuiModel) modelRows() []*modelRow {
	by := map[string]*modelRow{}
	get := func(logical, up, model string) *modelRow {
		k := logical + "\x00" + up + "\x00" + model
		r := by[k]
		if r == nil {
			r = &modelRow{logical: logical, up: up, model: model}
			by[k] = r
		}
		return r
	}
	for _, rr := range m.routes {
		if rr.Upstream == "" && rr.Model == "" {
			continue // 没路由到任何上游的记录 (如 unknown model) 不属于模型视角
		}
		r := get(rr.Logical, rr.Upstream, rr.Model)
		r.Reqs += rr.Reqs
		r.Errs += rr.Errs
		r.Fallbacks += rr.Fallbacks
		r.Total += rr.Total
		r.Cached += rr.Cached
		r.TtfbPts = append(r.TtfbPts, rr.TtfbPts...)
		r.DecodeRate = append(r.DecodeRate, rr.DecodeRate...)
		r.TotalRate = append(r.TotalRate, rr.TotalRate...)
		r.ByteRate = append(r.ByteRate, rr.ByteRate...)
		if rr.Last.After(r.Last) {
			r.Last = rr.Last
		}
	}
	// 配置归属: (上游, 真实模型) -> 引用它的逻辑名们 (候选链顺序不算顺序, 只数集合)。
	// 组 (别名的别名) 展开后与逻辑名同属一张候选表, 组名也是引用它的逻辑名。
	// 配置读不到 (如 -tui 时 cfgPath 缺失) 为 nil, 退回纯日志分组的老行为。
	aliasGroups := map[string][]string{}
	if c, err := loadConfigOpt(m.cfgPath, true); err == nil {
		for logical, list := range c.expandedAliases() {
			for _, tgt := range list {
				k := tgt[0] + "\x00" + tgt[1]
				aliasGroups[k] = append(aliasGroups[k], logical)
			}
		}
	}
	for _, cand := range m.pulse.Cands {
		for _, md := range cand.Models {
			// 队列状态补到所有引用该 (上游/模型) 的逻辑名组; 窗口内没有任何日志归属时,
			// 按配置候选链找引用它的逻辑名们 (同一个逻辑名不管有没有记录都归一组),
			// 配置里也没有才落空逻辑名组 (只有观测面状态、没路由去向的模型也要看得到)。
			found := false
			for _, r := range by {
				if r.up == cand.ID && r.model == md.Model {
					r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
					r.LockWhy = md.Why
					found = true
				}
			}
			if found {
				continue
			}
			k := cand.ID + "\x00" + md.Model
			owners := aliasGroups[k]
			if len(owners) == 0 {
				// 配置里也没有这个 (上游/模型): 只有观测面状态, 单独垫底一组。
				r := get("", cand.ID, md.Model)
				r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
				r.LockWhy = md.Why
				continue
			}
			for _, logical := range owners {
				r := get(logical, cand.ID, md.Model)
				r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
				r.LockWhy = md.Why
			}
		}
	}
	out := make([]*modelRow, 0, len(by))
	for _, r := range by {
		if r.blank() {
			continue // 全是 0 / - 的行没有信息量 (只是配置里有这个模型), 不占版面
		}
		out = append(out, r)
	}
	// 组间: 有逻辑名的按组内最近请求时间降序 (常用的在前), 空逻辑名 (只观测面) 垫底;
	// 组内: 最近时间降序, 同刻按请求数 / 上游 / 模型兜底 (与旧的混排口径一致)。
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
		case !out[i].Last.Equal(out[j].Last):
			return out[i].Last.After(out[j].Last)
		case out[i].Reqs != out[j].Reqs:
			return out[i].Reqs > out[j].Reqs
		case out[i].up != out[j].up:
			return out[i].up < out[j].up
		default:
			return out[i].model < out[j].model
		}
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
