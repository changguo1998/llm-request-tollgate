// recover_probe_test.go: 锁恢复探测 —— 到期定时器 / 请求触发 / 通过解锁 / 失败重锁。

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

// fakeProbeUp 是假上游: 记录收到的请求, 按配置的状态码回。
type fakeProbeUp struct {
	mu     sync.Mutex
	status int
	hits   []string
}

func (f *fakeProbeUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.hits = append(f.hits, string(b))
	f.mu.Unlock()
	w.WriteHeader(f.status)
	w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"p"},"finish_reason":"stop"}]}`))
}

func (f *fakeProbeUp) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fakeProbeUp) first() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hits) == 0 {
		return "", false
	}
	return f.hits[0], true
}

// recoverApp 起一个指向假上游、开启了恢复探测的单候选 App。
func recoverApp(t *testing.T, status int) (*App, *fakeProbeUp, *httptest.Server) {
	t.Helper()
	up := &fakeProbeUp{status: status}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	on := true
	cfg := &Config{
		ProbeEnabled: &on,
		Upstreams: []UpstreamCfg{{
			ID: "u1", BaseURL: srv.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true,
			Models: []ModelCfg{{ID: "real-1"}},
		}},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}}},
	}
	app := appWithGate(t, cfg, GateCfg{})
	return app, up, srv
}

// waitUp 轮询假上游直到收到 ≥n 笔 (超时失败)。
func waitUp(t *testing.T, up *fakeProbeUp, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if up.count() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到假上游收到 %d 笔请求 (实际 %d)", n, up.count())
}

// --- 门控单元: 恢复态语义 ---

func TestRecoverProbeableCategories(t *testing.T) {
	for _, r := range []string{"quota", "breaker", "lowperf"} {
		if !recoverProbeable(r) {
			t.Fatalf("%s 应可恢复探测", r)
		}
	}
	for _, r := range []string{"concurrency", ""} {
		if recoverProbeable(r) {
			t.Fatalf("%q 不应可恢复探测", r)
		}
	}
}

func TestRecoveryLockedSemantics(t *testing.T) {
	c := newCand("u1")
	c.recoverProbe = true
	c.lockModel("m1", "breaker", time.Minute)
	if locked, _, _ := c.locked("m1", ""); !locked {
		t.Fatal("活跃锁应锁定")
	}
	// 到期但仍处恢复态: 仍锁定; 关闭探测则到期即放行。
	c.mu.Lock()
	g := c.gate("m1")
	g.lock = lockState{reason: "breaker", until: time.Now().Add(-time.Second), recover: true}
	c.mu.Unlock()
	if locked, _, _ := c.locked("m1", ""); !locked {
		t.Fatal("恢复态应仍锁定")
	}
	c.recoverProbe = false
	c.denyRecovery() // 关闭探测 = 清掉恢复态 (热加载路径)
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("关闭探测时到期锁应放行")
	}
}

func TestRecoveryClaimDedupAndSettle(t *testing.T) {
	c := newCand("u1")
	c.recoverProbe = true
	c.lockModel("m1", "breaker", time.Minute)
	c.mu.Lock()
	g := c.gate("m1")
	g.lock = lockState{reason: "breaker", until: time.Now().Add(-time.Second)}
	c.mu.Unlock()

	r, claimed := c.beginRecovery("m1", "")
	if !claimed {
		t.Fatal("到期可探测锁应能认领")
	}
	if _, again := c.beginRecovery("m1", ""); again {
		t.Fatal("已在探时不应重复认领")
	}
	// 通过解锁
	c.settleRecovery(r, true, GateCfg{})
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("探测通过后应解锁")
	}
}

func TestRecoveryFailRelocks(t *testing.T) {
	c := newCand("u1")
	c.recoverProbe = true
	c.lockModel("m1", "breaker", time.Minute)
	c.mu.Lock()
	g := c.gate("m1")
	g.lock = lockState{reason: "breaker", until: time.Now().Add(-time.Second)}
	c.mu.Unlock()

	r, claimed := c.beginRecovery("m1", "")
	if !claimed {
		t.Fatal("应能认领")
	}
	until := c.settleRecovery(r, false, GateCfg{BreakerLockMs: 5000})
	if !until.After(time.Now()) {
		t.Fatal("失败后应重锁到未来")
	}
	locked, left, reason := c.locked("m1", "")
	if !locked || reason != "breaker" || left <= 4*time.Second {
		t.Fatalf("失败后应按原锁时长重锁: locked=%v reason=%s", locked, reason)
	}
	// 重锁后不再进恢复态 (recover 已清)
	if _, again := c.beginRecovery("m1", ""); again {
		t.Fatal("重锁(未到期)不应进入恢复")
	}
}

func TestRecoveryGroupLockSemantics(t *testing.T) {
	c := newCand("u1")
	c.recoverProbe = true
	c.lockGroup("g1", "quota", time.Minute)
	c.mu.Lock()
	c.groupLocks["g1"] = lockState{reason: "quota", until: time.Now().Add(-time.Second)}
	c.mu.Unlock()
	if locked, _, reason := c.locked("m1", "g1"); !locked || reason != "quota" {
		t.Fatalf("到期组锁在探测开启时应仍锁定 (locked=%v reason=%s)", locked, reason)
	}
	r, claimed := c.beginRecovery("m1", "g1")
	if !claimed || r.isModel {
		t.Fatalf("组锁应认领且 isModel=false (claimed=%v)", claimed)
	}
	if _, again := c.beginRecovery("m1", "g1"); again {
		t.Fatal("组锁探测不应重复")
	}
	c.settleRecovery(r, true, GateCfg{})
	if locked, _, _ := c.locked("m9", "g1"); locked {
		t.Fatal("组锁通过后整组解锁")
	}
}

// --- 集成: 到期定时器自动探测 ---

func TestRecoveryTimerProbeUnlocks(t *testing.T) {
	app, up, _ := recoverApp(t, http.StatusOK)
	app.addLockModel(app.cands["u1"], "real-1", "breaker", 150*time.Millisecond)
	waitUp(t, up, 1)
	body, ok := up.first()
	if !ok || !strings.Contains(body, `"real-1"`) {
		t.Fatalf("探测应带真实模型 id: %s (ok=%v)", body, ok)
	}
	if locked, _, _ := app.cands["u1"].locked("real-1", ""); locked {
		t.Fatal("探测通过后应解锁")
	}
}

func TestRecoveryTimerProbeFailRelocks(t *testing.T) {
	app, up, _ := recoverApp(t, http.StatusTooManyRequests)
	app.addLockModel(app.cands["u1"], "real-1", "breaker", 150*time.Millisecond)
	waitUp(t, up, 1)
	locked, left, reason := app.cands["u1"].locked("real-1", "")
	if !locked || reason != "breaker" || left <= 0 {
		t.Fatalf("探测失败后应重锁: locked=%v reason=%s left=%v", locked, reason, left)
	}
}

func TestRecoveryDisabledNoProbe(t *testing.T) {
	up := &fakeProbeUp{status: http.StatusOK}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	off := false
	cfg := &Config{
		ProbeEnabled: &off,
		Upstreams: []UpstreamCfg{{
			ID: "u1", BaseURL: srv.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true,
			Models: []ModelCfg{{ID: "real-1"}},
		}},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}}},
	}
	app := appWithGate(t, cfg, GateCfg{})
	app.addLockModel(app.cands["u1"], "real-1", "breaker", 150*time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	if up.count() != 0 {
		t.Fatalf("关闭探测时不应发探测, 收到 %d", up.count())
	}
	if locked, _, _ := app.cands["u1"].locked("real-1", ""); locked {
		t.Fatal("关闭探测时到期锁应放行")
	}
}

// --- 集成: 请求命中恢复态只踢探测, 本身按锁定进下一候选 ---

func TestRequestHitsRecoveryKicksProbeButSkips(t *testing.T) {
	app, up, _ := recoverApp(t, http.StatusOK)
	proxy := httptest.NewServer(app)
	t.Cleanup(proxy.Close)

	// 把 real-1 手工置为恢复中 (仍锁定, 待探测通过)。
	c := app.cands["u1"]
	c.mu.Lock()
	g := c.gate("real-1")
	g.lock = lockState{reason: "breaker", until: time.Now().Add(-time.Minute), recover: true}
	c.mu.Unlock()

	code, _ := postJSON(t, proxy.URL+"/v1/chat/completions",
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("命中恢复态的真实请求应按锁定回 429, 得到 %d", code)
	}
	// 请求本身不消费模型: 它只触发了一笔探测 (不带用户真实内容)。
	waitUp(t, up, 1)
	body, _ := up.first()
	if strings.Contains(body, `"hi"`) {
		t.Fatalf("真实请求不应把内容发给模型: %s", body)
	}
}
