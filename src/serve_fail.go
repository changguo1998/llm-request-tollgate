// serve_fail.go: 拿不到可转发响应时的四类应答, 每类都落一笔日志。

package main

import (
	"math"
	"net/http"
	"strconv"
)

// writeFailure 按候选链的结局挑应答形态。
func (s *serveState) writeFailure() {
	switch {
	case s.gateReject != "" && s.lastStatus == 0:
		s.writeRejected()
	case s.lastStatus != 0 && s.lastBody != nil:
		s.writePassthrough()
	case s.lockedSkips > 0 && s.lastStatus == 0:
		s.writeLocked()
	default:
		s.writeBadGateway()
	}
}

// writeRejected: 请求根本没发出去 (被锁 / 等不到信号量 / 排队超时), 合成 429 说明原因,
// 不能透传上游的错误。
func (s *serveState) writeRejected() {
	retry := s.gate.queueWait()
	if s.gateLeft > 0 {
		retry = s.gateLeft
	}
	s.w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
	writeError(s.w, http.StatusTooManyRequests, "rate_limit_error",
		"upstream busy ("+s.gateReject+"): no candidate for "+s.logical+" gave a send slot")
	s.finalStatus = http.StatusTooManyRequests
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.lastUpID, s.lastMod
	rec.Status, rec.Missing, rec.Err, rec.Gate = s.finalStatus, true, "rate_limited", s.gateReject
	s.app.logReq(rec)
}

// writePassthrough: 最近一次是 HTTP 错误, 状态码 / 头 / 体原样透传。
func (s *serveState) writePassthrough() {
	dst := s.w.Header()
	for k, vv := range s.lastHdr {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	s.w.WriteHeader(s.lastStatus)
	s.w.Write(s.lastBody)
	s.finalStatus = s.lastStatus
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.lastUpID, s.lastMod
	rec.Status, rec.Missing = s.finalStatus, true
	if s.lastErr != nil {
		rec.Err = s.lastErr.Error()
	}
	s.app.logReq(rec)
}

// writeLocked: 候选全在锁里, 回 429 + Retry-After 让 agent 退避 —— 这是上游限制的传导,
// 比 502 更贴近真实原因。
func (s *serveState) writeLocked() {
	s.w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(s.gateLeft.Seconds())))))
	writeError(s.w, http.StatusTooManyRequests, "upstream_locked",
		"all candidates for "+s.logical+" are locked after upstream limits")
	s.finalStatus = http.StatusTooManyRequests
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.lastUpID, s.lastMod
	rec.Status, rec.Missing, rec.Err = s.finalStatus, true, "upstream_locked"
	s.app.logReq(rec)
}

// writeBadGateway: 没有可透传的上游错误 (传输失败 / 配置问题), 统一 502。
func (s *serveState) writeBadGateway() {
	msg := "all upstreams failed"
	if s.lastErr != nil {
		msg = s.lastErr.Error()
	}
	writeError(s.w, http.StatusBadGateway, "upstream_error", msg)
	s.finalStatus = http.StatusBadGateway
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.lastUpID, s.lastMod
	rec.Status, rec.Missing, rec.Err = s.finalStatus, true, msg
	s.app.logReq(rec)
}
