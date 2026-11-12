// observe_flow.go: 请求流的登记与阶段推进。

package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// stageSep 是阶段链里两个阶段词之间的箭头: 一个字符宽 (比 "->" 省一列), 两侧各留一个空格。
const stageSep = " → "

// evAdd 记一条不挂在某笔请求上的事件 (锁 / 探测 / 解锁 / start / reload)。
func (a *App) evAdd(kind, logical, upstream, model, msg string) {
	a.evAddReq(0, kind, logical, upstream, model, msg)
}

// evAddReq 记一条事件; req = 请求编号, 0 = 不挂请求。两条路共用同一个环形缓冲与序号。
func (a *App) evAddReq(req uint64, kind, logical, upstream, model, msg string) {
	e := viewEvent{
		Ts: time.Now().Format(time.RFC3339Nano), Kind: kind, Req: req, Logical: logical,
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

// monthBefore03 返回 t 之前最近的一个「某月 1 日 03:00 本地」时刻: 月初 03:00 之后
// (含) 基准是当月 1 日 03:00, 之前基准是上月 1 日 03:00。编号的月边界落在凌晨 3 点,
// 不影响月末回绕的唯一性复查 (跨月要靠日志 ts 里的日期与分钟数一起看)。
func monthBefore03(t time.Time) time.Time {
	y, m, _ := t.Date()
	loc := t.Location()
	cur := time.Date(y, m, 1, 3, 0, 0, 0, loc)
	if !t.Before(cur) {
		return cur
	}
	prev := time.Date(y, m, 1, 0, 0, 0, 0, loc).AddDate(0, -1, 0)
	return time.Date(prev.Year(), prev.Month(), 1, 3, 0, 0, 0, loc)
}

// minsSinceBase 是 t 距基准时刻的整分钟数 (向下取整)。
func minsSinceBase(t, base time.Time) int {
	return int(t.Sub(base) / time.Minute)
}

// flowStart 为一笔请求分配跨重启唯一的编号并登记进进行中表。编号 = (分钟数+1)*100 +
// 该分钟内从 0 起的请求序号: 分钟数单调递增, 重启后从更大的分钟数继续, 不回落到旧的小号;
// +1 偏移保证编号最小 100, 与「无编号 = 0」不冲突。每笔请求只会走到这一步一次。
func (a *App) flowStart(logical, source, path string, stream bool) uint64 {
	now := time.Now()
	a.flowMu.Lock()
	mins := minsSinceBase(now, monthBefore03(now))
	if mins != a.idMin {
		a.idMin, a.idSeq = mins, 0
	}
	id := uint64(mins+1)*100 + uint64(a.idSeq)
	a.idSeq++                        // 同一分钟的第 100 笔起序号自然超出 2 位并继续递增, 保持唯一
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
		Stream: stream, StartTime: now, Stage: stageParse, stageAt: now,
		Tries: []flowTry{{Cur: stageParse, Segs: make([]float64, stageCount)}}}
	a.flowMu.Unlock()
	return id
}

// flowStage 更新一笔进行中请求的展示状态; up/mod 为空表示不改这两列。
// gate 由调用方先算好 (读 cand 的锁/节拍), 这里只写展示字段, 不碰调度。
// 返回值 = 是否真的发了一条阶段事件。
func (a *App) flowStage(id uint64, up, mod string, stage int, gate string, attempt int) bool {
	a.flowMu.Lock()
	f := a.flows[id]
	if f == nil {
		a.flowMu.Unlock()
		return false
	}
	now := time.Now()
	oldStage, oldUp, oldMod := f.Stage, f.Up, f.Model
	// 新的一段尝试: 换候选 (attempt 变了) 或从发送之后退回并发池 (同候选重试)。
	// 第一次进 sem 不算新段: 初始的 parse 段就是这笔请求的第一行。
	newTry := (attempt != f.Attempt && f.Attempt > 0) || (stage == stageSem && f.Stage >= stageSend)
	if newTry {
		f.closeTry(now)
		f.Tries = append(f.Tries, flowTry{Segs: make([]float64, stageCount), Cur: stage})
	}
	if tr := f.curTry(); tr != nil {
		// 离开当前阶段时把这一段耗时结算进本次尝试 (TUI 用 stageAt 现算还没结算的这段)。
		if !newTry && tr.Cur >= 0 && tr.Cur < stageCount && !f.stageAt.IsZero() {
			tr.Segs[tr.Cur] += now.Sub(f.stageAt).Seconds()
		}
		if up != "" {
			tr.Up = up
		}
		if mod != "" {
			tr.Model = mod
		}
		tr.Cur = stage
	}
	f.Stage, f.stageAt = stage, now
	if up != "" {
		f.Up = up
	}
	if mod != "" {
		f.Model = mod
	}
	f.Gate, f.Attempt = gate, attempt
	text, said := stageMoveText(oldStage, oldUp, oldMod, stage, f.Up, f.Model, f.Logical)
	logical := f.Logical
	a.flowMu.Unlock()
	if said {
		a.evAddReq(id, "stage", logical, "", "", text)
	}
	return said
}

// closeTry 结掉当前这一段尝试: 把还没结算的当前阶段耗时记进去, Cur = -1。
func (f *flow) closeTry(now time.Time) {
	tr := f.curTry()
	if tr == nil {
		return
	}
	if tr.Cur >= 0 && tr.Cur < stageCount && !f.stageAt.IsZero() {
		tr.Segs[tr.Cur] += now.Sub(f.stageAt).Seconds()
	}
	tr.Cur = -1
}

// curTry 是当前正在走的那一段尝试 (nil = 还没有尝试)。
func (f *flow) curTry() *flowTry {
	if len(f.Tries) == 0 {
		return nil
	}
	return &f.Tries[len(f.Tries)-1]
}

// flowFail 记当前尝试的结局词: 第 1 页把它写在停在的那一格末尾 (fail(502) / reject(…))。
func (a *App) flowFail(id uint64, from int, word, param string) {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()
	f := a.flows[id]
	if f == nil {
		return
	}
	tr := f.curTry()
	if tr == nil || tr.Fail != "" {
		return
	}
	f.closeTry(time.Now())
	tr.Fail = word
	if param != "" {
		tr.Fail += "(" + param + ")"
	}
	tr.Stop = from
}

// stageMoveText 写一条阶段切换事件: 「from(参数) → to(参数)」。
// 参数 = 该阶段归属的实体 (provider 级写供应商, 候选级写完整候选)。
// 进 sem 有三种来源词: 首次是 new(逻辑名), 换候选是 switch(新候选), 同候选重试是
// retry(候选); 其余过渡直接写 from → to 两个阶段词。
// 返回 said=false 表示这次切换不单独成事件 (同阶段同候选的重复上报)。
func stageMoveText(oldStage int, oldUp, oldMod string, stage int, up, mod, logical string) (string, bool) {
	oldC, newC := candName(oldUp, oldMod), candName(up, mod)
	if stage == oldStage && newC == oldC {
		return "", false // 同阶段、同候选的重复上报 (acquire 会再报一次 sem) 不单独成事件
	}
	if up == "" {
		return "", false
	}
	switch stage {
	case stageSem:
		switch {
		case oldStage == stageParse:
			return "new(" + logical + ")" + stageSep + "sem(" + up + ")", true
		case newC != "" && newC != oldC:
			return "switch(" + newC + ")" + stageSep + "sem(" + up + ")", true
		}
		return "retry(" + newC + ")" + stageSep + "sem(" + up + ")", true
	case stageQueue:
		if oldStage != stageSem {
			return "", false
		}
		return "sem(" + up + ")" + stageSep + "freq(" + up + ")", true
	case stageModelWait:
		if oldStage != stageQueue {
			return "", false
		}
		return "freq(" + up + ")" + stageSep + "queue(" + candName(up, mod) + ")", true
	case stageSend:
		if oldStage != stageModelWait {
			return "", false
		}
		return "queue(" + candName(up, mod) + ")" + stageSep + "send", true
	case stageRecv:
		if oldStage != stageSend {
			return "", false
		}
		return "send" + stageSep + "recv", true
	}
	return "", false
}

// stageTag 是展示阶段的短名 (事件正文与结局事件的 from 段都用它)。
func stageTag(stage int) string {
	switch stage {
	case stageParse:
		return "parse"
	case stageSem:
		return "sem"
	case stageQueue:
		return "freq"
	case stageModelWait:
		return "queue"
	case stageSend:
		return "send"
	case stageRecv:
		return "recv"
	case stageFailed:
		return "fail"
	}
	return ""
}

// candName / modelQueue 把 上游/模型 拼成事件文案里的主语。
func candName(up, mod string) string {
	switch {
	case up == "":
		return mod
	case mod == "":
		return up
	}
	return up + "/" + mod
}

// flowLive 刷新「接收中」这一笔已收到的输出 token 数与字节数, 以及首字节时刻 (只写
// 展示字段, 不碰阶段耗时)。由 relayStream 节流回写 (约 200ms 一次); 第 1 页用这三个
// 值算这笔自己的 token 吞吐与字节吞吐 (都从首字节起算)。
func (a *App) flowLive(id uint64, tokens int, bytes int64, firstAt time.Time) {
	a.flowMu.Lock()
	if f := a.flows[id]; f != nil {
		f.RecvTok, f.RecvBytes, f.RecvStart = tokens, bytes, firstAt.UnixMilli()
	}
	a.flowMu.Unlock()
}

// flowObs 把一笔请求的展示状态与事件喂给观测面 (TUI)。只读地看调度器, 不改调度:
// 它只读 cand 的锁/队列快照, 不做任何等待或记账。
type flowObs struct {
	a       *App
	id      uint64
	logical string

	up      string // 当前候选 (换候选时由阶段事件写清走向)
	model   string // 当前候选的真实模型 id (换候选时一起更新)
	group   string // 当前候选的额度组名 (查询组锁用; 换候选时一起更新)
	attempt int
	done    bool
}

// mark 刷新当前候选的展示阶段。status: 0 = 刚进入候选 (下一步取信号量),
// -1 = 传输层失败 (展示落到已失败块), 其余为上游状态码 (该候选在发送阶段失败)。
// 失败结局由 outcome 在失败点单独发事件, 这里只动展示。
func (o *flowObs) mark(status int) {
	st := stageSend
	if status == 0 {
		st = stageSem // 刚进入这个候选: 下一步就是取信号量
	}
	if status < 0 {
		st = stageFailed
	}
	o.say(st)
}

// stage 把展示阶段推到调度器当前的步骤; acquire 的回调与发包前后调用它。
func (o *flowObs) stage(st int) {
	o.say(st)
}

// say 把展示阶段推到调度器当前的步骤, 一路写阶段事件。
func (o *flowObs) say(st int) {
	o.a.flowStage(o.id, o.up, o.model, st, o.gateNow(), o.attempt)
}

// outcome 记一笔候选结局事件: 「from → fail/reject(原因)」。from 取该候选结束时所在
// 的阶段 (sem = 没发出去, send = 发出后失败); 原因是归类词 / 状态码 / 锁原因。请求的
// 整体结局就是链上最后一条, 这里发的就是这条。
func (o *flowObs) outcome(from int, word, param string) {
	text := stageTag(from) + stageSep + word
	if param != "" {
		text += "(" + param + ")"
	}
	o.a.evAddReq(o.id, "error", o.logical, o.up, "", text)
	o.a.flowFail(o.id, from, word, param)
}

// gateNow 是「这一笔现在卡在哪一步」: 锁 / 退避 / 出队节拍 / 已发出。文案用与事件页
// 同一套短词 (quota / breaker / lowperf / backoff), 不在这里写中文。
func (o *flowObs) gateNow() string {
	c := o.a.candOf(o.up)
	if c == nil {
		return "sent"
	}
	now := time.Now()
	if locked, left, reason := c.locked(o.model, o.group); locked {
		return lockParam(reason, left)
	}
	c.mu.Lock()
	paused, next := c.pauseOf(o.model), c.nextSend
	c.mu.Unlock()
	if paused.After(now) {
		return "backoff " + durStr(paused.Sub(now))
	}
	if next.After(now) {
		return "interval " + durStr(next.Sub(now))
	}
	return "sent"
}

func (a *App) candOf(id string) *cand {
	a.mu.RLock()
	c := a.cands[id]
	a.mu.RUnlock()
	return c
}

// finish 是这笔请求的成功收尾: 唯一发 ok 事件 (recv → ok(200))。失败结局已在各候选
// 失败点由 outcome 发出 (模型找不到时由 pickAttempts 补 parse → fail(404)),
// 这里不再重复。只记状态变化: 仅仅首字节慢、但没触发任何锁的请求不在这里留痕。
func (o *flowObs) finish(status int) {
	if o.done {
		return
	}
	o.done = true
	if status == 0 {
		status = 200 // 正常收尾 (含 4xx 透传之外的路径) 都算成功
	}
	if status < 400 {
		o.a.evAddReq(o.id, "ok", o.logical, o.up, "", "recv"+stageSep+"ok("+strconv.Itoa(status)+")")
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
