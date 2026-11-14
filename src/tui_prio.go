// tui_prio.go: 候选顺序调整与别名重建。

package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// buildAliases 复刻生成器的归并与排序, 右栏预览与落进配置的候选顺序一致; 停用模型
// (负优先级, 下限 -1) 除外: 预览里排在最后, 但生成器会跳过, 不会落进 aliases/tollgate 目录。
// 停用的 provider (负优先级) 整家跳过: 它的模型不进候选, 生成配置时也不进 upstreams。
func (e *tuiEditor) buildAliases() []tuiAlias {
	byAlias := e.collectPrioItems()
	names := make([]string, 0, len(byAlias))
	for name := range byAlias {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]tuiAlias, 0, len(names))
	for _, name := range names {
		items := byAlias[name]
		sortPrioItems(items)
		alias := tuiAlias{name: name}
		for _, it := range items {
			alias.cands = append(alias.cands, it.cand)
		}
		out = append(out, alias)
	}
	return out
}

// tuiCand 是模型面板的一个可选行: 别名模型候选 (可调优先级) 或组候选行 (只展示成员,
// 组不是权重条目)。row 是别名模型行在 e.filtered() 里的下标; 组候选行为 -1。
type tuiCand struct {
	logical string // 逻辑名 (别名或组名)
	member  string // 组候选行: 显示的成员逻辑名
	isGroup bool
	row     int // 别名模型行在 e.filtered() 里的下标; 组候选行为 -1
}

// cands 返回模型面板全部可选行: 先别名模型行 (filtered, 可调优先级), 再组候选行
// (只列组成员, 组内顺序即优先级, +/- 在组内上下移动成员); 配置读不到时只回模型行。
// 组候选行按配置 mtime 缓存, 组内移动后由 moveGroupMember 就地作废重建。
func (e *tuiEditor) cands() []tuiCand {
	rows := e.filtered()
	out := make([]tuiCand, 0, len(rows)+8)
	for i, r := range rows {
		out = append(out, tuiCand{logical: r.alias, row: i})
	}
	cm, err := os.Stat(e.config)
	if err == nil {
		if e.groupCands == nil || !cm.ModTime().Equal(e.groupCandsMod) {
			e.groupCands, e.groupCandsMod = e.buildGroupCands(), cm.ModTime()
		}
		out = append(out, e.groupCands...)
	}
	return out
}

// buildGroupCands 把组成员摊成候选行 (组名 -> 成员逻辑名, 不展开上游): 组名按字典序,
// 成员按内存里配置的顺序 (组内 +/- 移动后还没落盘时以内存为准)。成员没有可用候选
// (未注册或整家停用) 就不列; 配置读不到时返回 nil。
func (e *tuiEditor) buildGroupCands() []tuiCand {
	groups, ok := e.groups()
	if !ok {
		return nil
	}
	c, err := loadConfigOpt(e.config, true)
	if err != nil {
		return nil
	}
	ex := c.expandedAliases()
	var out []tuiCand
	for _, g := range groups {
		for _, member := range g.members {
			if len(ex[member]) == 0 {
				continue
			}
			out = append(out, tuiCand{logical: g.name, member: member, isGroup: true, row: -1})
		}
	}
	return out
}

// pickAt 取 cands 里第 i 个可选行 (越界夹紧), 空列表返回 nil。
func (e *tuiEditor) pickAt(i int) *tuiCand {
	cs := e.cands()
	if len(cs) == 0 {
		return nil
	}
	return &cs[clampInt(i, 0, len(cs)-1)]
}

// prioItem 是候选在逻辑名分组里的排序键与内容。
type prioItem struct {
	rank int
	cand tuiPrioCand
}

// collectPrioItems 按 provider 顺序扫模型并归到逻辑名; 停用的 provider 与黑名单模型跳过。
func (e *tuiEditor) collectPrioItems() map[string][]prioItem {
	dropped := e.droplist()
	byAlias := map[string][]prioItem{}
	for rank, name := range e.providers {
		entry := e.provEntry(name)
		if entry == nil {
			continue
		}
		provPrio := prioOf(entry)
		if provPrio < 0 {
			continue // 负优先级: 整个 provider 停用, 与生成器一致
		}
		for _, model := range modelsOf(entry) {
			id := modelID(model)
			if id == "" || dropped[id] {
				continue
			}
			alias := aliasOf(model)
			byAlias[alias] = append(byAlias[alias], prioItem{rank, tuiPrioCand{name, id, prioOf(model), provPrio}})
		}
	}
	return byAlias
}

// sortPrioItems 是组内顺序: 模型优先级 -> provider 优先级 -> 上游顺序 -> 模型 id。
func sortPrioItems(items []prioItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.cand.modelPrio != b.cand.modelPrio {
			return a.cand.modelPrio > b.cand.modelPrio
		}
		if a.cand.provPrio != b.cand.provPrio {
			return a.cand.provPrio > b.cand.provPrio
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.cand.modelID < b.cand.modelID
	})
}

// rebuild 重算列表 (别名升序, 别名内按候选顺序), 并把光标钉回原来那一行 —— 调优先级
// 会让行序实时变化, 不钉住的话光标会跳到别人身上。
func (e *tuiEditor) rebuild() {
	if e.prevKey == "" {
		if row := e.selected(); row != nil {
			e.prevKey = row.provider + "/" + modelID(row.model)
		}
	}
	e.order = e.order[:0]
	for _, alias := range e.buildAliases() {
		for _, c := range alias.cands {
			entry := e.provEntry(c.provider)
			for _, model := range modelsOf(entry) {
				if modelID(model) == c.modelID {
					e.order = append(e.order, tuiPrioRow{alias.name, c.provider, model, entry})
					break
				}
			}
		}
	}
	if e.prevKey != "" {
		for i, row := range e.order {
			if row.provider+"/"+modelID(row.model) == e.prevKey {
				e.modelSel = i
				break
			}
		}
	}
	cs := e.cands()
	if len(cs) == 0 {
		e.modelSel = 0
	} else if e.modelSel >= len(cs) {
		e.modelSel = len(cs) - 1
	}
	if e.modelSel < 0 {
		e.modelSel = 0
	}
	if e.provSel >= len(e.providers) {
		e.provSel = max(0, len(e.providers)-1)
	}
}

func (e *tuiEditor) filtered() []tuiPrioRow {
	if e.filter == "" {
		return e.order
	}
	var out []tuiPrioRow
	for _, row := range e.order {
		if strings.Contains(row.alias, e.filter) ||
			strings.Contains(row.provider, e.filter) ||
			strings.Contains(modelID(row.model), e.filter) {
			out = append(out, row)
		}
	}
	return out
}

func (e *tuiEditor) selected() *tuiPrioRow {
	p := e.pickAt(e.modelSel)
	if p == nil || p.row < 0 { // 组候选行没有权重可调 (只展示成员)
		return nil
	}
	rows := e.filtered()
	if p.row >= len(rows) {
		return nil
	}
	return &rows[p.row]
}

// groupRow 返回组候选行 (组名 + 成员) 在 cands 里的下标; 没有返回 -1。
func (e *tuiEditor) groupRow(gname, member string) int {
	for i, c := range e.cands() {
		if c.isGroup && c.logical == gname && c.member == member {
			return i
		}
	}
	return -1
}

// moveGroupMember 把组 gname 的成员 member 在组内移动一位: delta > 0 上移 (靠近候选链
// 首位, 优先级提高), delta < 0 下移; 到边界再按没有反应 (不改配置, 不标脏, 不提示)。
// 新顺序先写进内存里的配置, 按 s 才落盘; 光标跟着被移动的那一行走。
func (e *tuiEditor) moveGroupMember(gname, member string, delta int) {
	raw, ok := e.cfg["groups"].(map[string]any)
	if !ok {
		return
	}
	list, ok := raw[gname].([]any)
	if !ok || len(list) < 2 {
		return
	}
	from := -1
	for i, m := range list {
		if s, ok := m.(string); ok && s == member {
			from = i
			break
		}
	}
	if from < 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	to := from - step
	if to < 0 || to >= len(list) {
		return // 边界: 再按没有反应
	}
	list[from], list[to] = list[to], list[from]
	e.groupCands = nil // 组内顺序变了: 缓存作废, 下次 cands() 按内存重建
	e.dirty = true
	e.asked = false
	e.message = "组 " + gname + ": " + member + " 移到第 " + strconv.Itoa(to+1) + " 位"
	if i := e.groupRow(gname, member); i >= 0 {
		e.modelSel = i
	}
}

// candsFor 取某别名的候选顺序 (右栏预览)。
func (e *tuiEditor) candsFor(alias string) []tuiPrioCand {
	for _, a := range e.buildAliases() {
		if a.name == alias {
			return a.cands
		}
	}
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (e *tuiEditor) markDirty() {
	e.dirty = true
	e.asked = false
	e.rebuild()
}

func (e *tuiEditor) adjust(delta int) {
	if e.focus == 2 {
		e.adjustGate(delta)
		return
	}
	if !e.usable() {
		return
	}
	if e.focus == 1 {
		if len(e.providers) == 0 {
			return
		}
		name := e.providers[clampInt(e.provSel, 0, len(e.providers)-1)]
		entry := e.provEntry(name)
		setPrio(entry, prioOf(entry)+delta)
		e.message = name + ": provider 优先级 = " + strconv.Itoa(prioOf(entry))
		e.sortProviders()
	} else {
		if p := e.pickAt(e.modelSel); p != nil && p.isGroup {
			e.moveGroupMember(p.logical, p.member, delta) // 组行: +/- 在组内上下移成员
			return
		}
		row := e.selected()
		if row == nil {
			return
		}
		e.prevKey = row.provider + "/" + modelID(row.model)
		setPrio(row.model, prioOf(row.model)+delta)
		e.message = row.provider + "/" + modelID(row.model) + ": 模型优先级 = " + strconv.Itoa(prioOf(row.model))
	}
	e.markDirty()
}

func (e *tuiEditor) setValue(v int) {
	if !e.usable() {
		return
	}
	if e.focus == 1 {
		if len(e.providers) == 0 {
			return
		}
		entry := e.provEntry(e.providers[clampInt(e.provSel, 0, len(e.providers)-1)])
		setPrio(entry, v)
		e.message = "provider 优先级 = " + strconv.Itoa(prioOf(entry))
		e.sortProviders()
	} else {
		row := e.selected()
		if row == nil {
			return
		}
		e.prevKey = row.provider + "/" + modelID(row.model)
		setPrio(row.model, v)
		e.message = row.provider + "/" + modelID(row.model) + ": 模型优先级 = " + strconv.Itoa(prioOf(row.model))
	}
	e.markDirty()
}
