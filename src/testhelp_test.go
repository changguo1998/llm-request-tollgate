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
	"time"
)

// newTestAppCfg 用现成的 Config 起 App: 补上日志路径、监听地址与超时 (测试里必备的非零值)。
func newTestAppCfg(t *testing.T, cfg *Config) *App {
	t.Helper()
	cfg.Listen = "127.0.0.1:0"
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(t.TempDir(), "requests.jsonl")
	}
	if cfg.StreamIdleSec == 0 {
		// 超时不能留零值: 连接包装器会把每次读的死线设成 "现在", 读立刻超时。
		cfg.setTimeouts(Timeouts{ConnectMs: 5000, ResponseHeaderMs: 5000, StreamIdleMs: 5000})
	}
	if cfg.ProbeEnabled == nil {
		// 测试默认关掉锁恢复探测: 否则到期锁会去假上游发探测, 干扰既有用例的锁断言。
		off := false
		cfg.ProbeEnabled = &off
	}
	app, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// appWithGate 起 App 后用运行时毫秒 gate 覆盖配置里的门控参数 —— 测试常要毫秒级
// (如 interval=5ms), 而配置文件的 gate 是秒制, 不能经 setGate 取整。
func appWithGate(t *testing.T, cfg *Config, gate GateCfg) *App {
	t.Helper()
	app := newTestAppCfg(t, cfg)
	app.mu.Lock()
	app.gate = normGate(gate)
	app.mu.Unlock()
	return app
}

// newTestApp 起一个指向假上游的单候选 App (别名 m1 -> u1/real-1), 日志写在临时目录里。
func newTestApp(t *testing.T, upstreamURL string, gate GateCfg, rules *ErrRules) *App {
	t.Helper()
	cfg := &Config{
		Upstreams: []UpstreamCfg{{
			ID: "u1", BaseURL: upstreamURL, AuthHeader: "Authorization", Key: "k",
			IncludeUsage: true, Errors: rules,
		}},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}}},
	}
	return appWithGate(t, cfg, gate)
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

// readLastLog 读最后一行请求日志, 读到可解析记录返回 true (空文件/尾部空行不算)。
func readLastLog(app *App) (logRec, bool) {
	app.logMu.Lock()
	path := app.logW.Name()
	app.logMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return logRec{}, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		var rec logRec
		if json.Unmarshal([]byte(s), &rec) == nil {
			return rec, true
		}
	}
	return logRec{}, false
}

// lastLogRec 立即读最后一行请求日志 (非流式请求在返回前已写日志, 可直接读)。
func lastLogRec(t *testing.T, app *App) logRec {
	t.Helper()
	rec, ok := readLastLog(app)
	if !ok {
		t.Fatal("日志为空或最后一行不可解析")
	}
	return rec
}

// waitLogRec 轮询直到日志能读出最后一行 (处理流式请求收尾后才写日志的时序)。
func waitLogRec(t *testing.T, app *App, timeout time.Duration) logRec {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if rec, ok := readLastLog(app); ok {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatal("等待请求日志超时 (仍未写出行)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
