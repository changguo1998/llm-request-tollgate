package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newTpsApp 造一个只有调度状态、没有日志与落盘的 App, 供低流量锁用例直接喂样本。
func newTpsApp(g GateCfg) (*App, *cand) {
	c := newCand("u1")
	return &App{cands: map[string]*cand{"u1": c}, gate: g}, c
}

// 低流量锁: 窗口内解码吞吐的峰值与平均都低于阈值 (缺省 5 / 1 t/s) 时锁住该模型,
// 时长取 lowtps_lock_ms (缺省 5min); 单笔样本不够判定, 也不连坐同家别的模型。
func TestLowTpsLockTriggers(t *testing.T) {
	a, c := newTpsApp(normGate(GateCfg{}))

	a.noteDecodeTps("u1", "m1", 2, 5000) // 0.4 t/s: 只有一笔, 不判
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("单笔样本不该触发低流量锁")
	}
	a.noteDecodeTps("u1", "m1", 2, 5000) // 两笔 0.4 t/s: 峰值与平均都低于门槛
	locked, left, reason := c.locked("m1", "")
	if !locked || reason != lowTpsReason || left < 4*time.Minute {
		t.Fatalf("两笔 0.4 t/s 应触发低流量锁: locked=%v left=%s reason=%s", locked, left, reason)
	}
	c.mu.Lock()
	n := len(c.gate("m1").tps)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("触发后窗口样本应清空 (解锁后从新样本重判), 实为 %d 笔", n)
	}
	if locked, _, _ := c.locked("m2", ""); locked {
		t.Fatal("低流量锁只锁该模型, 不该连坐同家别的模型")
	}
}

// 峰值达标就不算低流量: 一笔 6 t/s 把窗口峰值抬到门槛之上, 平均再低也不锁。
func TestLowTpsLockPeakAboveThreshold(t *testing.T) {
	a, c := newTpsApp(normGate(GateCfg{}))

	a.noteDecodeTps("u1", "m1", 30, 5000) // 6 t/s: 峰值 >= 5
	a.noteDecodeTps("u1", "m1", 1, 5000)  // 0.2 t/s: 只把平均拉低
	if locked, _, reason := c.locked("m1", ""); locked {
		t.Fatalf("峰值 6 t/s 不该触发低流量锁: reason=%s", reason)
	}
}

// 窗口外的样本不计: 样本量不够判定, 也就不会锁。
func TestLowTpsLockIgnoresExpiredSamples(t *testing.T) {
	g := normGate(GateCfg{})
	a, c := newTpsApp(g)

	c.mu.Lock()
	c.gate("m1").tps = append(c.gate("m1").tps,
		tokPoint{ts: time.Now().Add(-2 * g.lowTpsWindow()), tok: 1, dur: 5000})
	c.mu.Unlock()
	a.noteDecodeTps("u1", "m1", 1, 5000)
	if locked, _, _ := c.locked("m1", ""); locked {
		t.Fatal("窗口外的旧样本不该参与判定")
	}
}

// 已经在低流量锁里就不再重复加锁, 也不再往窗口里堆样本。
func TestLowTpsLockNotRepeated(t *testing.T) {
	a, c := newTpsApp(normGate(GateCfg{}))

	a.noteDecodeTps("u1", "m1", 2, 5000)
	a.noteDecodeTps("u1", "m1", 2, 5000)
	_, first, _ := c.locked("m1", "")
	a.noteDecodeTps("u1", "m1", 2, 5000) // 锁内又完成一笔 (锁前已发出)
	locked, second, reason := c.locked("m1", "")
	if !locked || reason != lowTpsReason {
		t.Fatalf("锁内应保持低流量锁: locked=%v reason=%s", locked, reason)
	}
	if second > first {
		t.Fatalf("锁内不该延长到期时间: 之前 %s, 现在 %s", first, second)
	}
}

// 代理链路: 一笔流式成功请求的解码吞吐会进该模型的窗口样本 (低流量锁的输入);
// 非流式没有解码段, 不进样本。
func TestProxyRecordsDecodeTps(t *testing.T) {
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
	n := len(c.gate("real-1").tps)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("流式成功后应记一笔解码吞吐样本, 实为 %d 笔", n)
	}
}

// 低流量锁与熔断锁共用同一份落盘: 重启后按 reason=lowtps 恢复。
func TestLowTpsLockPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	a := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	a.addLockLowTps(a.cands["u1"], "m1", 0.4, 0.4, 5*time.Minute)

	restarted := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	restarted.loadGateState()
	locked, left, reason := restarted.cands["u1"].locked("m1", "")
	if !locked || reason != lowTpsReason || left < 4*time.Minute {
		t.Fatalf("重启后应恢复低流量锁: locked=%v left=%s reason=%s", locked, left, reason)
	}
}
