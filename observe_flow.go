// observe_flow.go: 请求流的登记与阶段推进。

package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

func (a *App) evAdd(kind, logical, upstream, model, msg string) {
	e := viewEvent{
		Ts: time.Now().Format(time.RFC3339Nano), Kind: kind, Logical: logical,
		Upstream: upstream, Model: model, Msg: msg,
	}
	a.evMu.Lock()
	a.evNext++
	e.Seq = a.evNext
	a.events[int(e.Seq)%viewEvents] = e
	a.evMu.Unlock()
}

// eventsSince 取序号 > since 的事件 (最多整环), 按序号升序。
func (a *App) eventsSince(since uint64) ([]viewEvent, uint64) {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	out := make([]viewEvent, 0, 16)
	for _, e := range a.events {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, a.evNext
}

func (a *App) flowStart(logical, source, path string, stream bool) uint64 {
	now := time.Now()
	a.flowMu.Lock()
	a.nextFlow++
	id := a.nextFlow
	if len(a.flows) >= viewActives { // 兜底: 异常堆积时丢最老的, 不让它无界长
		oldest, ok := uint64(0), false
		for k := range a.flows {
			if !ok || k < oldest {
				oldest, ok = k, true
			}
		}
		delete(a.flows, oldest) // 兜底丢弃: 挤掉最老的一笔
	}
	a.flows[id] = &flow{ID: id, Logical: logical, Source: source, Path: path,
		Stream: stream, StartTime: now, Stage: stageParse, MaxStage: stageParse, stageAt: now}
	a.flowMu.Unlock()
	return id
}

// flowStage 更新一笔进行中请求的展示状态; up/mod 为空表示不改这两列。
// gate 由调用方先算好 (读 cand 的锁/节拍), 这里只写展示字段, 不碰调度。
func (a *App) flowStage(id uint64, up, mod string, stage int, gate string, attempt int) {
	a.flowMu.Lock()
	if f := a.flows[id]; f != nil {
		now := time.Now()
		if !f.stageAt.IsZero() && f.Stage >= 0 && f.Stage < stageCount {
			f.stageDur[f.Stage] += now.Sub(f.stageAt).Nanoseconds()
		}
		f.Stage, f.stageAt = stage, now
		// 已失败不是流程上的阶段 (候选链走完的结局), 不参与"到过哪一段"的记账;
		// 回退到更早的阶段 (换候选) 也不该让已到过的段数缩回去。
		if stage >= 0 && stage < stageCount && stage != stageFailed && stage > f.MaxStage {
			f.MaxStage = stage
		}
		if up != "" {
			f.Up = up
		}
		if mod != "" {
			f.Model = mod
		}
		f.Gate, f.Attempt = gate, attempt
	}
	a.flowMu.Unlock()
}

// flowLive 刷新「接收中」这一笔已收到的输出 token 数与首字节时刻 (只写展示字段,
// 不碰阶段耗时)。由 relayStream 节流回写 (约 200ms 一次); 第 1 页用这两个值
// 算这笔自己的吞吐 (从首字节起算)。
func (a *App) flowLive(id uint64, tokens int, firstAt time.Time) {
	a.flowMu.Lock()
	if f := a.flows[id]; f != nil {
		f.RecvTok, f.RecvStart = tokens, firstAt.UnixMilli()
	}
	a.flowMu.Unlock()
}

// flowObs 把一笔请求的展示状态与事件喂给观测面 (TUI)。只读地看调度器, 不改调度:
// 它只读 cand 的锁/队列快照, 不做任何等待或记账。
type flowObs struct {
	a       *App
	id      uint64
	logical string

	up      string // 当前候选 (换候选时记一条 fallback 事件)
	model   string // 当前候选的真实模型 id (换候选时一起更新)
	group   string // 当前候选的额度组名 (查询组锁用; 换候选时一起更新)
	attempt int
	slowMs  int64 // 首字节超过它就另记一条 slow (0 = 不记)
	done    bool
}

// mark 刷新展示状态。status: 0 = 还没结论, -1 = 传输层失败, 其余为上游状态码。
// 换到新候选时记一条 fallback 事件 (这条候选链的走向就是 TUI 要看的路由过程)。
func (o *flowObs) mark(status int, msg string) {
	if status != 0 {
		o.a.evAdd("fallback", o.logical, o.up, o.model, firstLine(msg, status))
	}
	if status < 0 {
		o.a.flowStage(o.id, o.up, o.model, stageFailed, "", o.attempt)
		return
	}
	st := stageSend
	if status == 0 {
		st = stageSem // 刚进入这个候选: 下一步就是取信号量
	}
	o.a.flowStage(o.id, o.up, o.model, st, o.gateNow(), o.attempt)
}

// stage 把展示阶段推到调度器当前的步骤; acquire 的回调与发包前后调用它。
func (o *flowObs) stage(st int) {
	o.a.flowStage(o.id, "", "", st, o.gateNow(), o.attempt)
}

// gateNow 是「这一笔现在卡在哪一步」: 模型锁 / 组额度锁 / 退避 / 出队节拍 / 已发出。
func (o *flowObs) gateNow() string {
	c := o.a.candOf(o.up)
	if c == nil {
		return "已发出"
	}
	now := time.Now()
	if locked, left, reason := c.locked(o.model, o.group); locked {
		why := "额度"
		if reason == "breaker" {
			why = "熔断"
		}
		return why + "锁 " + durStr(left)
	}
	c.mu.Lock()
	paused, next := c.pauseOf(o.model), c.nextSend
	c.mu.Unlock()
	if paused.After(now) {
		return "退避 " + durStr(paused.Sub(now))
	}
	if next.After(now) {
		return "等节拍 " + durStr(next.Sub(now))
	}
	return "已发出"
}

// firstLine 拼事件正文 (状态码优先, 没有就截断错误串)。
func firstLine(msg string, status int) string {
	if status > 0 {
		return "状态码 " + strconv.Itoa(status)
	}
	if msg == "" {
		return "连接失败"
	}
	return bodySnippet([]byte(msg), 120)
}

func (a *App) candOf(id string) *cand {
	a.mu.RLock()
	c := a.cands[id]
	a.mu.RUnlock()
	return c
}

// finish 是这笔请求的结局: 进事件流 (2xx 记 ok, 其余记 error; 慢请求另记一条)。
func (o *flowObs) finish(status int, ttfbMs int64) {
	if o.done {
		return
	}
	o.done = true
	if status == 0 {
		status = 200 // 正常收尾 (含 4xx 透传之外的路径) 都算成功
	}
	if status >= 400 {
		o.a.evAdd("error", o.logical, o.up, "", "状态码 "+strconv.Itoa(status))
		return
	}
	o.a.evAdd("ok", o.logical, o.up, "", "状态码 "+strconv.Itoa(status))
	if o.slowMs > 0 && ttfbMs >= o.slowMs {
		o.a.evAdd("slow", o.logical, o.up, "", "首字节 "+durStr(time.Duration(ttfbMs)*time.Millisecond))
	}
}

// durStr 把时长写成屏幕上好读的形态 (时间段, 不是时刻): 前导的零段省掉 ——
// 有小时写 hh:mm:ss, 只有分秒写 mm:ss, 只有秒写 12s / 0.8s (带单位, 不然看不出是秒)。
func durStr(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	case m > 0:
		return fmt.Sprintf("%02d:%02d", m, s)
	}
	return strconv.Itoa(s) + "s"
}

func (a *App) flowEnd(id uint64) {
	a.flowMu.Lock()
	delete(a.flows, id)
	a.flowMu.Unlock()
}
