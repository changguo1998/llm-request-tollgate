package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseDur 解析 -usage 的窗口参数: 数字 + s / m / min / h / d, 其它一律不认。
func TestParseDur(t *testing.T) {
	ok := map[string]time.Duration{
		"30s":   30 * time.Second,
		"90m":   90 * time.Minute,
		"12min": 12 * time.Minute,
		"3h":    3 * time.Hour,
		"7d":    7 * 24 * time.Hour,
	}
	for in, want := range ok {
		if got, valid := parseDur(in); !valid || got != want {
			t.Errorf("%q 应解析成 %v, 得到 %v (ok=%v)", in, want, got, valid)
		}
	}
	for _, in := range []string{"", "h", "3", "3x", "0h", "-3h", "3 h", "m5"} {
		if got, valid := parseDur(in); valid {
			t.Errorf("%q 不该解析成功, 得到 %v", in, got)
		}
	}
}

// logFilesFor: 传目录列出目录下全部 *.jsonl (按名排序); 传文件时只看当前文件与窗口内的归档。
func TestLogFilesFor(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.jsonl", "a.jsonl", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := logFilesFor(dir, time.Hour)
	if len(got) != 2 || filepath.Base(got[0]) != "a.jsonl" || filepath.Base(got[1]) != "b.jsonl" {
		t.Errorf("目录模式应按名排序列出 *.jsonl, 得到 %v", got)
	}

	// 文件模式: 当前文件为空时不算, 写进内容后算; 窗口内的归档算, 太老的不算。
	cur := filepath.Join(dir, "requests.jsonl")
	if err := os.WriteFile(cur, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	recent := filepath.Join(dir, "requests-2026-10-30.jsonl")
	old := filepath.Join(dir, "requests-2026-10-01.jsonl")
	for _, p := range []string{recent, old} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, time.Now().Add(-72*time.Hour), time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	files := logFilesFor(cur, 6*time.Hour)
	if len(files) != 1 || filepath.Base(files[0]) != "requests-2026-10-30.jsonl" {
		t.Errorf("空当前文件不该列出, 老归档也该排除, 得到 %v", files)
	}
	if err := os.WriteFile(cur, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files = logFilesFor(cur, 6*time.Hour)
	if len(files) != 2 || filepath.Base(files[0]) != "requests.jsonl" {
		t.Errorf("当前文件应排在最前, 得到 %v", files)
	}
}

// archivePath: 归档名按日期, 同名时追加 -1 / -2, 不覆盖已有归档。
func TestArchivePath(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "requests.jsonl")
	day := "2026-10-12"
	want := filepath.Join(dir, "requests-"+day+".jsonl")
	if got := archivePath(cur, day); got != want {
		t.Errorf("首个归档名应为 %q, 得到 %q", want, got)
	}
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		got := archivePath(cur, day)
		if base := filepath.Base(got); !strings.HasSuffix(base, "-"+itoa(i)+".jsonl") {
			t.Errorf("第 %d 次冲突应追加 -%d, 得到 %q", i, i, base)
		}
		if err := os.WriteFile(got, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// rewriteModelKnown 把已知的真实模型 id 换回逻辑名 (用于嵌套在 response 对象里的 model)。
func TestRewriteModelKnown(t *testing.T) {
	in := []byte(`{"type":"response.created","response":{"model":"real-1"},"model":"real-1"}`)
	// 一次调用只换最先出现的那一处 (嵌套在 response 里的 model)。
	out := string(rewriteModelKnown(in, "real-1", "m1"))
	if strings.Count(out, `"m1"`) != 1 || strings.Count(out, `"real-1"`) != 1 {
		t.Errorf("应只换第一处 model, 得到 %s", out)
	}
	if !strings.Contains(out, `"response":{"model":"m1"}`) {
		t.Errorf("应换嵌套在 response 里的那一处, 得到 %s", out)
	}
	// 再调一次把剩下那处也换掉。
	if again := string(rewriteModelKnown([]byte(out), "real-1", "m1")); strings.Contains(again, `"real-1"`) {
		t.Errorf("第二次调用应换掉剩下的 model, 得到 %s", again)
	}
	// 逻辑名与真实 id 相同 (或变量为空 / 含转义) 时原样返回。
	for _, c := range [][3]string{
		{`{"model":"real-1"}`, "real-1", "real-1"},
		{`{"model":"real-1"}`, "real-1", ""},
		{`{"model":"real-1"}`, "", "m1"},
		{`{"model":"re\"al"}`, `re\"al`, "m1"},
	} {
		if got := string(rewriteModelKnown([]byte(c[0]), c[1], c[2])); got != c[0] {
			t.Errorf("real=%q logical=%q 时应原样返回, 得到 %s", c[1], c[2], got)
		}
	}
}

// flowLive 只写展示字段 (已收到 token 数 / 首字节时刻), 不碰阶段; 未知编号或已结束的请求忽略。
func TestFlowLive(t *testing.T) {
	a, err := newApp(&Config{Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl")})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()
	id := a.flowStart("m1", "agent", "/v1/chat/completions", true)
	a.flowStage(id, "up1", "real-m1", stageRecv, "", 1)
	first := time.Now().Add(-2 * time.Second)
	a.flowLive(id, 42, 2048, first)

	f := a.flowActives()[0]
	if f.RecvTok != 42 || f.RecvBytes != 2048 || f.RecvStart != first.UnixMilli() {
		t.Errorf("展示字段没写进去: %+v", f)
	}
	if f.Stage != stageRecv {
		t.Errorf("flowLive 不该改阶段, 得到 %d", f.Stage)
	}
	a.flowLive(9999, 1, 1, time.Now()) // 未知编号: 什么都不做
	a.flowEnd(id)
	a.flowLive(id, 7, 7, time.Now()) // 已结束: 同样忽略
	if got := len(a.flowActives()); got != 0 {
		t.Errorf("结束后不该再有进行中的请求, 得到 %d", got)
	}
}

// serveState 只读观测面: GET 返回观测载荷, 其它方法 405。
func TestServeState(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1/v1", GateCfg{}, nil)
	app.flowStart("m1", "agent", "/v1/chat/completions", true)

	rec := httptest.NewRecorder()
	app.serveState(rec, httptest.NewRequest(http.MethodGet, "/state?since=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应返回 200, 得到 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json, 得到 %q", ct)
	}
	var p pulse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v", err)
	}
	if p.Ver != viewVer || len(p.Active) != 1 {
		t.Errorf("载荷不对: ver=%q active=%+v", p.Ver, p.Active)
	}

	rec = httptest.NewRecorder()
	app.serveState(rec, httptest.NewRequest(http.MethodPost, "/state", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("非 GET 应返回 405, 得到 %d", rec.Code)
	}
}

// serveModels 列出别名表里的逻辑模型名 (排序后), 不暴露真实 id。
func TestServeModels(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1/v1", GateCfg{}, nil)
	app.mu.Lock()
	app.aliases["a-model"] = []aliasTarget{{Upstream: "u1", Model: "real-9"}}
	app.mu.Unlock()

	rec := httptest.NewRecorder()
	app.serveModels(rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200, 得到 %d", rec.Code)
	}
	var got struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("返回不是合法 JSON: %v", err)
	}
	if got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("清单不对: %+v", got)
	}
	if got.Data[0].ID != "a-model" || got.Data[1].ID != "m1" {
		t.Errorf("应按名字排序列出逻辑名, 得到 %+v", got.Data)
	}
	for _, d := range got.Data {
		if d.Object != "model" || d.OwnedBy != "tollgate" {
			t.Errorf("每项 object / owned_by 不对: %+v", d)
		}
	}
	if strings.Contains(rec.Body.String(), "real-9") || strings.Contains(rec.Body.String(), "real-1") {
		t.Errorf("不该暴露真实模型 id: %s", rec.Body.String())
	}
}
