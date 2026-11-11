// tui_prio.go: 候选顺序调整与别名重建。

package main

import (
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
	rows := e.filtered()
	if len(rows) == 0 {
		e.modelSel = 0
	} else if e.modelSel >= len(rows) {
		e.modelSel = len(rows) - 1
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
	rows := e.filtered()
	if len(rows) == 0 {
		return nil
	}
	i := clampInt(e.modelSel, 0, len(rows)-1)
	return &rows[i]
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
