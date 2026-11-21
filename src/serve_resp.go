// serve_resp.go: 转发上游响应 —— 头透传、流式/非流式、用量提取与被动时延采样。

package main

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

// forward 把上游响应发给客户端, 并落一笔带用量与耗时的日志。
func (s *serveState) forward() {
	a, resp := s.app, s.resp
	defer resp.Body.Close()
	realModel := s.servedMod
	// 转发响应头: 除 hop-by-hop 外原样透传。
	copyResponseHeaders(s.w.Header(), resp.Header)

	var usage *Usage
	missing := true
	status := resp.StatusCode
	var ttfbMs, decodeMs, bytesOut int64
	if s.stream {
		ro := s.forwardStream(resp, realModel)
		usage, missing, ttfbMs, decodeMs, bytesOut = ro.Usage, ro.Missing, ro.TtfbMs, ro.DecodeMs, ro.Bytes
	} else {
		status, usage, missing, ttfbMs, bytesOut = s.forwardBody(resp, realModel)
	}

	// 吞吐口径: 真实流式请求才有解码段, 所以 tps / bps 仅流式有值。
	tps, bps, completion := responseRates(s.stream, usage, decodeMs, bytesOut)
	if status < 400 {
		if s.aborted {
			// 打断的请求: 不计性能样本 (避免超低 r* 拉低历史平均、放宽相对门槛);
			// 给该 上游/模型 打低性能锁, 并发 abort 观测事件。
			s.applyAbort(realModel)
		} else {
			// 低性能锁的样本: 吞吐只取能拆出解码段的流式成功请求, 首字节时延取任何成功响应。
			a.notePerfRequest(s.servedUp.ID, realModel, completion, decodeMs, ttfbMs, bytesOut, s.upPath)
		}
	}
	s.logResponse(status, realModel, usage, missing, ttfbMs, decodeMs, tps, bps, bytesOut)
}

// copyResponseHeaders 除 hop-by-hop 外原样透传响应头。
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// forwardStream 走 SSE 透传, 返回 relay 结果 (用量 / 时延 / 字节 / 是否被打断)。
// 每约 200ms 的实时回写里顺带做打断判定 (checkStreamAbort), 命中后 relay 停读。
func (s *serveState) forwardStream(resp *http.Response, realModel string) relayOut {
	s.w.WriteHeader(resp.StatusCode)
	s.w.Header().Del("Content-Length")
	return relayStream(s.w, resp.Body, s.logical, realModel, s.sentAt, s.isResponses,
		func(tokens int, bytes int64, firstAt time.Time, hint *relayHint) {
			s.app.flowLive(s.flowID, tokens, bytes, firstAt)
			s.checkStreamAbort(realModel, tokens, bytes, firstAt, hint)
		})
}

// checkStreamAbort 在每次实时回写时做打断判定, 只对流式请求生效。四条件按固定顺序:
// 打断已开启 + 不在豁免名单 -> 解码时长超过打断超时 (从首字节起算, 与高延迟阈值无关)
// -> 实时性能低于绝对门槛 (lowperf_avg_max) -> 低于相对门槛 (历史平均的 lowperf_rel_max
// 倍, 样本不足不判)。命中即置打断态, 并让 relay 停读 (客户端侧由流未正常结束分支收尾)。
func (s *serveState) checkStreamAbort(realModel string, tokens int, bytes int64, firstAt time.Time, hint *relayHint) {
	if s.aborted {
		hint.Abort = true // 已经判过要打断: 让 relay 立即停
		return
	}
	g := s.gate
	if g.AbortTimeoutMs <= 0 || g.lowPerfExempt(realModel) {
		return
	}
	d := msCeil(time.Since(firstAt))
	if d <= int64(g.AbortTimeoutMs) || tokens <= 0 || bytes <= 0 {
		return
	}
	// 实时性能用同一套 r* 口径 (与低性能锁、日志、TUI 一致)。
	r := s.app.perfIndex(s.servedUp.ID, realModel, int64(tokens), d, bytes)
	c := s.heldCand
	if c == nil {
		c = s.app.candOf(s.servedUp.ID)
	}
	if c == nil {
		return // 没有调度器状态可查: 不判 (正常路径不会走到)
	}
	n, sum, ok := c.perfHist(realModel)
	if !ok || !shouldAbortStream(d, r, n, sum, g) {
		return
	}
	s.aborted, s.abortMs, s.abortR, s.abortHist = true, d, r, sum/float64(n)
	hint.Abort, hint.AbortMs, hint.AbortR = true, d, r
}

// applyAbort 处理打断收尾: 给该 上游/模型 打低性能锁 (沿用 addLockLowPerf 形态, 已在该
// 模型的 lowperf 锁内不重复加, 避免重锁翻倍被同一轮打断反复触发), 并发 abort 观测事件。
// 性能样本已在 forward 跳过。
func (s *serveState) applyAbort(realModel string) {
	a := s.app
	c := s.heldCand
	if c == nil {
		c = a.candOf(s.servedUp.ID)
	}
	if c != nil && !c.modelLowPerfLocked(realModel) {
		peak := c.windowLowPerfPeak(realModel)
		if peak <= 0 {
			peak = s.abortR // 窗口无样本: 退实时值当 peak
		}
		a.addLockLowPerf(c, realModel, peak, s.abortR, 0, s.gate.lowPerfLock(), s.upPath)
	}
	s.obs.abort(s.abortR, s.abortHist)
}

// forwardBody 读完整响应后发出 (非流式): 改写 model、提取用量并在缺失时估算。
// 读失败时状态码换成 502, 响应体换成 JSON 错误。
func (s *serveState) forwardBody(resp *http.Response, realModel string) (status int, usage *Usage, missing bool, ttfbMs, bytesOut int64) {
	buf, rerr := io.ReadAll(resp.Body)
	// 非流式: 首字节与整体等待几乎同时 (上游把响应写全才回头), tps 无从分解。
	ttfbMs = msCeil(time.Since(s.sentAt))
	status = resp.StatusCode
	dst := s.w.Header()
	if rerr != nil {
		status = http.StatusBadGateway
		s.obs.outcome(stageRecv, "fail", "connection")
		buf = []byte(`{"error":{"message":` + strconv.Quote(rerr.Error()) + `}}`)
		dst.Set("Content-Type", "application/json")
	}
	// /v1/responses 的 model 嵌在 response 对象里 (与流式事件同形), 按已知的真实 id 换;
	// 聊天接口只需换顶层字段。
	if s.isResponses {
		buf = rewriteModelKnown(buf, realModel, s.logical)
	} else {
		buf = rewriteModel(buf, s.logical)
	}
	usage = extractUsage(buf)
	missing = usage == nil
	if missing {
		est := estimateUsageFromBody(buf)
		usage = &Usage{Completion: est, Total: est}
	}
	bytesOut = int64(len(buf))
	dst.Set("Content-Length", strconv.Itoa(len(buf)))
	s.w.WriteHeader(status)
	s.w.Write(buf)
	return status, usage, missing, ttfbMs, bytesOut
}

// responseRates 算流式解码段的 tps / bps (非流式或无解码段时为 0), 并返回 completion。
func responseRates(stream bool, usage *Usage, decodeMs, bytesOut int64) (tps, bps float64, completion int) {
	if usage != nil {
		completion = usage.Completion
	}
	if stream && decodeMs > 0 {
		sec := float64(decodeMs) / 1000
		if completion > 0 {
			tps = float64(completion) / sec
		}
		if bytesOut > 0 {
			bps = float64(bytesOut) / sec
		}
	}
	return tps, bps, completion
}

// logResponse 落最终请求日志 (用量 / 耗时 / 吞吐与本次请求的计数)。
func (s *serveState) logResponse(status int, realModel string, usage *Usage, missing bool,
	ttfbMs, decodeMs int64, tps, bps float64, bytesOut int64) {
	s.finalStatus = status
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.servedUp.ID, realModel
	rec.Status, rec.Usage, rec.Missing = status, usage, missing
	rec.TtfbMs, rec.DecodeMs, rec.Tps = ttfbMs, decodeMs, tps
	rec.Bytes, rec.Bps = bytesOut, bps
	rec.Abort, rec.AbortMs, rec.AbortR, rec.AbortHist = s.aborted, s.abortMs, s.abortR, s.abortHist
	rec.DurationMs = msCeil(time.Since(s.reqStart))
	s.app.logReq(rec)
}
