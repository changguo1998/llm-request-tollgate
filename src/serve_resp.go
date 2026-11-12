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
		usage, missing, ttfbMs, decodeMs, bytesOut = s.forwardStream(resp, realModel)
	} else {
		status, usage, missing, ttfbMs, bytesOut = s.forwardBody(resp, realModel)
	}

	// 吞吐口径: 真实流式请求才有解码段, 所以 tps / bps 仅流式有值。
	tps, bps, completion := responseRates(s.stream, usage, decodeMs, bytesOut)
	if status < 400 {
		// 低性能锁的样本: 吞吐只取能拆出解码段的流式成功请求, 首字节时延取任何成功响应。
		a.notePerfRequest(s.servedUp.ID, realModel, completion, decodeMs, ttfbMs, bytesOut, s.upPath)
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

// forwardStream 走 SSE 透传, 返回用量与被动时延 / 字节统计。
func (s *serveState) forwardStream(resp *http.Response, realModel string) (usage *Usage, missing bool, ttfbMs, decodeMs, bytesOut int64) {
	s.w.WriteHeader(resp.StatusCode)
	s.w.Header().Del("Content-Length")
	ro := relayStream(s.w, resp.Body, s.logical, realModel, s.sentAt, s.isResponses,
		func(tokens int, bytes int64, firstAt time.Time) {
			s.app.flowLive(s.flowID, tokens, bytes, firstAt)
		})
	return ro.Usage, ro.Missing, ro.TtfbMs, ro.DecodeMs, ro.Bytes
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
	rec.DurationMs = msCeil(time.Since(s.reqStart))
	s.app.logReq(rec)
}
