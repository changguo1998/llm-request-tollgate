package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 模型/路由表的吞吐口径: 最近 1h 内的总输出 token 除以这些请求的耗时之和 (空闲不计);
// 同时给出单笔请求的峰值 (同一口径)。
func TestTokRate(t *testing.T) {
	now := time.Now()
	pts := []tokPoint{
		{ts: now.Add(-90 * time.Minute), tok: 99999, dur: 999000}, // 窗口外, 不计
		{ts: now.Add(-30 * time.Minute), tok: 1800, dur: 30000},   // 60 t/s
		{ts: now.Add(-10 * time.Minute), tok: 1800, dur: 30000},   // 60 t/s
		{ts: now.Add(-5 * time.Minute), tok: 600, dur: 5000},      // 120 t/s
	}
	avg, peak, ok := tokRate(pts, now, tuiTokWindow)
	if !ok || avg != 4200.0/65 || peak != 120 {
		t.Errorf("应为 平均 %.2f / 峰值 120, 得到 (%v, %v, %v)", 4200.0/65, avg, peak, ok)
	}
	if _, _, ok := tokRate([]tokPoint{{ts: now.Add(-2 * time.Hour), tok: 100, dur: 1000}}, now, tuiTokWindow); ok {
		t.Error("窗口内没有样本时应 ok=false")
	}
	if _, _, ok := tokRate([]tokPoint{{ts: now, tok: 100}}, now, tuiTokWindow); ok {
		t.Error("没有耗时的样本不能让分母为 0")
	}
}

// 显示口径 (rateShow): 平均 = avgWin 窗口内各样本速率的算术平均, 峰值 = peakWin 窗口内
// 单笔最高; 两个窗口互相独立, 窗口外的样本不计。
func TestRateShow(t *testing.T) {
	now := time.Now()
	pts := []tokPoint{
		{ts: now.Add(-10 * time.Minute), tok: 1000, dur: 10000}, // 100 t/s, avgWin 内
		{ts: now.Add(-30 * time.Minute), tok: 1000, dur: 10000}, // 100 t/s, avgWin 内
		{ts: now.Add(-2 * time.Hour), tok: 5000, dur: 10000},    // 500 t/s, 只在 peakWin 内
		{ts: now.Add(-4 * time.Hour), tok: 9999, dur: 10000},    // 两个窗口外, 不计
	}
	avg, peak, ok := rateShow(pts, now, time.Hour, 3*time.Hour)
	if !ok || avg != 100 || peak != 500 {
		t.Errorf("应为 平均 100 / 峰值 500, 得到 (%v, %v, %v)", avg, peak, ok)
	}
	if _, _, ok := rateShow(pts, now.Add(5*time.Hour), time.Hour, 3*time.Hour); ok {
		t.Error("窗口内没有样本时应 ok=false")
	}
}

// 首字节口口径: 平均 = win 窗口内算术平均, 谷值 = 全部样本中的最小一次。
func TestTtfbAvgMin(t *testing.T) {
	now := time.Now()
	pts := []ttfbPoint{
		{ts: now.Add(-10 * time.Minute), ms: 100},
		{ts: now.Add(-20 * time.Minute), ms: 300},
		{ts: now.Add(-2 * time.Hour), ms: 500}, // rateWin(1h) 外: 平均不计, 谷值仍算
	}
	avg, ok := ttfbAvg(pts, now, time.Hour)
	if !ok || avg != 200 {
		t.Errorf("1h 窗口内算术平均应为 200, 得到 (%v, %v)", avg, ok)
	}
	if got := ttfbMin(pts); got != 100 {
		t.Errorf("谷值应为 100, 得到 %d", got)
	}
	if _, ok := ttfbAvg(pts, now.Add(2*time.Hour), time.Hour); ok {
		t.Error("窗口内没有样本时应 ok=false")
	}
}

// 第 1 页阶段链在实时接收时按模型/路由表的同三个速度写实时吞吐, 只写一个, 不加前缀:
// 优先解码 (已收到的输出 token / 首字节到达至今), 其次总 (同一份输出 token / 请求总耗时),
// 最后下载 (已转发的响应字节 / 首字节到达至今)。
func TestFlowCellLiveTps(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	recv := flow{ID: 1, Logical: "m1", Stage: stageRecv, RecvTok: 6000, RecvBytes: 1_024_000,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		StartTime: now.Add(-500 * time.Second),
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount),
			CurAt: now.Add(-100 * time.Second).UnixMilli()}}}
	got := m.stageChain(recv, recv.Tries[0])
	if !strings.Contains(got, "60.0 t/s") { // 6000 / 100s
		t.Errorf("接收段的链尾应写解码速率 60.0 t/s, 实际 %q", got)
	}
	for _, unwanted := range []string{"12.0 t/s", "KB/s"} { // 总 / 下载都让位给解码
		if strings.Contains(got, unwanted) {
			t.Errorf("只该写一个速率, 不该出现 %q: %q", unwanted, got)
		}
	}
	// 只有字节 (没有可计的输出 token) 时只显示下载吞吐。
	onlyBytes := flow{ID: 4, Logical: "m1", Stage: stageRecv, RecvBytes: 512_000,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount),
			CurAt: now.Add(-100 * time.Second).UnixMilli()}}}
	if got := m.stageChain(onlyBytes, onlyBytes.Tries[0]); !strings.Contains(got, "5 KB/s") ||
		strings.Contains(got, "t/s") {
		t.Errorf("只有字节时应只写下载吞吐, 得到 %q", got)
	}
	// 还没收到任何东西 (非流式 / 刚开流) 时不写吞吐。
	fresh := flow{ID: 2, Logical: "m1", Stage: stageRecv, StartTime: now,
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount)}}}
	if got := m.stageChain(fresh, fresh.Tries[0]); strings.Contains(got, "t/s") {
		t.Errorf("没有收到内容时不该写吞吐, 得到 %q", got)
	}
	// 只有接收中那一段才显示吞吐。
	send := flow{ID: 3, Logical: "m1", Stage: stageSend, RecvTok: 6000, RecvBytes: 4096,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		Tries:     []flowTry{{Cur: stageSend, Segs: make([]float64, stageCount)}}}
	if got := m.stageChain(send, send.Tries[0]); strings.Contains(got, "t/s") {
		t.Errorf("只有接收中才显示吞吐, 得到 %q", got)
	}
}

// 只发思考增量 (delta.reasoning_content) 的流也要计入输出, 否则接收中的吞吐恒为空。
func TestRelayStreamCountsReasoning(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"thinking hard"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	var got []int
	ro := relayStream(httptest.NewRecorder(), body, "m1", "real-m1", time.Now(), false,
		func(tokens int, _ int64, firstAt time.Time) { got = append(got, tokens) })
	if len(got) == 0 || got[len(got)-1] <= 0 {
		t.Fatalf("思考增量应计入输出 token, 得到 %v", got)
	}
	if ro.Usage == nil || ro.Usage.Completion <= 0 {
		t.Errorf("没有 usage 时兜底估值也应大于 0, 得到 %+v", ro.Usage)
	}
}

// 日志聚合层: 每笔请求的 (prompt+completion) token 与耗时进 1h 窗口 (总吞吐),
// 非流式也算; 没带 decode/bytes 的日志, 解码与字节速率不记样本。
func TestModelRowsOneHourTokens(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	durs := []int{1000, 3000}
	for i, tok := range []int{100, 200} {
		line := fmt.Sprintf(`{"ts":%q,"logical":"m1","upstream":"up1","model":"real-m1","path":"/v1/chat/completions","duration_ms":%d,"usage":{"prompt":10,"completion":%d,"total":%d}}`,
			now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339Nano), durs[i], tok, tok+10)
		m.absorb([]byte(line))
	}
	rows := m.modelRows()
	if len(rows) != 1 {
		t.Fatalf("应有一行, 得到 %d", len(rows))
	}
	r := rows[0]
	avg, peak, ok := tokRate(r.TotalRate, now.Add(2*time.Minute), tuiTokWindow)
	if !ok || avg != 80 || peak != 110 {
		t.Errorf("(110+210) token / 4s 应为 平均 80 / 峰值 110, 得到 (%v, %v, %v)", avg, peak, ok)
	}
	if len(r.DecodeRate) != 0 || len(r.ByteRate) != 0 {
		t.Errorf("没有 decode_ms / bytes 的日志不该产生解码/字节样本: len=%d/%d",
			len(r.DecodeRate), len(r.ByteRate))
	}
}
