// sticky_test.go: 粘性偏好 —— 最近成功候选优先 + 只有持续真实失败才迁移。

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// --- 单元: 偏好表规则 ---

func TestStickyPrefIndexDefaultZero(t *testing.T) {
	s := newSticky()
	if got := s.prefIdx("m1", 3); got != 0 {
		t.Fatalf("无记录应回落到配置第一候选 (0), 实为 %d", got)
	}
}

// 服务在偏好候选上: 清零失败记忆, 不迁移。
func TestStickyServedOnPrefResetsStreak(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, false, false) // 最近成功的一次真实顶班
	if got := s.prefIdx("m1", 3); got != 0 {
		t.Fatalf("一次顶班不该迁移, 实为 %d", got)
	}
	s.served("m1", 0, 0, false, false) // 服务在偏好候选上 (第 0 候选)
	s.served("m1", 1, 0, false, false) // 顶班 #1
	if got := s.prefIdx("m1", 3); got != 0 {
		t.Fatalf("偏好上成功后一次顶班不该迁移, 实为 %d", got)
	}
	s.served("m1", 1, 0, false, false) // 顶班 #2
	if got := s.prefIdx("m1", 3); got != 1 {
		t.Fatalf("连续两次真实顶班后应迁移到 %d, 实为 %d", 1, got)
	}
}

// 真实失败顶班: 连续两次才迁移 (偶尔一次失败不影响粘性)。
func TestStickyRealTopupMigratesAfterTwo(t *testing.T) {
	s := newSticky()
	s.served("m1", 2, 0, false, false) // 顶班 #1
	s.served("m1", 2, 0, false, false) // 顶班 #2 → 迁移
	if got := s.prefIdx("m1", 3); got != 2 {
		t.Fatalf("连续两次真实顶班应迁移到 2, 实为 %d", got)
	}
}

// 顶班 (锁 / 退避 / 排队超时): 不迁移, 也不累计计数。
func TestStickyBlockedTopupDoesNotCount(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, true, false) // 临时不可用顶班
	s.served("m1", 1, 0, true, false)
	if got := s.prefIdx("m1", 3); got != 0 {
		t.Fatalf("临时不可用顶班不应迁移, 实为 %d", got)
	}
	// 顶班不累计: 后面只需一次真实顶班仍不迁移 (streak 还是 0)。
	s.served("m1", 1, 0, false, false)
	if got := s.prefIdx("m1", 3); got != 0 {
		t.Fatalf("顶班不该计入 streak, 一次真实顶班后仍应留在 0, 实为 %d", got)
	}
}

// 长期锁 (低性能 / 额度) 顶班: 直接迁移, 不再连撞。
func TestStickyLongLockMigratesImmediately(t *testing.T) {
	s := newSticky()
	s.served("m1", 2, 0, true, true) // 低性能锁顶班 → 立即迁移
	if got := s.prefIdx("m1", 3); got != 2 {
		t.Fatalf("长期锁顶班应立即迁移到 2, 实为 %d", got)
	}
}

// stickyLongLock 只认长期锁原因。
func TestStickyLongLockReasons(t *testing.T) {
	for _, r := range []string{"quota", lowPerfReason} {
		if !stickyLongLock(r) {
			t.Fatalf("%q 应视为长期锁", r)
		}
	}
	for _, r := range []string{"breaker", "concurrency", ""} {
		if stickyLongLock(r) {
			t.Fatalf("%q 不应视为长期锁", r)
		}
	}
}

// prune 热加载后清掉越界偏好; 有效偏好保留。
func TestStickyPruneInvalidatesOnConfigChange(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, false, false)
	s.served("m1", 1, 0, false, false) // m1 偏好 = 1
	s.served("m2", 0, 0, false, true)  // m2 偏好 = 0
	s.prune(map[string][]aliasTarget{
		"m1": {{Upstream: "u1", Model: "r1"}, {Upstream: "u2", Model: "r2"}},
		"m3": {{Upstream: "u1", Model: "r1"}}, // m2 从配置里消失
	})
	if got := s.prefIdx("m1", 2); got != 1 {
		t.Fatalf("候选集未变时偏好应保留, 实为 %d", got)
	}
	if got := s.prefIdx("m2", 1); got != 0 {
		t.Fatalf("逻辑名消失后偏好应作废回落 0, 实为 %d", got)
	}
}

// 观测面快照: 偏好下标解析成 上游/真实模型, 无偏好与越界的逻辑名不出现在快照里。
func TestStickyPairsSnapshot(t *testing.T) {
	a := &App{
		aliases: map[string][]aliasTarget{
			"m1": {{Upstream: "u1", Model: "r1"}, {Upstream: "u2", Model: "r2"}, {Upstream: "u3", Model: "r3"}},
			"m2": {{Upstream: "u1", Model: "r1"}},
		},
		sticky: newSticky(),
	}
	// m1 连续两次真实顶班 -> 偏好迁到 u2/r2; m2 从不使用。
	a.sticky.served("m1", 1, 0, false, false)
	a.sticky.served("m1", 1, 0, false, false)
	snap := a.stickySnapshot()
	if len(snap) != 1 {
		t.Fatalf("应只有 m1 有偏好, 得到 %v", snap)
	}
	if got := snap["m1"]; got != (stickyView{Upstream: "u2", Model: "r2"}) {
		t.Fatalf("m1 偏好应为 u2/r2, 得到 %+v", got)
	}
	if _, ok := snap["m2"]; ok {
		t.Fatal("无偏好的逻辑名不该出现在快照里")
	}
	// 热加载后越界的偏好不进入快照。
	a.sticky.mu.Lock()
	a.sticky.prefs["m2"] = &stickyPref{pref: 5}
	a.sticky.mu.Unlock()
	snap = a.stickySnapshot()
	if _, ok := snap["m2"]; ok {
		t.Fatal("越界偏好不应出现在快照里")
	}
	// 没有 sticky 的 App (老装配) 快照为空。
	if got := (&App{}).stickySnapshot(); got != nil {
		t.Fatalf("无 sticky 时快照应为空, 得到 %v", got)
	}
}

// --- 集成: 代理链路下的偏好排序与迁移 ---

// twoUpApp 起一个两候选 App: 都指向两个假上游, 别名 m1 -> [u1, u2]。
// 各 handler 返回状态由调用方设定 (nil 用原子计数变量)。
func twoUpApp(t *testing.T, u1, u2 http.Handler, gate GateCfg) (*App, *httptest.Server) {
	t.Helper()
	s1 := httptest.NewServer(u1)
	t.Cleanup(s1.Close)
	s2 := httptest.NewServer(u2)
	t.Cleanup(s2.Close)
	cfg := &Config{
		Upstreams: []UpstreamCfg{
			{ID: "u1", BaseURL: s1.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true},
			{ID: "u2", BaseURL: s2.URL, AuthHeader: "Authorization", Key: "k", IncludeUsage: true},
		},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}, {"u2", "real-2"}}},
	}
	app := appWithGate(t, cfg, gate)
	proxy := httptest.NewServer(app)
	t.Cleanup(proxy.Close)
	return app, proxy
}

// 持续 500 的候选: 每笔请求内退避到熔断 (4 次) 后换到备用; 熔断期短,
// 两笔请求之间等它到期, 让第二笔仍是「真实失败顶班」而不是「锁内跳过」。
func TestStickyProxyMigratesAfterTwoFailures(t *testing.T) {
	var u1hits, u2hits atomic.Int64
	u1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u1hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	})
	u2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u2hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	// 退避与熔断都调小: 一笔请求内走完 4 次失败熔断, 熔断 600ms 后候选可再试。
	gate := GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 300,
		RetryBackoffMs: []int{5, 10, 15}, BreakerLockMs: 600}
	app, proxy := twoUpApp(t, u1, u2, gate)
	url := proxy.URL + "/v1/chat/completions"

	// 第一笔: u1 熔断后落到 u2, 真实顶班 #1, 偏好仍 u1。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("应由 u2 服务, 实为 %d", status)
	}
	time.Sleep(700 * time.Millisecond) // 等 u1 熔断锁到期 (探测测试默认关闭, 到期即放行)

	// 第二笔: u1 已恢复可试用, 又熔断落到 u2 —— 连续真实顶班 #2, 偏好迁到 u2。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第二笔应由 u2 服务, 实为 %d", status)
	}
	if got := app.sticky.prefIdx("m1", 2); got != 1 {
		t.Fatalf("连续两次真实顶班后偏好应迁到 u2 (1), 实为 %d", got)
	}

	// 第三笔: 直接走 u2, u1 不再被碰。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第三笔应由 u2 服务, 实为 %d", status)
	}
	time.Sleep(50 * time.Millisecond) // 等异步计数写完
	if got := u1hits.Load(); got != 8 {
		t.Fatalf("迁移后 u1 不应再被试, 实为 %d 次 (期望 4+4)", got)
	}
	if got := u2hits.Load(); got != 3 {
		t.Fatalf("三笔都应由 u2 服务, 实为 %d 次", got)
	}
}

// 偶尔一次失败 (首笔 500, 随即恢复) 不迁移: 偏好仍 u1, 后续请求都走 u1。
func TestStickyProxySingleBlipDoesNotMigrate(t *testing.T) {
	// u1 只失败首次调用, 之后恢复 200。
	var first atomic.Bool
	first.Store(true)
	var u2hits atomic.Int64
	u1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first.Swap(false) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"real-1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	u2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u2hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	gate := GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 300, RetryBackoffMs: []int{50, 100, 150}}
	app, proxy := twoUpApp(t, u1, u2, gate)
	url := proxy.URL + "/v1/chat/completions"

	// 第一笔: u1 失败一次后退避重试同一候选, 恢复成功 -> 服务在偏好上, 不迁移。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第一笔应由 u1 服务, 实为 %d", status)
	}
	if got := app.sticky.prefIdx("m1", 2); got != 0 {
		t.Fatalf("一次失败后恢复成功不该迁移, 实为 %d", got)
	}
	// 等 u1 的退避 (50ms) 过完, 免得下一笔被退避跳过看错路。
	time.Sleep(120 * time.Millisecond)
	// 第二笔: 仍先试 u1 并成功, u2 一直没被碰。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第二笔应由 u1 服务, 实为 %d", status)
	}
	if got := u2hits.Load(); got != 0 {
		t.Fatalf("u1 健康时不该落到 u2, 实为 %d 次", got)
	}
}

// 偏好候选命中额度锁 (长期锁): 顶班后立即迁移, u1 解锁后也不再被优先。
func TestStickyProxyLongLockMigratesImmediately(t *testing.T) {
	var u1hits, u2hits atomic.Int64
	u1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u1hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	u2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u2hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	app, proxy := twoUpApp(t, u1, u2, GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 300,
		RetryBackoffMs: []int{5, 10, 15}, QuotaLockMs: 3000})
	url := proxy.URL + "/v1/chat/completions"

	// 先把 u1 的模型锁上额度锁 (仓库唯一候选 u1/real-1 的默认组)。
	app.addLockGroup(app.cands["u1"], "", "quota", 3*time.Second, upstreamPath)

	// 第一笔: u1 锁内预检跳过 -> u2 服务, 长期锁顶班, 偏好立即迁移到 u2。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("应由 u2 服务, 实为 %d", status)
	}
	if got := app.sticky.prefIdx("m1", 2); got != 1 {
		t.Fatalf("长期锁顶班后偏好应立即迁到 u2 (1), 实为 %d", got)
	}
	time.Sleep(50 * time.Millisecond)
	n1 := u1hits.Load()
	if n1 != 0 {
		t.Fatalf("锁内不该真发 u1, 实为 %d 次", n1)
	}

	// 第二笔: 偏好已是 u2, 仍直接走 u2; u1 不再被碰。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第二笔应由 u2 服务, 实为 %d", status)
	}
	time.Sleep(50 * time.Millisecond)
	if got := u1hits.Load(); got != 0 {
		t.Fatalf("迁移后 u1 不该再被优先, 实为 %d 次", got)
	}
	if got := u2hits.Load(); got != 2 {
		t.Fatalf("两笔都应由 u2 服务, 实为 %d 次", got)
	}
}
