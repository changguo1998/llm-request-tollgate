// tui_sticky_test.go: 粘性偏好与解锁都在第 1 页 —— 模型表按逻辑名分组 (标题带剩余
// 倒计时, 只列最近用过的逻辑名), 偏好候选行标 *, Enter 置偏好 / c 清除, u 解锁;
// settings 页只管配置, 倒计时 / * / Enter / c 都不在那里。

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stickyTestConfig = `{
    "listen": "127.0.0.1:8787",
    "log_path": "var/requests.jsonl",
    "upstreams": [
        { "id": "u1", "base_url": "http://u1.invalid/v1", "models": [
            {"id": "real-1", "alias": "m1"}
        ]},
        { "id": "u2", "base_url": "http://u2.invalid/v1", "models": [
            {"id": "real-2", "alias": "m1"}, {"id": "real-2b", "alias": "m2"}
        ]}
    ]
}`

func stickyFixture(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(stickyTestConfig), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return cfg
}

// settings 模型列表的知识: store 在配置同目录 (llm-models.json 缺省不存在时退回
// FFF_HOME / ~/fff, 会读到真环境), 这里给一份与配置同形的 store。
func stickyStore(t *testing.T, cfg string) {
	t.Helper()
	store := filepath.Join(filepath.Dir(cfg), "llm-models.json")
	if err := os.WriteFile(store, []byte(`{
	  "providers": {
	    "u1": {"baseUrl": "http://u1.invalid/v1", "models": [{"id": "real-1"}]},
	    "u2": {"baseUrl": "http://u2.invalid/v1", "models": [{"id": "real-2"}, {"id": "real-2b"}]}
	  }
	}`), 0o600); err != nil {
		t.Fatalf("写 store: %v", err)
	}
}

// flowFixture 给第 1 页准备两行日志聚合 (m1/u1/real-1 更新, m2/u2/real-2 稍旧) 与
// 两个上游 (u1 声明了一个命名记账组 g1)。
func flowFixture(t *testing.T) *tuiModel {
	t.Helper()
	cfg := stickyFixture(t)
	stickyStore(t, cfg)
	m := newTUIModel(cfg)
	m.tab = tabActive
	m.rateWin, m.window = time.Hour, 3*time.Hour
	now := time.Now()
	m.routes = map[string]*routeRow{
		"m1\x00u1\x00real-1": {Logical: "m1", Upstream: "u1", Model: "real-1",
			Reqs: 2, Total: 100, Last: now},
		"m2\x00u2\x00real-2": {Logical: "m2", Upstream: "u2", Model: "real-2",
			Reqs: 1, Total: 50, Last: now.Add(-time.Minute)},
	}
	m.pulse.Cands = []candView{
		{ID: "u1", SoftCap: 2, Groups: []candGroup{{Name: defaultGroup}, {Name: "g1"}}},
		{ID: "u2", SoftCap: 2, Groups: []candGroup{{Name: defaultGroup}}},
	}
	return m
}

// 第 1 页模型表: 每组先来一行逻辑名标题, 有粘性偏好时接剩余倒计时 (mm:ss, 不带文字),
// 偏好候选行标 *; 表里不再用空行分组。
func TestModelTableGroupHeaderAndMark(t *testing.T) {
	m := flowFixture(t)
	m.pulse.Sticky = map[string]stickyView{
		"m1": {Upstream: "u1", Model: "real-1", RemainingMs: 272000}, // 4 分 32 秒
	}
	out := stripANSI(m.draw())
	if !strings.Contains(out, "m1  04:32") {
		t.Errorf("m1 组标题应带倒计时 04:32: %q", out)
	}
	if !strings.Contains(out, "*u1/real-1") {
		t.Errorf("偏好候选应标 *: %q", out)
	}
	if strings.Contains(out, "*u2/real-2") {
		t.Errorf("非偏好候选不该标 *: %q", out)
	}
	if strings.Contains(out, "剩余") || strings.Contains(out, "已失效") {
		t.Errorf("倒计时不应带文字前缀: %q", out)
	}
	// 标题紧跟第一行: 组与组之间不再有空行。
	lines := strings.Split(out, "\r\n")
	hdr, row := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "m1  04:32") {
			hdr = i
		}
		if strings.Contains(ln, "u1/real-1") {
			row = i
		}
	}
	if hdr < 0 || row != hdr+1 {
		t.Errorf("m1 标题 (行 %d) 后面应紧跟它的行 (行 %d):\n%s", hdr+1, row+1, out)
	}
}

// 只列最近用过的逻辑名: 配置里有、但窗口内没有请求的逻辑名不会出现在模型表里
// (也不会多出一行空标题)。
func TestModelTableOnlyRecentLogicals(t *testing.T) {
	m := flowFixture(t)
	// 只留 m1 的行; m2 在配置与 store 里还在, 但没有最近数据。
	delete(m.routes, "m2\x00u2\x00real-2")
	out := stripANSI(m.draw())
	if !strings.Contains(out, "m1") {
		t.Errorf("最近用过的逻辑名应有标题: %q", out)
	}
	if strings.Contains(out, "m2") {
		t.Errorf("没用过的逻辑名不该出现: %q", out)
	}
}

// settings 页只管配置: 粘性倒计时 / * 标记都不在模型面板, Enter / c 也不再写服务。
func TestSettingsPageDropsSticky(t *testing.T) {
	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := flowFixture(t)
	m.prio = newTuiEditor(m.cfgPath)
	m.tab = tabSettings
	m.stateURL = srv.URL
	m.pulse.Sticky = map[string]stickyView{
		"m1": {Upstream: "u1", Model: "real-1", RemainingMs: 272000},
	}
	out := stripANSI(m.draw())
	if strings.Contains(out, "04:32") {
		t.Errorf("settings 页不该再画粘性倒计时: %q", out)
	}
	if strings.Contains(out, "* ") || strings.Contains(out, "*u1/real-1") {
		t.Errorf("settings 页不该再标偏好候选: %q", out)
	}
	pending := []byte{}
	m.handleKey('\r', &pending)
	m.handleKey('c', &pending)
	if posted != 0 {
		t.Errorf("settings 页的 Enter / c 不该写服务, 实发 %d 次", posted)
	}
}

// 组 (别名的别名) 仍是配置内容: settings 模型面板照旧列出成员逻辑名 (不展开上游),
// 组行不带权重; 粘性偏好已归第 1 页, 这里不再标星。
func TestSettingsModelPanelListsGroups(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{
	    "listen": "127.0.0.1:8787",
	    "upstreams": [
	        { "id": "u1", "base_url": "http://u1.invalid/v1", "models": [
	            {"id": "real-1", "alias": "m1"}
	        ]},
	        { "id": "u2", "base_url": "http://u2.invalid/v1", "models": [
	            {"id": "real-2", "alias": "m1"}, {"id": "real-2b", "alias": "m2"}
	        ]}
	    ],
	    "groups": {
	        "g1": ["m2", "m1"]
	    }
	}`), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	stickyStore(t, cfg)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	m.pulse.Sticky = map[string]stickyView{"g1": {Upstream: "u2", Model: "real-2"}}
	s := stripANSI(m.draw())
	// 组名标题不带「组」字 (靠黄色区分), 组候选只列成员逻辑名 (不展开上游)。
	if !strings.Contains(s, "g1") || strings.Contains(s, "组 g1") {
		t.Errorf("组名标题不应带组字: %q", s)
	}
	if !strings.Contains(s, "  m2") || !strings.Contains(s, "  m1") {
		t.Errorf("组候选应列成员逻辑名: %q", s)
	}
	if strings.Contains(s, "*") {
		t.Errorf("settings 页不该再有偏好星号: %q", s)
	}
}

// 组行只列成员、不参与权重: 光标落在组行上 +/- 不改任何别名优先级。
func TestSettingsStickyGroupNoWeights(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{
	    "listen": "127.0.0.1:8787",
	    "upstreams": [
	        { "id": "u1", "base_url": "http://u1.invalid/v1", "models": [
	            {"id": "real-1", "alias": "m1"}
	        ]}
	    ],
	    "groups": {"g1": ["m1"]}
	}`), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	stickyStore(t, cfg)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	m.prio.focus = 0
	m.prio.modelSel = 1 // 组候选行
	if r := m.prio.selected(); r != nil {
		t.Fatalf("组候选行不该有可编辑的权重行, 实得 %+v", r)
	}
	m.prio.adjust(3)
	if rows := m.prio.filtered(); len(rows) != 1 || prioOf(rows[0].model) != 0 {
		t.Fatalf("组行上调权重不该改别名行: %+v", rows)
	}
}

// Enter 把光标所在的模型行置为该逻辑名的粘性偏好 (POST /sticky), c 清除; 失败写反馈行。
// 逻辑名是组名时同样可以置 (组名对客户端与逻辑名无差别)。
func TestFlowStickyKeysPost(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := flowFixture(t)
	m.stateURL = srv.URL
	m.cur = flowCursor{table: 1, logical: "m2", up: "u2", model: "real-2"}
	pending := []byte{}
	send := func(b byte) {
		if quit := m.handleKey(b, &pending); quit {
			t.Fatalf("不该退出")
		}
	}
	send('\r')
	if len(got) != 1 || !strings.Contains(got[0], "logical=m2") ||
		!strings.Contains(got[0], "upstream=u2") || !strings.Contains(got[0], "model=real-2") {
		t.Fatalf("Enter 应 POST /sticky?logical=m2&upstream=u2&model=real-2, 实为 %v", got)
	}
	if !strings.Contains(m.flowMsg, "已置 m2 -> u2/real-2") {
		t.Errorf("成功应写反馈行: %q", m.flowMsg)
	}
	send('c')
	if len(got) != 2 || !strings.Contains(got[1], "logical=m2") ||
		strings.Contains(got[1], "upstream=") {
		t.Fatalf("c 应 POST 清除 (只带 logical), 实为 %v", got)
	}

	// 组名也是逻辑名: 日志里按组名记的行照样能置偏好。
	m.routes["g1\x00u1\x00real-1"] = &routeRow{Logical: "g1", Upstream: "u1", Model: "real-1",
		Reqs: 1, Last: time.Now()}
	m.cur = flowCursor{table: 1, logical: "g1", up: "u1", model: "real-1"}
	send('\r')
	if len(got) != 3 || !strings.Contains(got[2], "logical=g1") {
		t.Fatalf("组名也应能置偏好, 实为 %v", got)
	}

	// 失败: 服务端返回非 200, 反馈行写错误。
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad candidate"))
	}))
	defer failSrv.Close()
	mf := flowFixture(t)
	mf.stateURL = failSrv.URL
	mf.cur = flowCursor{table: 1, logical: "m1", up: "u1", model: "real-1"}
	mf.handleKey('\r', &pending)
	if !strings.Contains(mf.flowMsg, "置偏好失败") || !mf.flowErr {
		t.Errorf("失败应写反馈行: %q (err=%v)", mf.flowMsg, mf.flowErr)
	}
}

// u 解锁光标处的锁: 上游名 = 该上游全部锁 (不带 model/group), 命名组 = 只带 group,
// 默认组写 group=def, 模型行 = upstream + model。
func TestFlowUnlockPost(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"cleared":2}`))
	}))
	defer srv.Close()

	m := flowFixture(t)
	m.stateURL = srv.URL
	m.pulse.Cands[0].Groups = []candGroup{
		{Name: defaultGroup, LockMs: 60000}, {Name: "g1", LockMs: 60000},
	}
	pending := []byte{}
	send := func() {
		if quit := m.handleKey('u', &pending); quit {
			t.Fatalf("不该退出")
		}
	}
	m.cur = flowCursor{table: 0, up: "u1", name: true}
	send()
	if len(got) != 1 || got[0] != "upstream=u1" {
		t.Fatalf("上游名应只带 upstream, 实为 %v", got)
	}
	m.cur = flowCursor{table: 0, up: "u1", group: "g1"}
	send()
	if len(got) != 2 || got[1] != "group=g1&upstream=u1" {
		t.Fatalf("命名组应带 upstream+group, 实为 %v", got)
	}
	m.cur = flowCursor{table: 0, up: "u1"} // 默认组 (group 是空串)
	send()
	if len(got) != 3 || !strings.Contains(got[2], "group=def") {
		t.Fatalf("默认组应写 group=def, 实为 %v", got)
	}
	m.cur = flowCursor{table: 1, logical: "m1", up: "u1", model: "real-1"}
	send()
	if len(got) != 4 || !strings.Contains(got[3], "upstream=u1") ||
		!strings.Contains(got[3], "model=real-1") || strings.Contains(got[3], "group=") {
		t.Fatalf("模型行应带 upstream+model, 实为 %v", got)
	}
	if !strings.Contains(m.flowMsg, "已解锁 m1") && !strings.Contains(m.flowMsg, "已解锁 u1/real-1") {
		t.Errorf("成功应写反馈行: %q", m.flowMsg)
	}
}

// 光标移动: 上游表与模型表按页面顺序连着走, 边界停住; h/l 在上游行里换目标格
// (上游名 / 默认组 / 命名组), 模型行只有一格。
func TestFlowCursorMoves(t *testing.T) {
	m := flowFixture(t)
	m.flowMove(1) // 初始在 u1 上游名 -> u2
	if m.cur.table != 0 || m.cur.up != "u2" || !m.cur.name {
		t.Fatalf("j 应到 u2 的上游名: %+v", m.cur)
	}
	m.flowMove(1) // -> 第一行模型 (m1/u1/real-1)
	if m.cur.table != 1 || m.cur.logical != "m1" || m.cur.model != "real-1" {
		t.Fatalf("j 应从上游表走到模型表第一行: %+v", m.cur)
	}
	m.flowMove(1) // -> 第二行模型
	if m.cur.logical != "m2" || m.cur.model != "real-2" {
		t.Fatalf("j 应到第二行模型: %+v", m.cur)
	}
	m.flowMove(1) // 边界: 停住
	if m.cur.logical != "m2" {
		t.Fatalf("最后一行再 j 应停住: %+v", m.cur)
	}
	m.flowMove(-1) // 回到第一行模型
	if m.cur.logical != "m1" {
		t.Fatalf("k 应回到第一行模型: %+v", m.cur)
	}
	m.flowMove(-1) // 回到上游表末行
	if m.cur.table != 0 || m.cur.up != "u2" || !m.cur.name {
		t.Fatalf("模型表首行再 k 应回上游表末行: %+v", m.cur)
	}

	// h/l: u1 行有 默认组 + 命名组 g1, 光标在上游名 -> 默认组 -> g1 -> 不动。
	m.cur = flowCursor{table: 0, up: "u1", name: true}
	m.flowCol(1)
	if m.cur.name || m.cur.group != defaultGroup {
		t.Fatalf("l 应到默认组: %+v", m.cur)
	}
	m.flowCol(1)
	if m.cur.name || m.cur.group != "g1" {
		t.Fatalf("l 再到 g1: %+v", m.cur)
	}
	m.flowCol(1)
	if m.cur.group != "g1" {
		t.Fatalf("最后一格再 l 应停住: %+v", m.cur)
	}
	m.flowCol(-2)
	if !m.cur.name {
		t.Fatalf("h 应回到上游名: %+v", m.cur)
	}
	// 模型行 h/l 不动 (只有 上游/模型 一格)。
	m.cur = flowCursor{table: 1, logical: "m1", up: "u1", model: "real-1"}
	m.flowCol(1)
	if m.cur.table != 1 || m.cur.logical != "m1" {
		t.Fatalf("模型行 h/l 不该动: %+v", m.cur)
	}
}

// 光标锚在行内容上: 组的最近时间变了 (组序重排) 光标仍钉在原行, 不会被行号带偏。
func TestFlowCursorAnchorsRow(t *testing.T) {
	m := flowFixture(t)
	m.cur = flowCursor{table: 1, logical: "m2", up: "u2", model: "real-2"}
	m.routes["m1\x00u1\x00real-1"].Last = time.Now().Add(time.Minute) // m1 组更新
	_ = m.draw()
	if m.cur.table != 1 || m.cur.logical != "m2" || m.cur.model != "real-2" {
		t.Fatalf("行序变了光标应跟着内容: %+v", m.cur)
	}
	// 锚点行不在了: 退到同表第一行。
	delete(m.routes, "m2\x00u2\x00real-2")
	_ = m.draw()
	if m.cur.table != 1 || m.cur.logical != "m1" {
		t.Fatalf("锚点行没了应退到同表首行: %+v", m.cur)
	}
}

// 光标格反显: 上游表选中记账组、模型表选中 上游/模型 时, 该格以反显开头 (格内颜色
// 保留, 每个 reset 后补一次反显)。
func TestFlowCursorHighlight(t *testing.T) {
	m := flowFixture(t)
	m.cur = flowCursor{table: 0, up: "u1", group: "g1"}
	raw := m.draw()
	if !strings.Contains(raw, tuiRev+tuiGreen+"g1") {
		t.Errorf("选中的记账组应反显 (带组色): %q", raw)
	}

	m.cur = flowCursor{table: 1, logical: "m1", up: "u1", model: "real-1"}
	raw = m.draw()
	if !strings.Contains(raw, tuiRev+" "+m.upColors["u1"]+"u1") {
		t.Errorf("选中的模型行应反显 上游/模型 格: %q", raw)
	}
}
