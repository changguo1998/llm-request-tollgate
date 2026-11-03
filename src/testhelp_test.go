// testhelp_test.go: 集成测试共用的夹具 (起 App / 发请求 / 读日志), 各用例文件只放断言。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestAppCfg 用现成的 Config 起 App: 补上日志路径、监听地址与超时 (测试里必备的非零值)。
func newTestAppCfg(t *testing.T, cfg *Config) *App {
	t.Helper()
	cfg.Listen = "127.0.0.1:0"
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(t.TempDir(), "requests.jsonl")
	}
	if cfg.Timeouts.StreamIdleMs == 0 {
		// 超时不能留零值: 连接包装器会把每次读的死线设成 "现在", 读立刻超时。
		cfg.Timeouts = Timeouts{ConnectMs: 5000, ResponseHeaderMs: 5000, StreamIdleMs: 5000}
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// newTestApp 起一个指向假上游的单候选 App (别名 m1 -> u1/real-1), 日志写在临时目录里。
func newTestApp(t *testing.T, upstreamURL string, gate GateCfg, rules *ErrRules) *App {
	t.Helper()
	return newTestAppCfg(t, &Config{
		Gate: gate,
		Upstreams: []UpstreamCfg{{
			ID: "u1", BaseURL: upstreamURL, AuthHeader: "Authorization", Key: "k",
			IncludeUsage: true, Errors: rules,
		}},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}}},
	})
}

// postJSON 发一个 JSON POST, 返回状态码与响应体。
func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, b
}

// lastLogRec 读最后一行请求日志。
func lastLogRec(t *testing.T, app *App) logRec {
	t.Helper()
	app.logMu.Lock()
	path := app.logW.Name()
	app.logMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var rec logRec
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("日志解析失败: %v", err)
	}
	return rec
}
