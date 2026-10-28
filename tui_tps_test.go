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
	avg, peak, ok := tokRate(pts, now)
	if !ok || avg != 4200.0/65 || peak != 120 {
		t.Errorf("应为 平均 %.2f / 峰值 120, 得到 (%v, %v, %v)", 4200.0/65, avg, peak, ok)
	}
	if _, _, ok := tokRate([]tokPoint{{ts: now.Add(-2 * time.Hour), tok: 100, dur: 1000}}, now); ok {
		t.Error("窗口内没有样本时应 ok=false")
	}
	if _, _, ok := tokRate([]tokPoint{{ts: now, tok: 100}}, now); ok {
		t.Error("没有耗时的样本不能让分母为 0")
	}
}

// 第 1 页「接收中」的行在状态列写吞吐 = 已收到的输出 token / 首字节到达至今的秒数
// (排队与等首字节的时间不算在内)。
func TestFlowCellLiveTps(t *testing.T) {
	m := newTUIModel("cfg")
	recv := flow{ID: 1, Logical: "m1", Stage: stageRecv, RecvTok: 6000,
		RecvStart: time.Now().Add(-100 * time.Second).UnixMilli(),
		StartTime: time.Now().Add(-500 * time.Second)}
	cells := m.flowCells(recv)
	if got := cells[len(cells)-1]; got != "60.0 t/s" {
		t.Errorf("接收中的状态列应写 6000 token / 100s (从首字节起算), 得到 %q", got)
	}
	// 还没收到 token (非流式 / 刚开流) 时不显示, 与旧行为一致。
	fresh := flow{ID: 2, Logical: "m1", Stage: stageRecv, StartTime: time.Now()}
	if got := m.flowCells(fresh); got[len(got)-1] != "" {
		t.Errorf("没有 token 时状态列应为空, 得到 %q", got[len(got)-1])
	}
	send := flow{ID: 3, Logical: "m1", Stage: stageSend, RecvTok: 6000,
		RecvStart: time.Now().Add(-100 * time.Second).UnixMilli()}
	if got := m.flowCells(send); got[len(got)-1] != "" {
		t.Errorf("只有接收中才显示吞吐, 得到 %q", got[len(got)-1])
	}
}

// relayStream 边转发边回写已收到的输出 token 数与首字节时刻。
func TestRelayStreamLiveProgress(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":" world"}}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))
	var got []int
	var first time.Time
	ro := relayStream(httptest.NewRecorder(), body, "m1", "real-m1", time.Now(), false,
		func(tokens int, firstAt time.Time) { got = append(got, tokens); first = firstAt })
	if len(got) == 0 || got[len(got)-1] <= 0 {
		t.Fatalf("live 应至少回写一次正数 token, 得到 %v", got)
	}
	if first.IsZero() {
		t.Error("live 应带上首字节时刻")
	}
	if ro.Usage == nil || ro.Usage.Completion <= 0 {
		t.Errorf("流末 usage 应有用量, 得到 %+v", ro.Usage)
	}
}

// 日志聚合层: 每笔请求的 completion token 与耗时进 1h 窗口, 非流式也算。
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
	avg, peak, ok := tokRate(rows[0].Tok, now.Add(2*time.Minute))
	if !ok || avg != 75 || peak != 100 {
		t.Errorf("300 token / 4s 应为 平均 75 / 峰值 100, 得到 (%v, %v, %v)", avg, peak, ok)
	}
}
