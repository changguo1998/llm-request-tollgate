package main

import (
	"testing"
	"time"
)

// 统计窗口里还没有样本时退回缺省档。
func TestFlowMinRowsFallback(t *testing.T) {
	m := newTUIModel("cfg")
	if got := m.flowMinRows(); !sameInts(got, defaultMinRows) {
		t.Errorf("没有样本时应退回缺省档 %v, 得到 %v", defaultMinRows, got)
	}
}

// 行数 = max(此刻笔数, 参考行数, 1); 参考行数 = 各样本占比按高斯权重平均后乘以同时在飞
// 笔数峰值。取比例的平均而不是累计耗时的比例: 某一刻挂着很多笔不会独占统计; 远的样本按
// 高斯衰减 (平均耗时处的权重 0.9), 没有硬截断。
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
	// 日志里还没有请求时 t0 取缺省 35s: 1min 前的样本权重 0.9^((60/35)²)≈0.73, 刚发生的为 1,
	// 峰值为 8, 于是已发送 4 行 (向上取整), 接收中 5 行。
	m.rowHist = []rowSample{
		sample(now.Add(-2*time.Minute), map[int]int{3: 1}),
		sample(now.Add(-time.Minute), map[int]int{4: 8}),
	}
	want := []int{1, 1, 1, 4, 5, 1}
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
	// 高斯权重按日志里的平均耗时缩放: t0 = 10s 时, 20s 前的 8 笔样本权重 0.9⁴≈0.66,
	// 近的 1 笔接收中权重 1, 按占比给行数 (已发送 4 行, 接收中 5 行, 向上取整), 旧样本只把峰值抬到 8。
	m.pulse.Active = nil
	m.durSum, m.durN = 10000, 1 // 平均耗时 10s
	m.rowHist = []rowSample{
		sample(now.Add(-80*time.Second), map[int]int{3: 8}),
		sample(now.Add(-time.Minute), map[int]int{4: 1}),
	}
	if got := m.flowMinRows(); got[3] != 4 || got[4] != 5 {
		t.Errorf("远的样本应按高斯衰减 (已发送 4 行, 接收中 5 行), 得到 %v", got)
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
