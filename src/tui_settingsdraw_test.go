// tui_settingsdraw_test.go: settings 页三列布局与底部信息区的守成测试 (拆分绘制的特征测试)。

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	e.message, e.msgAt = "", time.Time{} // 这条用例直接量网格行数, 先清掉装载时的瞬时说明
	e.providers = nil
	for i := 0; i < 10; i++ {
		e.providers = append(e.providers, "p"+strconv.Itoa(i))
	}
	const body = 30
	c := &tuiCanvas{cols: m.cols}
	m.drawSettings(c, body)
	plain := stripANSI(c.b.String())

	// provH = min(provider 数, 内容区 (信息区已扣除) - 参数区 (标题 + gateLabels 行) - 6)。
	want := min(10, max(1, body-(1+len(gateLabels))-6))
	if got := strings.Count(plain, "priority="); got != want {
		t.Fatalf("供应商行数应为 %d, 得到 %d:\n%s", want, got, plain)
	}
	for i := want; i < len(e.providers); i++ {
		if strings.Contains(plain, "p"+strconv.Itoa(i)+"  priority=") {
			t.Errorf("超出高度的供应商 p%d 不该画:\n%s", i, plain)
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
	m.drawSettings(c, 6) // window = body-1 = 5 (标题占 1 行), 两栏共 10 行
	if e.vis != 5 {
		t.Fatalf("每栏应显示 5 行, 得到 %d", e.vis)
	}
	// 显示行 = 1 个分组标题 + 12 个模型 = 13; 选中第 12 行 (下标 12),
	// span=10 -> scrollTop = 12-10+1 = 3 (第 3 行 = 模型 m02)。
	if e.scrollTop != 3 {
		t.Fatalf("滚动位置应钉住选中行 (3), 得到 %d", e.scrollTop)
	}
	out := stripANSI(strings.TrimSuffix(c.b.String(), "\r\n"))
	for _, gone := range []string{"m00", "m01"} {
		if strings.Contains(out, "/"+gone) {
			t.Errorf("已滚出窗口的 %s 不该出现:\n%s", gone, out)
		}
	}
	for i := 2; i < 12; i++ {
		id := "m0" + strconv.Itoa(i)
		if i >= 10 {
			id = "m" + strconv.Itoa(i)
		}
		if !strings.Contains(out, "/"+id) {
			t.Errorf("两栏应连续铺满, 缺少 %s:\n%s", id, out)
		}
	}
}

// 瞬时结果: 固定 10 秒后消失 (与按键无关), 消失后不再占信息区的行。
func TestSettingsNoticeExpires(t *testing.T) {
	e := &tuiEditor{}
	e.notice("已保存 x")
	now := time.Now() // 从消息出现的时刻起算
	if text, isErr, ok := e.noticeText(now); !ok || text != "已保存 x" || isErr {
		t.Fatalf("瞬时结果应可见且不是失败色: %q %v %v", text, isErr, ok)
	}
	if _, _, ok := e.noticeText(now.Add(msgTTL - time.Millisecond)); !ok {
		t.Errorf("没到点不该消失")
	}
	if _, _, ok := e.noticeText(now.Add(msgTTL)); ok {
		t.Errorf("到 %v 应消失", msgTTL)
	}
	if n := e.infoRows(now.Add(msgTTL)); n != 0 {
		t.Errorf("消失后不该占行, 得到 %d", n)
	}
	e.fail("保存失败")
	if text, isErr, ok := e.noticeText(time.Now()); !ok || !isErr || text != "保存失败" {
		t.Fatalf("失败提示应带红色标记: %q %v %v", text, isErr, ok)
	}
}

// 常驻告警: 未保存标记与读不到的错误按固定顺序给出, 不随时间消失。
func TestSettingsAlertsOrder(t *testing.T) {
	e := &tuiEditor{dirty: true, gateDirty: true, gateErr: "boom", err: "store 读不到"}
	want := "[优先级未保存] | [调度参数未保存] | 配置读不到: boom | store 读不到"
	if got := strings.Join(e.alerts(), " | "); got != want {
		t.Fatalf("告警顺序不对:\n得到 %q\n期望 %q", got, want)
	}
	if n := e.infoRows(time.Now()); n != 4 {
		t.Errorf("信息区应占 4 行, 得到 %d", n)
	}
	e.dirty, e.gateDirty, e.gateErr, e.err = false, false, "", ""
	if len(e.alerts()) != 0 {
		t.Errorf("状态解除后不该还有告警: %v", e.alerts())
	}
}

// 底部不再有整行反色条: 常驻告警与瞬时结果各占一行, 只到文字宽度, 不铺满,
// 且没有内容时不占行 (内容区拿到全部行数)。
func TestSettingsInfoDrawNoBar(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	e := m.prio
	frame := m.draw()
	if !strings.Contains(frame, e.message) {
		t.Fatalf("初始瞬时说明应显示: %q", e.message)
	}
	if n := strings.Count(stripANSI(frame), e.message); n != 1 {
		t.Errorf("初始说明应只出现一次, 得到 %d", n)
	}

	e.dirty = true
	e.notice("已保存")
	frame = m.draw()
	want := tuiRed + tuiIndent + "[优先级未保存]" + tuiReset
	if !strings.Contains(frame, want) {
		t.Fatalf("未保存应是红色常驻告警 %q", want)
	}
	for _, line := range strings.Split(frame, "\r\n") {
		plain := stripANSI(line)
		if strings.Contains(plain, "[优先级未保存]") || strings.Contains(plain, "已保存") {
			if tuiWidth(line) >= m.cols {
				t.Errorf("信息行不该铺满整行: %q", plain)
			}
			if strings.HasPrefix(line, tuiRev) {
				t.Errorf("信息行不该整行反显: %q", plain)
			}
		}
	}

	e.dirty, e.message, e.msgAt = false, "", time.Time{}
	if n := m.prio.infoRows(time.Now()); n != 0 {
		t.Fatalf("没有信息时不该占行, 得到 %d", n)
	}
}

// 全局参数按 = 原位输入: 缓冲画在该行数值格上 (反显), 不占底部输入行; 回车提交后
// 标脏并给瞬时结果, Esc 取消不改值。
func TestSettingsInlineGateEdit(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	e := m.prio
	e.focus = 2
	e.gateSel = 0 // interval_sec
	before := e.gateField(0)

	e.gateKey('=')
	if e.prompt == nil || !e.prompt.inline || e.inlineSel != 0 {
		t.Fatalf("= 应开原位输入: prompt=%+v inlineSel=%d", e.prompt, e.inlineSel)
	}
	if e.prompt.buffer != fsec(before) {
		t.Fatalf("缓冲应以当前值为初值, 得到 %q (期望 %q)", e.prompt.buffer, fsec(before))
	}
	if e.hasBottomPrompt() {
		t.Errorf("原位输入不该占底部的临时输入行")
	}
	for range e.prompt.buffer {
		e.promptKey(0x7f)
	}
	for _, b := range []byte("1.5") {
		e.promptKey(b)
	}
	if frame := m.draw(); !strings.Contains(frame, tuiRev+"1.5"+tuiReset) {
		t.Fatalf("输入缓冲应反显在数值格上")
	}
	e.promptKey('\r')
	if e.gate.IntervalMs != 1500 || !e.gateDirty {
		t.Fatalf("回车应提交并标脏: ms=%d dirty=%v", e.gate.IntervalMs, e.gateDirty)
	}
	if e.prompt != nil || e.inlineSel != -1 {
		t.Errorf("提交后应结束输入: prompt=%+v inlineSel=%d", e.prompt, e.inlineSel)
	}
	if !strings.Contains(e.message, gateCN[0]) {
		t.Errorf("提交后应给瞬时结果: %q", e.message)
	}

	e.gateSel = 0
	e.gateKey('=')
	e.promptKey(0x1b) // Esc 取消
	if e.prompt != nil || e.gate.IntervalMs != 1500 {
		t.Fatalf("Esc 应取消且不改值: prompt=%+v ms=%d", e.prompt, e.gate.IntervalMs)
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

// settings 全局参数四区分组: 其他 (调度等待 + 粘性 + 探测) | 锁时长 | 低性能设置 | 主题,
// 区之间各空一行; 区内的键顺序与 gateLabels 一致。
func TestSettingsGateSections(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	lines := m.settingsGateLines(m.prio, 40)
	var seq []string
	for _, l := range lines {
		p := stripANSI(l)
		if strings.HasPrefix(p, "全局参数") {
			continue
		}
		seq = append(seq, p)
	}
	var want []string
	for i := 0; i <= 5; i++ {
		want = append(want, gateCN[i]) // 其他: 调度等待 + 粘性 + 探测
	}
	want = append(want, "")
	for i := 6; i <= 9; i++ {
		want = append(want, gateCN[i]) // 锁时长
	}
	want = append(want, "")
	for i := 10; i <= gateIdxAbort; i++ {
		want = append(want, gateCN[i]) // 低性能设置 (含打断超时)
	}
	want = append(want, "")
	want = append(want, gateCN[gateIdxTheme]) // 主题: 只在内存里的显示开关
	if len(seq) != len(want) {
		t.Fatalf("全局参数应有 %d 行 (含 2 空行), 得到 %d:\n%v", len(want), len(seq), seq)
	}
	for i, name := range want {
		if name == "" {
			if strings.TrimSpace(seq[i]) != "" {
				t.Errorf("第 %d 行应为空行, 得到 %q", i, seq[i])
			}
			continue
		}
		if !strings.HasPrefix(seq[i], "  "+name+": ") {
			t.Errorf("第 %d 行应为 %q, 得到 %q", i, name, seq[i])
		}
	}
}
