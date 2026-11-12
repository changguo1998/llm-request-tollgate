// tui_weight_test.go: 逻辑名配色权重 (请求结束时刻的 10min 高斯衰减求和)。

package main

import (
	"testing"
	"time"
)

// 权重按请求求和, 不取平均: 三笔 10min 前的请求 (3e^-1 ≈ 1.10) 压过一笔刚结束的 (1.00)。
func TestWeightOrderSumNotAverage(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	m.nameEnds = map[string][]time.Time{
		"many": {old, old, old},
		"one":  {now},
	}
	got := m.weightOrder([]string{"one", "many"}, now)
	if !eqSlice(got, []string{"many", "one"}) {
		t.Errorf("三笔旧请求的权重和应压过一笔新请求, 得到 %v", got)
	}
}

// 越近的请求权重越大; 没有请求的名字排在有请求的后面 (同为 0 时按字典序)。
func TestWeightOrderByRecency(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	m.nameEnds = map[string][]time.Time{
		"fresh": {now.Add(-time.Minute)},
		"mid":   {now.Add(-30 * time.Minute)},
	}
	got := m.weightOrder([]string{"idle-b", "mid", "idle-a", "fresh"}, now)
	want := []string{"fresh", "mid", "idle-a", "idle-b"}
	if !eqSlice(got, want) {
		t.Errorf("权重降序应为 %v, 得到 %v", want, got)
	}
}

// 只算回看窗口内的请求: 窗口外的结束时刻在排位时清掉, 内存不随运行时长涨。
func TestWeightOrderPrunesOldEnds(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	m.nameEnds = map[string][]time.Time{
		"a": {now.Add(-time.Minute), now.Add(-m.window - time.Hour)},
	}
	m.weightOrder([]string{"a"}, now)
	if got := len(m.nameEnds["a"]); got != 1 {
		t.Errorf("窗口外的结束时刻应清掉, 还剩 %d 条", got)
	}
}

// 分色跟着权重走, 不再按字典序: 请求多的逻辑名先拿标准色。
func TestLogicalColorsFollowWeights(t *testing.T) {
	m := newTUIModel(stickyFixture(t))
	now := time.Now()
	m.nameEnds = map[string][]time.Time{
		"m2": {now, now},
	}
	m.draw()
	if m.logicalColors["m2"] != logicalPalette[0] || m.logicalColors["m1"] != logicalPalette[1] {
		t.Errorf("权重高的逻辑名应先拿标准色, 得到 %v", m.logicalColors)
	}
}
