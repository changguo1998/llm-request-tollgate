// tui_settingsdraw_test.go: settings 页三列布局与消息行的回归 (拆分绘制的特征测试)。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 供应商列的高度 = 内容区去掉参数区与固定留白, 超出的供应商不画; 参数行与模型列照常。
func TestDrawSettingsProviderCap(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.cols = 100
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	e := m.prio
	if !e.usable() {
		t.Fatalf("fixture 应可编辑: %q", e.err)
	}
	e.providers = nil
	for i := 0; i < 10; i++ {
		e.providers = append(e.providers, "p"+strconv.Itoa(i))
	}
	const body = 30
	c := &tuiCanvas{cols: m.cols}
	m.drawSettings(c, body)
	plain := stripANSI(c.b.String())

	// provH = min(provider 数, 内容区 - 参数区 (标题 + 16 行) - 7)。
	want := min(10, max(1, body-(1+len(gateLabels))-7))
	if got := strings.Count(plain, "priority="); got != want {
		t.Fatalf("供应商行数应为 %d, 得到 %d:\n%s", want, got, plain)
	}
	for _, hidden := range []string{"p9", "p6"} {
		if strings.Contains(plain, hidden+"  priority=") {
			t.Errorf("超出高度的供应商 %s 不该画:\n%s", hidden, plain)
		}
	}
	if !strings.Contains(plain, "p0  priority=") {
		t.Errorf("第一个供应商应画出来:\n%s", plain)
	}
	if n := strings.Count(strings.TrimSuffix(c.b.String(), "\r\n"), "\r\n") + 1; n != body {
		t.Errorf("内容区应恰好 %d 行, 得到 %d", body, n)
	}
}

// 模型列表滚动按显示行 (含分组标题) 算两栏: 选中最后一行时窗口跟着走,
// 两栏连续铺满, 前面的模型移出画面。
func TestDrawSettingsScrollPinsSelection(t *testing.T) {
	_, cfg := settingsManyModels(t, 12)
	m := newTUIModel(cfg)
	m.cols = 100
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	e := m.prio
	if len(e.filtered()) != 12 {
		t.Fatalf("应有 12 个模型, 得到 %d", len(e.filtered()))
	}
	e.modelSel = 11
	c := &tuiCanvas{cols: m.cols}
	m.drawSettings(c, 6) // window = body-2 = 4, 两栏共 8 行
	if e.vis != 4 {
		t.Fatalf("每栏应显示 4 行, 得到 %d", e.vis)
	}
	// 显示行 = 1 个分组标题 + 12 个模型 = 13; 选中第 12 行 (下标 12),
	// span=8 -> scrollTop = 12-8+1 = 5 (第 5 行 = 模型 m04)。
	if e.scrollTop != 5 {
		t.Fatalf("滚动位置应钉住选中行 (5), 得到 %d", e.scrollTop)
	}
	out := stripANSI(strings.TrimSuffix(c.b.String(), "\r\n"))
	for _, gone := range []string{"m00", "m01", "m02", "m03"} {
		if strings.Contains(out, "/"+gone) {
			t.Errorf("已滚出窗口的 %s 不该出现:\n%s", gone, out)
		}
	}
	for i := 4; i < 12; i++ {
		id := "m0" + strconv.Itoa(i)
		if i >= 10 {
			id = "m" + strconv.Itoa(i)
		}
		if !strings.Contains(out, "/"+id) {
			t.Errorf("两栏应连续铺满, 缺少 %s:\n%s", id, out)
		}
	}
}

// 消息行: 输入态替换正文, 未保存标记与错误按顺序追加, 整行铺底并带背景色。
func TestPrioMessageParts(t *testing.T) {
	m := newTUIModel("cfg")
	m.cols = 80
	e := &tuiEditor{message: "就绪"}
	got := m.prioMessage(e)
	if !strings.HasPrefix(got, tuiRev) || !strings.HasSuffix(got, tuiReset) {
		t.Fatalf("整行应带反显与收尾序列: %q", got)
	}
	if w := tuiWidth(got); w != m.cols-1 {
		t.Errorf("消息行应补到 %d 列, 得到 %d", m.cols-1, w)
	}
	plain := stripANSI(got)

	e.dirty, e.gateDirty = true, true
	e.gateErr, e.err = "读不到", "保存失败"
	got = m.prioMessage(e)
	plain = stripANSI(got)
	order := []string{"就绪", "[优先级未保存]", "[调度参数未保存]", "配置读不到: 读不到", "保存失败"}
	at := 0
	for _, part := range order {
		i := strings.Index(plain[at:], part)
		if i < 0 {
			t.Fatalf("消息行应依次包含 %q:\n%s", part, plain)
		}
		at += i + len(part)
	}

	// 输入态: 用 label + buffer 取代 message, 背景换成编辑色, 未保存标记照旧。
	e.prompt = &tuiPrompt{label: "M", buffer: "3"}
	got = m.prioMessage(e)
	if !strings.HasPrefix(got, tuiEditBg) {
		t.Errorf("输入态应用编辑底色: %q", got)
	}
	plain = stripANSI(got)
	if !strings.Contains(plain, "M 3") || strings.Contains(plain, "就绪") {
		t.Errorf("输入态应显示 label+buffer 而不是 message:\n%s", plain)
	}
}

// settingsManyModels 写一份含 n 个模型 (m00..) 的 store 与配置, 返回配置路径;
// 模型同属逻辑名 g, 顺序按模型 id。
func settingsManyModels(t *testing.T, n int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "m0" + strconv.Itoa(i)
		if i >= 10 {
			id = "m" + strconv.Itoa(i)
		}
		ids = append(ids, id)
	}
	storeModels := make([]map[string]any, 0, n)
	cfgModels := make([]map[string]any, 0, n)
	for _, id := range ids {
		storeModels = append(storeModels, map[string]any{"id": id})
		cfgModels = append(cfgModels, map[string]any{"id": id, "priority": 0, "alias": "g"})
	}
	store := map[string]any{"providers": map[string]any{
		"ustc": map[string]any{"baseUrl": "https://u.invalid/v1", "models": storeModels},
	}}
	cfgText := map[string]any{"listen": "127.0.0.1:8787", "upstreams": []any{
		map[string]any{"id": "ustc", "base_url": "https://u.invalid/v1", "priority": 0, "models": cfgModels},
	}}
	for _, it := range []struct {
		name string
		v    any
	}{{"llm-models.json", store}, {"tollgate.json", cfgText}} {
		b, err := json.Marshal(it.v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, it.name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "llm-models.json"), filepath.Join(dir, "tollgate.json")
}
