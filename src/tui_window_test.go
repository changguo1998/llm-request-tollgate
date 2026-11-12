// tui_windows_test.go: 扁平窗口配置 (perf_window_sec) 相关测试。

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tuiWindowConfig: 从扁平顶层键 (秒) 读窗口; 读不到/非正数回退缺省。
func TestTUIWindowConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{"perf_window_sec":300}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rate := tuiWindowConfig(p)
	if rate != 300*time.Second {
		t.Fatalf("rate=%v, want 5min", rate)
	}

	p2 := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(p2, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rate2 := tuiWindowConfig(p2)
	if rate2 != 300*time.Second {
		t.Fatalf("缺省 rate=%v, want 5min", rate2)
	}
}

// loadConfig 给扁平顶层窗口键 (秒) 设缺省 (300s), 也认显式值。
func TestConfigWindowDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.PerfWindowSec != 300 {
		t.Fatalf("缺省 perf=%v, want 300", c.PerfWindowSec)
	}

	p2 := filepath.Join(dir, "c2.json")
	if err := os.WriteFile(p2, []byte(`{"perf_window_sec":300}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := loadConfig(p2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.PerfWindowSec != 300 {
		t.Fatalf("显式 perf=%v, want 300", c2.PerfWindowSec)
	}
}

// 聚合: 日志带 decode_ms + bytes + completion 时记一笔性能样本 (性能列依据)。
func TestModelRowsPerfSamples(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	// completion=100, decode=500ms, bytes=2000 -> 样本 (c=100, d=500, b=2000)
	line := fmt.Sprintf(`{"ts":%q,"logical":"m1","upstream":"up1","model":"real-m1","path":"/v1/chat/completions","duration_ms":2000,"decode_ms":500,"bytes":2000,"usage":{"prompt":10,"completion":100,"total":110}}`,
		now.Format(time.RFC3339Nano))
	m.absorb([]byte(line))
	rows := m.modelRows()
	if len(rows) != 1 {
		t.Fatalf("应有一行, 得到 %d", len(rows))
	}
	r := rows[0]
	if len(r.PerfPts) != 1 {
		t.Fatalf("应有 1 笔性能样本, 得到 %d", len(r.PerfPts))
	}
	p := r.PerfPts[0]
	if p.c != 100 || p.d != 500 || p.b != 2000 {
		t.Errorf("性能样本应为 (100,500,2000), 得到 %+v", p)
	}
}

// settings 页编辑性能窗 (+/-), 保存写回配置顶层键。
func TestTUISettingsWindowsEditAndSave(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	e.gateSel = 10 // perf_window_sec (gateLabels 下标)
	e.adjustGate(1)
	if e.gate.PerfWindowMs != 300000+60000 {
		t.Fatalf("性能窗 +1min 应到 %d, 得到 %d", 300000+60000, e.gate.PerfWindowMs)
	}
	if !e.gateDirty {
		t.Fatal("改了窗口应标记未保存")
	}
	if err := e.saveGate(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Perf int `json:"perf_window_sec"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Perf != 360 {
		t.Fatalf("配置应写回 perf=%d, want 360 (秒)", got.Perf)
	}
}

// settings 页编辑粘性失效窗口 (+/- 每次 1min), 保存写回配置顶层键 sticky_ttl_sec (秒)。
func TestTUISettingsStickyTtlEditAndSave(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	if e.gate.StickyTTLMs != 300000 {
		t.Fatalf("缺省粘性失效窗口应为 300000ms (5min), 实为 %d", e.gate.StickyTTLMs)
	}
	e.gateSel = 4 // sticky_ttl_sec (gateLabels 下标)
	e.adjustGate(1)
	if e.gate.StickyTTLMs != 300000+60000 {
		t.Fatalf("粘性失效窗口 +1 应到 %d, 实为 %d", 300000+60000, e.gate.StickyTTLMs)
	}
	if !e.gateDirty {
		t.Fatal("改了应标记未保存")
	}
	if err := e.saveGate(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sticky float64 `json:"sticky_ttl_sec"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sticky != 360 {
		t.Fatalf("配置应写回 sticky_ttl_sec=360 (秒), 实为 %v", got.Sticky)
	}
}

// colKeep: 整列全是 0 或 - 的列被收掉, 任一格有值则保留。
func TestColKeepDropsAllZeroAndDash(t *testing.T) {
	cols := []tuiCol{{title: "排队"}, {title: "上游/模型"}, {title: "连错"}}
	rows := [][]string{
		{"0", "ustc", "-"},
		{"0", "group", "-"},
	}
	keep := colKeep(cols, rows)
	if keep[0] || keep[2] {
		t.Errorf("全 0 / 全 - 的列应被收掉, 得到 keep=%v", keep)
	}
	if !keep[1] {
		t.Errorf("有值列(上游/模型)应保留")
	}

	rows2 := [][]string{{"3", "ustc", "-"}}
	keep2 := colKeep(cols, rows2)
	if !keep2[0] {
		t.Errorf("排队有非 0 值应保留")
	}
}
