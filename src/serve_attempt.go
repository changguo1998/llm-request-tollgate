// serve_attempt.go: 候选链尝试 —— 取资格、转发与失败归类。

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

// attemptLoop 按候选顺序尝试, 只在还没给客户端写任何字节前换下一个候选 (传输失败 /
// 上游非 200); 全部失败时保留最近一次错误, 由 writeFailure 决定应答。
func (s *serveState) attemptLoop() {
	for _, at := range s.attempts {
		// 进入这个候选: 展示状态跟着走; 失败原因由下一次阶段切换的战报带出。
		s.obs.attempt++
		s.obs.up, s.obs.model, s.obs.group = at.up.ID, at.mod, at.group
		s.obs.mark(0)
		// pref: 正在处理的是不是本笔的偏好候选 (排位首位那个)。
		pref := at.idx == s.prefIdx
		if at.cd == nil {
			s.obs.outcome(stageSem, "reject", "disabled")
			s.noteFailure(fmt.Errorf("upstream %s has no scheduler", at.up.ID), at.up.ID, at.mod)
			continue
		}
		if locked, left, reason := at.cd.locked(at.mod, at.group); locked {
			// 锁内不真发 (该模型的熔断锁 / 它所属组的额度锁), 直接换下一个候选。
			// 若这是恢复态 (锁已到期待探测通过): 后台顺手发起一笔恢复探测, 请求本身
			// 仍照锁定规则进下一个候选, 不等待结果。
			s.app.kickRecovery(at.cd, at.mod, at.group)
			// 偏好候选被长期锁 (额度 / 低性能 / 熔断) 顶班: 成功后直接迁移偏好。
			if pref && stickyLongLock(reason) {
				s.stickyLong = true
			}
			s.obs.outcome(stageSem, "reject", lockParam(reason, left))
			s.noteFailure(fmt.Errorf("upstream %s locked for %s (%s)",
				at.up.ID, left.Round(time.Second), reason), at.up.ID, at.mod)
			s.noteLeft(left)
			s.lockedSkips++
			continue
		}
		if paused, left := at.cd.paused(at.mod); paused {
			// 该模型正在退避: 不进它的队列白等 (退避比排队预算长), 直接换候选。
			s.obs.outcome(stageSem, "reject", "backoff "+durStr(left))
			s.noteFailure(fmt.Errorf("upstream %s/%s backing off for %s",
				at.up.ID, at.mod, left.Round(time.Second)), at.up.ID, at.mod)
			s.noteLeft(left)
			s.backoffSkips++
			continue
		}
		// /v1/responses 没有 stream_options, 不做 include_usage 注入 (用量本就在响应里)。
		body, err := buildAttemptPayload(s.payload, at.mod,
			at.up.IncludeUsage && !s.isResponses, s.stream)
		if err != nil {
			s.noteFailure(err, at.up.ID, at.mod)
			continue
		}
		s.tryCandidate(at, body)
		if s.servedUp != nil {
			break
		}
	}
}

// tryCandidate 在一个候选上重试到出结果: 退避后回到出队就是重新排队, 退避长过排队上限时
// 自然变成 queue_timeout, 于是换下一个候选。
func (s *serveState) tryCandidate(at attempt, body []byte) {
	a := s.app
	for {
		// 每轮都重建出站请求: 上一轮的 body 已经被读过, 复用同一个 Request 会带空体。
		outReq, err := http.NewRequestWithContext(s.r.Context(), s.r.Method,
			at.up.BaseURL+s.upPath, bytes.NewReader(body))
		if err != nil {
			s.noteFailure(err, at.up.ID, at.mod)
			return
		}
		outReq.ContentLength = int64(len(body))
		prepareOutboundHeaders(outReq.Header, s.r.Header, at.up)
		// 取资格放在请求体准备之后, 构造失败就不白占槽位。
		kind, why, sem, qms, ok := at.cd.acquire(s.r.Context(), s.gate, at.mod, at.group, s.obs.stage)
		s.semMs += sem
		s.queueMs += qms
		if !ok {
			// 偏好候选取资格失败 (锁 / 信号量超时 / 排队超时): 是临时不可用, 顶班
			// 不算迁移; 锁且是长期锁 (额度 / 低性能 / 熔断) 则按候选失效直接迁移。
			if at.idx == s.prefIdx && why == "locked" {
				if _, _, reason := at.cd.locked(at.mod, at.group); stickyLongLock(reason) {
					s.stickyLong = true
				}
			}
			s.gateReject = why
			s.obs.outcome(stageSem, "reject", acquireFailParam(at, why))
			s.lastUpID, s.lastMod = at.up.ID, at.mod
			s.failed++
			if why == "locked" {
				a.kickRecovery(at.cd, at.mod, at.group)
				s.lockedSkips++
				if _, left, _ := at.cd.locked(at.mod, at.group); left > s.gateLeft {
					s.gateLeft = left
				}
			}
			return
		}
		s.sentAt = time.Now()
		s.obs.stage(stageSend)
		resp, rerr := a.transport.RoundTrip(outReq)
		if rerr != nil {
			// 传输层失败: 没有状态码与 body 可判, 按通用兜底走 (也没响应可解析恢复时间)。
			at.cd.finish(kind, false)
			a.recordGenericFailure(at.cd, at.mod, s.gate, s.upPath, 0)
			s.noteFailure(rerr, at.up.ID, at.mod)
			s.obs.mark(-1)
			s.obs.outcome(stageSend, "fail", "connection")
			s.logUpstreamError(at, 0, nil, nil, rerr)
			continue
		}
		s.obs.stage(stageRecv)
		if shouldFallback(resp.StatusCode) {
			b, berr := io.ReadAll(resp.Body)
			resp.Body.Close()
			at.cd.finish(kind, false)
			s.obs.mark(resp.StatusCode)
			moveOn, longLock := a.classifyFailure(at, kind, resp, b, s.gate, s.upPath)
			s.obs.outcome(stageSend, "fail", failClassParam(at, resp.StatusCode, b))
			// 偏好候选真实报错后换走: 命中长期锁 (额度 / 低性能 / 熔断) 才迁移 —— 熔断是
			// 连续失败到阈值才上锁, 一上锁即把偏好迁走; 没落锁的单次波动不动偏好, 下笔
			// 照常先试它 (锁查询在 classifyFailure 之后, 拿到的是本笔失败刚上的锁)。
			if at.idx == s.prefIdx {
				if _, _, reason := at.cd.locked(at.mod, at.group); longLock || stickyLongLock(reason) {
					s.stickyLong = true
				}
			}
			s.lastErr = fmt.Errorf("upstream %s returned %d", at.up.ID, resp.StatusCode)
			s.lastStatus, s.lastHdr, s.lastBody = resp.StatusCode, resp.Header.Clone(), b
			s.lastUpID, s.lastMod = at.up.ID, at.mod
			s.failed++
			if berr != nil {
				s.lastErr = berr
			}
			// 每次上游真实请求以错误收场都落一笔日志 (带上原始响应体/头与归类);
			// 后续候选兜底成功时这笔记错误也留痕, 供排查额度与到期时间。
			s.logUpstreamError(at, resp.StatusCode, b, s.lastHdr, nil)
			if moveOn {
				return
			}
			continue
		}
		at.cd.recordSuccess(at.mod)
		if kind == slotProbe {
			at.cd.finish(slotProbe, true) // 试探成功: 上限 +1 (不设天花板)
		} else {
			s.heldCand, s.heldKind = at.cd, kind // 响应发完才归还槽位
		}
		s.servedUp, s.servedMod, s.resp = at.up, at.mod, resp
		s.servedIdx = at.idx
		s.obs.stage(stageRecv) // 响应头已到, 之后的整段转发都算「接收中」, 别再退回并发池
		return
	}
}

// classifyFailure 按上游自己的规则给失败归类并按类上锁, 返回 (moveOn, longLock):
// moveOn 表示是否该换下一个候选; longLock 表示命中的是长期锁 (额度), 偏好候选被这种
// 锁顶班时应视为候选失效直接迁移。path 是这笔请求用的上游路径, 锁会记下它, 供恢复探测沿用。
func (a *App) classifyFailure(at attempt, kind slotKind, resp *http.Response, body []byte, gate GateCfg, path string) (bool, bool) {
	switch at.up.classify(resp.StatusCode, body) {
	case classQuota:
		// 额度限制: 只锁该模型所属的额度组 (额度按组共享), 组内排队的请求随之迁移,
		// 同家别的组照常发送。上游响应若带额度重置时间 (响应体 / 响应头), 按解析出的
		// 重置时刻设锁 (recoveryDur); 解析不到才回退配置的固定时长。
		dur := gate.quotaLock()
		if d := recoveryDur(body, resp.Header, time.Now()); d > 0 {
			dur = d
		}
		a.addLockGroup(at.cd, at.group, "quota", dur, path)
		return true, true
	case classConcurrency:
		// 并发限制: 锁掉超发试探。试探撞上就锁完直接换下一个候选; 普通请求锁完后本笔
		// 按普通错误走 —— 退避后重试同一候选。
		at.cd.lockConcurrency("concurrency", gate.concurrencyLock())
		if kind != slotProbe {
			return a.recordGenericFailure(at.cd, at.mod, gate, path,
				recoveryDur(body, resp.Header, time.Now())), false
		}
	default:
		// 规则表没覆盖的样本: 把上游 + 状态码 + body 片段记进服务日志, 便于按真实文案补规则。
		log.Printf("unclassified upstream error: upstream=%s status=%d body=%s",
			at.up.ID, resp.StatusCode, bodySnippet(body, 200))
		// 通用兜底 (退避 + 熔断): 熔断锁的时长优先用响应里解析出的恢复时间 (如 503 并发
		// 限制的 Retry-After, 常比固定 5min 长), 解析不到才回退 breaker_lock_ms。
		return a.recordGenericFailure(at.cd, at.mod, gate, path,
			recoveryDur(body, resp.Header, time.Now())), false
	}
	return true, false
}

// noteFailure 记下最近一次失败: 换候选的统计与最终应答都用它。
func (s *serveState) noteFailure(err error, upID, mod string) {
	s.lastErr = err
	s.lastUpID, s.lastMod = upID, mod
	s.failed++
}

// logUpstreamError 在每次上游真实请求以错误收场时落一笔日志: 携带原始响应体 (截断)
// 与全部响应头, 以及按该上游规则表归类的结果 (quota / concurrency / other)。即使
// 后续候选兜底成功, 这笔上游错误也留痕, 供排查额度限制与到期时间 (Retry-After /
// x-ratelimit-reset-* / body 内 budget 字段)。传输出错 (terr != nil) 时没有响应
// 内容可记, 归类记为 transport。
func (s *serveState) logUpstreamError(at attempt, status int, body []byte, hdr http.Header, terr error) {
	rec := s.baseLog()
	rec.Upstream, rec.Model = at.up.ID, at.mod
	rec.Missing = true
	if terr != nil {
		rec.Status, rec.Err, rec.Class = 0, terr.Error(), "transport"
	} else {
		rec.Status = status
		rec.Err = fmt.Sprintf("upstream %s returned %d", at.up.ID, status)
		rec.ErrBody = errBodyText(body)
		rec.ErrHeaders = hdr.Clone()
		rec.Class = at.up.classify(status, body).String()
	}
	s.app.logReq(rec)
}

// noteLeft 只保留最长的那把锁剩余时间 (Retry-After 用)。
func (s *serveState) noteLeft(left time.Duration) {
	if left > s.gateLeft {
		s.gateLeft = left
	}
}

// acquireFailParam 是取资格失败的原因词 (reject 的参数)。
func acquireFailParam(at attempt, why string) string {
	switch why {
	case "locked":
		if _, left, reason := at.cd.locked(at.mod, at.group); left > 0 {
			return lockParam(reason, left)
		}
		return "locked"
	case "backoff_timeout":
		return "backoff-timeout"
	case "sem_timeout":
		return "sem-timeout"
	case "queue_timeout":
		return "queue-timeout"
	case "canceled":
		return "cancelled"
	}
	if why == "" {
		return "candidate"
	}
	return why
}

// failClassParam 是「发出后失败」的原因词: 命中上游归类词用归类词 (quota / concurrency),
// 没归类才写状态码数字。
func failClassParam(at attempt, status int, body []byte) string {
	switch at.up.classify(status, body) {
	case classQuota:
		return "quota"
	case classConcurrency:
		return "concurrency"
	}
	return strconv.Itoa(status)
}

// lockParam 是锁原因 + 剩余时间的短词 (reject 的参数), 时长保留 mm:ss。
func lockParam(reason string, left time.Duration) string {
	name := "quota"
	switch reason {
	case "breaker":
		name = "breaker"
	case lowPerfReason:
		name = "lowperf"
	}
	return name + " " + durStr(left)
}
