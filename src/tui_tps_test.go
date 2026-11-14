package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

// 第 1 页阶段链在实时接收时把链尾写成实时性能 (r*), 用当前参数表现算;
// 样本不齐 (没有 token 或字节) 时不写, 缺参数时退原始解码速率。
func TestFlowCellLiveTps(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	m.perfIdx = func(up, model string, c, d, b int64) float64 {
		if up == "ustc" && model == "real1" {
			return float64(c) / float64(d) * 1000 * 2 // 模拟长度归一: 原始速率的 2 倍
		}
		return float64(c) / float64(d) * 1000
	}
	recv := flow{ID: 1, Logical: "m1", Up: "ustc", Model: "real1", Stage: stageRecv,
		RecvTok: 6000, RecvBytes: 1_024_000,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		StartTime: now.Add(-500 * time.Second),
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount),
			CurAt: now.Add(-100 * time.Second).UnixMilli()}}}
	got := m.stageChain(recv, recv.Tries[0])
	if !strings.Contains(got, "120.0") { // 6000/100s=60 t/s, perfIdx 修正成 120
		t.Errorf("接收段链尾应写性能 120.0, 实际 %q", got)
	}
	// 实时性能带青色, 同一行的耗时仍是前景色 (青色只出现一次)。
	if !strings.Contains(got, tuiLive+"120.0"+tuiReset) {
		t.Errorf("实时性能应带青色: %q", got)
	}
	if strings.Count(got, tuiLive) != 1 {
		t.Errorf("青色只该出现在性能上: %q", got)
	}
	for _, unwanted := range []string{"60.0", " t/s", "KB/s"} { // 只写修正后的性能, 不带单位, 不再写原始/下载
		if strings.Contains(got, unwanted) {
			t.Errorf("只该写一个性能值, 不该出现 %q: %q", unwanted, got)
		}
	}
	// 只有字节 (没有可计的输出 token) 时性能不可算, 不写。
	onlyBytes := flow{ID: 4, Logical: "m1", Up: "ustc", Model: "real1", Stage: stageRecv,
		RecvBytes: 512_000,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount),
			CurAt: now.Add(-100 * time.Second).UnixMilli()}}}
	if got := m.livePerf(onlyBytes); got != "" {
		t.Errorf("只有字节时不该写速率/性能, 得到 %q", got)
	}
	// 还没收到任何东西 (非流式 / 刚开流) 时不写性能。
	fresh := flow{ID: 2, Logical: "m1", Stage: stageRecv, StartTime: now,
		Tries: []flowTry{{Cur: stageRecv, Segs: make([]float64, stageCount)}}}
	if got := m.livePerf(fresh); got != "" {
		t.Errorf("没有收到内容时不该写性能, 得到 %q", got)
	}
	// 只有接收中那一段才显示性能。
	send := flow{ID: 3, Logical: "m1", Up: "ustc", Model: "real1", Stage: stageSend,
		RecvTok: 6000, RecvBytes: 4096,
		RecvStart: now.Add(-100 * time.Second).UnixMilli(),
		Tries:     []flowTry{{Cur: stageSend, Segs: make([]float64, stageCount)}}}
	if got := m.livePerf(send); got != "" {
		t.Errorf("只有接收中才显示性能, 得到 %q", got)
	}
	// 缺参数 (远端模式) 时退原始解码速率。
	m.perfIdx = nil
	if got := m.livePerf(recv); !strings.Contains(got, "60.0") {
		t.Errorf("无参数时应退原始速率 60.0, 得到 %q", got)
	}
	// 请求表链尾的性能只写数字, 不带 t/s 单位。
	if got := m.stageChain(recv, recv.Tries[0]); strings.Contains(got, " t/s") {
		t.Errorf("请求表性能应只写数字, 不带 t/s 单位: %q", got)
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

// 日志聚合层: 没带 decode_ms / bytes 的日志不产生性能样本。
func TestModelRowsNoPerfSamples(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	for i, tok := range []int{100, 200} {
		line := fmt.Sprintf(`{"ts":%q,"logical":"m1","upstream":"up1","model":"real-m1","path":"/v1/chat/completions","duration_ms":%d,"usage":{"prompt":10,"completion":%d,"total":%d}}`,
			now.Add(-time.Duration(i)*time.Minute).Format(time.RFC3339Nano), []int{1000, 3000}[i], tok, tok+10)
		m.absorb([]byte(line))
	}
	rows := m.modelRows()
	if len(rows) != 1 {
		t.Fatalf("应有一行, 得到 %d", len(rows))
	}
	if len(rows[0].PerfPts) != 0 {
		t.Errorf("没有 decode_ms / bytes 的日志不该产生性能样本: len=%d", len(rows[0].PerfPts))
	}
}
