// tui_priority_test.go: priority 页 (权重编辑) 的回归: 存盘口径、候选顺序与生成器一致、
// 键位与未保存守门、渲染不越界。
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	if got := strings.Join(e.providers, ","); got != "ustc,group" {
		t.Fatalf("上游顺序应为 ustc,group (代理自身排除), 得到 %q", got)
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
	m.tab = 3
	pending := []byte{}
	key := func(b byte) bool {
		quit := m.handleKey(b, &pending)
		if quit {
			t.Fatal("不该在这一步退出")
		}
		return quit
	}

	// 选中 ustc/deepseek-flash (M2): 减到底夹在 0, 再加回去。
	for i := 0; i < 5; i++ {
		key('-')
	}
	if got := prioOf(m.prio.selected().model); got != 0 {
		t.Fatalf("优先级应夹在 0, 得到 %d", got)
	}
	key('+')
	if got := prioOf(m.prio.selected().model); got != 1 {
		t.Fatalf("加一后应为 1, 得到 %d", got)
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

	// Tab 换到 provider 面板并按 + 调 provider 优先级。
	key('\t')
	if m.prio.focus != 1 {
		t.Fatalf("Tab 之后焦点应在 provider 面板")
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

// 页签: 4 键进 priority; priority 页的 h 是热加载 (失败也只报错, 不切页);
// 状态页的 h/bash 语义不变。
func TestPrioTabScopedKeys(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	pending := []byte{}
	m.handleKey('4', &pending)
	if m.tab != 3 || m.prio == nil {
		t.Fatalf("4 应进 priority 页并装载编辑器 (tab=%d)", m.tab)
	}
	// 把 root 指到空目录: h 会去跑不存在的 deploy/tollgate.sh, 只该报错。
	m.prio.root = t.TempDir()
	m.handleKey('h', &pending)
	if m.tab != 3 {
		t.Fatalf("priority 页的 h 是热加载, 不该切页 (tab=%d)", m.tab)
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
	if m.tab != 3 {
		t.Errorf("active 页按 h 应回绕到 priority, 得到 %d", m.tab)
	}
}

// 渲染: 每行不越界、页签里有 priority、未保存可见; 没有编辑器也不能崩。
func TestPrioDraw(t *testing.T) {
	_, cfg := prioFixture(t)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = 3
	m.prio.adjust(1)
	frame := m.draw()
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	if len(lines) != 30 { // tuiSize() 非 TTY 时是 100x30
		t.Fatalf("帧应是 30 行, 得到 %d", len(lines))
	}
	for i, line := range lines {
		if w := tuiWidth(line); w > 100 {
			t.Errorf("第 %d 行宽 %d 超过 100: %q", i+1, w, line)
		}
	}
	if !strings.Contains(frame, "priority") {
		t.Errorf("页签里应有 priority: %q", lines[0])
	}
	if !strings.Contains(frame, "[未保存]") {
		t.Errorf("改了没存应显示 [未保存]")
	}
	if !strings.Contains(frame, "候选顺序预览") {
		t.Errorf("右栏应是候选顺序预览")
	}

	// store 读不到时只报错, 不 panic (把 FFF_HOME 指到空目录, 免得退回真 store)。
	t.Setenv("FFF_HOME", t.TempDir())
	bad := newTUIModel(filepath.Join(t.TempDir(), "nope.json"))
	bad.prio = newTuiEditor(bad.cfgPath)
	bad.tab = 3
	bad.draw()
	if bad.prio.err == "" {
		t.Errorf("store 不存在时应记下错误")
	}
	if !strings.Contains(bad.draw(), "store 读不到") {
		t.Errorf("画面应说明 store 读不到")
	}
}
