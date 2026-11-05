// tui_priority_test.go: settings 页 (调度参数 + 权重编辑) 的回归: 存盘口径、候选顺序与生成器一致、
// 键位与未保存守门、渲染不越界。
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const prioTestStore = `{
    "providers": {
        "ustc": {
            "api": "openai-completions",
            "apiKey": "$USTC_API_KEY",
            "baseUrl": "https://ustc.invalid/v1",
            "models": [
                {
                    "contextWindow": 1000000,
                    "id": "deepseek-flash",
                    "tollgate_group": "deepseek-v4.1-flash",
                    "tollgate_priority": 2
                },
                {
                    "contextWindow": 1000000,
                    "id": "deepseek-flash-2",
                    "tollgate_group": "deepseek-v4.1-flash"
                }
            ],
            "tollgate_priority": 0,
            "x_unknown": {
                "keep": true
            }
        },
        "group": {
            "api": "openai-responses",
            "apiKey": "$GROUP_API_KEY",
            "baseUrl": "http://group.invalid/v1",
            "models": [
                {
                    "id": "deepseek-v4.1-flash",
                    "tollgate_group": "deepseek-v4.1-flash"
                },
                {
                    "id": "solo",
                    "tollgate_priority": 5
                }
            ],
            "tollgate_priority": 3
        },
        "tollgate": {
            "baseUrl": "http://127.0.0.1:8787/v1",
            "kind": "proxy",
            "models": [
                {
                    "id": "deepseek-v4.1-flash"
                }
            ]
        }
    }
}
`

const prioTestConfig = `{
    "listen": "127.0.0.1:8787",
    "log_path": "var/requests.jsonl",
    "upstreams": [
        {
            "id": "ustc",
            "base_url": "https://ustc.invalid/v1"
        },
        {
            "id": "group",
            "base_url": "http://group.invalid/v1"
        }
    ],
    "aliases": {}
}
`

// prioFixture 落一份 store + 配置到临时目录, 返回 (store 路径, 配置路径)。
func prioFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "llm-models.json")
	cfg := filepath.Join(dir, "tollgate.json")
	if err := os.WriteFile(store, []byte(prioTestStore), 0o600); err != nil {
		t.Fatalf("写 store: %v", err)
	}
	if err := os.WriteFile(cfg, []byte(prioTestConfig), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return store, cfg
}

func candIDs(cands []tuiPrioCand) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.provider+"/"+c.modelID)
	}
	return out
}

// 候选顺序 = 模型优先级降序 -> provider 优先级降序 -> 上游顺序 -> 模型 id;
// 本代理自己 (kind=proxy) 与黑名单里的模型都不该出现在列表里。
func TestPrioCandidateOrder(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	if !e.usable() {
		t.Fatalf("store 装载失败: %v", e.err)
	}
	if got := strings.Join(e.providers, ","); got != "group,ustc" {
		t.Fatalf("上游应按优先级降序 (group 3 > ustc 0, 代理自身排除), 得到 %q", got)
	}
	cands := e.candsFor("deepseek-v4.1-flash")
	want := []string{"ustc/deepseek-flash", "group/deepseek-v4.1-flash", "ustc/deepseek-flash-2"}
	if got := candIDs(cands); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("候选顺序不对:\n得到 %v\n期望 %v", got, want)
	}
	if e.candsFor("solo") == nil {
		t.Errorf("单候选别名也该在预览里")
	}
	if rows := e.filtered(); len(rows) != 4 {
		t.Errorf("列表应是 4 行 (ustc 2 + group 2, 代理自身的模型不算), 得到 %d", len(rows))
	}
}

// 预览的顺序必须和生成器落进配置的一致 —— 这是当初手写排序最怕漂移的地方,
// 所以直接拿真生成器跑一遍对答案 (没 python3 就跳过)。
func TestPrioOrderMatchesGenerator(t *testing.T) {
	store, cfg := prioFixture(t)
	root := repoRootFor(cfg)
	gen := filepath.Join(root, "scripts", "gen_tollgate_config.py")
	if _, err := os.Stat(gen); err != nil {
		t.Skipf("生成器不在: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("没有 python3")
	}
	cmd := exec.Command("python3", gen, "--dry-run", "--fff-dir", filepath.Dir(store),
		"--out", cfg, "--providers", "ustc,group")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("生成器跑不起来 (跳过): %v %s", err, stderr.String())
	}
	var generated struct {
		Aliases map[string][][2]string `json:"aliases"`
	}
	if err := json.Unmarshal(out, &generated); err != nil {
		t.Fatalf("生成器输出不是 JSON: %v", err)
	}
	e := newTuiEditor(cfg)
	if !e.usable() {
		t.Fatalf("store 装载失败: %v", e.err)
	}
	for alias, pairs := range generated.Aliases {
		want := make([]string, 0, len(pairs))
		for _, p := range pairs {
			want = append(want, p[0]+"/"+p[1])
		}
		got := candIDs(e.candsFor(alias))
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("别名 %s 的候选顺序与生成器不一致:\n预览 %v\n生成 %v", alias, got, want)
		}
	}
}

// 存盘: 数值原样 (不出现 .0)、没动过的字段还在、4 空格缩进、不留 .tmp。
func TestPrioSaveRoundTrip(t *testing.T) {
	store, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	e.adjust(5) // 选中行是 ustc/deepseek-flash (M2)
	e.saveStore()
	if e.dirty {
		t.Fatalf("存盘后不该还是脏的: %s", e.message)
	}
	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !bytes.Contains(b, []byte("\n    \"providers\"")) {
		t.Errorf("缩进应为 4 空格:\n%s", b[:80])
	}
	if !bytes.HasSuffix(b, []byte("}\n")) {
		t.Errorf("结尾应有一个换行")
	}
	if _, err := os.Stat(store + ".tmp"); err == nil {
		t.Errorf("不该留下 .tmp")
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("存出来的不是 JSON: %v", err)
	}
	providers := got["providers"].(map[string]any)
	ustc := providers["ustc"].(map[string]any)
	if _, ok := ustc["x_unknown"]; !ok {
		t.Errorf("没动过的字段丢了: %v", ustc)
	}
	if prio := prioOf(ustc); prio != 0 {
		t.Errorf("provider 优先级不该被动过, 得到 %d", prio)
	}
	models := modelsOf(ustc)
	if len(models) != 2 || prioOf(models[0]) != 7 {
		t.Errorf("模型优先级应为 7, 得到 %v", models)
	}
	if !bytes.Contains(b, []byte("\"tollgate_priority\": 7")) {
		t.Errorf("存盘把整数写成了别的形式:\n%s", b)
	}
}

// 键位: 调值夹到 0、输入框、provider 面板、未保存守门 (第一次 q 不退出)。
func TestPrioKeys(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	pending := []byte{}
	key := func(b byte) bool {
		quit := m.handleKey(b, &pending)
		if quit {
			t.Fatal("不该在这一步退出")
		}
		return quit
	}

	// 选中 ustc/deepseek-flash (M2, 优先级 2): 减 3 次到 -1 (负值 = 停用, 下限)。
	// 停用后该行仍显示, 光标钉在原行; 再按 - 停在 -1 不再减, + 调回 0。
	row := m.prio.selected()
	if row == nil || row.provider+"/"+modelID(row.model) != "ustc/deepseek-flash" {
		t.Fatalf("初始选中应为 ustc/deepseek-flash, 得到 %+v", row)
	}
	target := row.model
	for i := 0; i < 3; i++ {
		key('-')
	}
	if got := prioOf(target); got != -1 {
		t.Fatalf("优先级应能减到 -1 (停用), 得到 %d", got)
	}
	if got := prioOf(m.prio.selected().model); got != -1 {
		t.Fatalf("停用后该行应保持选中, 得到 %d", got)
	}
	for i := 0; i < 2; i++ {
		key('-')
	}
	if got := prioOf(target); got != -1 {
		t.Fatalf("到 -1 后再按 - 应停在 -1, 得到 %d", got)
	}
	// 预览仍显示停用模型, 排最后 (优先级最低)。
	if got := candIDs(m.prio.candsFor("deepseek-v4.1-flash")); strings.Join(got, " ") != "group/deepseek-v4.1-flash ustc/deepseek-flash-2 ustc/deepseek-flash" {
		t.Fatalf("停用模型应在预览里排最后: %v", got)
	}
	key('+')
	if got := prioOf(m.prio.selected().model); got != 0 {
		t.Fatalf("加一后应为 0, 得到 %d", got)
	}

	// "=" 输入 12; 退格再补一位, 回车提交。
	key('=')
	key('1')
	key('2')
	key('3')
	key(0x7f)
	key('\r')
	if got := prioOf(m.prio.selected().model); got != 12 {
		t.Fatalf("输入框提交后应为 12, 得到 %d", got)
	}
	if m.prio.prompt != nil {
		t.Fatalf("提交后输入态应结束")
	}

	// "/" 过滤: 只留 group 的行。
	key('/')
	for _, b := range []byte("group") {
		key(b)
	}
	key('\r')
	if rows := m.prio.filtered(); len(rows) != 2 {
		t.Fatalf("过滤 group 应剩 2 行, 得到 %d", len(rows))
	}

	// p 换到 provider 面板并按 + 调 provider 优先级。
	key('p')
	if m.prio.focus != 1 {
		t.Fatalf("p 之后焦点应在 provider 面板")
	}
	key('+')
	if got := prioOf(m.prio.provEntry("ustc")); got != 1 {
		t.Fatalf("provider 优先级应为 1, 得到 %d", got)
	}

	// 未保存: 第一次 q 只提示, 第二次才放行。
	if quit := m.handleKey('q', &pending); quit {
		t.Fatalf("有未保存改动时第一次 q 不该退出")
	}
	if !strings.Contains(m.prio.message, "未保存") {
		t.Errorf("应提示未保存: %q", m.prio.message)
	}
	if quit := m.handleKey('q', &pending); !quit {
		t.Fatalf("第二次 q 应放弃改动退出")
	}
}

// 负优先级 (下限 -1) = 停用: adjust / setValue 都钳到 -1, 存盘写进 store;
// 该行保持显示 (列表在, 预览排在最后), 重读后仍是停用; provider 优先级不受影响。
func TestPrioNegativeDisableClampAndVisible(t *testing.T) {
	store, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	row := e.selected()
	if row == nil || row.provider+"/"+modelID(row.model) != "ustc/deepseek-flash" {
		t.Fatalf("初始选中应为 ustc/deepseek-flash, 得到 %+v", row)
	}
	target := row.model
	e.adjust(-10) // 2 -> 钳到 -1
	if got := prioOf(target); got != -1 {
		t.Fatalf("优先级下限应为 -1, 得到 %d", got)
	}
	if got := candIDs(e.candsFor("deepseek-v4.1-flash")); strings.Join(got, " ") != "group/deepseek-v4.1-flash ustc/deepseek-flash-2 ustc/deepseek-flash" {
		t.Fatalf("停用模型应在预览里排最后: %v", got)
	}
	if rows := e.filtered(); len(rows) != 4 {
		t.Fatalf("停用后该行仍应显示 (列表 4 行), 得到 %d", len(rows))
	}
	e.saveStore()
	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("\"tollgate_priority\": -1")) {
		t.Errorf("存盘应为 -1:\n%s", b)
	}
	// 重读 store: -1 原样读回, 行仍可见, provider 优先级不受影响。
	e2 := newTuiEditor(cfg)
	if got := prioOf(e2.provEntry("ustc")); got != 0 {
		t.Fatalf("provider 优先级不该被模型调整动到, 得到 %d", got)
	}
	gotRows := e2.filtered()
	if len(gotRows) != 4 {
		t.Fatalf("重读后停用行仍应显示, 得到 %d", len(gotRows))
	}
	for _, r := range gotRows {
		if r.provider == "ustc" && modelID(r.model) == "deepseek-flash" && prioOf(r.model) != -1 {
			t.Fatalf("重读后停用模型应为 -1, 得到 %d", prioOf(r.model))
		}
	}
	// = 直接输入超大负数也钳到 -1, 再输入正数正常调回。
	probe := e2.selected()
	if probe == nil {
		t.Fatal("该有选中行")
	}
	probeModel := probe.model
	e2.setValue(-999)
	if got := prioOf(probeModel); got != -1 {
		t.Fatalf("直接输入超大负数应钳到 -1, 得到 %d", got)
	}
	e2.setValue(3)
	if got := prioOf(probeModel); got != 3 {
		t.Fatalf("3 应能正常写入 (从停用调回), 得到 %d", got)
	}
}

// 页签: 3 键进 settings; settings 页的 h 是热加载 (失败也只报错, 不切页);
// 状态页的 h/bash 语义不变。
func TestPrioTabScopedKeys(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	pending := []byte{}
	m.handleKey('3', &pending)
	if m.tab != tabSettings || m.prio == nil {
		t.Fatalf("3 应进 settings 页并装载编辑器 (tab=%d)", m.tab)
	}
	// 把 root 指到空目录: h 会去跑不存在的 deploy/tollgate.sh, 只该报错。
	m.prio.root = t.TempDir()
	m.handleKey('h', &pending)
	if m.tab != tabSettings {
		t.Fatalf("settings 页的 h 是热加载, 不该切页 (tab=%d)", m.tab)
	}
	if !strings.Contains(m.prio.message, "hup 失败") {
		t.Errorf("hup 失败应写在消息行: %q", m.prio.message)
	}
	// 状态页: h 仍是"上一个页签", r 仍是重读日志。
	m.handleKey('1', &pending)
	m.scanned = 7
	m.handleKey('r', &pending)
	if m.scanned != 0 {
		t.Errorf("状态页的 r 应重读日志")
	}
	m.handleKey('h', &pending)
	if m.tab != tabSettings {
		t.Errorf("active 页按 h 应回绕到 priority, 得到 %d", m.tab)
	}
}

// 渲染: 每行不越界、页签里有 settings、未保存可见; 没有编辑器也不能崩。
func TestPrioDraw(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	m.prio.adjust(1)
	frame := m.draw()
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	if len(lines) != 30 { // tuiSize() 非 TTY 时是 100x30
		t.Fatalf("帧应是 30 行, 得到 %d", len(lines))
	}
	for i, line := range lines {
		// 行尾的 \r 不对应显示列, 量宽度前先去掉 (状态行顶部内容仍在 100 列内)。
		if w := tuiWidth(strings.TrimSuffix(line, "\r")); w > 100 {
			t.Errorf("第 %d 行宽 %d 超过 100: %q", i+1, w, line)
		}
	}
	if !strings.Contains(frame, "settings") {
		t.Errorf("页签里应有 settings: %q", lines[0])
	}
	if !strings.Contains(frame, "[优先级未保存]") {
		t.Errorf("改了没存应显示 [优先级未保存]")
	}
	if !strings.Contains(frame, "预览") {
		t.Errorf("右栏应是预览")
	}

	// store 读不到时只报错, 不 panic (把 FFF_HOME 指到空目录, 免得退回真 store)。
	t.Setenv("FFF_HOME", t.TempDir())
	bad := newTUIModel(filepath.Join(t.TempDir(), "nope.json"))
	bad.prio = newTuiEditor(bad.cfgPath)
	bad.tab = tabSettings
	bad.draw()
	if bad.prio.err == "" {
		t.Errorf("store 不存在时应记下错误")
	}
	if !strings.Contains(bad.draw(), "store 读不到") {
		t.Errorf("画面应说明 store 读不到")
	}
}

// 焦点切换: m 切到模型面板, p 切到 provider 面板; Tab 不再参与换面板。
func TestPrioFocusKeys(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	pending := []byte{}
	send := func(b byte) {
		if quit := m.handleKey(b, &pending); quit {
			t.Fatal("不该退出")
		}
	}
	if m.prio.focus != 0 {
		t.Fatalf("初始焦点应在模型面板, 得到 %d", m.prio.focus)
	}
	send('p')
	if m.prio.focus != 1 {
		t.Fatalf("p 应切到 provider 面板, 得到 %d", m.prio.focus)
	}
	send('m')
	if m.prio.focus != 0 {
		t.Fatalf("m 应切回模型面板, 得到 %d", m.prio.focus)
	}
	send('\t')
	if m.prio.focus != 0 {
		t.Fatalf("Tab 不该再切换面板, 得到 %d", m.prio.focus)
	}
	if m.tab == tabSettings {
		t.Errorf("Tab 现在只切页, 不该留在 settings")
	}
	send('3')
	if m.tab != tabSettings {
		t.Fatalf("3 应回到 settings, 得到 %d", m.tab)
	}
	send('p')
	// 切到 provider 后 +/- 改的是 provider 优先级, 不是模型优先级。
	before := prioOf(m.prio.selected().model)
	send('+')
	if got := prioOf(m.prio.selected().model); got != before {
		t.Fatalf("provider 面板下 + 不该改模型优先级: %d -> %d", before, got)
	}
}

// 设置页的调度参数: g 聚焦, +/- 调值, s 写回配置并热加载; 配置文件其余字段原样保留。
func TestSettingsGateSave(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	pending := []byte{}
	send := func(b byte) {
		if quit := m.handleKey(b, &pending); quit {
			t.Fatal("不该退出")
		}
	}
	send('g')
	if m.prio.focus != 2 {
		t.Fatalf("g 应把焦点切到调度参数, 得到 %d", m.prio.focus)
	}
	before := m.prio.gate.SemWaitMs
	m.prio.gateSel = 1 // 编辑 sem_wait (整秒友好, 秒精度可往返)
	send('+')
	if got := m.prio.gate.SemWaitMs; got != before+500 {
		t.Fatalf("sem_wait 应 +500: %d -> %d", before, got)
	}
	if !m.prio.gateDirty {
		t.Fatalf("改了参数应标记未保存")
	}
	called := false
	m.reloadFn = func() error {
		called = true
		return nil
	}
	send('s')
	if !called {
		t.Errorf("保存后应触发热加载")
	}
	if m.prio.gateDirty {
		t.Errorf("保存成功后不该再标记未保存")
	}

	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("写回的配置不是合法 JSON: %v", err)
	}
	for _, k := range []string{"listen", "log_path", "upstreams", "aliases"} {
		if _, ok := top[k]; !ok {
			t.Errorf("写回配置丢了字段 %q", k)
		}
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	gg := got.gate()
	if gg.SemWaitMs != before+500 {
		t.Errorf("配置里的 sem_wait = %d, 期望 %d", gg.SemWaitMs, before+500)
	}
	if strings.Contains(string(raw), "sk-") || strings.Contains(string(raw), "key\": \"") {
		t.Errorf("写回配置不该出现密钥: %s", raw)
	}
}

// 编辑 probe_enabled (settings 第 3 页的开关行) 应写回配置并可热重载。
func TestSettingsProbeEditAndSave(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	pending := []byte{}
	send := func(b byte) {
		if quit := m.handleKey(b, &pending); quit {
			t.Fatal("不该退出")
		}
	}
	send('g')
	if m.prio.focus != 2 {
		t.Fatalf("g 应切到调度参数, 得到 %d", m.prio.focus)
	}
	m.prio.gateSel = gateIdxProbe
	if !m.prio.probeOn {
		t.Fatal("夹具未写 probe_enabled, 缺省应为开")
	}
	send('-')
	if m.prio.probeOn {
		t.Fatal("'-' 应把 probe_enabled 切成关")
	}
	if !m.prio.gateDirty {
		t.Fatal("改动该标记未保存")
	}
	called := false
	m.reloadFn = func() error { called = true; return nil }
	send('s')
	if !called {
		t.Errorf("保存后应触发热加载")
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("写回非合法 JSON: %v", err)
	}
	if got.probeEnabled() {
		t.Fatal("开关改成关后, 配置 probe_enabled 应为 false")
	}
	// 用 = 显式改回开, 再保存。
	e := newTuiEditor(cfg) // 重读, probeOn 应为关
	if e.probeOn {
		t.Fatal("重读配置 probe_enabled 应为关")
	}
	e.probeOn = false
	e.gateDirty = true
	e.setGatePrompt(gateIdxProbe, "on")
	if !e.probeOn {
		t.Fatal("= on 应打开开关")
	}
	if err := e.saveGate(); err != nil {
		t.Fatalf("保存 gate: %v", err)
	}
	raw2, _ := os.ReadFile(cfg)
	var got2 Config
	if err := json.Unmarshal(raw2, &got2); err != nil {
		t.Fatal(err)
	}
	if !got2.probeEnabled() {
		t.Fatal("= on 保存后配置 probe_enabled 应为 true")
	}
}

// 退避序列只能用 = 按逗号输入秒, 非法输入不落盘。
func TestSettingsBackoffInput(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	e.setGatePrompt(gateIdxRetry, "1, 2,3")
	if got := strings.Join(itoaSlice(e.gate.RetryBackoffMs), ","); got != "1000,2000,3000" {
		t.Errorf("退避序列(秒)解析不对: %q", got)
	}
	e.gateDirty = false
	e.setGatePrompt(gateIdxRetry, "1,x")
	if e.gateDirty {
		t.Errorf("非法输入不该标记未保存")
	}
	if got := strings.Join(itoaSlice(e.gate.RetryBackoffMs), ","); got != "1000,2000,3000" {
		t.Errorf("非法输入不该改值: %q", got)
	}
}

func itoaSlice(v []int) []string {
	out := make([]string, 0, len(v))
	for _, n := range v {
		out = append(out, strconv.Itoa(n))
	}
	return out
}

// 配置是软链 (config.local.json -> ~/fff/config/tollgate.json) 时, 保存 gate
// 必须写真实目标, 不能把软链换成普通文件。
func TestSettingsGateSaveKeepsSymlink(t *testing.T) {
	_, real := prioFixture(t)
	link := filepath.Join(filepath.Dir(real), "config.link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("建软链: %v", err)
	}
	e := newTuiEditor(link)
	e.focus = 2
	e.gateSel = 1 // sem_wait (整秒可往返)
	before := e.gate.SemWaitMs
	e.adjustGate(1)
	if err := e.saveGate(); err != nil {
		t.Fatalf("保存 gate: %v", err)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("软链被替换成了普通文件")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if gg := got.gate(); gg.SemWaitMs != before+500 {
		t.Errorf("真实配置没更新: sem_wait=%d, 期望 %d", gg.SemWaitMs, before+500)
	}
}

// 上游列表按优先级降序; 调高某个上游后立刻重排, 选中项跟着它走。
func TestProviderSortByPriority(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	if got := strings.Join(e.providers, ","); got != "group,ustc" {
		t.Fatalf("初始应按优先级 group(3),ustc(0), 得到 %q", got)
	}
	e.focus = 1
	for i, n := range e.providers {
		if n == "ustc" {
			e.provSel = i
		}
	}
	e.setValue(9)
	if got := strings.Join(e.providers, ","); got != "ustc,group" {
		t.Fatalf("改完优先级应重排, 得到 %q", got)
	}
	if e.providers[e.provSel] != "ustc" {
		t.Errorf("选中项应还停在 ustc, 实际 %q", e.providers[e.provSel])
	}
}

func candsOrder(cands []candView) string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ID)
	}
	return strings.Join(out, ",")
}

// 第 1 页上游表按 provider 优先级降序, 与 settings 页的 provider 面板同口径。
func TestModelsCandsSortByPriority(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.pulse.Cands = []candView{{ID: "ustc"}, {ID: "group"}}
	if got := candsOrder(m.sortedCands()); got != "group,ustc" {
		t.Fatalf("上游表应按优先级降序 (group 3 > ustc 0), 得到 %q", got)
	}
}

// 同优先级的两个上游保持配置顺序, 不看 id 字典序。
func TestModelsCandsTieByConfigOrder(t *testing.T) {
	dir := t.TempDir()
	store := `{"providers": {"zeta": {"tollgate_priority": 1}, "alpha": {"tollgate_priority": 1}}}`
	cfgText := `{"upstreams": [{"id": "zeta"}, {"id": "alpha"}]}`
	if err := os.WriteFile(filepath.Join(dir, "llm-models.json"), []byte(store), 0o600); err != nil {
		t.Fatalf("写 store: %v", err)
	}
	cfg := filepath.Join(dir, "tollgate.json")
	if err := os.WriteFile(cfg, []byte(cfgText), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	m := newTUIModel(cfg)
	m.pulse.Cands = []candView{{ID: "alpha"}, {ID: "zeta"}}
	if got := candsOrder(m.sortedCands()); got != "zeta,alpha" {
		t.Fatalf("同优先级应按配置顺序 zeta,alpha, 得到 %q", got)
	}
}

// store 读不到时保持观测面原序, 不在看板里硬排。
func TestModelsCandsNoStoreKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FFF_HOME", dir)
	cfg := filepath.Join(dir, "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{"upstreams": [{"id": "ustc"}, {"id": "group"}]}`), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	m := newTUIModel(cfg)
	m.pulse.Cands = []candView{{ID: "ustc"}, {ID: "group"}}
	if got := candsOrder(m.sortedCands()); got != "ustc,group" {
		t.Fatalf("store 读不到应保持原序, 得到 %q", got)
	}
}

// 光标移动: moveFocus / jump 在每个面板里都夹在有效范围内, visibleRows 给出翻页步长。
func TestEditorNavFocus(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	if !e.usable() {
		t.Fatalf("store 装载失败: %v", e.err)
	}
	models := len(e.filtered())
	if models < 2 {
		t.Fatalf("夹具里应至少有 2 个模型, 得到 %d", models)
	}

	// 模型面板。
	e.focus, e.modelSel = 0, 0
	e.moveFocus(1)
	if e.modelSel != 1 {
		t.Errorf("下移一行应到 1, 得到 %d", e.modelSel)
	}
	e.moveFocus(-5)
	if e.modelSel != 0 {
		t.Errorf("上移不能越过第 0 行, 得到 %d", e.modelSel)
	}
	e.jump(1 << 10)
	if e.modelSel != models-1 {
		t.Errorf("跳到末尾应为 %d, 得到 %d", models-1, e.modelSel)
	}

	// provider 面板。
	e.focus, e.provSel = 1, 0
	e.moveFocus(1)
	if e.provSel != 1 || len(e.providers) < 2 {
		t.Errorf("provider 下移应到 1 (共 %d 个), 得到 %d", len(e.providers), e.provSel)
	}
	e.jump(1 << 10)
	if e.provSel != len(e.providers)-1 {
		t.Errorf("provider 跳到末尾应为 %d, 得到 %d", len(e.providers)-1, e.provSel)
	}
	e.moveFocus(-1 << 10)
	if e.provSel != 0 {
		t.Errorf("provider 上移不能越过 0, 得到 %d", e.provSel)
	}

	// 调度参数面板。
	e.focus, e.gateSel = 2, 0
	e.moveFocus(2)
	if e.gateSel != 2 {
		t.Errorf("gate 下移两行应到 2, 得到 %d", e.gateSel)
	}
	e.jump(1 << 10)
	if e.gateSel != len(gateLabels)-1 {
		t.Errorf("gate 跳到末尾应为 %d, 得到 %d", len(gateLabels)-1, e.gateSel)
	}
	e.moveFocus(-1 << 10)
	if e.gateSel != 0 {
		t.Errorf("gate 上移不能越过 0, 得到 %d", e.gateSel)
	}

	// 翻页步长: 还没渲染过 (vis=0) 时用 10, 渲染后按实际行数。
	e.vis = 0
	if got := e.visibleRows(); got != 10 {
		t.Errorf("还没渲染时步长应为 10, 得到 %d", got)
	}
	e.vis = 4
	if got := e.visibleRows(); got != 4 {
		t.Errorf("渲染后步长应为 4, 得到 %d", got)
	}
}
