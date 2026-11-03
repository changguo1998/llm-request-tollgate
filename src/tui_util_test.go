package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// move 滚动当前页: 累加, 不小于 0, 且有上限 (真正的上限留给下一次 draw 按行数夹紧)。
func TestMove(t *testing.T) {
	m := newTUIModel("cfg")
	m.tab = tabActive
	m.move(3)
	if m.scroll[tabActive] != 3 {
		t.Errorf("应滚到 3, 得到 %d", m.scroll[tabActive])
	}
	m.move(-5)
	if m.scroll[tabActive] != 0 {
		t.Errorf("向上滚不能小于 0, 得到 %d", m.scroll[tabActive])
	}
	m.move(1 << 21)
	if m.scroll[tabActive] != 1<<20 {
		t.Errorf("滚动位置应有上限 1<<20, 得到 %d", m.scroll[tabActive])
	}
	// 每页各算各的: 滚动 active 页不动 settings 页。
	m.tab = tabSettings
	m.move(2)
	if m.scroll[tabActive] != 1<<20 || m.scroll[tabSettings] != 2 {
		t.Errorf("各页滚动位置应互相独立: %v", m.scroll)
	}
}

// 状态文件与被观测服务的日志同目录 (var/); 日志路径为空时没有状态文件。
func TestStatePaths(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.jsonl")
	if got, want := pulsePathFor(logPath), filepath.Join(dir, "tui-pulse.json"); got != want {
		t.Errorf("pulse 路径应为 %q, 得到 %q", want, got)
	}
	if got, want := gatePathFor(logPath), filepath.Join(dir, "gate-state.json"); got != want {
		t.Errorf("gate 路径应为 %q, 得到 %q", want, got)
	}
	if pulsePathFor("") != "" || gatePathFor("") != "" {
		t.Error("日志路径为空时不该给出状态文件路径")
	}
}

// App.logPath 读配置里的日志路径; 没配置时为空串。
func TestAppLogPath(t *testing.T) {
	if got := (&App{}).logPath(); got != "" {
		t.Errorf("没有配置时应为空串, 得到 %q", got)
	}
	a := &App{cfg: &Config{LogPath: "var/requests.jsonl"}}
	if got := a.logPath(); got != "var/requests.jsonl" {
		t.Errorf("应返回配置里的路径, 得到 %q", got)
	}
}

// humanTokens: 千 / 百万按一位小数缩写, 更小的原样输出。
func TestHumanTokens(t *testing.T) {
	cases := map[int]string{
		0: "0", 999: "999", 1000: "1.0k", 1500: "1.5k",
		999_999: "1000.0k", 1_000_000: "1.0M", 2_345_678: "2.3M",
	}
	for in, want := range cases {
		if got := humanTokens(in); got != want {
			t.Errorf("%d 应写作 %q, 得到 %q", in, want, got)
		}
	}
}

// eventColor: 锁 / 错误红, 等待类黄, 成功绿, 阶段事件不染色, 其余暗色。
func TestEventColor(t *testing.T) {
	cases := map[string]string{
		"error": tuiRed, "lock": tuiRed,
		"fallback": tuiYell, "queue": tuiYell, "gate": tuiYell,
		"ok": tuiGreen, "unlock": tuiGreen,
		"stage": "", "probe": tuiDim, "start": tuiDim, "自定义": tuiDim,
	}
	for kind, want := range cases {
		if got := eventColor(kind); got != want {
			t.Errorf("%q 的颜色应为 %q, 得到 %q", kind, want, got)
		}
	}
}

// eventWho: 请求写 编号/逻辑名, 日志回放写 -/逻辑名, 模型写 供应商/模型,
// 供应商写 供应商, 都没有就写 -。
func TestEventWho(t *testing.T) {
	m := newTUIModel("cfg")
	cases := []struct {
		e    routeEvent
		want string
	}{
		{routeEvent{Req: 12, Logical: "gpt-5"}, "12/gpt-5"},
		{routeEvent{Req: 12}, "12"},
		{routeEvent{Logical: "gpt-5"}, "-/gpt-5"},
		{routeEvent{Upstream: "ustc", Model: "m1"}, "ustc/m1"},
		{routeEvent{Upstream: "ustc"}, "ustc"},
		{routeEvent{}, "-"},
	}
	for _, c := range cases {
		if got := m.eventWho(c.e); got != c.want {
			t.Errorf("%+v 的标识应为 %q, 得到 %q", c.e, c.want, got)
		}
	}
}

// segStr: 一分钟以内按 0.1s 四舍五入 (整数写整秒), 更长交给 durStr 的 mm:ss / hh:mm:ss。
func TestSegStr(t *testing.T) {
	cases := map[float64]string{
		0:     "0s",
		0.04:  "0s",
		0.06:  "0.1s",
		2.84:  "2.8s",
		3.0:   "3s",
		59.96: "60s",
		60:    "01:00",
		3661:  "01:01:01",
		-1:    "0s",
	}
	for in, want := range cases {
		if got := segStr(in); got != want {
			t.Errorf("segStr(%v) 应为 %q, 得到 %q", in, want, got)
		}
	}
}

// gaussWeight: 参看时刻权重 1, 停留 t0 那么久权重 0.8, 越远越轻; t0 无效时权重恒为 1。
func TestGaussWeight(t *testing.T) {
	ref := time.Now()
	tau := 10 * time.Second
	if got := gaussWeight(ref, ref, tau); got != 1 {
		t.Errorf("同时刻权重应为 1, 得到 %v", got)
	}
	if got := gaussWeight(ref.Add(-tau), ref, tau); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("t0 处的权重应为 0.8, 得到 %v", got)
	}
	if got := gaussWeight(ref.Add(-2*tau), ref, tau); math.Abs(got-0.4096) > 1e-9 {
		t.Errorf("2 倍 t0 处权重应为 0.8⁴ = 0.4096, 得到 %v", got)
	}
	if got, far := gaussWeight(ref.Add(-10*tau), ref, tau), gaussWeight(ref.Add(-2*tau), ref, tau); got >= far {
		t.Errorf("越远的样本权重应越小: %v >= %v", got, far)
	}
	if got := gaussWeight(ref.Add(-time.Hour), ref, 0); got != 1 {
		t.Errorf("t0 无效时权重应为 1, 得到 %v", got)
	}
}

// rowTau: 平均耗时按全部能读到的请求算; 还没有请求时用缺省档。
func TestRowTau(t *testing.T) {
	m := newTUIModel("cfg")
	if got := m.rowTau(); got != rowStatTau0 {
		t.Errorf("没有请求时应取缺省 t0 (%v), 得到 %v", rowStatTau0, got)
	}
	m.durSum, m.durN = 35000, 1 // 毫秒合计 / 笔数
	if got := m.rowTau(); got != 35*time.Second {
		t.Errorf("平均耗时应为 35s, 得到 %v", got)
	}
}
