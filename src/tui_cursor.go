// tui_cursor.go: 第 1 页的行光标与行操作 (解锁 / 粘性偏好)。
//
// 光标锚在行内容上 (上游 id / 记账组名 / 逻辑名 + 上游/模型), 行序随数据变化时仍钉在
// 同一行。j/k 上下移动 (上游表与模型表按页面顺序连着走, 到边界不绕回), h/l 在上游行
// 的格子间左右走: 上游名 / 默认组 / 各命名的记账组 (模型行只有 上游/模型 一格)。
// u 解锁光标处的锁: 上游名 = 该上游全部锁, 记账组 = 该组的额度锁, 模型行 = 该上游/模型
// 的熔断锁与退避; Enter 把光标所在的模型行置为它所属逻辑名的粘性偏好, c 清除该逻辑名
// 的偏好。写操作都走 HTTP (POST /unlock / POST /sticky): 同进程发给自己, -tui-state 发给
// 被观测服务。

package main

import (
	"encoding/json"
	"net/url"
	"strconv"
)

// flowCursor 是第 1 页的光标锚点。
//   - table 0 (上游表): up + name 选中上游名那一格; 否则选 group 那一列 (默认组是空串);
//   - table 1 (模型表): logical + up + model 选中一行。
type flowCursor struct {
	table int
	up    string
	name  bool
	group string

	logical, model string
}

// flowUpRow 是上游表的一行可操作目标: 上游 id + 记账组 (默认组在前, 与上游表的列序一致)。
type flowUpRow struct {
	id     string
	groups []string
}

// flowModelRow 是模型表的一行可操作目标。
type flowModelRow struct {
	logical, up, model string
}

// flowNav 是第 1 页当前的可操作行布局: 上游表在前, 模型表在后, 与页面自上而下的顺序一致。
type flowNav struct {
	ups    []flowUpRow
	models []flowModelRow
}

// flowNav 按同一份观测数据算可操作行 (绘制与按键共用): 表里没数据的行不进光标范围。
func (m *tuiModel) flowNav() flowNav {
	return flowNavFrom(m.sortedCands(), m.modelRows())
}

func flowNavFrom(cands []candView, rows []*modelRow) flowNav {
	var n flowNav
	for _, c := range cands {
		row := flowUpRow{id: c.ID, groups: []string{defaultGroup}}
		for _, g := range c.Groups {
			if g.Name != defaultGroup {
				row.groups = append(row.groups, g.Name)
			}
		}
		n.ups = append(n.ups, row)
	}
	for _, r := range rows {
		n.models = append(n.models, flowModelRow{r.logical, r.up, r.model})
	}
	return n
}

// resolve 把锚点钉到当前行序上: 锚点行不在了就退到同表第一行 (该表空了换另一张表);
// 记账组不在了退回该行的上游名列。
func (n flowNav) resolve(c flowCursor) (flowCursor, bool) {
	if c.table == 0 {
		for _, r := range n.ups {
			if r.id != c.up {
				continue
			}
			cur := flowCursor{table: 0, up: r.id, name: true}
			if !c.name {
				if i := indexOfStr(r.groups, c.group); i >= 0 {
					cur.name, cur.group = false, r.groups[i]
				}
			}
			return cur, true
		}
		return n.first(0)
	}
	for _, r := range n.models {
		if r.logical == c.logical && r.up == c.up && r.model == c.model {
			return flowCursor{table: 1, up: r.up, logical: r.logical, model: r.model}, true
		}
	}
	return n.first(1)
}

// first 取某张表的第一行; 该表空了换另一张表, 两张都空则没有可操作行。
func (n flowNav) first(table int) (flowCursor, bool) {
	if table == 0 && len(n.ups) > 0 {
		return flowCursor{table: 0, up: n.ups[0].id, name: true}, true
	}
	if table == 1 && len(n.models) > 0 {
		r := n.models[0]
		return flowCursor{table: 1, up: r.up, logical: r.logical, model: r.model}, true
	}
	if len(n.ups) > 0 {
		return flowCursor{table: 0, up: n.ups[0].id, name: true}, true
	}
	if len(n.models) > 0 {
		r := n.models[0]
		return flowCursor{table: 1, up: r.up, logical: r.logical, model: r.model}, true
	}
	return flowCursor{}, false
}

// rowOf 是光标在整页可操作行里的下标 (上游行在前, 模型行在后)。
func (n flowNav) rowOf(c flowCursor) int {
	if c.table == 0 {
		for i, r := range n.ups {
			if r.id == c.up {
				return i
			}
		}
		return 0
	}
	for i, r := range n.models {
		if r.logical == c.logical && r.up == c.up && r.model == c.model {
			return len(n.ups) + i
		}
	}
	return len(n.ups)
}

// at 取整页第 i 个可操作行的锚点: 上游行尽量保住原来的记账组 (组不在就落到上游名列),
// 模型行只有 上游/模型 一格。
func (n flowNav) at(i int, want flowCursor) (flowCursor, bool) {
	if i < 0 || i >= len(n.ups)+len(n.models) {
		return flowCursor{}, false
	}
	if i < len(n.ups) {
		r := n.ups[i]
		cur := flowCursor{table: 0, up: r.id, name: true}
		if want.table == 0 && !want.name {
			if j := indexOfStr(r.groups, want.group); j >= 0 {
				cur.name, cur.group = false, r.groups[j]
			}
		}
		return cur, true
	}
	r := n.models[i-len(n.ups)]
	return flowCursor{table: 1, up: r.up, logical: r.logical, model: r.model}, true
}

// flowMove 上下移动光标 (j/k): 上游表与模型表按页面顺序连着走, 边界处停住。
func (m *tuiModel) flowMove(d int) {
	n := m.flowNav()
	c, ok := n.resolve(m.cur)
	if !ok {
		return
	}
	i := clampInt(n.rowOf(c)+d, 0, len(n.ups)+len(n.models)-1)
	if next, ok := n.at(i, c); ok {
		m.cur = next
		m.followCursorRow()
	}
}

// flowCol 左右移动光标 (h/l): 上游行在 上游名 / 默认组 / 命名的组合格之间走;
// 模型行只有 上游/模型 一格, h/l 不动。
func (m *tuiModel) flowCol(d int) {
	n := m.flowNav()
	c, ok := n.resolve(m.cur)
	if !ok {
		return
	}
	m.cur = c
	if c.table != 0 {
		return
	}
	for _, r := range n.ups {
		if r.id != c.up {
			continue
		}
		col := 0
		if !c.name {
			col = 1 + indexOfStr(r.groups, c.group)
		}
		col = clampInt(col+d, 0, len(r.groups))
		if col == 0 {
			m.cur.name, m.cur.group = true, defaultGroup
		} else {
			m.cur.name, m.cur.group = false, r.groups[col-1]
		}
		m.followCursorRow()
		return
	}
}

// followCursorRow 记下「光标动过」, 让下一帧把窗口滚到光标行; 顺手清掉上一条操作反馈。
func (m *tuiModel) followCursorRow() {
	m.curFollow = true
	m.flowMsg, m.flowErr = "", false
}

// flowUnlock 解锁光标处的锁 (u): 上游名 = 该上游全部锁; 记账组 = 该组额度锁;
// 模型行 = 该 上游/模型 的熔断锁与退避。
func (m *tuiModel) flowUnlock() {
	n := m.flowNav()
	c, ok := n.resolve(m.cur)
	if !ok {
		m.flowFail("没有可操作的行")
		return
	}
	m.cur = c
	q := url.Values{}
	q.Set("upstream", c.up)
	var what string
	switch {
	case c.table == 1:
		q.Set("model", c.model)
		what = c.up + "/" + c.model
	case !c.name:
		g := c.group
		if g == defaultGroup {
			g, what = "def", c.up+" 的默认组"
		} else {
			what = c.up + " 的记账组 " + g
		}
		q.Set("group", g)
	default:
		what = c.up
	}
	body, err := m.postSide("/unlock", q)
	if err != nil {
		m.flowFail("解锁失败: " + err.Error())
		return
	}
	m.flowOK("已解锁 " + what + " (" + strconv.Itoa(clearedCount(body)) + " 处)")
}

// clearedCount 读 /unlock 与 /sticky 响应里的 cleared 计数; 读不到就按 0。
func clearedCount(body []byte) int {
	var v struct {
		Cleared int `json:"cleared"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	return v.Cleared
}

// flowStickySet 把光标所在的模型行置为它所属逻辑名的一次性偏好 (Enter)。
func (m *tuiModel) flowStickySet() {
	c, ok := m.flowCursorModel()
	if !ok {
		m.flowFail("先把光标移到模型行 (j/k)")
		return
	}
	if c.logical == "" {
		m.flowFail("这一行没有逻辑名, 不能置偏好")
		return
	}
	q := url.Values{}
	q.Set("logical", c.logical)
	q.Set("upstream", c.up)
	q.Set("model", c.model)
	if _, err := m.postSide("/sticky", q); err != nil {
		m.flowFail("置偏好失败: " + err.Error())
		return
	}
	m.flowOK("已置 " + c.logical + " -> " + c.up + "/" + c.model)
}

// flowStickyClear 清除光标所在模型行的逻辑名的偏好 (c)。
func (m *tuiModel) flowStickyClear() {
	c, ok := m.flowCursorModel()
	if !ok {
		m.flowFail("先把光标移到模型行 (j/k)")
		return
	}
	if c.logical == "" {
		m.flowFail("这一行没有逻辑名, 没有偏好可清除")
		return
	}
	q := url.Values{}
	q.Set("logical", c.logical)
	if _, err := m.postSide("/sticky", q); err != nil {
		m.flowFail("清除失败: " + err.Error())
		return
	}
	m.flowOK("已清除 " + c.logical + " 的偏好 (回落配置首候选)")
}

// flowCursorModel 取光标处的模型行锚点 (不在模型表返回 false)。
func (m *tuiModel) flowCursorModel() (flowCursor, bool) {
	n := m.flowNav()
	c, ok := n.resolve(m.cur)
	if !ok || c.table != 1 {
		return flowCursor{}, false
	}
	m.cur = c
	return c, true
}

// flowOK / flowFail 写第 1 页底部的操作反馈 (成功黄字, 失败红字)。
func (m *tuiModel) flowOK(msg string) { m.flowMsg, m.flowErr = msg, false }

func (m *tuiModel) flowFail(msg string) { m.flowMsg, m.flowErr = msg, true }

// indexOfStr 返回 v 在 s 里的下标 (没有是 -1)。
func indexOfStr(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
