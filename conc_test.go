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

// 第 1 页块高度按并发水平定: 块内阶段相加 (互斥), 四舍五入后夹进 1 至 6 行。
func TestFlowMinRowsConc(t *testing.T) {
	m := newTUIModel("cfg")
	m.pulse.Conc = make([]float64, stageCount)
	m.pulse.Conc[stageSend] = 1.2
	m.pulse.Conc[stageRecv] = 2.4
	rows := m.flowMinRows()
	if len(rows) != len(flowBlocks) {
		t.Fatalf("行数应与分块数一致, 得到 %d", len(rows))
	}
	want := map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 2, 5: 1}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("第 %d 块应为 %d 行, 得到 %d (%v)", i, w, rows[i], rows)
		}
	}
	// 旧对端没有 Conc 时退回缺省档。
	m.pulse.Conc = nil
	if got := m.flowMinRows(); len(got) != len(defaultMinRows) {
		t.Errorf("没有 Conc 时应退回缺省档, 得到 %v", got)
	}
}
