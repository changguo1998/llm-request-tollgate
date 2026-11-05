// tui_sticky_test.go: sticky 页 —— 候选展开 / 偏好标记 / 按键置偏好与清除。

package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const stickyTestConfig = `{
    "listen": "127.0.0.1:8787",
    "log_path": "var/requests.jsonl",
    "upstreams": [
        { "id": "u1", "base_url": "http://u1.invalid/v1" },
        { "id": "u2", "base_url": "http://u2.invalid/v1" }
    ],
    "aliases": {
        "m1": [["u1", "real-1"], ["u2", "real-2"]],
        "m2": [["u2", "real-2"]]
    }
}`

func stickyFixture(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	if err := os.WriteFile(cfg, []byte(stickyTestConfig), 0o600); err != nil {
		t.Fatalf("写配置: %v", err)
	}
	return cfg
}

// 候选按逻辑名字典序 + 配置顺序展开, 观测面的偏好候选标 pref。
func TestStickyCandsAndMark(t *testing.T) {
	m := newTUIModel(stickyFixture(t))
	m.pulse.Sticky = map[string]stickyView{"m1": {Upstream: "u2", Model: "real-2"}}
	cands, err := m.stickyCands()
	if err != nil {
		t.Fatal(err)
	}
	want := []stickyCand{
		{"m1", "u1", "real-1", false},
		{"m1", "u2", "real-2", true},
		{"m2", "u2", "real-2", false},
	}
	if len(cands) != len(want) {
		t.Fatalf("候选数应为 %d, 得到 %+v", len(want), cands)
	}
	for i := range want {
		if cands[i] != want[i] {
			t.Errorf("第 %d 个候选应为 %+v, 得到 %+v", i, want[i], cands[i])
		}
	}

	m.tab = tabSticky
	frame := m.draw()
	if !strings.Contains(frame, "粘性偏好") || !strings.Contains(frame, "sticky") {
		t.Errorf("页面上应有标题与页签名: %q", frame)
	}
	if !strings.Contains(frame, "* u2/real-2") {
		t.Errorf("当前偏好候选应标 *")
	}
	if !strings.Contains(frame, "m1") || !strings.Contains(frame, "m2") {
		t.Errorf("应列出全部逻辑名")
	}
}

// 4 进页, j 下移, Enter 置偏好, c 清除; 写入走 POST /sticky。
func TestStickyPageKeysAndPost(t *testing.T) {
	var got []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sticky" {
			t.Errorf("应 POST /sticky, 得到 %s %s", r.Method, r.URL.Path)
		}
		got = append(got, r.URL.Query())
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	m := newTUIModel(stickyFixture(t))
	m.stateURL = srv.URL
	pending := []byte{}
	m.handleKey('4', &pending)
	if m.tab != tabSticky {
		t.Fatalf("4 应进 sticky 页, 得到 %d", m.tab)
	}
	m.handleKey('j', &pending)
	if m.stickySel != 1 {
		t.Fatalf("j 应把光标移到第二个候选, 得到 %d", m.stickySel)
	}
	m.handleKey('\r', &pending)
	if len(got) != 1 {
		t.Fatalf("Enter 应发一次 POST, 实为 %d 次", len(got))
	}
	if q := got[0]; q.Get("logical") != "m1" || q.Get("upstream") != "u2" || q.Get("model") != "real-2" {
		t.Errorf("置偏好参数不对: %v", q)
	}
	if m.stickyMsg == "" || m.stickyErr {
		t.Errorf("成功应写绿色消息: %q err=%v", m.stickyMsg, m.stickyErr)
	}

	m.handleKey('c', &pending)
	if len(got) != 2 {
		t.Fatalf("c 应再发一次 POST, 实为 %d 次", len(got))
	}
	if q := got[1]; q.Get("logical") != "m1" || q.Get("upstream") != "" {
		t.Errorf("清除只该带 logical: %v", q)
	}
	if m.stickyMsg == "" || m.stickyErr {
		t.Errorf("清除成功应写绿色消息: %q err=%v", m.stickyMsg, m.stickyErr)
	}

	// 光标上下有边界: 连按 j 停在最后一个候选, 连按 k 回到第一个。
	for i := 0; i < 10; i++ {
		m.handleKey('j', &pending)
	}
	if m.stickySel != 2 {
		t.Errorf("光标应停在最后一个候选 (2), 得到 %d", m.stickySel)
	}
	for i := 0; i < 10; i++ {
		m.handleKey('k', &pending)
	}
	if m.stickySel != 0 {
		t.Errorf("光标应停在第一个候选 (0), 得到 %d", m.stickySel)
	}
}

// 写入失败时留红色错误消息, 不 panic。
func TestStickyPagePostFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := newTUIModel(stickyFixture(t))
	m.stateURL = srv.URL
	m.stickyApply()
	if !m.stickyErr || !strings.Contains(m.stickyMsg, "置偏好失败") {
		t.Errorf("失败应写红色消息: %q err=%v", m.stickyMsg, m.stickyErr)
	}
}
