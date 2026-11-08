// quota_reset_test.go: 额度重置时间解析 (recoveryDur) 与「按解析时间设额度锁倒计时」
// 的端到端行为。

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestQuotaResetDurBodyUstc 覆盖 ustc (litellm 网关) 的每 key 限流文案:
// 响应体 message 里带 "Limit resets at: <UTC 时间>"。
func TestQuotaResetDurBodyUstc(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	body := []byte(`{"error":{"message":"Rate limit exceeded for api_key: abc123. Limit type: requests. Current limit: 20, Remaining: 0. Limit resets at: 2026-11-07 13:36:10 UTC","type":"throttling_error","code":"429"}}`)
	if d := recoveryDur(body, nil, now); d != 60*time.Second {
		t.Fatalf("ustc 文本应解析出 60s, 实为 %s", d)
	}
}

// TestQuotaResetDurBodyArkcp 覆盖方舟周额度过期文案: message 里带
// "It will reset at <+0800 CST>"。数字偏移优先于 "CST" 缩写 (缩写默认是美国中部时区)。
func TestQuotaResetDurBodyArkcp(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) // 本地 (CST=UTC+8) 是 20:00
	body := []byte(`{"error":{"code":"AccountQuotaExceeded","message":"You have exceeded the weekly usage quota. It will reset at 2026-11-09 00:00:00 +0800 CST. We recommend upgrading your plan for more quota, or waiting for the reset.","param":"","type":"TooManyRequests"}}`)
	// 2026-11-09 00:00:00 +0800 == 2026-11-08 16:00:00 UTC。
	want := time.Date(2026, 11, 8, 16, 0, 0, 0, time.UTC).Sub(now)
	if d := recoveryDur(body, nil, now); d != want {
		t.Fatalf("arkcp 文本应解析出 %s, 实为 %s", want, d)
	}
}

// TestQuotaResetDurBodyPicksKeywordTime 正文里先出现一个也在边界内的无关时间戳时,
// 应优先取「重置 / 到期」关键字后紧跟的那个, 不能拿第一个时间戳错当重置时刻。
func TestQuotaResetDurBodyPicksKeywordTime(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	// "queued at 13:36:00" (50s, 也在边界内) 先出现; 真正重置是 13:36:10 (60s)。
	body := []byte(`{"error":{"message":"Request queued at 2026-11-07 13:36:00 UTC. Rate limit exceeded. Limit resets at: 2026-11-07 13:36:10 UTC","type":"throttling_error"}}`)
	if d := recoveryDur(body, nil, now); d != 60*time.Second {
		t.Fatalf("应取关键字后的重置时刻 (60s), 实为 %s", d)
	}
}

// TestQuotaResetDurBodyScanCap 重置时间落在扫描上限 (8KB) 之外时不认, 回退固定时长。
func TestQuotaResetDurBodyScanCap(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	body := []byte(strings.Repeat("A", bodyScanCap) + " It will reset at 2026-11-07 13:36:10 UTC")
	if d := recoveryDur(body, nil, now); d != 0 {
		t.Fatalf("扫描上限之外的重置时间应判无效 (0), 实为 %s", d)
	}
}

// TestQuotaResetDurBodyWinsOverRetryAfter 同一笔错误既带 body 重置时间又带 Retry-After 时,
// 绝对时间优先 (来源 1 先于来源 3)。
func TestQuotaResetDurBodyWinsOverRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	body := []byte("Limit resets at: 2026-11-07 13:36:10 UTC")
	hdr := http.Header{"Retry-After": {"5"}}
	if d := recoveryDur(body, hdr, now); d != 60*time.Second {
		t.Fatalf("body 绝对时间应优先于 Retry-After, 实为 %s", d)
	}
}

// TestQuotaResetDurHeaders 覆盖响应头来源: Reset_at 的绝对时间、x-ratelimit-reset 的
// 时长形态 (含小写键), 以及 Retry-After 的相对秒数。
func TestQuotaResetDurHeaders(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	cases := []struct {
		name string
		hdr  http.Header
		want time.Duration
	}{
		{"Reset_at 绝对时间", http.Header{"Reset_at": {"2026-11-07 13:36:10 UTC"}}, 60 * time.Second},
		{"x-ratelimit-reset 时长", http.Header{"x-ratelimit-reset": {"6m0s"}}, 6 * time.Minute},
		{"Retry-After 秒数", http.Header{"Retry-After": {"60"}}, 60 * time.Second},
		{"Retry-After 非数字", http.Header{"Retry-After": {"soon"}}, 0},
		{"无相关头", http.Header{"Content-Type": {"application/json"}}, 0},
	}
	for _, c := range cases {
		if got := recoveryDur(nil, c.hdr, now); got != c.want {
			t.Errorf("%s: 应得 %s, 实为 %s", c.name, c.want, got)
		}
	}
}

// TestQuotaResetDurHeaderOrder 多个 reset 头并存时按固定优先级取 (Reset_at 先于
// x-ratelimit-reset), 不因 map 遍历序抖动。
func TestQuotaResetDurHeaderOrder(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	hdr := http.Header{
		"Reset_at":          {now.Add(25 * time.Minute).UTC().Format("2006-01-02 15:04:05 MST")},
		"x-ratelimit-reset": {"6m0s"}, // 6min, 应被 Reset_at 盖过
	}
	if d := recoveryDur(nil, hdr, now); d != 25*time.Minute {
		t.Fatalf("Reset_at 应优先于 x-ratelimit-reset, 实为 %s", d)
	}
}

// TestQuotaResetDurRetryAfterHTTPDate HTTP-date 形态的 Retry-After 也要能解析
// (RFC7231 §7.1.1.1 允许秒数与 HTTP-date 两种)。
func TestQuotaResetDurRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 8, 5, 42, 3, 0, time.UTC)
	retry := time.Date(2026, 11, 8, 5, 43, 3, 0, time.UTC).Format(http.TimeFormat)
	hdr := http.Header{"Retry-After": {retry}}
	if d := recoveryDur(nil, hdr, now); d != 60*time.Second {
		t.Fatalf("HTTP-date 形态应解析出 60s, 实为 %s", d)
	}
}

// TestQuotaResetDurBounds 覆盖边界: 已过的、太近的 (<1s)、太远的 (>7 天) 与无时区的
// 裸时间都不认, 回退固定时长。
func TestQuotaResetDurBounds(t *testing.T) {
	now := time.Date(2026, 11, 7, 13, 35, 10, 0, time.UTC)
	cases := []struct {
		name string
		body string
	}{
		{"已过", "It will reset at 2026-11-07 13:30:00 UTC"},
		{"太近", "Limit resets at: 2026-11-07 13:35:10 UTC"},
		{"无时区裸时间", "Limit resets at: 2026-11-07 13:36:10"},
	}
	far := now.Add(8 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 MST")
	cases = append(cases, struct {
		name string
		body string
	}{"超过一周", "It will reset at " + far})
	for _, c := range cases {
		if d := recoveryDur([]byte(c.body), nil, now); d != 0 {
			t.Errorf("%s: 应判无效 (0), 实为 %s", c.name, d)
		}
	}
}

// TestProxyQuotaLockUsesParsedReset 端到端: 上游 429 的响应体带 "Limit resets at:
// <20s 后>" 时, 额度锁按真实重置时刻设定倒计时 (约 20s), 而不是固定 quota_lock_ms
// (10min)。
func TestProxyQuotaLockUsesParsedReset(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		reset := time.Now().Add(20 * time.Second).UTC().Format("2006-01-02 15:04:05 MST")
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded for api_key: abc123. Limit type: requests. Remaining: 0. Limit resets at: `+reset+`","type":"throttling_error"}}`)
	}))
	defer up.Close()
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{429}, Contains: []string{"Limit resets at"}}}}
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 200, QueueWaitMs: 200}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("应透传上游 429, 实为 %d", status)
	}
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "quota" {
		t.Fatalf("命中额度规则后该组应被锁: locked=%v reason=%s", locked, reason)
	}
	if left <= 0 || left > 45*time.Second {
		t.Fatalf("额度锁应按解析出的重置时刻 (~20s) 设定, 剩余 %s (解析失败则会是固定 10min)", left)
	}
}

// TestProxyQuotaLockUsesResetHeader 端到端: 只有 Reset_at 响应头 (body 无时间) 时,
// 额度锁也按该时刻设倒计时。
func TestProxyQuotaLockUsesResetHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Reset_at", time.Now().Add(20*time.Second).UTC().Format("2006-01-02 15:04:05 MST"))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded","type":"throttling_error"}}`)
	}))
	defer up.Close()
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{429}, Contains: []string{"quota exceeded"}}}}
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 200, QueueWaitMs: 200}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("应透传上游 429, 实为 %d", status)
	}
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "quota" {
		t.Fatalf("命中额度规则后该组应被锁: locked=%v reason=%s", locked, reason)
	}
	if left <= 0 || left > 45*time.Second {
		t.Fatalf("额度锁应按 Reset_at 头 (~20s) 设定, 剩余 %s", left)
	}
}

// TestProxyQuotaLockLongReset 端到端: 方舟式长重置 (数小时后) 也能被解析并用于设锁
// (锁持约 3h, 而不是固定 10min 早早放行)。
func TestProxyQuotaLockLongReset(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		reset := time.Now().Add(3 * time.Hour).UTC().Format("2006-01-02 15:04:05 MST")
		_, _ = io.WriteString(w, `{"error":{"code":"AccountQuotaExceeded","message":"You have exceeded the weekly usage quota. It will reset at `+reset+`. We recommend upgrading your plan.","param":"","type":"TooManyRequests"}}`)
	}))
	defer up.Close()
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{429}, Contains: []string{"It will reset at"}}}}
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 200, QueueWaitMs: 200}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("应透传上游 429, 实为 %d", status)
	}
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "quota" {
		t.Fatalf("命中额度规则后该组应被锁: locked=%v reason=%s", locked, reason)
	}
	if left <= time.Hour {
		t.Fatalf("长重置应按 ~3h 设锁, 剩余 %s (若被限高则应回退 10min)", left)
	}
}

// TestProxyQuotaLockFallsBackOnOutOfBounds 端到端: 响应体里的重置时间超过上限 (30 天,
// 超过 7 天封顶) 时按无效处理, 回退固定 quota_lock_ms (10min)。
func TestProxyQuotaLockFallsBackOnOutOfBounds(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		far := time.Now().Add(30 * 24 * time.Hour).UTC().Format("2006-01-02 15:04:05 MST")
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded. Limit resets at: `+far+`","type":"throttling_error"}}`)
	}))
	defer up.Close()
	rules := &ErrRules{Quota: []ErrRule{{Status: []int{429}, Contains: []string{"Limit resets at"}}}}
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 200, QueueWaitMs: 200}, rules)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("应透传上游 429, 实为 %d", status)
	}
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "quota" {
		t.Fatalf("命中额度规则后该组应被锁: locked=%v reason=%s", locked, reason)
	}
	if left < 9*time.Minute {
		t.Fatalf("越界的重置时间应回退固定 10min, 剩余 %s", left)
	}
}

// TestProxyBreakerUsesParsedRecovery 端到端: 通用兜底 (类 other, 如 503 并发限制) 的
// 响应带 Retry-After 恢复时间时, 熔断锁按解析出的恢复时长设倒计时 (~20s), 而不是固定
// breaker_lock_ms (5min)。
func TestProxyBreakerUsesParsedRecovery(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"no auth available (concurrent limit)","type":"throttling_error"}}`)
	}))
	defer up.Close()
	gate := GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200, RetryBackoffMs: []int{5, 10, 15}}
	app := newTestApp(t, up.URL, gate, nil) // 无规则 -> 全部归通用兜底 (other)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	url := proxy.URL + "/v1/chat/completions"

	// 第一笔: 连续 4 次失败, 第 4 次触发熔断锁, 恢复时长取 Retry-After (20s)。
	status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("应透传上游 503, 实为 %d", status)
	}
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "breaker" {
		t.Fatalf("连续 4 次失败后应熔断: locked=%v reason=%s", locked, reason)
	}
	if left <= 0 || left > 45*time.Second {
		t.Fatalf("熔断锁应优先按 Retry-After (~20s) 设倒计时, 剩余 %s (若解析失败则会是固定 5min)", left)
	}

	// 第二笔: 候选在锁内, 不再真发。
	status, _ = postJSON(t, url, `{"model":"m1","messages":[]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("熔断后应给 429, 实为 %d", status)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 4 {
		t.Fatalf("锁内不该真发, 上游被调用 %d 次", n)
	}
}
