// observe_stage_test.go: 事件的阶段战报与结局 —— 请求编号可回溯, 正文按短词表
// (from(参数) -> to(参数), new/switch/retry -> sem, 结局 fail/reject/ok)。
package main

import (
	"testing"
	"time"
)

// stageMsgs 取全部事件正文, 便于按顺序断言。
func stageMsgs(a *App) []viewEvent {
	evs, _ := a.eventsSince(0)
	return evs
}

// 一笔请求从收到到接收结束: 每一跳都有一条战报, 收尾是 ok, 编号与逻辑名一致。
func TestFlowStageNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0) // 收到请求, 首次进 sem
	o.stage(stageQueue)
	o.stage(stageModelWait)
	o.stage(stageSend)
	o.stage(stageRecv)
	o.finish(200)

	want := []string{
		"new(u1/real-1) -> sem(u1)",
		"sem(u1) -> freq(u1)",
		"freq(u1) -> queue(u1/real-1)",
		"queue(u1/real-1) -> send",
		"send -> recv",
		"recv -> ok(200)",
	}
	evs := stageMsgs(a)
	if len(evs) != len(want) {
		t.Fatalf("应有 %d 条事件, 得到 %d: %+v", len(want), len(evs), evs)
	}
	for i, w := range want {
		e := evs[i]
		if e.Msg != w {
			t.Errorf("第 %d 条应为 %q, 得到 %q", i+1, w, e.Msg)
		}
		if e.Req != id || e.Logical != "gpt-5" {
			t.Errorf("第 %d 条应挂在请求 %d/gpt-5 上, 得到 %+v", i+1, id, e)
		}
	}
	if kind := evs[len(evs)-1].Kind; kind != "ok" {
		t.Errorf("收尾应为 ok, 得到 %q", kind)
	}
}

// 换候选: 前一个候选的失败由 outcome 单独发, 下一候选进 sem 写 switch。
func TestFlowStageSwitchNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.stage(stageQueue)
	o.stage(stageModelWait)
	o.stage(stageSend)
	o.stage(stageRecv)
	o.mark(503) // 该候选失败: 展示回退到已发送, 不单独成事件
	o.outcome(stageSend, "fail", "503")
	o.up, o.model = "u2", "real-2"
	o.attempt++
	o.mark(0) // 进入下一个候选

	evs := stageMsgs(a)
	if len(evs) != 7 {
		t.Fatalf("应共 7 条 (切换 5 条 + 失败 1 条 + switch 1 条), 得到 %d: %+v", len(evs), evs)
	}
	if got, want := evs[len(evs)-1].Msg, "switch(u2/real-2) -> sem(u2)"; got != want {
		t.Errorf("换候选文案应为 %q, 得到 %q", want, got)
	}
	if got, want := evs[len(evs)-2].Msg, "send -> fail(503)"; got != want {
		t.Errorf("失败结局应为 %q, 得到 %q", want, got)
	}
}

// 同候选重试: 失败结局后回到 sem 写 retry。
func TestFlowStageRetryNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.mark(-1) // 传输失败: 展示落到已失败块, 不单独成事件
	o.outcome(stageSend, "fail", "connection")
	o.stage(stageSem) // 重试同一候选: acquire 重新报 sem

	evs := stageMsgs(a)
	if got, want := evs[len(evs)-1].Msg, "retry(u1/real-1) -> sem(u1)"; got != want {
		t.Errorf("重试文案应为 %q, 得到 %q", want, got)
	}
	if got, want := evs[len(evs)-2].Msg, "send -> fail(connection)"; got != want {
		t.Errorf("失败结局应为 %q, 得到 %q", want, got)
	}
}

// 发出前被挡 (锁 / 退避): 结局从 sem 写 reject, 时长按 mm:ss。
func TestFlowOutcomeReject(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.outcome(stageSem, "reject", lockParam("breaker", 5*time.Minute))
	o.outcome(stageSem, "reject", "backoff-timeout")

	evs := stageMsgs(a)
	want := []string{
		"sem -> reject(breaker 05:00)",
		"sem -> reject(backoff-timeout)",
	}
	if len(evs) != 3 {
		t.Fatalf("应有 3 条事件, 得到 %d: %+v", len(evs), evs)
	}
	for i, w := range want {
		if evs[i+1].Kind != "error" || evs[i+1].Msg != w {
			t.Errorf("第 %d 条结局应为 error %q, 得到 %+v", i+1, w, evs[i+1])
		}
	}
}

// 模型找不到: 没有候选可试, 收尾补 parse -> fail(404), 不重复。
func TestFlowFinishModelNotFound(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5"}
	o.outcome(stageParse, "fail", "404")
	o.finish(404)

	evs := stageMsgs(a)
	if len(evs) != 1 {
		t.Fatalf("没有候选时应有 1 条收尾, 得到 %d: %+v", len(evs), evs)
	}
	if got, want := evs[0].Msg, "parse -> fail(404)"; got != want {
		t.Errorf("收尾应为 %q, 得到 %q", want, got)
	}
	if evs[0].Kind != "error" {
		t.Errorf("收尾应为 error, 得到 %q", evs[0].Kind)
	}
}

// 失败结局已发出后, finish 不再补 ok / error; 成功才发 ok。
func TestFlowFinishAfterOutcome(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.outcome(stageSend, "fail", "502")
	o.finish(502)
	evs := stageMsgs(a)
	if len(evs) != 2 {
		t.Fatalf("失败结局已发时 finish 不应再补事件, 得到 %d: %+v", len(evs), evs)
	}
}

// 低性能锁是状态变化: 无论吞吐还是高延迟分支都要记一条, 倒计时按 mm:ss。
func TestLowPerfLockEventText(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	c := newCand("u1")
	a.addLockLowPerf(c, "real-1", 2, 1, 0, time.Minute, "")
	a.addLockLowPerf(c, "real-1", 0, 0, 45000, time.Minute, "")
	evs := stageMsgs(a)
	if len(evs) != 2 {
		t.Fatalf("应记 2 条低性能锁事件, 得到 %d: %+v", len(evs), evs)
	}
	want := []string{
		"吞吐 峰 2.0 / 均 1.0 t/s, 触发 低性能锁, 01:00",
		"平均首字节 45s, 触发 低性能锁, 01:00",
	}
	for i, w := range want {
		if evs[i].Kind != "lock" || evs[i].Msg != w {
			t.Errorf("第 %d 条应为 lock %q, 得到 %+v", i+1, w, evs[i])
		}
		if evs[i].Upstream != "u1" || evs[i].Model != "real-1" {
			t.Errorf("第 %d 条应挂 u1/real-1, 得到 %+v", i+1, evs[i])
		}
	}
}
