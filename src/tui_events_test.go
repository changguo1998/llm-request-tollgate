// tui_events_test.go: 事件流显示合并 —— 同一秒同一请求的阶段切换连成一条链,
// 桥接阶段去重取信息量更多的表达, req=0 的事件不合并, 跨秒拆条。
package main

import (
	"sort"
	"strings"
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
		{[]string{"new(a) -> sem(u1)", "sem(u1) -> freq(u1)", "freq(u1) -> queue(u1/real-1)"},
			"new(a) -> sem(u1) -> freq(u1) -> queue(u1/real-1)"},
		// 桥接阶段两种写法: 取信息量更多的 (sem(u1) 优于 sem)。
		{[]string{"new(a) -> sem(u1)", "sem -> reject(quota 05:00)"},
			"new(a) -> sem(u1) -> reject(quota 05:00)"},
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
		routeEvent{Ts: sec, Req: 1, Logical: "a", Kind: "stage", Msg: "new(a) -> sem(u1)"},
		routeEvent{Ts: sec, Req: 0, Logical: "", Kind: "lock", Upstream: "ustc", Msg: "lock(quota 默认 05:00)"},
		routeEvent{Ts: sec, Req: 2, Logical: "b", Kind: "stage", Msg: "new(b) -> sem(u2)"},
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
			if r.Msg != "lock(quota 默认 05:00)" {
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
	if want := []string{"new(a) -> sem(u1) -> freq(u1)"}; !msgsAt(got[1], want) {
		t.Errorf("请求 a 应为 %v, 得到 %v", want, got[1])
	}
	// 请求 b: 同秒一条 + 跨秒一条 (从断点接着出); 显示按时间倒序, 断言先按时间排好。
	if want := []string{"new(b) -> sem(u2) -> freq(u2)", "freq(u2) -> queue(u2/real-2)"}; !msgsAt(got[2], want) {
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

// colorEventBody: 失败结局 fail(原因) 红字, 异常过渡词 switch / retry 黄字, 收尾词
// ok(200) 绿字; 阶段词参数里的供应商上专属色 (模型部分与 new(逻辑名) 不动),
// 配置里没有的上游名原样保留。
func TestColorEventBody(t *testing.T) {
	m := newTUIModel("cfg")
	m.upColors = map[string]string{"ustc": "\x1b[36m", "ark": "\x1b[35m"}
	ark := m.upName("ark")
	cases := []struct{ in, want string }{
		{"new(gpt-5) -> sem(ustc)", "new(gpt-5) -> sem(" + m.upName("ustc") + ")"},
		{"freq(ustc) -> queue(ustc/real-1)",
			"freq(" + m.upName("ustc") + ") -> queue(" + m.upName("ustc") + "/real-1)"},
		// 换候选: 整词黄字, 参数里的供应商名保持专属色 (reset 之后补回黄色)
		{"switch(ark/real-2) -> sem(ark)",
			tuiYell + "switch(" + ark + tuiYell + "/real-2)" + tuiReset +
				" -> sem(" + ark + ")"},
		// 同候选重试: 参数里没有专属色时整词黄字
		{"retry(u1/real-1) -> sem(u1)", tuiYell + "retry(u1/real-1)" + tuiReset + " -> sem(u1)"},
		{"send -> fail(502)", "send -> " + tuiRed + "fail(502)" + tuiReset},               // 失败结局红字
		{"sem -> reject(quota 05:00)", "sem -> reject(quota 05:00)"},                      // 参数不是供应商
		{"new(gpt-5) -> sem(nope)", "new(gpt-5) -> sem(nope)"},                            // 配置里没有的上游
		{"send -> recv -> ok(200)", "send -> recv -> " + tuiGreen + "ok(200)" + tuiReset}, // 收尾词绿字
		{"probe -> ok", "probe -> ok"},                                                    // 探测成功不是收尾词, 不上色
	}
	for _, c := range cases {
		if got := m.colorEventBody(c.in); got != c.want {
			t.Errorf("colorEventBody(%q) 应为 %q, 得到 %q", c.in, c.want, got)
		}
	}
}

// eventWho: 标识列的请求逻辑名带专属色 (加粗与上游名区分); 配置里没有的名字不上色。
func TestEventWhoLogicalColor(t *testing.T) {
	m := newTUIModel("cfg")
	m.logicalColors = map[string]string{"gpt-5": "\x1b[36m"}
	want := "12/" + tuiBold + "\x1b[36m" + "gpt-5" + tuiReset
	if got := m.eventWho(routeEvent{Req: 12, Logical: "gpt-5"}); got != want {
		t.Errorf("标识应为 %q, 得到 %q", want, got)
	}
	if got := m.eventWho(routeEvent{Req: 12, Logical: "gone"}); got != "12/gone" {
		t.Errorf("配置里没有的逻辑名应原样输出, 得到 %q", got)
	}
}

// 逻辑名名单按配置里的逻辑名 (含组名) 字典序: 每个逻辑名一种专属色, 不随帧变。
func TestLogicalNameColors(t *testing.T) {
	m := newTUIModel(stickyFixture(t))
	names := m.cachedLogicalNames()
	if !eqSlice(names, []string{"m1", "m2"}) {
		t.Fatalf("逻辑名应为字典序的 m1 / m2, 得到 %v", names)
	}
	colors := make(map[string]string, len(names))
	for i, name := range names {
		colors[name] = logicalPalette[i%len(logicalPalette)]
	}
	if got := logicalPalette[0]; got != "\x1b[34m" || logicalPalette[1] != "\x1b[35m" || logicalPalette[2] != "\x1b[36m" {
		t.Errorf("最先分配的三个颜色应是标准色 (蓝/品红/青), 得到 %v", logicalPalette[:3])
	}
	if colors["m1"] == colors["m2"] {
		t.Errorf("两个逻辑名不该同色: %v", colors)
	}
	if colors["m1"] != logicalPalette[0] || colors["m2"] != logicalPalette[1] {
		t.Errorf("字典序靠前的逻辑名应先用标准色, 得到 %v", colors)
	}
}

// 第 2 页: 标识列的逻辑名与正文里的供应商名带专属色; 失败结局红字, 收尾词绿字,
// 其余文字是前景色。
func TestDrawEventsColors(t *testing.T) {
	m := newTUIModel("cfg")
	m.logicalColors = map[string]string{"gpt-5": "\x1b[36m"}
	m.upColors = map[string]string{"ustc": "\x1b[36m"}
	m.events = []routeEvent{
		{Ts: time.Now(), Req: 12, Logical: "gpt-5", Kind: "stage", Msg: "new(gpt-5) -> sem(ustc)"},
		{Ts: time.Now().Add(-time.Second), Req: 13, Logical: "gpt-5", Kind: "error",
			Msg: "send -> fail(502)"},
		{Ts: time.Now().Add(-2 * time.Second), Req: 14, Logical: "gpt-5", Kind: "ok",
			Msg: "recv -> ok(200)"},
	}
	c := &tuiCanvas{cols: 120}
	m.drawEvents(c, 20)
	out := c.b.String()
	who := "12/" + tuiBold + "\x1b[36m" + "gpt-5" + tuiReset
	if !strings.Contains(out, who) {
		t.Errorf("标识列的逻辑名没带专属色 %q", who)
	}
	body := "sem(" + "\x1b[36m" + "ustc" + tuiReset + ")"
	if !strings.Contains(out, body) {
		t.Errorf("正文里的供应商名没带专属色 %q", body)
	}
	if ok := tuiGreen + "ok(200)" + tuiReset; !strings.Contains(out, ok) {
		t.Errorf("成功收尾词应为绿字 %q, 实际没有", ok)
	}
	if fail := tuiRed + "fail(502)" + tuiReset; !strings.Contains(out, fail) {
		t.Errorf("失败结局应为红字 %q, 实际没有", fail)
	}
}
