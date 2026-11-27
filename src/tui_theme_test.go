// tui_theme_test.go: settings 页的主题开关 —— 只切内存里的说明文字配色, 不写配置。

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 主题行 +/- 翻转说明文字色: 深色用亮黑 (90), 浅色用浅蓝 (94); 不标脏, 也不进配置。
func TestThemeToggleOnlyInMemory(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	t.Cleanup(func() { setTuiTheme(false) }) // 主题是包级状态: 用例之间不能漏

	e.gateSel = gateIdxTheme
	e.gateDirty = false
	if got := e.gateField(gateIdxTheme); got != 0 || themeCN() != "深色" {
		t.Fatalf("默认应是深色主题: field=%d name=%q", got, themeCN())
	}
	if tuiGray != tuiGrayByTheme[0] {
		t.Fatalf("深色主题的说明文字色应是亮黑: %q", tuiGray)
	}

	e.adjustGate(1)
	if !tuiLightTheme || tuiGray != tuiGrayByTheme[1] {
		t.Fatalf("+ 应切到浅色主题且说明文字色换成浅蓝: light=%v c=%q", tuiLightTheme, tuiGray)
	}
	if got := e.gateField(gateIdxTheme); got != 1 {
		t.Errorf("浅色主题的行值应为 1, 得到 %d", got)
	}
	if e.gateDirty {
		t.Error("主题切换不该标脏 (它不写配置)")
	}
	if !strings.Contains(e.message, "浅色") || !strings.Contains(e.message, "不写配置") {
		t.Errorf("切换后应提示只在内存: %q", e.message)
	}

	e.adjustGate(-1)
	if tuiLightTheme || tuiGray != tuiGrayByTheme[0] || themeCN() != "深色" {
		t.Fatalf("- 应切回深色主题: light=%v c=%q", tuiLightTheme, tuiGray)
	}

	// = 输入不接主题: 只提示用 +/-。
	e.message = ""
	e.setGatePrompt(gateIdxTheme, "浅色")
	if !strings.Contains(e.message, "+/-") || tuiLightTheme {
		t.Errorf("= 输入应只提示用 +/- 且不改主题: %q", e.message)
	}
	// 按键路径同理: 不开输入态。
	e.focus, e.gateSel = 2, gateIdxTheme
	e.prompt, e.message = nil, ""
	e.key('=')
	if e.prompt != nil || !strings.Contains(e.message, "+/-") {
		t.Errorf("主题行按 = 应直接提示用 +/-, 不开输入态: prompt=%v msg=%q", e.prompt, e.message)
	}

	// 保存调度参数时不该把主题写进配置。
	setTuiTheme(true)
	if err := e.saveGate(); err != nil {
		t.Fatalf("保存调度参数失败: %v", err)
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["theme"]; ok {
		t.Error("配置里不该出现 theme 键 (主题只在内存)")
	}
	if !tuiLightTheme {
		t.Error("保存不该动内存里的主题")
	}
}

// 主题不依赖配置: 配置读不到 (编辑器不可用) 时也能切, 因为主题分支在 usable 检查之前。
func TestThemeToggleWithoutConfig(t *testing.T) {
	t.Cleanup(func() { setTuiTheme(false) })
	e := &tuiEditor{focus: 2, gateSel: gateIdxTheme}
	e.adjust(1)
	if !tuiLightTheme {
		t.Fatal("配置读不到时也应能切主题")
	}
}

// 主题行可导航 (末行, j 走到它, G 停在它上面), 并画成全局参数区最后一行的 深色/浅色。
func TestThemeRowNavAndDraw(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.cols = 100
	m.prio = newTuiEditor(cfg)
	t.Cleanup(func() { setTuiTheme(false) })
	e := m.prio

	e.focus = 2
	e.gateSel = gateIdxAbort
	e.moveFocus(1)
	if e.gateSel != gateIdxTheme {
		t.Fatalf("j 应走到主题行 (%d), 得到 %d", gateIdxTheme, e.gateSel)
	}
	e.moveFocus(1)
	if e.gateSel != gateIdxTheme {
		t.Errorf("主题行是末行, 再按 j 应停住, 得到 %d", e.gateSel)
	}
	e.jump(1 << 30)
	if e.gateSel != gateIdxTheme {
		t.Errorf("G 应停在主题行, 得到 %d", e.gateSel)
	}

	lines := m.settingsGateLines(e, 40)
	last := stripANSI(lines[len(lines)-1])
	if !strings.HasPrefix(last, "  主题: ") || !strings.Contains(last, "深色") {
		t.Errorf("全局参数末行应是主题行 (深色): %q", last)
	}
	setTuiTheme(true)
	lines = m.settingsGateLines(e, 40)
	if last = stripANSI(lines[len(lines)-1]); !strings.Contains(last, "浅色") {
		t.Errorf("切成浅色后主题行应显示浅色: %q", last)
	}

	// 说明文字色跟着主题走: 顶栏的版本号用的就是 tuiGray。
	c := &tuiCanvas{cols: m.cols}
	m.drawHeader(c)
	if !strings.Contains(c.b.String(), tuiGrayByTheme[1]) {
		t.Error("浅色主题下画出的说明文字应换成浅蓝")
	}
}
