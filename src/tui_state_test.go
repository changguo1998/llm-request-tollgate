package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockOfStage 把阶段归到分块: 解析与信号量同属并发池, 已失败自成一块, 未知阶段返回 -1。
func TestBlockOfStage(t *testing.T) {
	cases := []struct {
		stage int
		want  int
	}{
		{stageParse, 0},
		{stageSem, 0},
		{stageQueue, 1},
		{stageModelWait, 2},
		{stageSend, 3},
		{stageRecv, 4},
		{stageFailed, 5},
		{-1, -1},
		{stageCount + 10, -1},
	}
	for _, c := range cases {
		if got := blockOfStage(c.stage); got != c.want {
			t.Errorf("阶段 %d 应属于第 %d 块, 得到 %d", c.stage, c.want, got)
		}
	}
}

// noteRows 每次采样记下各块的在飞笔数; blockCounts 用同一口径算此刻的笔数。
func TestNoteRows(t *testing.T) {
	m := newTUIModel("cfg")
	m.pulse.Active = []flow{
		{ID: 1, Stage: stageParse},     // 并发池
		{ID: 2, Stage: stageSem},       // 并发池
		{ID: 3, Stage: stageQueue},     // 频率队列
		{ID: 4, Stage: stageModelWait}, // 状态锁
		{ID: 5, Stage: stageSend},      // 已发送
		{ID: 6, Stage: stageRecv},      // 接收中
		{ID: 7, Stage: stageFailed},    // 已失败
	}
	want := []int{2, 1, 1, 1, 1, 1}
	m.noteRows(time.Now())
	if len(m.rowHist) != 1 {
		t.Fatalf("应记下 1 个样本, 得到 %d", len(m.rowHist))
	}
	if got := m.rowHist[0].cnt; !sameInts(got, want) {
		t.Errorf("样本里的块笔数应为 %v, 得到 %v", want, got)
	}
	if got := m.blockCounts(); !sameInts(got, want) {
		t.Errorf("此刻笔数应为 %v, 得到 %v", want, got)
	}
	// 没有进行中的请求时记下全零样本 (可以拿来判空窗口)。
	m.pulse.Active = nil
	m.noteRows(time.Now())
	if len(m.rowHist) != 2 || !sameInts(m.rowHist[1].cnt, make([]int, len(flowBlocks))) {
		t.Errorf("空表样本应全是 0, 得到 %v", m.rowHist)
	}
}

// sampleFlowBytes 把进行中请求的累计字节换算成每秒速率: 第一拍只记基线, 之后每拍只取
// 相比上一拍的增量, 除以采样间隔; 新请求的字节算进它出现的那一拍, 请求结束不出现负数。
func TestSampleFlowBytes(t *testing.T) {
	m := newTUIModel("cfg")
	t0 := time.Unix(1000, 0)
	m.pulse.Active = []flow{{ID: 1, RecvBytes: 5000}, {ID: 2, RecvBytes: 1000}}
	m.sampleFlowBytes(t0)
	if m.flowRate != 0 {
		t.Errorf("第一拍只记基线, 速率应为 0, 得到 %v", m.flowRate)
	}
	// 1s 后: 1 号从 5000 涨到 9000 (增量 4000), 2 号结束, 3 号这期间才开始 (500 字节全算这一拍)。
	m.pulse.Active = []flow{{ID: 1, RecvBytes: 9000}, {ID: 3, RecvBytes: 500}}
	m.sampleFlowBytes(t0.Add(time.Second))
	if m.flowRate != 4500 {
		t.Errorf("速率应为 4500 B/s, 得到 %v", m.flowRate)
	}
	// 2s 后全部结束: 增量 0, 速率回落到 0 (不是负数)。
	m.pulse.Active = nil
	m.sampleFlowBytes(t0.Add(2 * time.Second))
	if m.flowRate != 0 {
		t.Errorf("没有在飞请求时速率应为 0, 得到 %v", m.flowRate)
	}
	// 半秒的采样间隔按实际间隔折算: 1000 B / 0.5s = 2000 B/s。
	m.pulse.Active = []flow{{ID: 4, RecvBytes: 100}}
	m.sampleFlowBytes(t0.Add(3 * time.Second))
	m.pulse.Active = []flow{{ID: 4, RecvBytes: 1100}}
	m.sampleFlowBytes(t0.Add(3500 * time.Millisecond))
	if m.flowRate != 2000 {
		t.Errorf("速率应按实际间隔折算成 2000 B/s, 得到 %v", m.flowRate)
	}
}

// gateReason 把日志里的 gate 值换成中文短语, 未知值原样返回。
func TestGateReason(t *testing.T) {
	cases := map[string]string{
		"locked":          "候选都在锁里",
		"sem_timeout":     "等并发槽位超时",
		"queue_timeout":   "排队超时",
		"backoff_timeout": "等退避结束超时",
		"canceled":        "客户端断开",
		"other":           "other",
		"":                "",
	}
	for in, want := range cases {
		if got := gateReason(in); got != want {
			t.Errorf("gate %q 应写作 %q, 得到 %q", in, want, got)
		}
	}
}

// mergeEvents 把服务侧事件 (RFC3339Nano 时间) 并进 TUI 的事件流。
func TestMergeEvents(t *testing.T) {
	m := newTUIModel("cfg")
	ts := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	m.mergeEvents([]viewEvent{
		{Seq: 1, Ts: ts.Format(time.RFC3339Nano), Kind: "lock", Upstream: "ustc", Msg: "锁 quota"},
		{Seq: 2, Ts: "", Kind: "ok", Logical: "sol", Msg: "恢复"}, // 时间坏了也不能丢事件
	})
	if len(m.events) != 2 {
		t.Fatalf("应并入 2 条事件, 得到 %d", len(m.events))
	}
	if e := m.events[0]; e.Kind != "lock" || e.Upstream != "ustc" || e.Msg != "锁 quota" ||
		!e.Ts.Equal(ts) {
		t.Errorf("第 1 条事件不对: %+v", e)
	}
	if e := m.events[1]; e.Kind != "ok" || !e.Ts.IsZero() {
		t.Errorf("时间解析失败时应留零值: %+v", e)
	}
}

// pollState 同进程直接读内存: 载荷 / 事件 / 采样都要落到 TUI 模型上。
func TestPollStateSameProcess(t *testing.T) {
	a, err := newApp(&Config{Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl")})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()
	id := a.flowStart("m1", "agent", "/v1/chat/completions", true)
	defer a.flowEnd(id)
	a.evAdd("lock", "m1", "up1", "", "锁 quota")

	m := newTUIModel("cfg")
	since := m.pollState(a, 0)
	if !m.live || m.lastError() != "" {
		t.Fatalf("同进程取数应成功: live=%v err=%q", m.live, m.lastError())
	}
	if m.pulse.Ver != viewVer || len(m.pulse.Active) != 1 || m.pulse.Active[0].ID != id {
		t.Errorf("载荷不对: ver=%q active=%+v", m.pulse.Ver, m.pulse.Active)
	}
	if since != 1 || len(m.events) != 1 || m.events[0].Kind != "lock" {
		t.Errorf("事件应并入且游标为 1: since=%d events=%+v", since, m.events)
	}
	if len(m.rowHist) != 1 || m.rowHist[0].cnt[0] != 1 {
		t.Errorf("取数后应记一笔采样, 得到 %+v", m.rowHist)
	}
	// 游标传回去: 没有新事件时 events 为空。
	if n := m.pollState(a, since); n != since || len(m.pulse.Events) != 0 {
		t.Errorf("没有新事件时游标与事件都不该变: since=%d events=%+v", n, m.pulse.Events)
	}
}

// pollState 另进程走 GET /state?since=: 正常 / 坏载荷 / 老对端三种情况。
func TestPollStateHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "7" {
			t.Errorf("since 应原样带过去, 得到 %q", got)
		}
		p := pulse{Ver: viewVer, Listen: "127.0.0.1:8787", MaxSeq: 9,
			Events: []viewEvent{{Seq: 8, Ts: time.Now().Format(time.RFC3339Nano), Kind: "ok"}}}
		json.NewEncoder(w).Encode(p)
	}))
	defer srv.Close()

	m := newTUIModel("cfg")
	m.stateURL = srv.URL
	if got := m.pollState(nil, 7); got != 9 {
		t.Errorf("游标应取载荷里的 max_seq (9), 得到 %d", got)
	}
	if !m.live || m.pulse.Listen != "127.0.0.1:8787" || len(m.events) != 1 {
		t.Errorf("HTTP 取数不对: live=%v pulse=%+v events=%+v", m.live, m.pulse, m.events)
	}

	// 坏载荷: 不判为在线, 记下原因。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer bad.Close()
	m2 := newTUIModel("cfg")
	m2.stateURL = bad.URL
	if got := m2.pollState(nil, 3); got != 3 {
		t.Errorf("取数失败时游标不该前进, 得到 %d", got)
	}
	if m2.live || !strings.Contains(m2.lastError(), "观测面返回异常") {
		t.Errorf("坏载荷应记下原因: live=%v err=%q", m2.live, m2.lastError())
	}

	// 老对端 (版本不符): 清空载荷, 提示换二进制。
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(pulse{Ver: "tollgate-state/0"})
	}))
	defer old.Close()
	m3 := newTUIModel("cfg")
	m3.stateURL = old.URL
	if got := m3.pollState(nil, 4); got != 4 {
		t.Errorf("老对端时游标不该前进, 得到 %d", got)
	}
	if m3.live || len(m3.pulse.Active) != 0 || !strings.Contains(m3.lastError(), "观测面接口") {
		t.Errorf("老对端应清空载荷并提示: live=%v err=%q", m3.live, m3.lastError())
	}
}
