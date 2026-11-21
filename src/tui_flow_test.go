// tui_flow_test.go: 第 1 页阶段链拼装的回归 (拆分 stageChain 前的特征测试)。

package main

import (
	"strings"
	"testing"
	"time"
)

// 阶段链: 到过的阶段按流程顺序用 " → " 连起, 每段带耗时; 当前段实时走 (不足 1s 也写),
// 已结束且不足 1s 的段不写耗时; 链尾按需追加卡住原因与结局词。
func TestStageChainBuild(t *testing.T) {
	m := newTUIModel("cfg")
	f := flow{Logical: "lg", Gate: "quota 05:00", Stage: stageRecv}
	tr := flowTry{Up: "u1", Model: "mm", Cur: stageRecv, Segs: make([]float64, stageCount)}
	tr.Segs[stageParse] = 2
	tr.Segs[stageSem] = 3
	tr.Segs[stageQueue] = 5
	tr.Segs[stageModelWait] = 10
	tr.Segs[stageSend] = 0.5 // 已结束且不足 1s: 不写耗时
	tr.Segs[stageRecv] = 4
	got := stripANSI(m.stageChain(f, tr))
	// 已取过信号量: 解析段由 sem 代表, new 不再单独出现。
	want := "sem(u1) 3s → freq 5s → queue(mm) 10s → send → recv 4s quota 05:00"
	if got != want {
		t.Fatalf("阶段链应为 %q, 得到 %q", want, got)
	}

	// 耗时仍用前景色, 青色只给接收中的实时性能。
	raw := m.stageChain(f, tr)
	if strings.Contains(raw, tuiLive) {
		t.Errorf("耗时不该带进行中色: %q", raw)
	}

	// 还没取信号量时解析段单独出现, 不写 sem。
	takeParse := flowTry{Cur: stageParse, Segs: make([]float64, stageCount)}
	takeParse.Segs[stageParse] = 2
	if got := stripANSI(m.stageChain(flow{Logical: "lg"}, takeParse)); got != "new(lg) 2s" {
		t.Errorf("只有解析段时应为 %q, 得到 %q", "new(lg) 2s", got)
	}

	// 结局词与卡住原因都在链尾; fail 红字。
	fail := flow{Logical: "lg"}
	trf := flowTry{Up: "u1", Cur: stageSend, Fail: "fail(502)", Segs: make([]float64, stageCount)}
	trf.Segs[stageSem] = 1
	failRaw := m.stageChain(fail, trf)
	if !strings.Contains(failRaw, tuiRed+"fail(502)"+tuiReset) {
		t.Errorf("失败结局应红字: %q", failRaw)
	}
	if got := stripANSI(failRaw); !strings.HasSuffix(got, "send 0s fail(502)") {
		t.Errorf("链尾应为 send 0s fail(502), 得到 %q", got)
	}
}

// 阶段网格的对齐: 各显示行的第 n 阶段从同一列起 (位宽按位取各显示行的最大内容宽)。
func TestFlowGridAlign(t *testing.T) {
	lines := []flowLine{
		{stages: []string{"sem(u1) 3s", "freq 5s", "send"}},
		{stages: []string{"queue(real1) 0.5s", "recv 4s"}, tail: []string{"quota 05:00"}},
	}
	w := flowPosWidth(lines)
	if w[0] != 19 || w[1] != 9 || w[2] != 6 { // 位 0 = max(10,17)+2, 位 1 = 7+2, 位 2 = 4+2
		t.Fatalf("位宽应按位取最大: %v", w)
	}
	blocks := flowBlocks(w, 80)
	if len(blocks) != 1 || blocks[0] != [2]int{0, 3} {
		t.Fatalf("宽列应一行放下: %v", blocks)
	}
	g0 := stripANSI(flowGrid(lines[0].stages, lines[0].tail, w, blocks, 80)[0])
	g1 := stripANSI(flowGrid(lines[1].stages, lines[1].tail, w, blocks, 80)[0])
	i0 := strings.Index(g0, "2 ")
	i1 := strings.Index(g1, "2 ")
	if i0 < 0 || i1 < 0 {
		t.Fatalf("两行都应显示第 2 阶段: %q / %q", g0, g1)
	}
	if i0 != i1 {
		t.Errorf("第 2 阶段应同一列: 行0 在 %d, 行1 在 %d\n%q\n%q", i0, i1, g0, g1)
	}
	if !strings.Contains(g0, "3 send") {
		t.Errorf("行0 应有第 3 阶段: %q", g0)
	}
	if !strings.HasSuffix(g1, "quota 05:00") {
		t.Errorf("链尾应挂最后: %q", g1)
	}
	// 短行只出到自己的阶段数, 但也从 1 起头、位宽对齐。
	if !strings.HasPrefix(g1, "1 ") {
		t.Errorf("行1 应从 1 起头: %q", g1)
	}
}

// 阶段网格折行: 位排不下时折行, 折出的新显示行重新从 1 计数; 每块总宽不超阶段列宽;
// 链尾挂在最后一行末尾。
func TestFlowGridWrapRenumber(t *testing.T) {
	lines := []flowLine{{
		stages: []string{"new(m1) 2s", "sem(u1) 3s", "freq 5s", "queue(mm) 10s", "send", "recv 4s"},
	}}
	w := flowPosWidth(lines)
	if len(w) != 6 {
		t.Fatalf("应有 6 个阶段位: %v", w)
	}
	colW := 30 // 窄列强制折行
	blocks := flowBlocks(w, colW)
	for _, blk := range blocks {
		width := 0
		for j, p := 0, blk[0]; p < blk[0]+blk[1]; p++ {
			if j > 0 {
				width += flowSepW
			}
			width += w[p]
			j++
		}
		if width > colW {
			t.Errorf("块宽超过列宽: %d > %d (块 %v)", width, colW, blk)
		}
	}
	grid := flowGrid(lines[0].stages, []string{"fail(502)"}, w, blocks, colW)
	if len(grid) < 2 {
		t.Fatalf("窄列应折成多行: %v", grid)
	}
	for i, g := range grid {
		g = stripANSI(g)
		if !strings.HasPrefix(g, "1 ") {
			t.Errorf("第 %d 显示行应从 1 起头: %q", i+1, g)
		}
	}
	if !strings.HasSuffix(stripANSI(grid[len(grid)-1]), "fail(502)") {
		t.Errorf("链尾应挂最后一行: %q", grid[len(grid)-1])
	}
}

// 整页画出来也按位对齐: 两个请求的第 2 阶段 ("→ 2 ") 落在同一列。
func TestFlowGridDrawnAligned(t *testing.T) {
	openTestPTY(t, 40, 140)
	m := newTUIModel("cfg")
	m.tab = tabActive
	now := time.Now()
	m.pulse.Active = []flow{
		{ID: 2, Logical: "m2", Up: "ustc", Model: "deepseek", Stage: stageQueue, StartTime: now,
			Tries: []flowTry{{Up: "ustc", Model: "d1", Cur: stageQueue,
				Segs: []float64{2, 0.3, 0.2, 0, 0, 0}}}},
		{ID: 1, Logical: "m1", Up: "ustc", Model: "deepseek", Stage: stageRecv, StartTime: now,
			Tries: []flowTry{{Up: "ustc", Model: "d2", Cur: stageRecv,
				Segs: []float64{2, 3, 5, 10, 0.5, 4}}}},
	}
	out := stripANSI(m.draw())
	if !strings.Contains(out, "1 sem(") {
		t.Fatal("阶段列应有序数 1 前置")
	}
	var col2 []int
	for _, ln := range strings.Split(out, "\r\n") {
		if i := strings.Index(ln, "→ 2 "); i >= 0 {
			col2 = append(col2, i+2) // "2" 的列
		}
	}
	if len(col2) < 2 {
		t.Fatalf("至少两行应显示第 2 阶段: %v", col2)
	}
	for _, c := range col2[1:] {
		if c != col2[0] {
			t.Errorf("第 2 阶段列应一致: %v", col2)
		}
	}
}

// 链尾只留「为什么卡住」的四个短词; interval (排队必然在等) 与 sent (发送阶段已说明)
// 不进链尾。
func TestGateDetailShortWords(t *testing.T) {
	cases := []struct {
		gate string
		want bool
	}{
		{"quota 05:00", true},
		{"breaker 05:00", true},
		{"lowperf 05:00", true},
		{"backoff 8s", true},
		{"interval 1s", false},
		{"sent", false},
	}
	for _, c := range cases {
		if got := isGateDetail(c.gate); got != c.want {
			t.Errorf("isGateDetail(%q) 应为 %v, 得到 %v", c.gate, c.want, got)
		}
	}
}
