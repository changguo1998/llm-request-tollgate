// tui_windows_test.go: 扁平窗口配置 (rate_window_ms / block_window_ms) 相关测试。

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
	if err := os.WriteFile(p, []byte(`{"rate_window_sec":7200,"block_window_sec":10}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rate, block := tuiWindowConfig(p)
	if rate != 2*time.Hour || block != 10*time.Second {
		t.Fatalf("rate=%v block=%v, want 2h/10s", rate, block)
	}

	p2 := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(p2, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rate2, block2 := tuiWindowConfig(p2)
	if rate2 != time.Hour || block2 != 35*time.Second {
		t.Fatalf("缺省 rate=%v block=%v, want 1h/35s", rate2, block2)
	}
}

// loadConfig 给扁平顶层窗口键 (秒) 设缺省 (3600s / 35s), 也认显式值。
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
	if c.RateWindowSec != 3600 || c.BlockWindowSec != 35 {
		t.Fatalf("缺省 rate=%d block=%d, want 3600/35", c.RateWindowSec, c.BlockWindowSec)
	}

	p2 := filepath.Join(dir, "c2.json")
	if err := os.WriteFile(p2, []byte(`{"rate_window_sec":7200,"block_window_sec":10}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c2, err := loadConfig(p2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.RateWindowSec != 7200 || c2.BlockWindowSec != 10 {
		t.Fatalf("显式 rate=%d block=%d, want 7200/10", c2.RateWindowSec, c2.BlockWindowSec)
	}
}

// 聚合: 日志带 decode_ms / bytes 时, 解码与字节速率各记样本 (TUI 模型路由表三列的依据)。
func TestModelRowsDecodeAndByteRates(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	// completion=100, decode=500ms -> 200 t/s; bytes=2000, duration=2000ms -> 1000 B/s
	line := fmt.Sprintf(`{"ts":%q,"logical":"m1","upstream":"up1","model":"real-m1","path":"/v1/chat/completions","duration_ms":2000,"decode_ms":500,"bytes":2000,"usage":{"prompt":10,"completion":100,"total":110}}`,
		now.Format(time.RFC3339Nano))
	m.absorb([]byte(line))
	rows := m.modelRows()
	if len(rows) != 1 {
		t.Fatalf("应有一行, 得到 %d", len(rows))
	}
	r := rows[0]
	davg, dpeak, dok := tokRate(r.DecodeRate, now.Add(time.Minute), time.Hour)
	if !dok || davg != 200 || dpeak != 200 {
		t.Errorf("解码速率应 avg/peak=200 t/s, 得到 (%v,%v,%v)", davg, dpeak, dok)
	}
	bavg, bpeak, bok := tokRate(r.ByteRate, now.Add(time.Minute), time.Hour)
	if !bok || bavg != 1000 || bpeak != 1000 {
		t.Errorf("字节速率应 avg/peak=1000 B/s, 得到 (%v,%v,%v)", bavg, bpeak, bok)
	}
}

// settings 页编辑两个扁平窗口项 (+/-), 保存写回配置顶层键。
func TestTUISettingsWindowsEditAndSave(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	e.gateSel = 12 // rate_window_sec (gateLabels 下标)
	e.adjustGate(1)
	if e.rateWinMs != 3600000+600000 {
		t.Fatalf("rate +10min 应到 %d, 得到 %d", 3600000+600000, e.rateWinMs)
	}
	e.gateSel = 13
	e.adjustGate(1)
	if e.blockWinMs != 35000+5000 {
		t.Fatalf("block +5s 应到 %d, 得到 %d", 35000+5000, e.blockWinMs)
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
		Rate  int `json:"rate_window_sec"`
		Block int `json:"block_window_sec"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Rate != 4200 || got.Block != 40 {
		t.Fatalf("配置应写回 rate=%d block=%d, want 4200/40 (秒)", got.Rate, got.Block)
	}
}

// colKeep: 整列全是 0 或 - 的列被收掉, 任一格有值则保留。
func TestColKeepDropsAllZeroAndDash(t *testing.T) {
	cols := []tuiCol{{title: "排队"}, {title: "上游/模型"}, {title: "连错"}}
	rows := [][]string{
		{"0", "ustc", "-"},
		{"0", "group", "5"},
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
