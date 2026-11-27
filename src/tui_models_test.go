// tui_models_test.go: 第 1 页模型视角的守成测试 (拆分 modelRows 与绘制前的特征测试)。

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 观测面里的队列状态按配置归属补到每个引用它的逻辑名组: 组展开后成员逻辑名与
// 组名各得一行, 都带同样的队列 / 连错状态; 不从配置候选铺开。
func TestModelRowsQueueForAllOwners(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{
	  "upstreams":[{"id":"u1","base_url":"http://x","models":[{"id":"m1","alias":"a"}]}],
	  "groups":{"ga":["a"]}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg)
	m.pulse.Cands = []candView{{
		ID: "u1", Models: []candModelLock{{Model: "m1", Queue: 2, Fails: 1}},
	}}
	rows := m.modelRows()
	if len(rows) != 2 {
		t.Fatalf("组名与成员逻辑名应各一行, 得到 %d 行: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.logical == "" || r.Queue != 2 || r.Fails != 1 {
			t.Errorf("每行都应归到逻辑名并带上队列状态: %+v", r)
		}
		seen[r.logical] = true
	}
	if !seen["a"] || !seen["ga"] {
		t.Errorf("应同时有成员逻辑名 a 与组名 ga: %v", seen)
	}
}

// 用过逻辑名的配置候选全部显示: 逻辑名最近用过 (窗口内有非空行) 时, 名下配置的
// 上游/模型 无论有没有进过队列都占一行 (未用行 blank 但 forced); 完全没用过的
// 逻辑名仍不出现 (标题与行都没有)。
func TestModelRowsExpandsUsedLogicalCandidates(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{
	  "upstreams":[
	    {"id":"u1","base_url":"http://x","models":[
	      {"id":"m-used","alias":"a"},{"id":"m-idle","alias":"a"}]},
	    {"id":"u2","base_url":"http://y","models":[
	      {"id":"m-twin","alias":"a"},{"id":"m-other","alias":"b"}]}
	  ]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg)
	now := time.Now()
	m.routes = map[string]*routeRow{
		"a\x00u1\x00m-used": {Logical: "a", Upstream: "u1", Model: "m-used",
			Reqs: 1, Last: now},
	}
	m.pulse.Cands = []candView{{ID: "u1", Models: []candModelLock{{Model: "m-used"}}}}
	rows := m.modelRows()
	if len(rows) != 3 {
		t.Fatalf("逻辑名 a 用过应展开全部 3 个候选, 得到 %d 行: %+v", len(rows), rows)
	}
	// 组内按配置候选链顺序 (与 settings 页面板同口径): u1/m-used 首选 ->
	// u1/m-idle -> u2/m-twin; 不用过的 m-used 靠前与它最近用过无关。
	wantOrder := []string{"u1/m-used", "u1/m-idle", "u2/m-twin"}
	for i, r := range rows {
		if got := r.up + "/" + r.model; got != wantOrder[i] {
			t.Fatalf("组内应按配置优先级排: 第 %d 行 %s != %s", i, got, wantOrder[i])
		}
	}
	seen := map[string]*modelRow{}
	for _, r := range rows {
		seen[r.up+"/"+r.model] = r
	}
	for _, upMod := range []string{"u1/m-used", "u1/m-idle", "u2/m-twin"} {
		r, ok := seen[upMod]
		if !ok {
			t.Errorf("缺少用过逻辑名 a 的候选行 %s: %v", upMod, seen)
			continue
		}
		if r.logical != "a" {
			t.Errorf("%s 应归到 a 组, 得到 logical=%q", upMod, r.logical)
		}
	}
	// 没进过队列的两个候选: blank 但 forced。
	for _, upMod := range []string{"u1/m-idle", "u2/m-twin"} {
		r := seen[upMod]
		if !r.forced || !r.blank() {
			t.Errorf("%s 应是 forced 空行: %+v", upMod, r)
		}
	}
	// 逻辑名 b 没用过: 它的候选不该出现。
	if _, ok := seen["u2/m-other"]; ok {
		t.Errorf("没用过的逻辑名 b 的候选不该出现: %v", seen)
	}
}

// 没用过的模型行: 统计格 (Token / 首字节 / 性能) 留空格对齐, 「最近」占位写 --:--;
// 用过行照常画数值 (含 0/ 与 ---)。
func TestModelTableUnusedRowPlaceholders(t *testing.T) {
	m := flowFixture(t)
	out := stripANSI(m.draw())
	lines := strings.Split(out, "\r\n")
	var unused, used string
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "u2/real-2b"):
			unused = ln // m2 的未用候选 (forced, 没有请求)
		case strings.Contains(ln, "u1/real-1"):
			used = ln // m1 的用过行
		}
	}
	if used == "" || unused == "" {
		t.Fatalf("应同时有已用与未用行:\n%s", out)
	}
	if !strings.Contains(unused, "--:--") {
		t.Errorf("未用行的「最近」应占位 --:--: %q", unused)
	}
	if strings.Contains(unused, "0/") || strings.Contains(unused, "---") {
		t.Errorf("未用行的统计格应留空 (空格), 不画 0/--- 假数据: %q", unused)
	}
	if !strings.Contains(used, "0/") || !strings.Contains(used, "---") {
		t.Errorf("用过行的统计格应照常画: %q", used)
	}
}

// 模型表统计四列 (Token / 首字节 / 性能 / 最近) 之间用边框竖线分隔; 组名行在
// 同样的列位也画竖线 (穿过分组标题, 不断开)。
func TestModelTableStatsBars(t *testing.T) {
	openTestPTY(t, 60, 140)
	m := flowFixture(t)
	lines := strings.Split(stripANSI(m.draw()), "\r\n")
	barCols := func(s string) []int {
		var idx []int
		for i, r := range s {
			if r == '│' {
				idx = append(idx, i)
			}
		}
		return idx
	}
	var dataIdx, groupIdx []int
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "u1/real-1"):
			if dataIdx == nil {
				dataIdx = barCols(ln)
			}
		case strings.HasPrefix(strings.TrimSpace(ln), "m1"): // 组名行 (没设粘性, 无倒计时)
			if groupIdx == nil {
				groupIdx = barCols(ln)
			}
		}
	}
	if dataIdx == nil || len(dataIdx) != 3 {
		t.Fatalf("数据行应有 3 条竖线 (Token/首字节/性能 之后): %v\n%s", dataIdx, strings.Join(lines, "\n"))
	}
	if groupIdx == nil || len(groupIdx) != 3 {
		t.Fatalf("组名行也应有 3 条竖线: %v\n%s", groupIdx, strings.Join(lines, "\n"))
	}
	for i := range dataIdx {
		if dataIdx[i] != groupIdx[i] {
			t.Errorf("组名行竖线列位应与数据行一致: data=%v group=%v", dataIdx, groupIdx)
		}
	}
}

// 模型表的格子按同一口径拼: Token = cached/total, 首字节 = 最近/短均/长均/谷 四段,
// 性能 = 最近/短均/长均/峰 四段 (每请求 r*, 无参数时退原始速率); 熔断锁红、退避黄。
func TestDrawModelsRouteCells(t *testing.T) {
	m := newTUIModel("cfg")
	m.cols = 200
	m.rateWin, m.window = time.Hour, 3*time.Hour
	now := time.Now()
	m.routes = map[string]*routeRow{
		"a\x00ustc\x00m1": {
			Logical: "a", Upstream: "ustc", Model: "m1", Reqs: 2,
			Cached: 1000, Total: 2000,
			TtfbPts: []ttfbPoint{{ts: now.Add(-5 * time.Minute), ms: 100},
				{ts: now.Add(-15 * time.Minute), ms: 300}},
			PerfPts: []perfPoint{{ts: now.Add(-time.Minute), c: 500, d: 1000, b: 2000}},
			Last:    now,
		},
	}
	m.pulse.Cands = []candView{{
		ID: "ustc",
		Models: []candModelLock{{
			Model: "m1", Queue: 2, PausedMs: 8000, LeftMs: 300000, Why: "breaker",
		}},
	}}
	c := &tuiCanvas{cols: m.cols}
	m.drawModelsBody(c)
	out := stripANSI(c.b.String())

	if !strings.Contains(out, "ustc/m1") {
		t.Fatalf("模型行没画出来:\n%s", out)
	}
	// Token / 首字节 / 性能三格各自的四段/斜杠串 (首字节与性能的统计在 cell 里现算)。
	gray := func(d string) string { return tuiGray + d + tuiReset }
	// 首字节每段 2 格: 1 位整数 (前景色) + 灰色十分位, 不带单位 (秒)。
	for _, want := range []string{
		humanTokens(1000) + "/" + humanTokens(2000),
		// 首字节四段 (空格分隔): 最近 100ms / 短均 200ms / 长均 200ms / 谷 100ms。
		ttfbNum(100) + " " + ttfbNum(200) + " " + ttfbNum(200) + " " + ttfbNum(100),
		// 性能四段: 单样本 (500 t/s, 无参数退原始速率) 四段同值; 整数 500 + 灰色 0 (500.0)。
		strings.Repeat("500"+gray("0")+" ", 3) + "500" + gray("0"),
		tuiYell + "8s" + tuiReset,
		tuiRed + "05:00" + tuiReset,
	} {
		if !strings.Contains(c.b.String(), want) {
			t.Errorf("缺少格子 %q:\n%s", want, out)
		}
	}
}

// 「最近」格: 组内最近一次服务的上游上绿; 有进行中请求的行值固定 0s, 同样上绿。
func TestModelsLastCellHighlight(t *testing.T) {
	now := time.Now()
	m := newTUIModel("cfg")
	m.cols = 200
	m.rateWin, m.window = time.Hour, 3*time.Hour
	m.routes = map[string]*routeRow{
		"a\x00ustc\x00m1": {Logical: "a", Upstream: "ustc", Model: "m1",
			Reqs: 1, Total: 10, Last: now.Add(-10 * time.Minute)},
		"a\x00ark\x00m2": {Logical: "a", Upstream: "ark", Model: "m2",
			Reqs: 1, Total: 10, Last: now.Add(-2 * time.Minute)},
	}
	m.pulse.Cands = []candView{
		{ID: "ustc", Models: []candModelLock{{Model: "m1"}}},
		{ID: "ark", Models: []candModelLock{{Model: "m2"}}},
	}
	c := &tuiCanvas{cols: m.cols}
	m.drawModelsBody(c)

	// 组内最近一次是 ark/m2 (Last 最晚): 它的「最近」格上绿; ustc/m1 不是, 不上绿。
	recent := tuiGreen + mages(now.Add(-2*time.Minute)) + tuiReset
	stale := tuiGreen + mages(now.Add(-10*time.Minute)) + tuiReset
	if !strings.Contains(c.b.String(), recent) {
		t.Errorf("最近一次服务的上游的「最近」格应上绿 %q:\n%s", recent, stripANSI(c.b.String()))
	}
	if strings.Contains(c.b.String(), stale) {
		t.Errorf("非最近一次的行不该上绿: %q", stale)
	}

	// ustc/m1 上有一笔进行中请求 (不是组内最近): 「最近」固定 0s 且上绿。
	m.pulse.Active = []flow{{Logical: "a", Up: "ustc", Model: "m1"}}
	c = &tuiCanvas{cols: m.cols}
	m.drawModelsBody(c)
	zero := tuiGreen + "0s" + tuiReset
	if !strings.Contains(c.b.String(), zero) {
		t.Errorf("进行中请求所在行「最近」应固定 0s 且上绿 %q:\n%s", zero, stripANSI(c.b.String()))
	}
	if !strings.Contains(c.b.String(), recent) {
		t.Errorf("组内最近一次的行仍应上绿 %q", recent)
	}
	// 进行中请求还没落到候选 (parse 阶段, Up 空): 不命中任何行, 「最近」照旧。
	m.pulse.Active = []flow{{Logical: "a", Stage: stageParse}}
	c = &tuiCanvas{cols: m.cols}
	m.drawModelsBody(c)
	if strings.Contains(c.b.String(), zero) {
		t.Errorf("候选未知的进行中请求不该把「最近」置 0")
	}
}

// 第 1 页上游表: 记账组列数取各行最大值, 组数少的行补空; 队列后面跟下一个发车时刻。
func TestDrawModelsUpstreamTable(t *testing.T) {
	m := newTUIModel("cfg")
	m.cols = 200
	m.pulse.Cands = []candView{
		{ID: "u1", InFlight: 2, SoftCap: 3, Queue: 3, NextMs: 12000,
			Groups: []candGroup{{Name: defaultGroup}, {Name: "g1"}, {Name: "g2"}},
			Models: []candModelLock{{Model: "m1"}}},
		{ID: "u2", InFlight: 1, SoftCap: 2, Probing: true,
			Groups: []candGroup{{Name: defaultGroup}}},
	}
	m.routes = map[string]*routeRow{
		"a\x00u1\x00m1": {Logical: "a", Upstream: "u1", Model: "m1", Reqs: 1, Last: time.Now()},
	}
	c := &tuiCanvas{cols: m.cols}
	m.drawModelsBody(c)
	out := stripANSI(c.b.String())
	for _, want := range []string{"u1", "u2", "3/12s", "g1", "g2", "记账"} {
		if !strings.Contains(out, want) {
			t.Errorf("上游表缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(c.b.String(), tuiYell+"(+1)"+tuiReset) {
		t.Errorf("试探中的上游应标 (+1)")
	}
	// 行按候选顺序 (sortedCands 已排好), u1 在前; u1 的单元格数 (5+2) 多于 u2 (5+0)。
	lines := strings.Split(out, "\r\n")
	var u1, u2 string
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		// 上游表的行以 " id " 开头 (id 列补到标题宽), 模型表行是 "*id/model" 或 " id/model"。
		if strings.HasPrefix(trimmed, "u1 ") {
			u1 = ln
		}
		if strings.HasPrefix(trimmed, "u2 ") {
			u2 = ln
		}
	}
	if u1 == "" || u2 == "" {
		t.Fatalf("两行都应画出来: %q / %q", u1, u2)
	}
	// 记账组列: 组多的一行列出 g1 / g2, 组少的一行不该多出这两列。
	if !strings.Contains(u1, "g1") || !strings.Contains(u1, "g2") {
		t.Errorf("u1 应列出两个命名组: %q", u1)
	}
	if strings.Contains(u2, "g1") || strings.Contains(u2, "g2") {
		t.Errorf("u2 只有一个默认组, 不该出现命名组: %q", u2)
	}
	if !strings.Contains(u2, "(+1)") {
		t.Errorf("u2 试探中应标 (+1): %q", u2)
	}
}

// 记账组格: 未锁绿组名, 锁上组名与倒计时同色且名字不被倒计时替换;
// 默认组平时写 -, 锁上时只写倒计时。
func TestGroupCellNamesAndLocks(t *testing.T) {
	cases := []struct {
		name string
		g    candGroup
		def  bool
		want string
	}{
		{"未锁命名组", candGroup{Name: "g1"}, false, tuiGreen + "g1" + tuiReset},
		{"锁上命名组", candGroup{Name: "g1", LockMs: 300000}, false, tuiRedBright + "g1 05:00" + tuiReset},
		{"未锁默认组", candGroup{Name: defaultGroup}, true, "-"},
		{"锁上默认组", candGroup{Name: defaultGroup, LockMs: 600000}, true, tuiRedBright + "10:00" + tuiReset},
	}
	for _, c := range cases {
		if got := groupCell(c.g, c.def); got != c.want {
			t.Errorf("%s: 期望 %q, 得到 %q", c.name, c.want, got)
		}
	}
}

// 性能窗 (短时) 内没有样本时, 对应段显示 ---, 最近 / 长均 / 峰(谷) 仍照算 (除零防护)。
func TestTtfbPerfCellEmptyShortWindow(t *testing.T) {
	now := time.Now()
	ts := []ttfbPoint{{ts: now.Add(-30 * time.Minute), ms: 400}}
	one := tuiGray + "400" + tuiReset // 0.4 秒 (400ms): 不足 1 秒, 3 位灰色小数全灰
	if got := ttfbCell(ts, now, 5*time.Minute, 3*time.Hour); got != one+" "+ttfbDash+" "+one+" "+one {
		t.Errorf("首字节短窗空应显示 ---: %q", got)
	}
	ps := []perfPoint{{ts: now.Add(-30 * time.Minute), c: 100, d: 1000, b: 1000}}
	v := "100" + tuiGray + "0" + tuiReset // 100.0, 整数前景 + 灰色小数
	if got := perfCell(ps, now, 5*time.Minute, 3*time.Hour, "u", "m", nil); got != v+" "+cellDash+" "+v+" "+v {
		t.Errorf("性能短窗空应显示 --: %q", got)
	}
}

// ttfbNum: 单位恒为秒, 3 位有效数字, 整数前景色 + 灰色小数 (最多 3 位到毫秒); 不足 1 秒
// 时 3 位灰小数补 0; 1 到 99 秒按整数位数减小数位数凑满 3 格; 100 秒以上整数秒撑宽。
func TestTtfbNum(t *testing.T) {
	gray := func(d string) string { return tuiGray + d + tuiReset }
	cases := map[int64]string{
		0:         gray("000"),      // 0.000s
		5:         gray("005"),      // 0.005s
		123:       gray("123"),      // 0.123s 全灰
		400:       gray("400"),      // 0.400s
		999:       gray("999"),      // 0.999s
		1000:      "1" + gray("00"), // 1.00s
		1500:      "1" + gray("50"), // 1.50s
		1_050:     "1" + gray("05"), // 1.05s
		9_999:     "9" + gray("99"), // 9.99s
		12_300:    "12" + gray("3"), // 12.3s
		65_400:    "65" + gray("4"), // 65.4s
		99_900:    "99" + gray("9"), // 99.9s
		100_000:   "100",            // 100s 整数秒
		123_400:   "123",            // 123s, 3 格挤满丢小数
		1_234_000: "1234",           // 1234s, 撑宽
	}
	for in, want := range cases {
		if got := ttfbNum(in); got != want {
			t.Errorf("ttfbNum(%d) 应为 %q, 得到 %q", in, want, got)
		}
	}
}

// 四段统计的多样本: 老样本只在长窗时, 短均只算短窗内的部分; 最近/峰(谷) 按全部样本。
func TestPerfTtfbFourValueStats(t *testing.T) {
	now := time.Now()
	tt := []ttfbPoint{
		{ts: now.Add(-time.Minute), ms: 100},
		{ts: now.Add(-2 * time.Minute), ms: 200},
		{ts: now.Add(-10 * time.Minute), ms: 300}, // 短窗 (5min) 外, 长窗 (1h) 内
	}
	part := func(ms int64) string { return ttfbNum(ms) }
	// 秒为单位 (不带单位): 最近 100ms / 短均 150ms / 长均 200ms / 谷 100ms, 空格分隔。
	want := part(100) + " " + part(150) + " " + part(200) + " " + part(100)
	if got := ttfbCell(tt, now, 5*time.Minute, time.Hour); got != want {
		t.Errorf("首字节四段应为 最近100/短均150/长均200/谷100 ms (%s), 得到 %q", want, got)
	}
	ps := []perfPoint{
		{ts: now.Add(-time.Minute), c: 100, d: 1000, b: 1000},
		{ts: now.Add(-2 * time.Minute), c: 200, d: 1000, b: 1000},
		{ts: now.Add(-10 * time.Minute), c: 300, d: 1000, b: 1000},
	}
	gray := func(d string) string { return tuiGray + d + tuiReset }
	wantPerf := "100" + gray("0") + " " + "150" + gray("0") + " " + "200" + gray("0") + " " + "300" + gray("0")
	if got := perfCell(ps, now, 5*time.Minute, time.Hour, "u", "m", nil); got != wantPerf {
		t.Errorf("性能四段应为 最近100/短均150/长均200/峰300, 得到 %q", got)
	}
}
