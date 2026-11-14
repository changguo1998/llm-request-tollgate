// tui_gate_edit_test.go: settings 页调度参数编辑的回归 (拆分 adjustGate / setGatePrompt 前).

package main

import (
	"strings"
	"testing"
)

// +/- 调整每个参数都夹在各自的取值范围内; 退避序列只提示用 = 输入。
func TestAdjustGateBounds(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	bounds := map[int][2]int{
		0: {100, 60000}, 1: {100, 600000}, 2: {100, 600000},
		4: {1000, 86400000}, 5: {0, 1},
		6: {1000, 86400000}, 7: {1000, 86400000}, 8: {1000, 86400000},
		9: {1000, 86400000}, 10: {1000, 86400000},
		11: {1, 100000}, 12: {1, 100000}, 13: {1, 100},
		14: {0, 86400000},
	}
	for i, b := range bounds {
		e.gateSel = i
		e.gateDirty = false
		for k := 0; k < 200000; k++ {
			e.adjustGate(-1)
		}
		if got := e.gateField(i); got != b[0] {
			t.Errorf("第 %d 项下界应为 %d, 得到 %d", i, b[0], got)
		}
		for k := 0; k < 200000; k++ {
			e.adjustGate(1)
		}
		if got := e.gateField(i); got != b[1] {
			t.Errorf("第 %d 项上界应为 %d, 得到 %d", i, b[1], got)
		}
		if !e.gateDirty || !strings.Contains(e.message, gateCN[i]) {
			t.Errorf("调整后应标脏并写消息: dirty=%v msg=%q", e.gateDirty, e.message)
		}
	}

	e.gateSel = gateIdxRetry
	e.message = ""
	e.adjustGate(1)
	if e.message != "退避序列用 = 按逗号输入" {
		t.Errorf("退避序列应提示用 = 输入: %q", e.message)
	}
}

// = 输入按字段类型校验: 坏输入只写消息不改值, 好输入写回并标脏。
func TestSetGatePromptInvalid(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	before := e.gateField(0)
	e.gateDirty = false

	cases := []struct {
		i, wantVal int
		text, msg  string
	}{
		{gateIdxRetry, -1, "", "退避序列不能为空"},
		{gateIdxRetry, -1, "1,x", "退避序列要逗号分隔的正秒数 (可小数)"},
		{gateIdxProbe, -1, "maybe", "probe_enabled 要用 0/1 / on/off / 开/关"},
		{0, before, "abc", "出队间隔 要非负秒数 (可小数)"},
		{gateIdxPeak, -1, "0", "低吞吐峰值 要正整数"},
		{gateIdxRel, -1, "0", "低吞吐历史比 要 1-100 的整数百分比"},
	}
	for _, c := range cases {
		e.gateDirty = false
		e.setGatePrompt(c.i, c.text)
		if !strings.Contains(e.message, c.msg) {
			t.Errorf("输入 %q 应提示 %q, 得到 %q", c.text, c.msg, e.message)
		}
		if e.gateDirty {
			t.Errorf("坏输入 %q 不该标脏", c.text)
		}
		if c.wantVal >= 0 && e.gateField(c.i) != c.wantVal {
			t.Errorf("坏输入 %q 不该改值: %d -> %d", c.text, c.wantVal, e.gateField(c.i))
		}
	}

	e.setGatePrompt(gateIdxRetry, "1, 2.5")
	if got := e.gate.RetryBackoffMs; len(got) != 2 || got[0] != 1000 || got[1] != 2500 {
		t.Errorf("退避序列应解析成 [1000 2500] ms, 得到 %v", got)
	}
	e.setGatePrompt(gateIdxProbe, "off")
	if e.probeOn {
		t.Error("off 应关闭恢复探测")
	}
	e.setGatePrompt(0, "0.5")
	if e.gate.IntervalMs != 500 {
		t.Errorf("0.5s 应写成 500ms, 得到 %d", e.gate.IntervalMs)
	}
	e.setGatePrompt(gateIdxPeak, "7")
	if e.gate.LowPerfPeakMax != 7 {
		t.Errorf("峰值门槛应为 7, 得到 %d", e.gate.LowPerfPeakMax)
	}
	e.setGatePrompt(gateIdxRel, "35")
	if e.gate.LowPerfRelMax != 0.35 {
		t.Errorf("历史比门槛应为 0.35, 得到 %v", e.gate.LowPerfRelMax)
	}
}

// block_window_sec 已清理: gate 编辑列表不再含它, 长度与中文名一一对应。
func TestGateLabelsNoBlockWindow(t *testing.T) {
	for _, k := range gateLabels {
		if strings.Contains(k, "block_window") {
			t.Errorf("gateLabels 不应含 block_window 键: %q", k)
		}
	}
	if len(gateLabels) != 15 || len(gateCN) != len(gateLabels) {
		t.Fatalf("gateLabels 应为 15 项且与 gateCN 等长, 实际 %d/%d", len(gateLabels), len(gateCN))
	}
}
