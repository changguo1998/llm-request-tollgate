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

// 候选按逻辑名字典序 + 配置顺序展开 (含组: 组名与逻辑名并列, 候选 = 成员候选拼接),
// 观测面的偏好候选标 pref。
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
		{"m2", "u2", "real-2b", false},
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

// 粘性页分组标题带倒计时列: 只显剩余时间 (mm:ss), 不带文字前缀; 失效的偏好由服务端
// 从观测面摘除, 不出现星号也不出现倒计时。
func TestStickyCountdownColumn(t *testing.T) {
	m := newTUIModel(stickyFixture(t))
	m.pulse.Sticky = map[string]stickyView{
		"m1": {Upstream: "u2", Model: "real-2", RemainingMs: 272000}, // 4 分 32 秒
	}
	m.tab = tabSticky
	frame := m.draw()
	if !strings.Contains(frame, "04:32") {
		t.Errorf("窗口内的偏好应显示剩余时长 (不带文字): %q", frame)
	}
	for _, bad := range []string{"剩余", "已失效"} {
		if strings.Contains(frame, bad) {
			t.Errorf("倒计时不应带文字前缀: %q", bad)
		}
	}
	if strings.Contains(frame, "* u2/real-2b") {
		t.Errorf("无偏好的 m2 候选不该标 *")
	}
}

// 组 (别名的别名) 在 sticky 页与逻辑名并列: 组名作逻辑名列出候选链, 也能置组偏好。
func TestStickyCandsIncludeGroups(t *testing.T) {
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
	m := newTUIModel(cfg)
	m.pulse.Sticky = map[string]stickyView{"g1": {Upstream: "u2", Model: "real-2"}}
	cands, err := m.stickyCands()
	if err != nil {
		t.Fatal(err)
	}
	// 字典序: g1 (组), m1, m2。g1 候选 = m2 + m1 拼接去重:
	// [u2/real-2b (m2), u1/real-1, u2/real-2 (m1)] —— 无跨成员重复, 全部保留。
	want := []stickyCand{
		{"g1", "u2", "real-2b", false},
		{"g1", "u1", "real-1", false},
		{"g1", "u2", "real-2", true}, // 组偏好记在组名上 (第三个候选)
		{"m1", "u1", "real-1", false},
		{"m1", "u2", "real-2", false},
		{"m2", "u2", "real-2b", false},
	}
	if len(cands) != len(want) {
		t.Fatalf("候选数应为 %d, 得到 %+v", len(want), cands)
	}
	for i := range want {
		if cands[i] != want[i] {
			t.Errorf("第 %d 个候选应为 %+v, 得到 %+v", i, want[i], cands[i])
		}
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

// 偏好页的空态与出错态: 配置读不到只报错不 panic; 没有别名时给出提示。
// 消息行按成功/失败分色。
func TestDrawStickyEmptyAndError(t *testing.T) {
	bad := newTUIModel(filepath.Join(t.TempDir(), "nope.json"))
	c := &tuiCanvas{cols: 80}
	bad.drawSticky(c, 10)
	if out := stripANSI(c.b.String()); !strings.Contains(out, "配置读不到: ") {
		t.Errorf("配置读不到时应报错:\n%s", out)
	}

	cfg := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:0","upstreams":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg)
	c = &tuiCanvas{cols: 80}
	m.drawSticky(c, 10)
	if out := stripANSI(c.b.String()); !strings.Contains(out, "(配置里没有别名)") {
		t.Errorf("没有别名时应给提示:\n%s", out)
	}

	m.stickyMsg = "已置偏好"
	m.stickyErr = false
	c = &tuiCanvas{cols: 80}
	m.drawSticky(c, 10)
	if raw := c.b.String(); !strings.Contains(raw, tuiGreen+"已置偏好"+tuiReset) {
		t.Errorf("成功消息应为绿色: %q", raw)
	}
	m.stickyErr = true
	c = &tuiCanvas{cols: 80}
	m.drawSticky(c, 10)
	if raw := c.b.String(); !strings.Contains(raw, tuiRed+"已置偏好"+tuiReset) {
		t.Errorf("失败消息应为红色: %q", raw)
	}
}

// 偏好页按显示行滚动: 光标走到底部时窗口跟着走, 只画最后几组。
func TestDrawStickyScrollWindow(t *testing.T) {
	dir := t.TempDir()
	var models []string
	for i := 0; i < 12; i++ {
		name := "a" + itoa(i)
		if i < 10 {
			name = "a0" + itoa(i) // 等宽名字, 字典序与数值序一致
		}
		models = append(models, `{"id":"r`+itoa(i)+`","alias":"`+name+`"}`)
	}
	cfg := filepath.Join(dir, "tollgate.json")
	body := `{"listen":"127.0.0.1:0","upstreams":[{"id":"u1","base_url":"http://x",
	  "models":[` + strings.Join(models, ",") + `]}]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(cfg)
	m.stickySel = 11
	const bodyRows = 8 // 标题 + 消息行, 列表窗口 6 行
	c := &tuiCanvas{cols: 80}
	m.drawSticky(c, bodyRows)
	// 显示行 = 12 个分组标题 + 12 个候选 = 24; 选中下标 11 的行在第 23 行。
	if want := 23 - 6 + 1; m.stickyTop != want {
		t.Fatalf("滚动位置应为 %d, 得到 %d", want, m.stickyTop)
	}
	out := stripANSI(c.b.String())
	for _, gone := range []string{"a00", "a08"} {
		if strings.Contains(out, gone) {
			t.Errorf("已滚出窗口的 %s 不该出现:\n%s", gone, out)
		}
	}
	for _, want := range []string{"a09", "a10", "a11"} {
		if !strings.Contains(out, want) {
			t.Errorf("窗口里应看到 %s:\n%s", want, out)
		}
	}
}
