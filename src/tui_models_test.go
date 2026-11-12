// tui_models_test.go: 第 1 页模型视角的回归 (拆分 modelRows 与绘制前的特征测试)。

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

// 模型表的格子按同一口径拼: Token = cached/total, 首字节 = 平均/谷值,
// 三种速度 = 平均/峰值; 熔断锁红、退避黄。
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
			DecodeRate: []tokPoint{{ts: now.Add(-time.Minute), tok: 500, dur: 1000}},
			TotalRate:  []tokPoint{{ts: now.Add(-time.Minute), tok: 300, dur: 2000}},
			ByteRate:   []tokPoint{{ts: now.Add(-time.Minute), tok: 5120, dur: 1000}},
			Last:       now,
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
	// Token 一格: cached/total。
	dec, decPeak, _ := rateShow(m.routes["a\x00ustc\x00m1"].DecodeRate, now, m.rateWin, m.window)
	avg, ok := ttfbAvg(m.routes["a\x00ustc\x00m1"].TtfbPts, now, m.rateWin)
	if !ok {
		t.Fatal("首字节样本应在窗口内")
	}
	for _, want := range []string{
		humanTokens(1000) + "/" + humanTokens(2000),
		durStr(time.Duration(avg)*time.Millisecond) + "/" +
			durStr(time.Duration(ttfbMin(m.routes["a\x00ustc\x00m1"].TtfbPts))*time.Millisecond),
		humanTPS(dec) + "/" + humanTPS(decPeak),
		humanByte(5120) + "/" + humanByte(5120),
		tuiYell + "8s" + tuiReset,
		tuiRed + "05:00" + tuiReset,
	} {
		if !strings.Contains(c.b.String(), want) {
			t.Errorf("缺少格子 %q:\n%s", want, out)
		}
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
