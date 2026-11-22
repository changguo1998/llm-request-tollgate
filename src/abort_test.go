// abort_test.go: 流式打断 (解码超时 + 实时性能过低) 的单测与端到端测试。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 打断判定的四条件边界: 关闭 / 时长未超 / 绝对门槛 / 历史不足 / 相对门槛 / 等号边界。
func TestShouldAbortStream(t *testing.T) {
	g := GateCfg{AbortTimeoutMs: 1000, LowPerfAvgMax: 15, LowPerfRelMax: 0.2}
	cases := []struct {
		name    string
		g       GateCfg
		d, r    float64
		perfN   int64
		perfSum float64
		want    bool
	}{
		{"关闭不打", GateCfg{AbortTimeoutMs: 0, LowPerfAvgMax: 15, LowPerfRelMax: 0.2}, 1500, 3, 10, 1000, false},
		{"时长未超过", g, 999, 3, 10, 1000, false},
		{"时长刚好等号不超", g, 1000, 3, 10, 1000, false},
		{"实时性能高于绝对门槛", g, 1500, 15, 10, 1000, false},
		{"实时性能为零不打断", g, 1500, 0, 10, 1000, false},
		{"历史样本不足", g, 1500, 3, 1, 1000, false},
		{"历史样本为空", g, 1500, 3, 0, 0, false},
		{"相对门槛未跌破", g, 1500, 25, 10, 1000, false},
		{"相对等号边界不打断", g, 1500, 20, 10, 1000, false}, // hist=100, 20% = 20
		{"命中", g, 1500, 3, 10, 1000, true},
		// 相对门槛的等号下沿: 平均门槛抬高到 30 后, r=19.9 低于 20%×hist 仍应命中。
		{"相对边界内命中", GateCfg{AbortTimeoutMs: 1000, LowPerfAvgMax: 30, LowPerfRelMax: 0.2}, 1500, 19.9, 10, 1000, true},
	}
	for _, c := range cases {
		if got := shouldAbortStream(int64(c.d), c.r, c.perfN, c.perfSum, c.g); got != c.want {
			t.Errorf("%s: shouldAbortStream = %v, 期望 %v", c.name, got, c.want)
		}
	}
}

// 配置口径: 未写 abort_timeout_sec 缺省 60s; 显式 0 = 关闭; 显式值按秒转毫秒。
func TestConfigAbortTimeout(t *testing.T) {
	var c Config
	if got := c.limitAbortTimeoutSec(); got != 60 {
		t.Errorf("未写应缺省 60s, 得到 %v", got)
	}
	if got := c.gate().AbortTimeoutMs; got != 60000 {
		t.Errorf("未写应转 60000ms, 得到 %d", got)
	}
	z := 0.0
	c.AbortTimeoutSec = &z
	if got := c.gate().AbortTimeoutMs; got != 0 {
		t.Errorf("显式 0 应关闭打断 (0ms), 得到 %d", got)
	}
	f := 15.0
	c.AbortTimeoutSec = &f
	if got := c.gate().AbortTimeoutMs; got != 15000 {
		t.Errorf("15s 应转 15000ms, 得到 %d", got)
	}
}

// 配置 JSON 装载: abort_timeout_sec 显式 0 保持关闭 (normGate 不该给它补缺省)。
func TestConfigAbortTimeoutLoadFromJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tollgate.json")
	if err := os.WriteFile(path, []byte(`{"abort_timeout_sec": 0, "upstreams": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfigOpt(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.gate().AbortTimeoutMs; got != 0 {
		t.Errorf("显式 0 装载后应为 0ms, 得到 %d", got)
	}
}

// cand 的历史性能 / 窗口峰值 / 低性能锁查询 (打断判定的数据来源)。
func TestCandAbortHelpers(t *testing.T) {
	cd := newCand("u1")
	if _, _, ok := cd.perfHist("m"); ok {
		t.Error("没有样本时应 ok=false")
	}
	if p := cd.windowLowPerfPeak("m"); p != 0 {
		t.Errorf("没有窗口样本时峰值应 0, 得到 %v", p)
	}
	if cd.modelLowPerfLocked("m") {
		t.Error("没有锁时不应判定为 lowperf")
	}

	cd.lockModel("m", lowPerfReason, time.Hour, "")
	if !cd.modelLowPerfLocked("m") {
		t.Error("lowperf 锁内应判定为 true")
	}
	cd.lockModel("m", "breaker", time.Hour, "") // 熔断锁覆盖: 不是 lowperf
	if cd.modelLowPerfLocked("m") {
		t.Error("熔断锁不是 lowperf, 不应判定")
	}

	cd.mu.Lock()
	mg := cd.gate("m")
	mg.perfN, mg.perfSum = 5, 100
	now := time.Now()
	mg.tps = []perfPt{{ts: now, v: 3}, {ts: now, v: 9}}
	cd.mu.Unlock()
	if n, sum, ok := cd.perfHist("m"); !ok || n != 5 || sum != 100 {
		t.Errorf("历史样本应 (5, 100), 得到 (%d, %v, %v)", n, sum, ok)
	}
	if p := cd.windowLowPerfPeak("m"); p != 9 {
		t.Errorf("窗口峰值应为 9, 得到 %v", p)
	}
}

// relay 层: live 回调查断后流立即停读, 补 error 事件, 且 relayOut 带 Aborted/AbortMs/AbortR。
func TestRelayStreamAbort(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n"))
	rec := httptest.NewRecorder()
	ro := relayStream(rec, body, "m1", "real-1", time.Now(), false,
		func(tokens int, bytes int64, firstAt time.Time, hint *relayHint) {
			if hint.Abort {
				return
			}
			hint.Abort, hint.AbortMs, hint.AbortR = true, 1234, 3.5
		})
	if !ro.Aborted || ro.AbortMs != 1234 || ro.AbortR != 3.5 {
		t.Fatalf("应记录打断: %+v", ro)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") {
		t.Errorf("打断应补 error 事件:\n%s", out)
	}
	if strings.Contains(out, `"content":"b"`) || strings.Contains(out, "[DONE]") {
		t.Errorf("打断后应停读, 后续块不该转发:\n%s", out)
	}
	if ro.Bytes <= 0 || !ro.Missing {
		t.Errorf("打断仍在转发中, 应缺 usage 且有字节: %+v", ro)
	}
}

// relay 层: 流已到终态 ([DONE]) 后不再判打断 —— 正常收尾的流不该被误判成打断。
func TestRelayStreamNoAbortAfterDone(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: [DONE]`,
		"",
	}, "\n"))
	rec := httptest.NewRecorder()
	ro := relayStream(rec, body, "m1", "real-1", time.Now(), false,
		func(tokens int, bytes int64, firstAt time.Time, hint *relayHint) {
			hint.Abort, hint.AbortMs, hint.AbortR = true, 1, 1 // 若被调用即判打断
		})
	if ro.Aborted {
		t.Errorf("终态后不该判打断: %+v", ro)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("终态数据应透传:\n%s", rec.Body.String())
	}
}

// checkStreamAbort 的豁免分支: 名单内的模型即使解码超时也不判打断; 非豁免且条件齐备会命中。
func TestCheckStreamAbortExemptAndHit(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1", GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200,
		AbortTimeoutMs: 100, LowPerfAvgMax: 15, LowPerfRelMax: 0.2, LowPerfLockMs: 2000,
		RetryBackoffMs: []int{5, 10, 15}}, nil)
	cd := app.candOf("u1")
	cd.mu.Lock()
	mg := cd.gate("real-1")
	mg.perfN, mg.perfSum = 2, 200 // hist = 100
	cd.mu.Unlock()
	up := &UpstreamCfg{ID: "u1"}

	// 豁免: 即使解码超时 + 性能极低也不命中。
	exempt := &serveState{app: app, gate: app.gate, servedUp: up, heldCand: cd}
	exempt.gate.LowPerfExempt = []string{"real-1"}
	h1 := &relayHint{}
	exempt.checkStreamAbort("real-1", 10, 100, time.Now().Add(-2*time.Second), h1)
	if exempt.aborted || h1.Abort {
		t.Errorf("豁免模型不应打断: aborted=%v hint=%+v", exempt.aborted, h1)
	}

	// 非豁免: d=2s > 100ms, r=10/2000*1000=5 < 15, hist=100, 5 < 20 => 命中。
	hit := &serveState{app: app, gate: app.gate, servedUp: up, heldCand: cd}
	h2 := &relayHint{}
	hit.checkStreamAbort("real-1", 10, 100, time.Now().Add(-2*time.Second), h2)
	if !hit.aborted || !h2.Abort || hit.abortMs <= 100 || hit.abortR <= 0 || hit.abortR >= 15 {
		t.Errorf("非豁免应命中: aborted=%v hint=%+v", hit.aborted, h2)
	}
	if n, sum, ok := cd.perfHist("real-1"); !ok || n != 2 || sum != 200 {
		t.Errorf("判定本身不应改样本: n=%d sum=%v ok=%v", n, sum, ok)
	}
}

// checkLowPerf 的展示判定: 四路低性能阈值 (绝对平均过低 / 相对过低 / 解码时长过长 /
// 首字节时延过长) 任一命中即 true, 只供 TUI 变色, 不置打断态; 豁免名单内的模型不判。
func TestCheckLowPerf(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1", GateCfg{IntervalMs: 5}, nil)
	cd := app.candOf("u1")
	cd.mu.Lock()
	mg := cd.gate("real-1")
	mg.perfN, mg.perfSum = 2, 200 // hist = 100, 相对门槛 20% = 20
	cd.mu.Unlock()

	base := GateCfg{LowPerfAvgMax: 15, LowPerfRelMax: 0.2, LowPerfTtfbMaxMs: 0, AbortTimeoutMs: 0}
	exempt := base
	exempt.LowPerfExempt = []string{"real-1"}
	now := time.Now()
	cases := []struct {
		name     string
		g        GateCfg
		tokens   int
		bytes    int64
		firstAgo time.Duration
		want     bool
	}{
		// r = tokens/firstAgo(ms)*1000; sentAt 固定 1s 前 (ttfb = 1s - firstAgo)。
		{"未命中", base, 1, 100, 40 * time.Millisecond, false},     // r≈24.4, >= 20 且 >= 15
		{"绝对平均过低", base, 10, 100, 800 * time.Millisecond, true}, // r≈12.5 < 15
		{"相对过低", base, 1, 100, 53 * time.Millisecond, true},     // r≈18.5 < 20, 且 >= 15
		{"解码时长过长", GateCfg{LowPerfAvgMax: 15, AbortTimeoutMs: 500}, 10, 100, time.Second, true},
		{"高延迟(ttfb过长)", GateCfg{LowPerfTtfbMaxMs: 300}, 0, 0, 500 * time.Millisecond, true}, // ttfb=500ms>300
		{"豁免名单", exempt, 10, 100, 800 * time.Millisecond, false},
	}
	for _, c := range cases {
		st := &serveState{app: app, gate: c.g, servedUp: &UpstreamCfg{ID: "u1"}, heldCand: cd,
			sentAt: now.Add(-time.Second)}
		if got := st.checkLowPerf("real-1", c.tokens, c.bytes, now.Add(-c.firstAgo)); got != c.want {
			t.Errorf("%s: checkLowPerf = %v, 期望 %v", c.name, got, c.want)
		}
		if st.aborted {
			t.Errorf("%s: checkLowPerf 只做展示判定, 不该置打断态", c.name)
		}
	}
}

// settings 页: 打断超时的编辑 (0 = 关闭) 与 flat 写回往返。
func TestGateAbortSettings(t *testing.T) {
	_, cfg := prioFixture(t)
	e := newTuiEditor(cfg)
	if got := e.gateField(gateIdxAbort); got != 60000 {
		t.Fatalf("缺省打断超时应 60000ms, 得到 %d", got)
	}
	e.gateDirty = false
	e.setGatePrompt(gateIdxAbort, "0")
	if e.gate.AbortTimeoutMs != 0 || !e.gateDirty {
		t.Errorf("输入 0 应关闭打断并标脏: ms=%d dirty=%v", e.gate.AbortTimeoutMs, e.gateDirty)
	}
	e.setGatePrompt(gateIdxAbort, "30")
	if e.gate.AbortTimeoutMs != 30000 {
		t.Errorf("30s 应写 30000ms, 得到 %d", e.gate.AbortTimeoutMs)
	}
	v := e.gate.AbortTimeoutMs
	e.gateDirty = false
	e.setGatePrompt(gateIdxAbort, "-1")
	if e.gate.AbortTimeoutMs != v || e.gateDirty || !strings.Contains(e.message, gateCN[gateIdxAbort]) {
		t.Errorf("负值应被拒且不改值: ms=%d dirty=%v msg=%q", e.gate.AbortTimeoutMs, e.gateDirty, e.message)
	}
	flat := e.gateFlat(normGate(e.gate))
	if f, ok := flat["abort_timeout_sec"]; !ok || f != float64(30) {
		t.Errorf("gateFlat 应写 abort_timeout_sec=30, 得到 %v ok=%v", f, ok)
	}
}

// 日志回放: 打断行还原成 abort 事件, 文案与实时事件一致 (带 hist)。
func TestReplayAbortEvent(t *testing.T) {
	ts := time.Now()
	r := logRec{Ts: ts.Format(time.RFC3339Nano), Req: 100, Logical: "m1", Upstream: "u1",
		Model: "r1", Status: 200, Abort: true, AbortR: 3.5, AbortHist: 58}
	e, ok := replayEventOf(r, ts)
	if !ok || e.Kind != "abort" || !strings.Contains(e.Msg, "abort(r 3.5 / hist 58") {
		t.Errorf("应还原带 hist 的 abort 事件: ok=%v e=%+v", ok, e)
	}
	r2 := r
	r2.AbortHist = 0
	e2, ok2 := replayEventOf(r2, ts)
	if !ok2 || strings.Contains(e2.Msg, "hist") {
		t.Errorf("无历史样本时文案不应带 hist: ok=%v msg=%q", ok2, e2.Msg)
	}
}

// 端到端: 慢流在解码超过打断超时且实时性能过低时被掐断, 客户端收到 error 事件,
// 该 上游/模型 进低性能锁, 日志带 abort 字段, 且打断请求不计入性能样本。
func TestStreamAbortIntegration(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl, _ := w.(http.Flusher)
		delay := 20 * time.Millisecond
		if r.Header.Get("X-Slow") != "" {
			delay = 250 * time.Millisecond
		}
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(delay)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer up.Close()

	app := newTestApp(t, up.URL, GateCfg{IntervalMs: 5, SemWaitMs: 50, QueueWaitMs: 200,
		AbortTimeoutMs: 600, LowPerfAvgMax: 15, LowPerfRelMax: 0.2, LowPerfLockMs: 2000,
		RetryBackoffMs: []int{5, 10, 15}}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	streamBody := `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	// 先两笔快流, 攒够 2 个历史性能样本 (相对门槛需要 minPerfSamples)。
	for i := 0; i < 2; i++ {
		if status, _ := postJSON(t, proxy.URL+"/v1/chat/completions", streamBody); status != 200 {
			t.Fatalf("快流应成功, 实为 %d", status)
		}
	}
	cd := app.candOf("u1")
	n0, sum0, ok := cd.perfHist("real-1")
	if !ok || n0 < 2 {
		t.Fatalf("快流应攒够历史样本, 得到 n=%d ok=%v", n0, ok)
	}

	// 慢流: 解码约 750ms 后该被打断。
	req, err := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(streamBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Slow", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("打断流应保持 HTTP 200, 实为 %d", resp.StatusCode)
	}
	if !strings.Contains(string(b), "event: error") {
		t.Errorf("客户端应收到 error 事件, 实际响应:\n%s", b)
	}

	rec := waitLogRec(t, app, 3*time.Second)
	if !rec.Abort {
		t.Fatalf("慢流日志应标 abort: %+v", rec)
	}
	if rec.AbortMs <= 600 || rec.AbortR <= 0 || rec.AbortHist <= 0 {
		t.Errorf("abort_ms 应超过打断超时、abort_r 与 abort_hist 应为正: %+v", rec)
	}
	if !cd.modelLowPerfLocked("real-1") {
		t.Error("打断后该模型应进低性能锁")
	}
	// 打断请求不计样本: 历史样本笔数与总和都不该变。
	if n, sum, ok := cd.perfHist("real-1"); !ok || n != n0 || sum != sum0 {
		t.Errorf("打断请求不应计入性能样本: 前 (%d, %v) -> 后 (%d, %v)", n0, sum0, n, sum)
	}
	// 锁落盘: gate-state.json 里有该模型的 lowperf 锁。
	stPath := filepath.Join(filepath.Dir(app.logW.Name()), "gate-state.json")
	stb, err := os.ReadFile(stPath)
	if err != nil {
		t.Fatalf("gate-state.json 应存在: %v", err)
	}
	var st gateState
	if err := json.Unmarshal(stb, &st); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range st.Locks {
		if l.Upstream == "u1" && l.Model == "real-1" && l.Reason == lowPerfReason {
			found = true
		}
	}
	if !found {
		t.Errorf("gate-state.json 应有 u1/real-1 的 lowperf 锁: %s", stb)
	}
	// 观测: 有 abort 事件, 且同一请求不再发 ok。
	evs, _ := app.eventsSince(0)
	var abortReq uint64
	hasAbort, hasOk := false, false
	for _, e := range evs {
		if e.Kind == "abort" && e.Req > 0 {
			hasAbort, abortReq = true, e.Req
		}
		if e.Kind == "ok" && e.Req == abortReq {
			hasOk = true
		}
	}
	if !hasAbort {
		t.Error("事件流应有 abort 事件")
	}
	if hasOk {
		t.Error("打断的请求不该发 ok 事件")
	}
}
