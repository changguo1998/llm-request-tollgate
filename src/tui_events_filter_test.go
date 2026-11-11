// tui_events_filter_test.go: 第 2 页筛选 —— 文本筛 编号/逻辑名/事件, 时间范围解析与
// 日志回读 (范围结果不受 256 条事件上限约束), 以及 f / t / c 的按键路径。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseEvRange: 相对时长 / 时刻 / 区间 / 开区间 / 跨午夜 / 夹到回放窗口 / 写法错误。
func TestParseEvRange(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local)
	at := func(day, h, m, s int) time.Time {
		return time.Date(2026, 11, day, h, m, s, 0, time.Local)
	}
	cases := []struct {
		in    string
		from  time.Time
		to    time.Time
		slide time.Duration
		err   bool
	}{
		{"", time.Time{}, time.Time{}, 0, false},
		{"30m", now.Add(-30 * time.Minute), time.Time{}, 30 * time.Minute, false},
		{"2h", now.Add(-2 * time.Hour), time.Time{}, 2 * time.Hour, false},
		{"90s", now.Add(-90 * time.Second), time.Time{}, 90 * time.Second, false},
		{"10:00", at(11, 10, 0, 0), time.Time{}, 0, false},
		{"10:00-11:00", at(11, 10, 0, 0), at(11, 11, 0, 0), 0, false},
		{"9:05-10:05", at(11, 9, 5, 0), at(11, 10, 5, 0), 0, false},
		{"10:00:30-11:00:30", at(11, 10, 0, 30), at(11, 11, 0, 30), 0, false},
		{"10:00-", at(11, 10, 0, 0), time.Time{}, 0, false},
		{"10:00~11:00", at(11, 10, 0, 0), at(11, 11, 0, 0), 0, false},
		// 起点比现在还晚: 锚到昨天; 终点落在昨天, 不受now影响。
		{"23:00-01:00", at(10, 23, 0, 0), at(11, 1, 0, 0), 0, false},
		// 超过回放窗口的时长: 起点夹到前一日 00:00。
		{"100d", eventReplayFrom(now), time.Time{}, 100 * 24 * time.Hour, false},
		{"abc", time.Time{}, time.Time{}, 0, true},
		{"25:00-26:00", time.Time{}, time.Time{}, 0, true},
		{"10:00-11", time.Time{}, time.Time{}, 0, true},
	}
	for _, c := range cases {
		from, to, slide, errMsg := parseEvRange(c.in, now)
		if c.err {
			if errMsg == "" {
				t.Errorf("parseEvRange(%q) 应报写法错误", c.in)
			}
			continue
		}
		if errMsg != "" {
			t.Errorf("parseEvRange(%q) 不该报错: %s", c.in, errMsg)
		}
		if !from.Equal(c.from) || !to.Equal(c.to) || slide != c.slide {
			t.Errorf("parseEvRange(%q) = (%v, %v, %v), 期望 (%v, %v, %v)",
				c.in, from, to, slide, c.from, c.to, c.slide)
		}
	}

	// 终点还没到: 按 "到现在" 处理, 持续跟进。
	early := time.Date(2026, 11, 11, 10, 30, 0, 0, time.Local)
	from, to, _, errMsg := parseEvRange("10:00-11:00", early)
	if errMsg != "" || !from.Equal(at(11, 10, 0, 0)) || !to.IsZero() {
		t.Errorf("终点在未来时应跟到现在, 得到 from=%v to=%v err=%q", from, to, errMsg)
	}
}

// matchText: 编号 / 逻辑名 / 事件任一命中就保留, 不区分大小写; req=0 不参与编号匹配。
func TestEvFilterMatchText(t *testing.T) {
	f := evFilter{}
	cases := []struct {
		q    string
		r    routeEvent
		want bool
	}{
		{"12", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → ok(200)"}, true},
		{"gpt", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → ok(200)"}, true},
		{"ok(200)", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → ok(200)"}, true},
		{"FAIL", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → fail(502)"}, true},
		{"", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → ok(200)"}, true},
		{"0", routeEvent{Req: 0, Msg: "lock(quota)"}, false},
		{"zzz", routeEvent{Req: 12, Logical: "gpt-5", Msg: "send → ok(200)"}, false},
	}
	for _, c := range cases {
		f.setText(c.q)
		if got := f.matchText(c.r); got != c.want {
			t.Errorf("matchText(q=%q, %+v) = %v, 期望 %v", c.q, c.r, got, c.want)
		}
	}
}

// mergedEventRows: 文本筛选叠在合并后的链上; 时间范围按原始时刻先筛, 两档可叠加。
func TestMergedEventRowsFilter(t *testing.T) {
	m := newTUIModel("cfg")
	base := time.Now()
	m.events = []routeEvent{
		{Ts: base, Req: 1, Logical: "alpha", Kind: "stage", Msg: "new(alpha) → sem(u1)"},
		{Ts: base.Add(-time.Minute), Req: 2, Logical: "beta", Kind: "error", Msg: "send → fail(502)"},
		{Ts: base.Add(-2 * time.Minute), Req: 0, Kind: "lock", Upstream: "u1", Msg: "lock(quota 05:00)"},
	}

	m.ev.setText("fail")
	if rows := m.mergedEventRows(); len(rows) != 1 || rows[0].Req != 2 {
		t.Fatalf("文本筛选应只留 req=2, 得到 %+v", rows)
	}

	m.ev.setText("alpha")
	if rows := m.mergedEventRows(); len(rows) != 1 || rows[0].Req != 1 {
		t.Fatalf("逻辑名命中应只留 req=1, 得到 %+v", rows)
	}
	m.ev.setText("")
	m.ev.from = base.Add(-90 * time.Second)
	if rows := m.mergedEventRows(); len(rows) != 2 {
		t.Fatalf("时间范围应只留前两条, 得到 %+v", rows)
	}
	m.ev.setText("fail")
	if rows := m.mergedEventRows(); len(rows) != 1 || rows[0].Req != 2 {
		t.Fatalf("时间 + 文本叠加应只留 req=2, 得到 %+v", rows)
	}
	m.ev.clear()
	if rows := m.mergedEventRows(); len(rows) != 3 {
		t.Fatalf("清除筛选后应恢复 3 条, 得到 %+v", rows)
	}
}

// 时间范围生效时从日志回读, 结果不受 256 条上限约束; 再扫一遍不重复;
// 文本筛选在回读结果上继续生效。
func TestEventRangeOverridesKeep(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "requests.jsonl")
	now := time.Now()
	wanted := now.Add(-5 * time.Minute)
	var buf strings.Builder
	for i := 1; i <= 300; i++ {
		rec := logRec{Ts: wanted.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
			Req: uint64(i), Logical: "m1", Upstream: "u1", Model: "real", Status: 500}
		js, _ := json.Marshal(rec)
		buf.Write(js)
		buf.WriteByte('\n')
	}
	// 范围外一条: 不该进结果。
	old := logRec{Ts: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), Req: 999, Logical: "m1",
		Upstream: "u1", Model: "real", Status: 500}
	js, _ := json.Marshal(old)
	buf.Write(js)
	buf.WriteByte('\n')
	if err := os.WriteFile(cur, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel("cfg")
	m.logPath = cur
	// 时间范围生效时页面只认日志回读结果, 内存里的事件环 (256 条) 不再混进来。
	m.addEvent(routeEvent{Ts: now.Add(-6 * time.Minute), Req: 888, Logical: "m1", Msg: "lock(quota)"})
	m.ev.from, m.ev.to = now.Add(-10*time.Minute), now.Add(-time.Minute)
	m.ev.resetScan()
	m.pollEventFilter()
	if len(m.ev.rows) != 300 {
		t.Fatalf("时间范围应回读 300 条 (覆盖 256 上限), 得到 %d", len(m.ev.rows))
	}
	m.pollEventFilter()
	if len(m.ev.rows) != 300 {
		t.Fatalf("重复回读不该加条, 得到 %d", len(m.ev.rows))
	}
	if rows := m.mergedEventRows(); len(rows) != 300 {
		t.Fatalf("显示行应 300 条, 得到 %d", len(rows))
	}

	m.ev.setText("299")
	rows := m.mergedEventRows()
	if len(rows) != 1 || rows[0].Req != 299 {
		t.Fatalf("按编号筛选应只留 req=299, 得到 %+v", rows)
	}
}

// f / t 打开输入框, 回车提交, Esc 取消, c 清除; 其他页不受影响。
func TestEventFilterKeys(t *testing.T) {
	m := newTUIModel("cfg")
	m.tab = tabEvents
	var pending []byte

	// f: 输入 fail 后回车。
	m.handleKey('f', &pending)
	if m.evPrompt == nil {
		t.Fatal("f 应打开文本筛选输入框")
	}
	for _, b := range []byte("fail") {
		m.handleKey(b, &pending)
	}
	m.handleKey('\r', &pending)
	if m.evPrompt != nil || m.ev.text != "fail" {
		t.Fatalf("提交后应为 text=fail, 得到 prompt=%v text=%q", m.evPrompt != nil, m.ev.text)
	}

	// Esc 取消: 不改动已有筛选。
	m.handleKey('f', &pending)
	m.handleKey('x', &pending)
	m.handleKey(0x1b, &pending)
	if m.ev.text != "fail" {
		t.Fatalf("Esc 应取消输入, 得到 %q", m.ev.text)
	}

	// t: 相对时长。
	m.handleKey('t', &pending)
	for _, b := range []byte("30m") {
		m.handleKey(b, &pending)
	}
	m.handleKey('\r', &pending)
	if m.ev.slide != 30*time.Minute || m.ev.from.IsZero() {
		t.Fatalf("时间范围应为最近 30m, 得到 slide=%v from=%v", m.ev.slide, m.ev.from)
	}

	// c: 一次清掉文本与时间范围。
	m.handleKey('c', &pending)
	if m.ev.active() {
		t.Fatalf("c 应清除全部筛选, 得到 %+v", m.ev)
	}

	// 写法不对只提示, 范围不变。
	m.handleKey('t', &pending)
	for _, b := range []byte("abc") {
		m.handleKey(b, &pending)
	}
	m.handleKey('\r', &pending)
	if m.evMsg == "" || !m.ev.from.IsZero() {
		t.Fatalf("非法输入应提示且不动范围, 得到 msg=%q from=%v", m.evMsg, m.ev.from)
	}

	// 其他页: f / t / c 不拦截。
	m.tab = tabActive
	m.handleKey('f', &pending)
	if m.evPrompt != nil {
		t.Fatal("active 页的 f 不该打开筛选输入框")
	}
}

// 标题带上筛选条件与命中条数; 筛选没有命中时给出提示。
func TestDrawEventsFilterTitle(t *testing.T) {
	m := newTUIModel("cfg")
	m.events = []routeEvent{{Ts: time.Now(), Req: 7, Logical: "a", Msg: "send → fail(502)"}}
	m.ev.setText("fail")
	c := &tuiCanvas{cols: 120}
	m.drawEvents(c, 20)
	out := stripANSI(c.b.String())
	if !strings.Contains(out, `筛选 "fail"`) || !strings.Contains(out, "命中 1 条") {
		t.Errorf("标题应带筛选条件与命中数: %q", strings.SplitN(out, "\r\n", 2)[0])
	}

	m.events = nil
	m.ev.setText("zzz")
	c = &tuiCanvas{cols: 120}
	m.drawEvents(c, 20)
	if got := stripANSI(c.b.String()); !strings.Contains(got, "(筛选没有命中)") {
		t.Errorf("筛选无命中时应提示, 得到 %q", got)
	}
}
