// group_test.go: 组 (别名的别名) —— 配置校验 / 展开 / 按组名路由 / 组粘性 / TUI 展示。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 配置校验: 组名利名重名 / 成员不是已注册逻辑名 / 空组 / 组嵌套引用别的组 都报错。
// 逻辑名来自上游 models 条目的 alias 字段 (重建; 顶层 aliases 键已废弃)。
func TestGroupConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	collision := `{
	  "upstreams": [{"id": "u1", "base_url": "http://x",
	    "models": [{"id": "a", "alias": "m1"}]}],
	  "groups": {"m1": ["m1"]}
	}`
	if _, err := loadConfig(write("collision.json", collision)); err == nil {
		t.Fatal("组名与逻辑名重名应报错")
	}

	missing := `{
	  "upstreams": [{"id": "u1", "base_url": "http://x",
	    "models": [{"id": "a", "alias": "m1"}]}],
	  "groups": {"g1": ["m1", "ghost"]}
	}`
	if _, err := loadConfig(write("missing.json", missing)); err == nil {
		t.Fatal("组成员不是已注册逻辑名应报错")
	}

	empty := `{
	  "upstreams": [{"id": "u1", "base_url": "http://x"}],
	  "groups": {"g1": []}
	}`
	if _, err := loadConfig(write("empty.json", empty)); err == nil {
		t.Fatal("空组应报错")
	}

	nested := `{
	  "upstreams": [{"id": "u1", "base_url": "http://x",
	    "models": [{"id": "a", "alias": "m1"}]}],
	  "groups": {"g1": ["m1"], "g2": ["g1"]}
	}`
	if _, err := loadConfig(write("nested.json", nested)); err == nil {
		t.Fatal("组引用别的组应报错 (禁止嵌套)")
	}

	ok := `{
	  "upstreams": [{"id": "u1", "base_url": "http://x",
	    "models": [{"id": "a", "alias": "m1"}, {"id": "b", "alias": "m2"}]}],
	  "groups": {"g1": ["m2", "m1"]}
	}`
	cfg, err := loadConfig(write("ok.json", ok))
	if err != nil {
		t.Fatalf("合法配置应照常加载: %v", err)
	}
	if got := cfg.Groups["g1"]; len(got) != 2 || got[0] != "m2" || got[1] != "m1" {
		t.Fatalf("组成员顺序应保留配置顺序, 实为 %#v", got)
	}
	chain, has := cfg.Aliases["m1"]
	if !has || len(chain) != 1 || chain[0][0] != "u1" || chain[0][1] != "a" {
		t.Fatalf("候选表应从 models 的 alias 重建, 实为 %#v", cfg.Aliases)
	}
}

// 展开: 组候选链 = 成员候选链按序拼接, 跨成员去重 (同一 上游/模型 只留第一次)。
func TestGroupExpandedAliases(t *testing.T) {
	c := &Config{
		Aliases: map[string][][2]string{
			"m1": {{"u1", "a"}, {"u2", "x"}},
			"m2": {{"u2", "x"}, {"u1", "b"}},
			"m3": {{"u3", "c"}},
		},
		Groups: map[string][]string{"g1": {"m2", "m1", "m3"}},
	}
	exp := c.expandedAliases()
	if got := exp["m1"]; len(got) != 2 {
		t.Fatalf("普通逻辑名的候选应原样保留, 实为 %#v", got)
	}
	want := [][2]string{{"u2", "x"}, {"u1", "b"}, {"u1", "a"}, {"u3", "c"}}
	got := exp["g1"]
	if len(got) != len(want) {
		t.Fatalf("组候选应为 %d 个, 实为 %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("组候选第 %d 个应为 %v, 实为 %v", i, want[i], got[i])
		}
	}
}

// 从新格式配置 (无顶层 aliases, 逻辑名在 models[].alias) 加载即重建候选表:
// 没有显式别名时必须从模型 id 兜底, 别名跨上游归并, 负优先级 (模型 / 整家) 停用。
func TestRebuildAliasesFromModelsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tollgate.json")
	body := `{
	  "upstreams": [
	    {"id": "u1", "base_url": "http://x", "priority": 1,
	     "models": [
	       {"id": "a", "priority": 2},
	       {"id": "b", "priority": 0, "alias": "shared"},
	       {"id": "dead", "priority": -1, "alias": "shared"}
	     ]},
	    {"id": "u2", "base_url": "http://x", "priority": 2,
	     "models": [
	       {"id": "c", "priority": 2, "alias": "shared"}
	     ]}
	  ]
	}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("新格式配置应照常加载: %v", err)
	}
	if _, has := cfg.Aliases["a"]; !has {
		t.Fatalf("无 alias 的模型应用模型 id 兜底为逻辑名: %#v", cfg.Aliases)
	}
	shared := cfg.Aliases["shared"]
	// u1/b (模型 prio 0) + u2/c (模型 prio 2): c 在前; dead 负优先级停用。
	if len(shared) != 2 || shared[0] != [2]string{"u2", "c"} || shared[1] != [2]string{"u1", "b"} {
		t.Fatalf("跨上游归并 + 按 priority 排序 + 停用剔除失败: %#v", shared)
	}
	if _, has := cfg.Aliases["dead"]; has {
		t.Fatalf("负优先级模型不应有逻辑名条目: %#v", cfg.Aliases)
	}
}

// 组 (别名的别名) 在 settings 页只读列出: 组名 -> 成员逻辑名, 出现在模型列表末尾。
func TestSettingsShowsGroups(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "llm-models.json")
	cfg := filepath.Join(dir, "tollgate.json")
	if err := os.WriteFile(store, []byte(`{
	  "providers": {
	    "p1": {"baseUrl": "http://p1.invalid/v1", "models": [
	      {"id": "a"}, {"id": "b"}
	    ]}
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{
	  "listen": "127.0.0.1:8787",
	  "log_path": "var/requests.jsonl",
	  "upstreams": [
	    {"id": "p1", "base_url": "http://p1.invalid/v1", "priority": 0,
	     "models": [{"id": "a", "priority": 0, "alias": "a"}, {"id": "b", "priority": 0, "alias": "b"}]}
	  ],
	  "groups": {"worker": ["a", "b"]}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg)
	m.prio = newTuiEditor(cfg)
	m.tab = tabSettings
	frame := stripANSI(m.draw())
	if !strings.Contains(frame, "组 (别名的别名") || !strings.Contains(frame, "worker") ||
		!strings.Contains(frame, "a, b") {
		t.Fatalf("settings 页应列出组与成员, 得到:\n%s", frame)
	}
}

// 端到端: 按组名请求 -> 落到成员的候选上, 上游收到真实模型 id, 响应 model 改回组名,
// 日志 logical 记组名。
func TestGroupRequestRoutesAndRewrites(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		w.Header().Set("Content-Type", "application/json")
		// model 回显候选对应的真实 id (组第一个成员 m2 的候选 u1/a)。
		_, _ = io.WriteString(w, `{"model":"a","choices":[{"message":{"role":"assistant",`+
			`"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer up.Close()
	cfg := &Config{
		Upstreams: []UpstreamCfg{{
			ID: "u1", BaseURL: up.URL, AuthHeader: "Authorization", Key: "k",
			IncludeUsage: true,
		}},
		Aliases: map[string][][2]string{
			"m1": {{"u1", "a"}},
			"m2": {{"u1", "a"}},
		},
		Groups: map[string][]string{"g1": {"m2", "m1"}},
	}
	app := newTestAppCfg(t, cfg)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	code, body := postJSON(t, proxy.URL+"/v1/chat/completions",
		`{"model":"g1","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("按组名请求应成功, 实为 %d %s", code, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["model"] != "g1" {
		t.Fatalf("响应里的 model 应改回组名, 实为 %v", out["model"])
	}
	rec := lastLogRec(t, app)
	if rec.Logical != "g1" || rec.Upstream != "u1" || rec.Model != "a" {
		t.Fatalf("日志应记 logical=g1 上游=u1 模型=a, 实为 %+v", rec)
	}
	if rec.Status != http.StatusOK || rec.Usage == nil || rec.Usage.Total != 5 {
		t.Fatalf("日志用量不符: %+v", rec)
	}
}

// /v1/models 应把组名与逻辑名一并列出。
func TestGroupListedInModels(t *testing.T) {
	cfg := &Config{
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://127.0.0.1:1/v1", Key: "k"}},
		Aliases:   map[string][][2]string{"m1": {{"u1", "a"}}, "m2": {{"u1", "b"}}},
		Groups:    map[string][]string{"g1": {"m2", "m1"}},
	}
	app := newTestAppCfg(t, cfg)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list struct {
		Data []map[string]string `json:"data"`
	}
	_ = json.Unmarshal(b, &list)
	got := map[string]bool{}
	for _, d := range list.Data {
		got[d["id"]] = true
	}
	if !got["m1"] || !got["m2"] || !got["g1"] {
		t.Fatalf("/v1/models 应含逻辑名与组名, 实为 %+v", got)
	}
}

// 组粘性: 组名与逻辑名一样有自己的偏好条目 —— POST /sticky 能对组名置偏好,
// 置后组请求优先走偏好候选 (prefIdx 返回偏好的下标), 成功服务后偏好保留。
func TestGroupStickyHonored(t *testing.T) {
	var hitsA, hitsB int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		if payload["model"] == "bad" {
			hitsA++
		} else {
			hitsB++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+payload["model"].(string)+`","usage":{"total_tokens":1}}`)
	}))
	defer up.Close()
	cfg := &Config{
		Upstreams: []UpstreamCfg{
			{ID: "u1", BaseURL: up.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true},
			{ID: "u2", BaseURL: up.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true},
		},
		Aliases: map[string][][2]string{
			"bad": {{"u1", "bad"}},
			"ok":  {{"u2", "ok"}},
		},
		Groups: map[string][]string{"g1": {"bad", "ok"}},
	}
	app := appWithGate(t, cfg, GateCfg{IntervalMs: 10, SemWaitMs: 3000, QueueWaitMs: 3000})
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	// 人工把组 g1 的偏好置到第二个候选 (ok), 应以组名记自己的条目。
	code, body := postJSON(t, proxy.URL+"/sticky?logical=g1&upstream=u2&model=ok", ``)
	if code != http.StatusOK {
		t.Fatalf("POST /sticky 应成功置组偏好, 实为 %d %s", code, body)
	}
	app.sticky.mu.Lock()
	p, ok := app.sticky.prefs["g1"]
	app.sticky.mu.Unlock()
	if !ok || p.pref != 1 {
		t.Fatalf("组应有自己的偏好条目且指向第二个候选, 实为 %+v ok=%v", p, ok)
	}

	// 偏好候选排在首位: 组请求应优先走 ok (u2), 不再从配置首候选 bad 开始。
	code, body = postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"g1","messages":[]}`)
	if code != http.StatusOK {
		t.Fatalf("组请求应成功, 实为 %d %s", code, body)
	}
	if hitsA != 0 || hitsB == 0 {
		t.Fatalf("组请求应粘在偏好候选 ok 上 (bad=%d ok=%d)", hitsA, hitsB)
	}
	// 成功服务后偏好保留 (不因落在偏好候选上被清掉)。
	app.sticky.mu.Lock()
	p, ok = app.sticky.prefs["g1"]
	app.sticky.mu.Unlock()
	if !ok || p.pref != 1 {
		t.Fatalf("成功服务后组偏好应保留, 实为 %+v ok=%v", p, ok)
	}
}
