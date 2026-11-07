// observe_flow.go: 请求流的登记与阶段推进。

package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

// evAdd 记一条不挂在某笔请求上的事件 (锁 / 探测 / 解锁 / 启动 / 热载)。
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
		Stream: stream, StartTime: now, Stage: stageParse, MaxStage: stageParse, stageAt: now}
	a.flowMu.Unlock()
	return id
}

// flowStage 更新一笔进行中请求的展示状态; up/mod 为空表示不改这两列。
// gate 由调用方先算好 (读 cand 的锁/节拍), 这里只写展示字段, 不碰调度。
// note 是这次切换要带出的「旧阶段结果」(失败原因); 返回值 = 是否真的发了一条阶段事件,
// 调用方据此决定要不要清空 note —— 静默过渡不发事件, 原因留给下一次切换。
func (a *App) flowStage(id uint64, up, mod string, stage int, gate string, attempt int, note string) bool {
	a.flowMu.Lock()
	f := a.flows[id]
	if f == nil {
		a.flowMu.Unlock()
		return false
	}
	now := time.Now()
	oldStage, oldUp, oldMod := f.Stage, f.Up, f.Model
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
	text, said := stageMoveText(oldStage, oldUp, oldMod, stage, f.Up, f.Model, gate, note)
	logical := f.Logical
	a.flowMu.Unlock()
	if said {
		a.evAddReq(id, "stage", logical, "", "", text)
	}
	return said
}

// stageMoveText 写一条阶段切换的战报: 「旧阶段结果, 新阶段操作」。
// 返回 said=false 表示这次切换不单独成事件 (失败回退只更新展示, 原因留给下一次切换)。
func stageMoveText(oldStage int, oldUp, oldMod string, stage int, up, mod, gate, note string) (string, bool) {
	oldC, newC := candName(oldUp, oldMod), candName(up, mod)
	// 同一阶段、同一候选的重复上报 (acquire 会再报一次 sem) 不单独成事件。
	if stage == oldStage && newC == oldC && note == "" {
		return "", false
	}
	switch stage {
	case stageSem:
		if oldStage == stageParse {
			return "收到请求, 等待信号量", true
		}
		head := note
		if head == "" {
			head = leaveText(oldStage, oldC)
		}
		if head == "" {
			return "", false
		}
		if newC != "" && newC != oldC {
			return head + ", 换到 " + newC + ", 等待信号量", true
		}
		return head + ", 重新等待信号量", true
	case stageQueue:
		if oldStage != stageSem {
			return "", false
		}
		return "获得信号量, 加入频率控制队列", true
	case stageModelWait:
		if oldStage != stageQueue {
			return "", false
		}
		text := "出频率控制队列, 加入 " + modelQueue(newC)
		if isGateDetail(gate) {
			text += " (" + gate + ")"
		}
		return text, true
	case stageSend:
		if oldStage == stageRecv {
			return "", false // 非 2xx 回退标记: 失败原因在 note 里, 留给下一次切换
		}
		if oldStage != stageModelWait {
			return "", false
		}
		return "出 " + modelQueue(oldC) + ", 发送请求", true
	case stageRecv:
		if oldStage != stageSend {
			return "", false
		}
		return "收到响应头, 开始接收", true
	case stageFailed:
		return "", false // 传输失败只更新展示, 原因在下一次切换或收尾事件里写
	}
	return "", false
}

// leaveText 是旧阶段结束时的那半句 (没有失败 note 时用)。
func leaveText(stage int, cand string) string {
	switch stage {
	case stageSem:
		return "等信号量结束"
	case stageQueue:
		return "出频率控制队列"
	case stageModelWait:
		return "出 " + modelQueue(cand)
	case stageSend:
		return "已发出的请求失败"
	case stageRecv:
		return "响应失败"
	case stageFailed:
		return "候选失败"
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

func modelQueue(cand string) string {
	if cand == "" {
		return "模型队列"
	}
	return cand + " 模型队列"
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
	note    string // 旧阶段结果 (失败 / 跳过原因), 逐次阶段切换时带出并清空
	done    bool
}

// mark 刷新展示状态。status: 0 = 刚进入候选 (下一步取信号量), -1 = 传输层失败,
// 其余为上游状态码 (该候选失败)。失败只记进 note: 由下一次阶段切换的战报写出来,
// 避免同一次失败既发「换候选」又发「阶段切换」两条。
func (o *flowObs) mark(status int) {
	if status > 0 {
		o.note = "收到 " + strconv.Itoa(status)
	} else if status < 0 {
		o.note = "连接失败"
	}
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

// say 推进展示阶段; 只有真的记了一条阶段事件才清空 note (静默过渡要把原因留住)。
func (o *flowObs) say(st int) {
	if o.a.flowStage(o.id, o.up, o.model, st, o.gateNow(), o.attempt, o.note) {
		o.note = ""
	}
}

// failNote 写下一次阶段切换要带出的失败原因 (候选预检跳过 / 取资格失败)。
func (o *flowObs) failNote(s string) {
	o.note = s
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
		switch reason {
		case "breaker":
			why = "熔断"
		case lowPerfReason:
			why = "低性能"
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

func (a *App) candOf(id string) *cand {
	a.mu.RLock()
	c := a.cands[id]
	a.mu.RUnlock()
	return c
}

// finish 是这笔请求的结局: 进事件流 (2xx 记 ok 接收结束, 其余记 error 候选链结束)。
// 只记状态变化: 仅仅首字节慢、但没触发任何锁的请求不在这里留痕。
func (o *flowObs) finish(status int) {
	if o.done {
		return
	}
	o.done = true
	if status == 0 {
		status = 200 // 正常收尾 (含 4xx 透传之外的路径) 都算成功
	}
	if status >= 400 {
		msg := "候选链结束, 状态码 " + strconv.Itoa(status)
		// 失败原因没被任何一次切换带出来时 (如唯一候选被锁到链尾) 补在收尾里;
		// note 就是状态码本身时不重复写。
		if o.note != "" && o.note != "收到 "+strconv.Itoa(status) {
			msg += " (" + o.note + ")"
		}
		o.a.evAddReq(o.id, "error", o.logical, o.up, "", msg)
		return
	}
	o.a.evAddReq(o.id, "ok", o.logical, o.up, "", "接收结束, 状态码 "+strconv.Itoa(status))
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
