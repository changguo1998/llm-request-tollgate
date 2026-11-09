// tui_events_test.go: 事件流显示合并 —— 同一秒同一请求的阶段切换连成一条链,
// 桥接阶段去重取信息量更多的表达, req=0 的事件不合并, 跨秒拆条。
package main

import (
	"sort"
	"testing"
	"time"
)

// mergeEventChain: 相邻过渡的桥接阶段只留一个; 同词两种写法 (sem 与 sem(u1)) 取带参数
// 的那个; 真正不同的过渡词 (fail / switch / new …) 直接相接。
func TestMergeEventChain(t *testing.T) {
	cases := []struct {
		msgs []string
		want string
	}{
		{[]string{"new(u1/real-1) -> sem(u1)", "sem(u1) -> freq(u1)", "freq(u1) -> queue(u1/real-1)"},
			"new(u1/real-1) -> sem(u1) -> freq(u1) -> queue(u1/real-1)"},
		// 桥接阶段两种写法: 取信息量更多的 (sem(u1) 优于 sem)。
		{[]string{"new(u1/real-1) -> sem(u1)", "sem -> reject(quota 05:00)"},
			"new(u1/real-1) -> sem(u1) -> reject(quota 05:00)"},
		// 不同过渡词直接相接 (fail 之后 switch)。
		{[]string{"send -> fail(502)", "switch(u2/real-2) -> sem(u2)"},
			"send -> fail(502) -> switch(u2/real-2) -> sem(u2)"},
		{[]string{"send -> recv", "recv -> ok(200)"},
			"send -> recv -> ok(200)"},
		// 单独一条原样返回; 空输入给空串。
		{[]string{"switch(u2/real-2) -> sem(u2)"},
			"switch(u2/real-2) -> sem(u2)"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := mergeEventChain(c.msgs); got != c.want {
			t.Errorf("mergeEventChain(%v) 应为 %q, 得到 %q", c.msgs, c.want, got)
		}
	}
}

// mergedEventRows: 同一秒内同一请求的连续切换并成一条链; 请求间互插也能各归各;
// req=0 的锁事件不合并、原样一行; 跨秒的链拆成两条。
func TestMergedEventRows(t *testing.T) {
	m := newTUIModel("cfg")
	sec := time.Now().Truncate(time.Second)
	m.events = append(m.events,
		routeEvent{Ts: sec, Req: 1, Logical: "a", Kind: "stage", Msg: "new(u1/real-1) -> sem(u1)"},
		routeEvent{Ts: sec, Req: 0, Logical: "", Kind: "lock", Upstream: "ustc", Msg: "额度限制, 触发 额度锁, 05:00"},
		routeEvent{Ts: sec, Req: 2, Logical: "b", Kind: "stage", Msg: "new(u2/real-2) -> sem(u2)"},
		routeEvent{Ts: sec, Req: 1, Logical: "a", Kind: "stage", Msg: "sem(u1) -> freq(u1)"},
		routeEvent{Ts: sec, Req: 2, Logical: "b", Kind: "stage", Msg: "sem(u2) -> freq(u2)"},
		routeEvent{Ts: sec.Add(time.Second), Req: 2, Logical: "b", Kind: "stage", Msg: "freq(u2) -> queue(u2/real-2)"},
	)
	rows := m.mergedEventRows()

	got := map[uint64][]struct {
		ts  time.Time
		msg string
	}{}
	lockRows := 0
	for _, r := range rows {
		if r.Req == 0 {
			lockRows++
			if r.Msg != "额度限制, 触发 额度锁, 05:00" {
				t.Errorf("锁事件应原样一行, 得到 %q", r.Msg)
			}
			continue
		}
		got[r.Req] = append(got[r.Req], struct {
			ts  time.Time
			msg string
		}{r.Ts, r.Msg})
	}
	if lockRows != 1 {
		t.Errorf("锁事件应占 1 行, 得到 %d", lockRows)
	}
	// 请求 a: 同一秒两段并成一条链。
	if want := []string{"new(u1/real-1) -> sem(u1) -> freq(u1)"}; !msgsAt(got[1], want) {
		t.Errorf("请求 a 应为 %v, 得到 %v", want, got[1])
	}
	// 请求 b: 同秒一条 + 跨秒一条 (从断点接着出); 显示按时间倒序, 断言先按时间排好。
	if want := []string{"new(u2/real-2) -> sem(u2) -> freq(u2)", "freq(u2) -> queue(u2/real-2)"}; !msgsAt(got[2], want) {
		t.Errorf("请求 b 应为 %v, 得到 %v", want, got[2])
	}
	// 时间列按秒: 跨秒的那条带下一秒钟。
	for _, r := range rows {
		if r.Req == 2 && r.Msg == "freq(u2) -> queue(u2/real-2)" && !r.Ts.Equal(sec.Add(time.Second)) {
			t.Errorf("跨秒条目的时间应为 %v, 得到 %v", sec.Add(time.Second), r.Ts)
		}
	}
}

// msgsAt 按时间升序取出消息并比对 (mergedEventRows 返回的是最新的在前)。
func msgsAt(rows []struct {
	ts  time.Time
	msg string
}, want []string) bool {
	sorted := append([]struct {
		ts  time.Time
		msg string
	}{}, rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ts.Before(sorted[j].ts) })
	got := make([]string, len(sorted))
	for i, r := range sorted {
		got[i] = r.msg
	}
	return eqSlice(got, want)
}

func eqSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
