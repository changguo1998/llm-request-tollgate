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
	dst := s.w.Header()
	for k, vv := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	var usage *Usage
	missing := true
	status := resp.StatusCode
	var ttfbMs, decodeMs, bytesOut int64
	if s.stream {
		s.w.WriteHeader(status)
		dst.Del("Content-Length")
		ro := relayStream(s.w, resp.Body, s.logical, realModel, s.sentAt, s.isResponses,
			func(tokens int, bytes int64, firstAt time.Time) {
				a.flowLive(s.flowID, tokens, bytes, firstAt)
			})
		usage, missing, ttfbMs, decodeMs, bytesOut = ro.Usage, ro.Missing, ro.TtfbMs, ro.DecodeMs, ro.Bytes
	} else {
		buf, rerr := io.ReadAll(resp.Body)
		// 非流式: 首字节与整体等待几乎同时 (上游把响应写全才回头), tps 无从分解。
		ttfbMs = msCeil(time.Since(s.sentAt))
		if rerr != nil {
			status = http.StatusBadGateway
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
	}

	// 吞吐口径: 真实流式请求才有解码段, 所以 tps / bps 仅流式有值。
	tps, bps := 0.0, 0.0
	completion := 0
	if usage != nil {
		completion = usage.Completion
	}
	if s.stream && decodeMs > 0 {
		if completion > 0 {
			tps = float64(completion) / (float64(decodeMs) / 1000)
		}
		if bytesOut > 0 {
			bps = float64(bytesOut) / (float64(decodeMs) / 1000)
		}
	}
	if status < 400 {
		// 低性能锁的样本: 吞吐只取能拆出解码段的流式成功请求, 首字节时延取任何成功响应。
		a.notePerfRequest(s.servedUp.ID, realModel, completion, decodeMs, ttfbMs, s.upPath)
	}

	s.finalStatus = status
	rec := s.baseLog()
	rec.Upstream, rec.Model = s.servedUp.ID, realModel
	rec.Status, rec.Usage, rec.Missing = status, usage, missing
	rec.TtfbMs, rec.DecodeMs, rec.Tps = ttfbMs, decodeMs, tps
	rec.Bytes, rec.Bps = bytesOut, bps
	rec.DurationMs = msCeil(time.Since(s.reqStart))
	a.logReq(rec)
}
