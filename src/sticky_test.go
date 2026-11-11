// sticky_test.go: 粘性偏好 —— 最近成功候选优先, 只有长期锁 (额度 / 低性能 / 熔断) 才迁移; 空闲超过失效窗口 (sticky_ttl_sec) 作废旧偏好, 下笔按优先级重选。

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// --- 单元: 偏好表规则 ---

func TestStickyPrefIndexDefaultZero(t *testing.T) {
	s := newSticky()
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("无记录应回落到配置第一候选 (0), 实为 %d", got)
	}
}

// 真实失败顶班: 不迁移 (连续失败自己会触发熔断锁, 锁上才迁移; 单次波动不影响粘性)。
func TestStickyRealTopupNeverMigrates(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, false, false) // 最近成功的一次真实顶班
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("一次真实顶班不该迁移, 实为 %d", got)
	}
	s.served("m1", 0, 0, false, false) // 服务在偏好候选上 (第 0 候选)
	s.served("m1", 1, 0, false, false) // 顶班
	s.served("m1", 1, 0, false, false) // 再来, 顶班次数再多也不迁移
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("真实失败顶班再多也不该迁移, 实为 %d", got)
	}
}

func TestStickyBlockedTopupDoesNotMigrate(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, false, false) // 临时不可用顶班
	s.served("m1", 1, 0, false, false)
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("临时不可用顶班不应迁移, 实为 %d", got)
	}
	// 临时锁过后同一候选真实失败顶班, 仍不迁移。
	s.served("m1", 1, 0, false, false)
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("真实失败顶班也不该迁移, 实为 %d", got)
	}
}

// 长期锁 (低性能 / 额度 / 熔断) 顶班: 直接迁移, 不再连撞。
func TestStickyLongLockMigratesImmediately(t *testing.T) {
	s := newSticky()
	s.served("m1", 2, 0, true, false) // 低性能锁顶班 -> 立即迁移
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 2 {
		t.Fatalf("长期锁顶班应立即迁移到 2, 实为 %d", got)
	}
}

// stickyLongLock 认三类长期锁原因。
func TestStickyLongLockReasons(t *testing.T) {
	for _, r := range []string{"quota", lowPerfReason, "breaker"} {
		if !stickyLongLock(r) {
			t.Fatalf("%q 应视为长期锁", r)
		}
	}
	for _, r := range []string{"concurrency", ""} {
		if stickyLongLock(r) {
			t.Fatalf("%q 不应视为长期锁", r)
		}
	}
}

// prune 热加载后清掉越界偏好; 有效偏好保留。
func TestStickyPruneInvalidatesOnConfigChange(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, true, false) // m1 长期锁顶班 -> 偏好 = 1
	s.served("m2", 0, 0, true, false) // m2 记录存在, 偏好 = 0 (服务在偏好上)
	s.prune(map[string][]aliasTarget{
		"m1": {{Upstream: "u1", Model: "r1"}, {Upstream: "u2", Model: "r2"}},
		"m3": {{Upstream: "u1", Model: "r1"}}, // m2 从配置里消失
	})
	if got, _ := s.prefIdx("m1", 2, 0, time.Time{}); got != 1 {
		t.Fatalf("候选集未变时偏好应保留, 实为 %d", got)
	}
	if got, _ := s.prefIdx("m2", 1, 0, time.Time{}); got != 0 {
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
	// m1 长期锁顶班 -> 偏好迁到 u2/r2; m2 从不使用。
	a.sticky.served("m1", 1, 0, true, false)
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

// 失效窗口: 空闲超过 ttl, 偏好作废; 下笔按优先级选, 成功后重置到实际服务候选。
func TestStickyTTLExpiresAndResets(t *testing.T) {
	s := newSticky()
	const ttl = 5 * time.Minute
	// 无记录: 按优先级 (0), 不标过期。
	if got, exp := s.prefIdx("m1", 3, ttl, time.Now()); got != 0 || exp {
		t.Fatalf("无记录应 (0, false), 实为 (%d, %v)", got, exp)
	}
	// 长期锁顶班迁到候选 2, 窗口从此刻起。
	s.served("m1", 2, 0, true, false)
	now := time.Now()
	if got, exp := s.prefIdx("m1", 3, ttl, now.Add(30*time.Second)); got != 2 || exp {
		t.Fatalf("窗口内应保持偏好 2, 实为 (%d, %v)", got, exp)
	}
	// 超窗: 作废并返回 expired=true。
	if got, exp := s.prefIdx("m1", 3, ttl, now.Add(ttl+time.Second)); got != 0 || !exp {
		t.Fatalf("超窗应 (0, true), 实为 (%d, %v)", got, exp)
	}
	// 作废后的第一笔成功: 按实际服务候选重置。
	s.served("m1", 1, 0, false, true)
	if got, exp := s.prefIdx("m1", 3, ttl, time.Now()); got != 1 || exp {
		t.Fatalf("过窗后第一笔成功应重置到 1, 实为 (%d, %v)", got, exp)
	}
}

// 窗口从最后一次请求结束起计时: 每笔请求结束时刷新, 活动期偏好保持。
func TestStickyTTLRefreshedByRequestEnd(t *testing.T) {
	s := newSticky()
	const ttl = 5 * time.Minute
	s.served("m1", 2, 0, true, false) // 迁到 2
	s.finish("m1", true, false)       // 请求结束, 窗口从此刻起
	// 4 分钟后看: 窗口内, 保持。
	if got, exp := s.prefIdx("m1", 3, ttl, time.Now().Add(4*time.Minute)); got != 2 || exp {
		t.Fatalf("请求结束后 4 分钟应保持偏好, 实为 (%d, %v)", got, exp)
	}
	// 6 分钟后看 (距上次结束 6 分钟 > 5 分钟): 过期。
	if got, exp := s.prefIdx("m1", 3, ttl, time.Now().Add(6*time.Minute)); got != 0 || !exp {
		t.Fatalf("距上次结束 6 分钟应过期, 实为 (%d, %v)", got, exp)
	}
}

// 过窗后的第一笔请求失败: 偏好作废 (pref = -1), 下笔按优先级选。
func TestStickyFinishInvalidatesAfterExpiredFailure(t *testing.T) {
	s := newSticky()
	const ttl = 5 * time.Minute
	s.served("m1", 2, 0, true, false)
	now := time.Now()
	if _, exp := s.prefIdx("m1", 3, ttl, now.Add(ttl+time.Second)); !exp {
		t.Fatal("超窗应标 expired")
	}
	// 这笔过窗后的请求失败: 偏好作废。
	s.finish("m1", false, true)
	if p := s.prefs["m1"]; p == nil || p.pref != -1 {
		t.Fatalf("过期后失败应作废偏好 (pref=-1), 实为 %+v", p)
	}
	// 下笔: 按优先级 (0), 不再标 expired。
	if got, exp := s.prefIdx("m1", 3, ttl, time.Now()); got != 0 || exp {
		t.Fatalf("已作废应 (0, false), 实为 (%d, %v)", got, exp)
	}
	// 成功在候选 1: 重置偏好。
	s.served("m1", 1, 0, false, false)
	if got, _ := s.prefIdx("m1", 3, ttl, time.Now()); got != 1 {
		t.Fatalf("成功后应重置到 1, 实为 %d", got)
	}
}

// 观测面快照带剩余保持时长: 请求结束不久剩接近 ttl, 过窗后该逻辑名不出现在快照里。
func TestStickyPairsRemaining(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, true, false)
	now := time.Now()
	aliases := map[string][]aliasTarget{
		"m1": {{Upstream: "u1", Model: "r1"}, {Upstream: "u2", Model: "r2"}},
	}
	const ttl = 5 * time.Minute
	if got := s.pairs(aliases, ttl, now, nil)["m1"].RemainingMs; got <= 0 || got > int64(ttl/time.Millisecond) {
		t.Fatalf("剩余时长应在 (0, ttl] 内, 实为 %d", got)
	}
	// 把 last 拨到窗口之外: 该逻辑名不再出现在快照里 (失效即摘除, 无倒计时也无星号)。
	s.mu.Lock()
	s.prefs["m1"].last = now.Add(-ttl - time.Minute)
	s.mu.Unlock()
	if _, ok := s.pairs(aliases, ttl, now, nil)["m1"]; ok {
		t.Fatal("过窗后该逻辑名不应再出现在快照里")
	}
}

// 请求进行中: 该逻辑名的倒计时保持设定值 (TTL 满值, 不递减)。即使 last 已到窗口外,
// 进行中也不摘除 (窗口从请求结束才起算); 请求结束后才按窗口判定。
func TestStickyPairsInFlightKeepsFullTtl(t *testing.T) {
	s := newSticky()
	s.served("m1", 1, 0, true, false)
	now := time.Now()
	aliases := map[string][]aliasTarget{
		"m1": {{Upstream: "u1", Model: "r1"}, {Upstream: "u2", Model: "r2"}},
	}
	const ttl = 5 * time.Minute
	// 把 last 拨到窗口之外, 但 m1 有请求进行中: 仍出现且倒计时 = 设定值。
	s.mu.Lock()
	s.prefs["m1"].last = now.Add(-ttl - time.Minute)
	s.mu.Unlock()
	inflight := map[string]bool{"m1": true}
	got := s.pairs(aliases, ttl, now, inflight)["m1"]
	if want := int64(ttl / time.Millisecond); got.RemainingMs != want {
		t.Fatalf("进行中应保持设定值 %d, 实为 %d", want, got.RemainingMs)
	}
	// 请求结束后 (不在 inflight): 窗口已过, 摘除。
	if _, ok := s.pairs(aliases, ttl, now, nil)["m1"]; ok {
		t.Fatal("请求结束且过窗后不应再出现在快照里")
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

// 持续 500 的候选: 一笔请求内退避到熔断 (4 次) 后换到备用; 熔断锁一上, 本笔就把偏好
// 迁到备用, 之后每笔直接走备用 (锁到期后 u1 也不再被优先)。
func TestStickyProxyBreakerMigratesImmediately(t *testing.T) {
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

	// 第一笔: u1 熔断后落到 u2 顶班成功, 熔断锁一上本笔就把偏好迁到 u2。
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("应由 u2 服务, 实为 %d", status)
	}
	if got, _ := app.sticky.prefIdx("m1", 2, 0, time.Time{}); got != 1 {
		t.Fatalf("熔断上锁后偏好应立即迁到 u2 (1), 实为 %d", got)
	}

	// 第二笔: 等熔断锁到期后, 也直接走 u2, u1 不再被碰 (迁移后 u1 不回流)。
	time.Sleep(700 * time.Millisecond)
	if status, _ := postJSON(t, url, `{"model":"m1","messages":[]}`); status != http.StatusOK {
		t.Fatalf("第二笔应由 u2 服务, 实为 %d", status)
	}
	time.Sleep(50 * time.Millisecond) // 等异步计数写完
	if got := u1hits.Load(); got != 4 {
		t.Fatalf("迁移后 u1 不应再被试, 实为 %d 次 (期望 4)", got)
	}
	if got := u2hits.Load(); got != 2 {
		t.Fatalf("两笔都应由 u2 服务, 实为 %d 次", got)
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
	if got, _ := app.sticky.prefIdx("m1", 2, 0, time.Time{}); got != 0 {
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

// 人工置一次: 立即生效; 自动迁移规则照旧 (长期锁顶班仍迁走); 清除后回落首候选。
func TestStickySetAndReset(t *testing.T) {
	s := newSticky()
	s.set("m1", 2)
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 2 {
		t.Fatalf("人工置偏好后应为 2, 实为 %d", got)
	}
	s.served("m1", 1, 2, false, false) // 置一次后真实顶班
	s.served("m1", 1, 2, false, false)
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 2 {
		t.Fatalf("真实顶班不该迁走人工偏好, 实为 %d", got)
	}
	s.served("m1", 1, 2, true, false) // 长期锁顶班 -> 自动迁移照旧
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 1 {
		t.Fatalf("人工置一次后长期锁迁移仍应生效 (应为 1), 实为 %d", got)
	}
	if n := s.reset(""); n != 1 {
		t.Fatalf("清全部应清掉 1 条, 实为 %d", n)
	}
	if got, _ := s.prefIdx("m1", 3, 0, time.Time{}); got != 0 {
		t.Fatalf("清除后应回落首候选, 实为 %d", got)
	}
	if n := s.reset("m1"); n != 0 {
		t.Fatalf("无偏好时清除应为 0 条, 实为 %d", n)
	}
}

// POST /sticky 人工置偏好后下一笔直接走指定候选; 无 upstream 清除; 参数错误有明确状态码。
func TestStickyEndpointSetAndClear(t *testing.T) {
	var u1hits, u2hits atomic.Int64
	u1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u1hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	u2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u2hits.Add(1)
		_, _ = w.Write([]byte(`{"model":"real-2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	app, proxy := twoUpApp(t, u1, u2, GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 300})
	base := proxy.URL + "/sticky"

	// 只接受 POST。
	resp, err := http.Get(base + "?logical=m1&upstream=u2&model=real-2")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /sticky 应返回 405, 得到 %d", resp.StatusCode)
	}

	// 置到 u2/real-2: 立即生效。
	code, body := postJSON(t, base+"?logical=m1&upstream=u2&model=real-2", "")
	if code != http.StatusOK {
		t.Fatalf("POST /sticky 应返回 200, 得到 %d (%s)", code, body)
	}
	if got, _ := app.sticky.prefIdx("m1", 2, 0, time.Time{}); got != 1 {
		t.Fatalf("人工偏好应为 u2 (1), 实为 %d", got)
	}

	// 下一笔直接走 u2, u1 不再被碰。
	if code, _ := postJSON(t, proxy.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); code != http.StatusOK {
		t.Fatalf("应返回 200, 得到 %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if got := u1hits.Load(); got != 0 {
		t.Fatalf("置偏好后不该先试 u1, 实为 %d 次", got)
	}
	if got := u2hits.Load(); got != 1 {
		t.Fatalf("应由 u2 服务一笔, 实为 %d 次", got)
	}

	// 参数校验: 未知逻辑名 / 候选链里没有的候选 / 缺 logical。
	if code, _ := postJSON(t, base+"?logical=nope&upstream=u2&model=real-2", ""); code != http.StatusNotFound {
		t.Fatalf("未知逻辑名应返回 404, 得到 %d", code)
	}
	if code, _ := postJSON(t, base+"?logical=m1&upstream=u9", ""); code != http.StatusNotFound {
		t.Fatalf("候选链外上游应返回 404, 得到 %d", code)
	}
	if code, _ := postJSON(t, base+"?upstream=u2", ""); code != http.StatusBadRequest {
		t.Fatalf("缺 logical 应返回 400, 得到 %d", code)
	}

	// 清除该逻辑名的偏好: 回落配置首候选。
	if code, body := postJSON(t, base+"?logical=m1", ""); code != http.StatusOK {
		t.Fatalf("清除应返回 200, 得到 %d (%s)", code, body)
	}
	if got, _ := app.sticky.prefIdx("m1", 2, 0, time.Time{}); got != 0 {
		t.Fatalf("清除后应回落首候选, 实为 %d", got)
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
	if got, _ := app.sticky.prefIdx("m1", 2, 0, time.Time{}); got != 1 {
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
