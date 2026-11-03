// observe_stage_test.go: 第 2 页的阶段战报 —— 旧阶段结果 + 新阶段操作, 请求编号可回溯。
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

// 一笔请求从收到到接收结束: 每一跳都有一条战报, 编号与逻辑名一致。
func TestFlowStageNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0) // 收到请求
	o.stage(stageQueue)
	o.stage(stageModelWait)
	o.stage(stageSend)
	o.stage(stageRecv)
	o.finish(200)

	want := []string{
		"收到请求, 等待信号量",
		"获得信号量, 加入频率控制队列",
		"出频率控制队列, 加入 u1/real-1 模型队列",
		"出 u1/real-1 模型队列, 发送请求",
		"收到响应头, 开始接收",
		"接收结束, 状态码 200",
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

// 收尾: 失败原因没被切换带出来时补进事件; 状态码与原因重复时只写一次。
func TestFlowFinishNote(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.failNote("命中熔断锁 05:00")
	o.finish(429)
	evs := stageMsgs(a)
	if got, want := evs[len(evs)-1].Msg, "候选链结束, 状态码 429 (命中熔断锁 05:00)"; got != want {
		t.Errorf("收尾应带上失败原因 %q, 得到 %q", want, got)
	}

	a2 := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id2 := a2.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o2 := &flowObs{a: a2, id: id2, logical: "gpt-5", up: "u1", model: "real-1"}
	o2.mark(0)
	o2.mark(503)
	o2.finish(503)
	evs2 := stageMsgs(a2)
	if got, want := evs2[len(evs2)-1].Msg, "候选链结束, 状态码 503"; got != want {
		t.Errorf("原因与状态码重复时应收敛为 %q, 得到 %q", want, got)
	}
}

// 候选失败只记进 note, 由下一次阶段切换带出, 不单独发事件。
func TestFlowStageSwitchNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.stage(stageQueue)
	o.stage(stageModelWait)
	o.stage(stageSend)
	o.stage(stageRecv)
	o.mark(503) // 该候选失败: 静默回退, note = 收到 503
	o.up, o.model = "u2", "real-2"
	o.attempt++
	o.mark(0) // 进入下一个候选

	evs := stageMsgs(a)
	if len(evs) != 6 {
		t.Fatalf("失败本身不该单独成事件, 应共 6 条, 得到 %d: %+v", len(evs), evs)
	}
	if got, want := evs[5].Msg, "收到 503, 换到 u2/real-2, 等待信号量"; got != want {
		t.Errorf("换候选文案应为 %q, 得到 %q", want, got)
	}
}

// 同一个候选退避后重试: 失败原因同样带出, 但写 重新等待信号量。
func TestFlowStageRetryNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.stage(stageSend)
	o.stage(stageRecv)
	o.mark(503)       // 失败: note = 收到 503, 阶段静默回退到 send
	o.stage(stageSem) // 重试同一候选: acquire 重新报 sem

	evs := stageMsgs(a)
	if got, want := evs[len(evs)-1].Msg, "收到 503, 重新等待信号量"; got != want {
		t.Errorf("重试文案应为 %q, 得到 %q", want, got)
	}
}

// 候选预检被锁跳过: note 带出锁原因与剩余时间, 再写换到哪个候选。
func TestFlowStageLockSkipNarrative(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	id := a.flowStart("gpt-5", "", "/v1/chat/completions", true)
	o := &flowObs{a: a, id: id, logical: "gpt-5", up: "u1", model: "real-1"}
	o.mark(0)
	o.failNote(lockNoteText("breaker", 5*time.Minute))
	o.up, o.model = "u2", "real-2"
	o.attempt++
	o.mark(0)

	evs := stageMsgs(a)
	if got, want := evs[len(evs)-1].Msg, "命中熔断锁 05:00, 换到 u2/real-2, 等待信号量"; got != want {
		t.Errorf("锁跳过文案应为 %q, 得到 %q", want, got)
	}
}

// 低性能锁是状态变化: 无论吞吐还是高延迟分支都要记一条, 倒计时按 mm:ss。
func TestLowPerfLockEventText(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	c := newCand("u1")
	a.addLockLowPerf(c, "real-1", 2, 1, 0, time.Minute)
	a.addLockLowPerf(c, "real-1", 0, 0, 45000, time.Minute)
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
