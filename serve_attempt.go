// serve_attempt.go: 候选链尝试 —— 取资格、转发与失败归类。

package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// attemptLoop 按候选顺序尝试, 只在还没给客户端写任何字节前 (传输失败 / 上游 429 或 5xx)
// 换下一个候选; 全部失败时保留最近一次错误, 由 writeFailure 决定应答。
func (s *serveState) attemptLoop() {
	for _, at := range s.attempts {
		// 进入这个候选: 展示状态跟着走 (换候选时 mark 收到失败码, 顺手记一条 fallback 事件)。
		s.obs.attempt++
		s.obs.up, s.obs.model, s.obs.group = at.up.ID, at.mod, at.group
		s.obs.mark(0, "")
		if at.cd == nil {
			s.noteFailure(fmt.Errorf("upstream %s has no scheduler", at.up.ID), at.up.ID, at.mod)
			continue
		}
		if locked, left, reason := at.cd.locked(at.mod, at.group); locked {
			// 锁内不真发 (该模型的熔断锁 / 它所属组的额度锁), 直接换下一个候选。
			s.noteFailure(fmt.Errorf("upstream %s locked for %s (%s)",
				at.up.ID, left.Round(time.Second), reason), at.up.ID, at.mod)
			s.noteLeft(left)
			s.lockedSkips++
			continue
		}
		if paused, left := at.cd.paused(at.mod); paused {
			// 该模型正在退避: 不进它的队列白等 (退避比排队预算长), 直接换候选。
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
			s.gateReject = why
			s.lastUpID, s.lastMod = at.up.ID, at.mod
			s.failed++
			if why == "locked" {
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
			// 传输层失败: 没有状态码与 body 可判, 按通用兜底走。
			at.cd.finish(kind, false)
			a.recordGenericFailure(at.cd, at.mod, s.gate)
			s.noteFailure(rerr, at.up.ID, at.mod)
			s.obs.mark(-1, rerr.Error())
			continue
		}
		s.obs.stage(stageRecv)
		if shouldFallback(resp.StatusCode) {
			b, berr := io.ReadAll(resp.Body)
			resp.Body.Close()
			at.cd.finish(kind, false)
			s.obs.mark(resp.StatusCode, "")
			moveOn := a.classifyFailure(at, kind, resp, b, s.gate)
			s.lastErr = fmt.Errorf("upstream %s returned %d", at.up.ID, resp.StatusCode)
			s.lastStatus, s.lastHdr, s.lastBody = resp.StatusCode, resp.Header.Clone(), b
			s.lastUpID, s.lastMod = at.up.ID, at.mod
			s.failed++
			if berr != nil {
				s.lastErr = berr
			}
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
		s.obs.stage(stageRecv) // 响应头已到, 之后的整段转发都算「接收中」, 别再退回并发池
		return
	}
}

// classifyFailure 按上游自己的规则给失败归类并按类上锁, 返回是否该换下一个候选。
func (a *App) classifyFailure(at attempt, kind slotKind, resp *http.Response, body []byte, gate GateCfg) bool {
	switch at.up.classify(resp.StatusCode, body) {
	case classQuota:
		// 额度限制: 只锁该模型所属的额度组 (额度按组共享), 组内排队的请求随之迁移,
		// 同家别的组照常发送。
		a.addLockGroup(at.cd, at.group, "quota", gate.quotaLock())
	case classConcurrency:
		// 并发限制: 锁掉超发试探。试探撞上就锁完直接换下一个候选; 普通请求锁完后本笔
		// 按普通错误走 —— 退避后重试同一候选。
		at.cd.lockConcurrency("concurrency", gate.concurrencyLock())
		if kind != slotProbe {
			return a.recordGenericFailure(at.cd, at.mod, gate)
		}
	default:
		// 规则表没覆盖的样本: 把上游 + 状态码 + body 片段记进服务日志, 便于按真实文案补规则。
		log.Printf("unclassified upstream error: upstream=%s status=%d body=%s",
			at.up.ID, resp.StatusCode, bodySnippet(body, 200))
		return a.recordGenericFailure(at.cd, at.mod, gate)
	}
	return true
}

// noteFailure 记下最近一次失败: 换候选的统计与最终应答都用它。
func (s *serveState) noteFailure(err error, upID, mod string) {
	s.lastErr = err
	s.lastUpID, s.lastMod = upID, mod
	s.failed++
}

// noteLeft 只保留最长的那把锁剩余时间 (Retry-After 用)。
func (s *serveState) noteLeft(left time.Duration) {
	if left > s.gateLeft {
		s.gateLeft = left
	}
}
