// tui_flow_test.go: 第 1 页阶段链拼装的回归 (拆分 stageChain 前的特征测试)。

package main

import (
	"strings"
	"testing"
)

// 阶段链: 到过的阶段按流程顺序用 " → " 连起, 每段带耗时; 当前段实时走 (不足 1s 也写),
// 已结束且不足 1s 的段不写耗时; 链尾按需追加卡住原因与结局词。
func TestStageChainBuild(t *testing.T) {
	m := newTUIModel("cfg")
	f := flow{Logical: "lg", Gate: "额度锁 05:00", Stage: stageRecv}
	tr := flowTry{Up: "u1", Model: "mm", Cur: stageRecv, Segs: make([]float64, stageCount)}
	tr.Segs[stageParse] = 2
	tr.Segs[stageSem] = 3
	tr.Segs[stageQueue] = 5
	tr.Segs[stageModelWait] = 10
	tr.Segs[stageSend] = 0.5 // 已结束且不足 1s: 不写耗时
	tr.Segs[stageRecv] = 4
	got := stripANSI(m.stageChain(f, tr))
	// 已取过信号量: 解析段由 sem 代表, new 不再单独出现。
	want := "sem(u1) 3s → freq 5s → queue(mm) 10s → send → recv 4s 额度锁 05:00"
	if got != want {
		t.Fatalf("阶段链应为 %q, 得到 %q", want, got)
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
	raw := m.stageChain(fail, trf)
	if !strings.Contains(raw, tuiRed+"fail(502)"+tuiReset) {
		t.Errorf("失败结局应红字: %q", raw)
	}
	if got := stripANSI(raw); !strings.HasSuffix(got, "send 0s fail(502)") {
		t.Errorf("链尾应为 send 0s fail(502), 得到 %q", got)
	}
}
