// tui_sticky_test.go: 粘性偏好合并进 settings 页模型列表 —— 候选展开 / 偏好标记 /
// 倒计时 / Enter 置偏好与 c 清除 (别名与组同款)。

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// 候选行: settings 模型列表按逻辑名分组, 当前偏好候选行标 * (别名行与组行同款),
// 分组标题带倒计时 (mm:ss, 不带文字前缀)。
func TestSettingsStickyMarkAndCountdown(t *testing.T) {
	cfg := stickyFixture(t)
	stickyStore(t, cfg)
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	m.pulse.Sticky = map[string]stickyView{
		"m1": {Upstream: "u2", Model: "real-2", RemainingMs: 272000}, // 4 分 32 秒
	}
	frame := m.draw()
	if !strings.Contains(frame, "*") {
		t.Errorf("偏好候选行应标 *: %q", frame)
	}
	if !strings.Contains(frame, "04:32") {
		t.Errorf("分组标题应显示剩余时长 (不带文字): %q", frame)
	}
	if strings.Contains(frame, "剩余") || strings.Contains(frame, "已失效") {
		t.Errorf("倒计时不应带文字前缀: %q", frame)
	}
	if !strings.Contains(frame, "m1") || !strings.Contains(frame, "m2") {
		t.Errorf("应列出全部逻辑名: %q", frame)
	}
	// settings 页仍在 (页签只 3 个, 无 sticky)。
	if strings.Contains(frame, "sticky") {
		t.Errorf("页签里不该再有 sticky: %q", frame)
	}
}

// 组 (别名的别名) 与别名并列出现在模型列表: 组候选 = 成员候选拼接去重, 也能标 * 与倒计时。
func TestSettingsStickyIncludeGroups(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(`{
	    "listen": "127.0.0.1:8787",
	    "log_path": "var/requests.jsonl",
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
	frame := m.draw()
	// 组区与别名区分开显示: 组名标题带「组」字, 组候选行可选中。
	if !strings.Contains(frame, "组 g1") {
		t.Errorf("组名标题应有组字区分: %q", frame)
	}
	if !strings.Contains(frame, "u2/real-2") {
		t.Errorf("组候选链应含 u2/real-2: %q", frame)
	}
}

// Enter 在 settings 模型面板把当前行置为粘性偏好 (POST /sticky), c 清除; 失败写消息。
func TestSettingsStickyKeysAndPost(t *testing.T) {
	var gotURLs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURLs = append(gotURLs, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := stickyFixture(t)
	stickyStore(t, cfg)
	m := newTUIModel(cfg)
	m.stateURL = srv.URL
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	pending := []byte{}
	send := func(b byte) {
		if quit := m.handleKey(b, &pending); quit {
			t.Fatalf("不该退出")
		}
	}
	// 选中第一行 (m1 的第一个候选)。j 移到下一行后 Enter = 置 m1 的第二偏好。
	m.prio.modelSel = 1
	send('\r')
	if len(gotURLs) != 1 || !strings.Contains(gotURLs[0], "logical=m1") ||
		!strings.Contains(gotURLs[0], "upstream=u2") || !strings.Contains(gotURLs[0], "model=real-2") {
		t.Fatalf("Enter 应 POST /sticky?logical=m1&upstream=u2&model=real-2, 实为 %v", gotURLs)
	}
	send('c')
	if len(gotURLs) != 2 || !strings.Contains(gotURLs[1], "logical=m1") ||
		strings.Contains(gotURLs[1], "upstream=") {
		t.Fatalf("c 应 POST 清除 (只带 logical), 实为 %v", gotURLs)
	}

	// 失败: 服务端返回非 200, 消息行写错误。
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad candidate"))
	}))
	defer failSrv.Close()
	m2 := newTUIModel(cfg)
	m2.stateURL = failSrv.URL
	m2.prio = newTuiEditor(cfg)
	m2.tab = tabSettings
	m2.handleKey('\r', &pending)
	if !strings.Contains(m2.prio.message, "置偏好失败") {
		t.Errorf("失败应写消息: %q", m2.prio.message)
	}
}

// 组候选行也能 Enter 置偏好 / c 清除: 光标能落在组区。
func TestSettingsStickyKeysOnGroup(t *testing.T) {
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
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newTUIModel(cfg)
	m.stateURL = srv.URL
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	if len(m.prio.cands()) != 2 { // m1 的 1 个候选 + 组 g1 的 1 个候选
		t.Fatalf("cands 应含别名与组候选, 实得 %d", len(m.prio.cands()))
	}
	m.prio.modelSel = 1 // 组候选行
	pending := []byte{}
	m.handleKey('\r', &pending)
	if len(got) != 1 || !strings.Contains(got[0], "logical=g1") ||
		!strings.Contains(got[0], "upstream=u1") || !strings.Contains(got[0], "model=real-1") {
		t.Fatalf("Enter 应对组名置偏好, 实为 %v", got)
	}
	m.handleKey('c', &pending)
	if len(got) != 2 || !strings.Contains(got[1], "logical=g1") {
		t.Fatalf("c 应清除组偏好, 实为 %v", got)
	}
}

// 组只列候选不参与权重: 组行上 +/- 不改任何优先级。
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
