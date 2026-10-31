package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// 阶段并发: 片内取峰值, 片间按年龄指数衰减加权平均。
func TestStageConc(t *testing.T) {
	a, err := newApp(&Config{Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl")})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()
	var ids []uint64
	for i := 0; i < 3; i++ {
		ids = append(ids, a.flowStart("m1", "agent", "/v1/chat/completions", true))
	}
	if got := a.snapshot(0).Conc[stageParse]; got != 3 {
		t.Errorf("解析阶段并发应为 3, 得到 %v", got)
	}
	for _, id := range ids {
		a.flowStage(id, "up1", "real-m1", stageRecv, "", 1)
	}
	if got := a.snapshot(0).Conc[stageRecv]; got != 3 {
		t.Errorf("接收中并发应为 3, 得到 %v", got)
	}
	// 片内峰值: 请求结束后本片峰值仍记 3, 要到下一片才回落。
	for _, id := range ids {
		a.flowEnd(id)
	}
	if got := a.snapshot(0).Conc[stageRecv]; got != 3 {
		t.Errorf("同一片内应保留峰值 3, 得到 %v", got)
	}
}

// 衰减权重: 片中心为现在时权重 1, 再早一个衰减常数时为 e^-1。
func TestConcWeight(t *testing.T) {
	now := time.Now()
	if w := concWeight(now.Add(-concSlice/2), now); w != 1 {
		t.Errorf("当前片权重应为 1, 得到 %v", w)
	}
	at := now.Add(-concSlice/2 - concDecay)
	if w := concWeight(at, now); math.Abs(w-math.Exp(-1)) > 1e-9 {
		t.Errorf("10min 前的片权重应为 e^-1, 得到 %v", w)
	}
}

// 加权平均: 当前片峰值 4、上一片峰值 1 时, 结果落在两者之间并偏向当前片。
func TestConcWeightedAvg(t *testing.T) {
	now := time.Now()
	a := &App{}
	a.chunkAt = now // 当前片中心在未来, 年龄夹到 0, 权重 1
	a.chunkPeak[stageRecv] = 4
	a.chunkHist[stageRecv] = []concChunk{{at: now.Add(-concSlice), peak: 1}}
	got := a.concAvgsLocked(now)[stageRecv]
	w := math.Exp(-0.5) // 上一片中心在 5min 前
	want := (4 + 1*w) / (1 + w)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("加权平均应为 %.4f, 得到 %.4f", want, got)
	}
}

// 第 1 页块高度同时用「窗口内最大并发」与「阶段停留时长」: 并发先取平方根再除以各块
// 最大值 (压住并发池), 与停留占比按 0.6 / 0.4 加权, 每个块保底 1 行。
func TestFlowMinRowsConc(t *testing.T) {
	m := newTUIModel("cfg")
	m.pulse.Conc = make([]float64, stageCount)
	m.pulse.Conc[stageSend] = 1.2
	m.pulse.Conc[stageRecv] = 2.4
	rows := m.flowMinRows()
	if len(rows) != len(flowBlocks) {
		t.Fatalf("行数应与分块数一致, 得到 %d", len(rows))
	}
	// 没有停留时长时: 并发占比 0.6 权重, 空块保底 1 行。
	want := map[int]int{0: 1, 1: 1, 2: 1, 3: 3, 4: 4, 5: 1}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("第 %d 块应为 %d 行, 得到 %d (%v)", i, w, rows[i], rows)
		}
	}
	// 停留时长参与加权: 已发送占 80%, 接收中占 20%。
	m.pulse.Dwell = make([]int64, stageCount)
	m.pulse.Dwell[stageSend] = 400
	m.pulse.Dwell[stageRecv] = 100
	rows = m.flowMinRows()
	if rows[3] != 5 || rows[4] != 4 {
		t.Errorf("停留时长应把已发送抬到 5 行, 接收中 4 行, 得到 %v", rows)
	}
	// 并发一家独大时也不顶满: 并发池按平方根压缩后落 4 行。
	m.pulse.Conc = make([]float64, stageCount)
	m.pulse.Conc[stageSem] = 20
	if got := m.flowMinRows()[0]; got != 4 {
		t.Errorf("并发池应被压到 4 行, 得到 %d", got)
	}
	// 旧对端没有 Conc 时退回缺省档。
	m.pulse.Conc = nil
	if got := m.flowMinRows(); len(got) != len(defaultMinRows) {
		t.Errorf("没有 Conc 时应退回缺省档, 得到 %v", got)
	}
}

// 行数 = max(此刻笔数, 参考行数, 1); 参考行数 = 各样本占比按高斯权重平均后乘以同时在飞
// 笔数峰值。取比例的平均而不是累计耗时的比例: 某一刻挂着很多笔不会独占统计; 远的样本按
// 高斯衰减 (平均耗时处的权重 0.8), 没有硬截断。
func TestFlowMinRowsWindow(t *testing.T) {
	now := time.Now()
	k := len(flowBlocks)
	// sample 造一个样本: cnt 是各块的在飞笔数。
	sample := func(at time.Time, cnt map[int]int) rowSample {
		s := rowSample{at: at, cnt: make([]int, k)}
		for i, n := range cnt {
			s.cnt[i] = n
		}
		return s
	}
	m := newTUIModel("cfg")
	// 日志里还没有请求时 t0 取缺省 35s: 1min 前的样本权重 0.8^((60/35)²)≈0.52, 刚发生的为 1,
	// 峰值为 8, 于是已发送 3 行, 接收中 5 行。
	m.rowHist = []rowSample{
		sample(now.Add(-2*time.Minute), map[int]int{3: 1}),
		sample(now.Add(-time.Minute), map[int]int{4: 8}),
	}
	want := []int{1, 1, 1, 3, 5, 1}
	if got := m.flowMinRows(); !sameInts(got, want) {
		t.Errorf("参考行数应为 %v, 得到 %v", want, got)
	}
	// 此刻笔数超过参考值时按实际留 (10 笔接收中远多于参考行数)。
	for i := 0; i < 10; i++ {
		m.pulse.Active = append(m.pulse.Active, flow{ID: uint64(i + 1), Stage: stageRecv, StartTime: now})
	}
	if got := m.flowMinRows()[4]; got != 10 {
		t.Errorf("接收中块应按 10 笔留 10 行, 得到 %d", got)
	}
	// 高斯权重按日志里的平均耗时缩放: t0 = 10s 时, 20s 前的 8 笔样本权重只剩 0.8⁴≈0.41,
	// 近的 1 笔接收中几乎独占统计 (参考行数 6 行封顶), 旧样本只把峰值抬到 8。
	m.pulse.Active = nil
	m.durSum, m.durN = 10000, 1 // 平均耗时 10s
	m.rowHist = []rowSample{
		sample(now.Add(-80*time.Second), map[int]int{3: 8}),
		sample(now.Add(-time.Minute), map[int]int{4: 1}),
	}
	if got := m.flowMinRows(); got[3] != 2 || got[4] != 6 {
		t.Errorf("远的样本应按高斯衰减 (已发送 2 行, 接收中 6 行), 得到 %v", got)
	}
	// 最近一段全是空样本: 参看时刻仍是最后一个有请求的样本, 不塌回缺省档。
	m.durSum, m.durN = 0, 0
	m.rowHist = []rowSample{sample(now.Add(-30*time.Minute), map[int]int{3: 5})}
	for i := 0; i < 5; i++ {
		m.rowHist = append(m.rowHist, sample(now.Add(-time.Duration(i)*time.Minute), nil))
	}
	if got := m.flowMinRows()[3]; got != 5 {
		t.Errorf("空闲时仍应沿用上一段有请求的样本 (已发送 5 行), 得到 %d", got)
	}
	// 参看时刻: 此刻有进行中的请求就用当前时间, 于是很久以前的样本衰减到不算数,
	// 退回缺省档 (对端也没给 conc / dwell)。
	m.rowHist = []rowSample{sample(now.Add(-30*time.Minute), map[int]int{3: 5})}
	m.pulse.Active = []flow{{ID: 1, Stage: stageRecv, StartTime: now}}
	if got := m.flowMinRows(); !sameInts(got, defaultMinRows) {
		t.Errorf("有进行中的请求时应以当前时间为参看时刻, 得到 %v", got)
	}
	// 一个样本都没有时同样退回缺省档。
	m.pulse.Active = nil
	m.rowHist = nil
	if got := m.flowMinRows(); !sameInts(got, defaultMinRows) {
		t.Errorf("没有样本时应退回缺省档 %v, 得到 %v", defaultMinRows, got)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
