package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newPerfApp 造一个只有调度状态、没有日志与落盘的 App, 供低性能锁用例直接喂样本。
func newPerfApp(g GateCfg) (*App, *cand) {
	c := newCand("u1")
	return &App{cands: map[string]*cand{"u1": c}, gate: g}, c
}

// seedPerfHistory 预置历史性能值 (各笔 r*, 或退化原始速率): n 笔, 总和 sum; 历史平均 = sum/n。
func seedPerfHistory(c *cand, model string, n int64, sum float64) {
	c.mu.Lock()
	mg := c.gate(model)
	mg.perfN, mg.perfSum = n, sum
	c.mu.Unlock()
}

// 新增的相对门槛缺省 0.2, 超过 1 时夹到 1。
func TestLowPerfRelMaxDefault(t *testing.T) {
	if got := normGate(GateCfg{}).LowPerfRelMax; got != defaultLowPerfRelMax {
		t.Fatalf("缺省历史比门槛应为 %v, 得到 %v", defaultLowPerfRelMax, got)
	}
	if got := normGate(GateCfg{LowPerfRelMax: 1.5}).LowPerfRelMax; got != 1 {
		t.Fatalf("历史比门槛应夹到 1, 得到 %v", got)
	}
}

// 低性能锁 (低吞吐分支): 窗口内峰值与平均都低于绝对门槛, 且当前平均跌到历史平均的
// lowperf_rel_max 以下时锁住该模型, 时长取 lowperf_lock_ms (缺省 5min); 单笔样本不够
// 判定, 也不连坐同家。
func TestLowPerfLockTriggers(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	seedPerfHistory(c, "m1", 10, 500) // 历史平均 50 t/s: 当前 0.4 t/s 远低于它

	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "") // 0.4 t/s: 只有一笔, 不判
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("单笔样本不该触发低性能锁")
	}
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "") // 两笔 0.4 t/s: 峰值与平均都低于门槛
	locked, left, reason := c.locked("m1", "")
	if !locked || reason != lowPerfReason || left < 4*time.Minute {
		t.Fatalf("两笔 0.4 t/s 应触发低性能锁: locked=%v left=%s reason=%s", locked, left, reason)
	}
	c.mu.Lock()
	n, hist := len(c.gate("m1").tps), c.gate("m1").perfN
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("触发后窗口样本应清空 (解锁后从新样本重判), 实为 %d 笔", n)
	}
	if hist != 12 {
		t.Fatalf("历史样本应累计 12 笔 (含触发这两笔), 实为 %d", hist)
	}
	if locked, _, _ := c.locked("m2", ""); locked {
		t.Fatal("低性能锁只锁该模型, 不该连坐同家别的模型")
	}
}

// 历史样本不足 minPerfSamples 笔时不判低吞吐: 新模型先积累历史, 避免刚启动就锁。
func TestLowPerfLockNeedsPerfHistory(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))

	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("历史不足时不该触发低吞吐锁: reason=%s", reason)
	}
}

// 历史平均也不高时不锁: 当前平均没跌到历史平均的 lowperf_rel_max 以下。
func TestLowPerfLockHistoryRatio(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	seedPerfHistory(c, "m1", 10, 5) // 历史平均 0.5 t/s

	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("当前平均未跌到历史平均的 20%% 以下, 不该锁: reason=%s", reason)
	}
}

// 低性能锁 (高延迟分支): 窗口内平均首字节时延超过 lowperf_ttfb_max_ms 时锁住该模型,
// 即使没有吞吐样本 (如非流式成功请求)。
func TestLowPerfLockHighLatencyTriggers(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{LowPerfTtfbMaxMs: 100})) // 阈值 100ms

	a.notePerfRequest("u1", "m1", 0, 0, 300, 0, "") // 300ms: 只有一笔, 不判
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("单笔高延迟样本不该触发低性能锁")
	}
	a.notePerfRequest("u1", "m1", 0, 0, 300, 0, "") // 两笔平均 300ms > 100ms
	locked, left, reason := c.locked("m1", "")
	if !locked || reason != lowPerfReason || left < 4*time.Minute {
		t.Fatalf("平均 300ms 高延迟应触发低性能锁: locked=%v left=%s reason=%s", locked, left, reason)
	}
}

// 高延迟没超阈值就不锁: 平均首字节时延低于 lowperf_ttfb_max_ms。
func TestLowPerfLockHighLatencyBelowThreshold(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{LowPerfTtfbMaxMs: 100}))

	a.notePerfRequest("u1", "m1", 0, 0, 50, 0, "")
	a.notePerfRequest("u1", "m1", 0, 0, 50, 0, "") // 平均 50ms < 100ms
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("平均 50ms 不该触发高延迟锁: reason=%s", reason)
	}
}

// 高延迟阈值缺省 0 = 关闭高延迟判定, 只有低吞吐能触发。
func TestLowPerfLockLatencyDisabledByDefault(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))

	a.notePerfRequest("u1", "m1", 0, 0, 5000, 0, "")
	a.notePerfRequest("u1", "m1", 0, 0, 5000, 0, "") // 平均 5s, 但阈值 0 = 关闭
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("缺省阈值 0 不应触发高延迟锁: reason=%s", reason)
	}
}

// 峰值达标就不算低吞吐: 一笔 6 t/s 把窗口峰值抬到门槛之上, 平均再低也不锁。
func TestLowPerfLockPeakAboveThreshold(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))

	a.notePerfRequest("u1", "m1", 30, 5000, 0, 0, "") // 6 t/s: 峰值 >= 5
	a.notePerfRequest("u1", "m1", 1, 5000, 0, 0, "")  // 0.2 t/s: 只把平均拉低
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("峰值 6 t/s 不该触发低性能锁: reason=%s", reason)
	}
}

// 窗口外的样本不计: 样本量不够判定, 也就不会锁。
func TestLowPerfLockIgnoresExpiredSamples(t *testing.T) {
	g := normGate(GateCfg{})
	a, c := newPerfApp(g)

	c.mu.Lock()
	c.gate("m1").tps = append(c.gate("m1").tps,
		perfPt{ts: time.Now().Add(-2 * g.perfWindow()), v: 0.2})
	c.mu.Unlock()
	a.notePerfRequest("u1", "m1", 1, 5000, 0, 0, "")
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("窗口外的旧样本不该参与判定")
	}
}

// 已经在低性能锁里就不再重复加锁, 也不再往窗口里堆样本。
func TestLowPerfLockNotRepeated(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	seedPerfHistory(c, "m1", 10, 500) // 历史平均 50 t/s, 两笔 0.4 t/s 可触发

	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	_, first, _ := c.locked("m1", "")
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "") // 锁内又完成一笔 (锁前已发出)
	locked, second, reason := c.locked("m1", "")
	if !locked || reason != lowPerfReason {
		t.Fatalf("锁内应保持低性能锁: locked=%v reason=%s", locked, reason)
	}
	if second > first {
		t.Fatalf("锁内不该延长到期时间: 之前 %s, 现在 %s", first, second)
	}
}

// 代理链路: 一笔流式成功请求会进该模型的吞吐与首字节时延窗口样本 (低性能锁的输入);
// 非流式没有解码段, 不进吞吐样本。
func TestProxyRecordsPerfSamples(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		time.Sleep(5 * time.Millisecond) // 保证解码段跨毫秒, tps 才拆得出来
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	status, body := postJSON(t, proxy.URL+"/v1/chat/completions",
		`{"model":"m1","messages":[],"stream":true}`)
	if status != http.StatusOK {
		t.Fatalf("流式请求应为 200, 实为 %d %s", status, body)
	}
	c := app.cands["u1"]
	c.mu.Lock()
	nt := len(c.gate("real-1").tps)
	nl := len(c.gate("real-1").lat)
	c.mu.Unlock()
	if nt != 1 {
		t.Fatalf("流式成功后应记一笔解码吞吐样本, 实为 %d 笔", nt)
	}
	if nl != 1 {
		t.Fatalf("流式成功后应记一笔首字节时延样本, 实为 %d 笔", nl)
	}
	// decode_ms 也应落进请求日志 (流式有解码段, 但要等 handler 收尾写完日志)。
	if rec := waitLogRec(t, app, 2*time.Second); rec.DecodeMs <= 0 {
		t.Errorf("流式请求的日志应带 decode_ms>0, 得到 %d", rec.DecodeMs)
	}
}

// 豁免名单 (lowperf_exempt) 内的模型禁止触发低性能锁: 低吞吐与高延迟都不锁,
// 也不收集样本; 名单外模型照常判定。
func TestLowPerfLockExempt(t *testing.T) {
	g := normGate(GateCfg{LowPerfTtfbMaxMs: 100, LowPerfExempt: []string{"m1"}})
	a, c := newPerfApp(g)

	a.notePerfRequest("u1", "m1", 1, 5000, 500, 0, "") // 0.2 t/s + 500ms: 任一分支都够触发
	a.notePerfRequest("u1", "m1", 1, 5000, 500, 0, "")
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("豁免名单内的模型不该触发低性能锁: reason=%s", reason)
	}
	c.mu.Lock()
	n := len(c.gate("m1").tps) + len(c.gate("m1").lat)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("豁免模型不应收集性能样本, 实为 %d 笔", n)
	}

	// 名单外: 同样的两笔低吞吐照常触发。
	seedPerfHistory(c, "m2", 10, 500) // 名单外模型也需要历史平均
	a.notePerfRequest("u1", "m2", 1, 5000, 0, 0, "")
	a.notePerfRequest("u1", "m2", 1, 5000, 0, 0, "")
	if locked, _, reason := c.locked("m2", ""); !locked || reason != lowPerfReason {
		t.Fatalf("名单外模型应照常触发低性能锁: locked=%v reason=%s", locked, reason)
	}
}

// 代理链路: 已经触发的低性能锁里, 再成功一笔也不影响豁免名单的拦截。
func TestLowPerfLockExemptInProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		time.Sleep(2 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, LowPerfExempt: []string{"real-1"}}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	for i := 0; i < 4; i++ {
		status, body := postJSON(t, proxy.URL+"/v1/chat/completions",
			`{"model":"m1","messages":[],"stream":true}`)
		if status != http.StatusOK {
			t.Fatalf("流式请求应为 200, 实为 %d %s", status, body)
		}
	}
	c := app.cands["u1"]
	if locked, _, reason := c.locked("real-1", ""); locked {
		t.Fatalf("代理链路上豁免模型不该触发低性能锁: reason=%s", reason)
	}
}

// 低性能锁与熔断锁共用同一份落盘: 重启后按 reason=lowperf 恢复。
func TestLowPerfLockPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	a := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	seedPerfHistory(a.cands["u1"], "m1", 3, 60) // 历史平均 20 t/s, 随锁一起落盘
	a.addLockLowPerf(a.cands["u1"], "m1", 0.4, 0.4, 0, 5*time.Minute, "")

	restarted := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	restarted.loadGateState()
	locked, left, reason := restarted.cands["u1"].locked("m1", "")
	if !locked || reason != lowPerfReason || left < 4*time.Minute {
		t.Fatalf("重启后应恢复低性能锁: locked=%v left=%s reason=%s", locked, left, reason)
	}
	mg := restarted.cands["u1"].gate("m1")
	if mg.perfN != 3 || mg.perfSum != 60 {
		t.Fatalf("重启后应恢复历史性能样本: n=%d sum=%v", mg.perfN, mg.perfSum)
	}
}

// 历史性能样本在没有锁时也会合并写盘, 重启后继续作为相对门槛的基线。
func TestPerfHistoryPersistsWithoutLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	a := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path, gate: normGate(GateCfg{})}
	a.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "") // 0.4 t/s, 单笔不够触发锁

	restarted := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	restarted.loadGateState()
	mg := restarted.cands["u1"].gate("m1")
	if mg.perfN != 1 || mg.perfSum < 0.39 || mg.perfSum > 0.41 {
		t.Fatalf("重启后应恢复历史性能样本: n=%d sum=%v", mg.perfN, mg.perfSum)
	}
}

// 重启恢复历史平均后, 新样本仍能按相对门槛触发低性能锁。
func TestLowPerfLockAfterHistoryRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	a := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	seedPerfHistory(a.cands["u1"], "m1", 10, 500) // 历史平均 50 t/s
	a.saveGateState()

	restarted := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path,
		gate: normGate(GateCfg{})}
	restarted.loadGateState()
	restarted.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	restarted.notePerfRequest("u1", "m1", 2, 5000, 0, 0, "")
	locked, _, reason := restarted.cands["u1"].locked("m1", "")
	if !locked || reason != lowPerfReason {
		t.Fatalf("恢复历史后两笔 0.4 t/s 应触发低性能锁: locked=%v reason=%s", locked, reason)
	}
}

// 低性能锁恢复解锁后, 观察窗口 (perf_window) 内又锁: 重锁时长翻倍递增, 封顶 8 倍
// 基础时长 —— 没真恢复的候选慢慢退成长期死锁, 抖动自衰减。
func TestLowPerfLockRelockEscalatesInWindow(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	base := 5 * time.Minute
	check := func(label string, want time.Duration) {
		t.Helper()
		locked, left, reason := c.locked("m1", "")
		if !locked || reason != lowPerfReason {
			t.Fatalf("%s: 应有低性能锁, locked=%v reason=%s", label, locked, reason)
		}
		if left < want-10*time.Second || left > want+10*time.Second {
			t.Fatalf("%s: 锁时长应约为 %s, 实为 %s", label, want, left)
		}
	}
	reUnlock := func() {
		t.Helper()
		c.mu.Lock()
		c.gate("m1").lock = lockState{} // 模拟探测通过解锁
		c.gate("m1").unlockAt = time.Now()
		c.mu.Unlock()
	}

	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "") // 首锁 (从未解锁): 基础时长
	check("首锁", base)
	reUnlock()
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "") // 窗口内第 1 次重锁: 2x
	check("第 1 次重锁", base*2)
	reUnlock()
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "") // 第 2 次: 4x
	check("第 2 次重锁", base*4)
	reUnlock()
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "") // 第 3 次: 8x 封顶
	check("第 3 次重锁", base*8)
	reUnlock()
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "") // 第 4 次: 仍 8x
	check("第 4 次重锁 (封顶)", base*8)
}

// 超过一个观察窗口才重锁: 轮次归零, 回到基础时长 (候选可能真恢复了)。
func TestLowPerfLockRelockResetsAfterWindow(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	base := 5 * time.Minute
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "")
	c.mu.Lock()
	c.gate("m1").lock = lockState{}
	c.gate("m1").unlockAt = time.Now().Add(-700 * time.Second) // 上次解锁超过窗口 (600s)
	c.mu.Unlock()
	a.addLockLowPerf(c, "m1", 0.4, 0.4, 0, base, "")
	locked, left, reason := c.locked("m1", "")
	if !locked || reason != lowPerfReason || left < 4*time.Minute || left > 6*time.Minute {
		t.Fatalf("超过观察窗口应回到基础时长: locked=%v left=%s reason=%s", locked, left, reason)
	}
}

// 有参数表 + 响应字节时, 低性能样本按该请求的 r* 记: 样本值 = (c/d)*(c/b)^(-g)。
func TestLowPerfRequestRecordsPerfValue(t *testing.T) {
	a, c := newPerfApp(normGate(GateCfg{}))
	a.perfRef = &PerfRef{byModel: map[string]perfParam{"u1/m1": {G: 1}}}
	// rate = 10/1000*1000 = 10 t/s; c/b = 10/1000 = 0.01; ^(-1) -> 10/0.01 = 1000
	a.notePerfRequest("u1", "m1", 10, 1000, 0, 1000, "")
	c.mu.Lock()
	n := len(c.gate("m1").tps)
	v := 0.0
	if n > 0 {
		v = c.gate("m1").tps[0].v
	}
	hist := c.gate("m1").perfSum
	c.mu.Unlock()
	if n != 1 || v < 999 || v > 1001 || hist < 999 || hist > 1001 {
		t.Fatalf("样本应按 r* 记 1000: n=%d v=%v hist=%v", n, v, hist)
	}
}
