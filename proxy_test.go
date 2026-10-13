package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// newTestApp 起一个指向假上游的单候选 App, 日志写在临时目录里。
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

// TestProxyForwardsAndRewritesModel 覆盖正常转发: 认证头替换, 上游收到真实模型 id,
// 响应里的 model 改回逻辑名, 日志记下用量, 白名单外仍 404。
func TestProxyForwardsAndRewritesModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("认证头没替换: %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		if payload["model"] != "real-1" {
			t.Errorf("上游应收到真实模型 id, 实为 %v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"real-1","choices":[{"message":{"role":"assistant",`+
			`"content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	status, body := postJSON(t, url, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码 %d: %s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["model"] != "m1" {
		t.Fatalf("响应里的 model 应改回逻辑名, 实为 %v", out["model"])
	}
	rec := lastLogRec(t, app)
	if rec.Status != http.StatusOK || rec.Usage == nil || rec.Usage.Total != 5 || rec.Gate != "" {
		t.Fatalf("日志不符: %+v", rec)
	}

	status, _ = postJSON(t, url, `{"model":"nope","messages":[]}`)
	if status != http.StatusNotFound {
		t.Fatalf("白名单外的模型名应 404, 实为 %d", status)
	}
}

// TestProxyOvershootGrowsConcurrency 覆盖并发满时的超发试探: 起点 1 的上限下面能有两笔
// 同时在飞, 试探成功后上限变成 2。
func TestProxyOvershootGrowsConcurrency(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, peak := 0, 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-release
		_, _ = io.WriteString(w, `{"model":"real-1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 20, SemWaitMs: 2000, QueueWaitMs: 2000}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	var wg sync.WaitGroup
	results := make([]int, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = postJSON(t, url, `{"model":"m1","messages":[]}`)
		}(i)
	}
	// 等到两笔都发出去 (一笔占槽位, 一笔超发), 再放行。
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		p := peak
		mu.Unlock()
		if p >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	for i, st := range results {
		if st != http.StatusOK {
			t.Fatalf("第 %d 笔状态码 %d", i+1, st)
		}
	}
	mu.Lock()
	p := peak
	mu.Unlock()
	if p != 2 {
		t.Fatalf("应有超发 (两笔同时在飞), 实为 %d", p)
	}
	app.mu.RLock()
	cap := app.cands["u1"].softCap
	app.mu.RUnlock()
	if cap != 2 {
		t.Fatalf("试探成功后并发上限应为 2, 实为 %d", cap)
	}
}

// TestProxyBusyReturnsSynthetic429 覆盖被挡下而没发出去的情况: 合成 429 说明原因。
func TestProxyBusyReturnsSynthetic429(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, `{"model":"real-1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()
	// 起点上限 1: 第一笔占槽位, 第二笔超发试探, 第三笔既没槽位也不许再试探。
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 20, SemWaitMs: 100, QueueWaitMs: 100}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	var wg sync.WaitGroup
	statuses := make([]int, 3)
	bodies := make([]string, 3)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, b := postJSON(t, url, `{"model":"m1","messages":[]}`)
			statuses[i], bodies[i] = st, string(b)
		}(i)
	}
	time.Sleep(300 * time.Millisecond) // 让第三笔的等信号量超时
	close(release)
	wg.Wait()

	rejected := 0
	for i, st := range statuses {
		switch st {
		case http.StatusOK:
		case http.StatusTooManyRequests:
			rejected++
			if !strings.Contains(bodies[i], "no candidate") {
				t.Fatalf("被挡下的 429 应说明原因, 实为 %s", bodies[i])
			}
		default:
			t.Fatalf("第 %d 笔状态码异常 %d: %s", i+1, st, bodies[i])
		}
	}
	if rejected != 1 {
		t.Fatalf("应有一笔被闸门挡下, 实为 %d", rejected)
	}
}

// TestProxyQuotaRuleLocksCandidate 覆盖额度限制: 命中规则的上游被锁 10min, 后续请求
// 不再真发 (整条候选链试完给 429)。
func TestProxyQuotaRuleLocksCandidate(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient balance"}}`)
	}))
	defer up.Close()
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{402, 429}, Contains: []string{"insufficient balance"}}}}
	app := newTestApp(t, up.URL, GateCfg{SemWaitMs: 50, QueueWaitMs: 50}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	// 第一笔真发出去: 429 与上游 body 原样透传, 同时该候选被锁。
	status, body := postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "insufficient balance") {
		t.Fatalf("上游 429 应原样透传, 实为 %d %s", status, body)
	}
	locked, _, reason := app.cands["u1"].locked()
	if !locked || reason != "quota" {
		t.Fatalf("命中额度规则后该候选应被锁: locked=%v reason=%s", locked, reason)
	}

	// 第二笔不再真发: 候选全被锁 → 429 说明是锁而不是上游错误。
	status, body = postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "locked") {
		t.Fatalf("锁内应给带原因的 429, 实为 %d %s", status, body)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("锁内不该真发, 上游被调用 %d 次", n)
	}
}

// TestProxyBreakerAfterRepeatedFailures 覆盖通用兜底: 连续失败按序列退避, 第 4 次熔断
// (锁 5min); 退避序列在测试里调小, 让一笔请求内就能走完 4 次。
func TestProxyBreakerAfterRepeatedFailures(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	// 节拍与退避都调小, 让一笔请求内就能走完 4 次失败; 真实配置是 500ms 节拍 + 10/20/30s 退避。
	gate := GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200, RetryBackoffMs: []int{5, 10, 15}}
	app := newTestApp(t, up.URL, gate, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	// 第一笔: 每次失败后退避再重试同一候选, 第 4 次熔断, 最后透传上游的 500。
	status, body := postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("应透传上游 500, 实为 %d %s", status, body)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	app.mu.RLock()
	streak := app.cands["u1"].errStreak
	app.mu.RUnlock()
	locked, _, reason := app.cands["u1"].locked()
	if !locked || reason != "breaker" {
		t.Fatalf("连续 4 次失败后应熔断: locked=%v reason=%s calls=%d streak=%d", locked, reason, n, streak)
	}
	if n != 4 {
		t.Fatalf("熔断前应重试到第 4 次, 上游被调用 %d 次", n)
	}

	// 第二笔: 候选在锁内, 不再真发, 给带原因的 429。
	status, body = postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "locked") {
		t.Fatalf("熔断后应给带原因的 429, 实为 %d %s", status, body)
	}
	mu.Lock()
	n = calls
	mu.Unlock()
	if n != 4 {
		t.Fatalf("锁内不该真发, 上游被调用 %d 次", n)
	}
}

// TestProxyBackoffBlocksQueuedRequest 覆盖退避期间的排队超时: 候选还在退避, 新请求
// 排不到队就换候选, 候选链试完给带原因的 429 (设计 §5.5.2)。
func TestProxyBackoffBlocksQueuedRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer up.Close()
	// 退避 10s 远大于排队上限 50ms: 第一笔自己会透传 500, 第二笔只能排队超时。
	app := newTestApp(t, up.URL, GateCfg{SemWaitMs: 50, QueueWaitMs: 50}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusInternalServerError {
		t.Fatalf("第一笔应透传上游 500, 实为 %d", status)
	}
	status, body := postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "queue_timeout") {
		t.Fatalf("退避期间应给排队超时的 429, 实为 %d %s", status, body)
	}
	if rec := lastLogRec(t, app); rec.Gate != "queue_timeout" || rec.Fallbacks == 0 {
		t.Fatalf("日志应记下被挡下的原因: %+v", rec)
	}
}

// TestProxyConcurrencyClassRetriesSameCandidate 覆盖调研文档 §1.2 第 5 条: 普通请求撞上
// 「并发上限」类错误时, 除了锁掉超发试探, 本笔仍按普通错误走 —— 退避后重试同一候选,
// 而不是立刻换下一个候选 (只有试探撞上才直接换)。
func TestProxyConcurrencyClassRetriesSameCandidate(t *testing.T) {
	var mu sync.Mutex
	badCalls := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		badCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w,
			`{"error":{"message":"Max parallel request limit reached","type":"throttling_error","code":"429"}}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w,
			`{"model":"real-2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer good.Close()

	rules := &ErrRules{Concurrency: []ErrRule{
		{Status: []int{429}, Contains: []string{"max parallel"}},
	}}
	app := newTestAppCfg(t, &Config{
		// 退避序列调小, 让一笔请求内走完 4 次失败并熔断。
		Gate: GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200, RetryBackoffMs: []int{5, 10, 15}},
		Upstreams: []UpstreamCfg{
			{ID: "bad", BaseURL: bad.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true, Errors: rules},
			{ID: "good", BaseURL: good.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true},
		},
		Aliases: map[string][][2]string{"m1": {{"bad", "real-1"}, {"good", "real-2"}}},
	})
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, body := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusOK {
		t.Fatalf("应由第二个候选服务, 实为 %d %s", status, body)
	}
	mu.Lock()
	n := badCalls
	mu.Unlock()
	if n != 4 {
		t.Fatalf("普通请求撞并发上限应退避重试同一候选到熔断 (4 次), 实为 %d 次", n)
	}
	// 并发类命中要锁掉超发试探。
	app.mu.RLock()
	cd := app.cands["bad"]
	app.mu.RUnlock()
	cd.mu.Lock()
	locked := cd.concLock.active(time.Now())
	cd.mu.Unlock()
	if !locked {
		t.Fatal("命中并发类应锁掉超发试探 1h")
	}
	rec := lastLogRec(t, app)
	if rec.Upstream != "good" || rec.Fallbacks < 1 {
		t.Fatalf("日志应记最终由 good 服务且有 fallback: %+v", rec)
	}
}

// TestUnclassifiedErrorIsLogged 覆盖调研文档 §1.2 第 5 条的后半句: 规则表没覆盖到的报错
// 要留下可核对的样本 (上游 + 状态码 + body 片段), 便于按真实文案补规则。
func TestUnclassifiedErrorIsLogged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"engine overloaded, retry later"}}`)
	}))
	defer up.Close()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	// 只有额度规则, 这条 503 落不进任何类 -> 走通用兜底并留样本。
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{402}, Contains: []string{"insufficient"}}}}
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200,
		RetryBackoffMs: []int{5, 10, 15}}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("应透传上游 503, 实为 %d", status)
	}
	out := buf.String()
	if !strings.Contains(out, "unclassified upstream error") ||
		!strings.Contains(out, "upstream=u1") ||
		!strings.Contains(out, "engine overloaded") {
		t.Fatalf("未分类样本没写进服务日志: %s", out)
	}
}
