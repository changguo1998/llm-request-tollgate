// probe_run.go: 探测目标挑选与单次探测。

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type probeTarget struct {
	alias string
	up    *UpstreamCfg
	model string
}

// probeTargets 列出要探测的候选: scope=next 时只取 fallback 顺序里的第二个
// (第一个候选由真实流量被动采样), 单候选别名不探。
func (a *App) probeTargets(c *ProbeCfg) []probeTarget {
	a.mu.RLock()
	defer a.mu.RUnlock()
	filter := map[string]bool{}
	for _, t := range c.Targets {
		filter[t] = true
	}
	names := make([]string, 0, len(a.aliases))
	for name := range a.aliases {
		if len(filter) > 0 && !filter[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var out []probeTarget
	for _, name := range names {
		cands := a.aliases[name]
		pick := cands
		if c.Scope != "all" {
			if len(cands) < 2 {
				continue
			}
			pick = cands[1:2] // 只探下一个候选
		}
		for _, cand := range pick {
			if u, ok := a.upstreams[cand.Upstream]; ok {
				out = append(out, probeTarget{alias: name, up: u, model: cand.Model})
			}
		}
	}
	return out
}

// probeCandidate 发一次探测: 走该上游的限速闸 (不与用户流量抢队), 流式读首字节
// 与流末 usage, 得到 ttfb_ms 与 tps; 全程不碰失败计数与锁。
func (a *App) probeCandidate(alias string, up *UpstreamCfg, model string, c *ProbeCfg) {
	sample := probeSample{Ts: time.Now().Format(time.RFC3339Nano)}
	payload := map[string]any{
		"model":          model,
		"messages":       []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens":     c.MaxTokens,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	a.mu.RLock()
	cd, gate := a.cands[up.ID], a.gate
	group := a.groups[up.ID][model]
	a.mu.RUnlock()
	if cd == nil {
		sample.Err = "no scheduler"
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	// 探测自己排队, 最多等 5s; 排不上就跳过这一轮, 不占用户请求的信号量与排队预算。
	gateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	kind, why, _, _, ok := cd.acquire(gateCtx, gate, model, group)
	cancel()
	if !ok {
		sample.Err = "gate busy: " + why
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	probeOK := false
	defer func() { cd.finish(kind, probeOK) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutMs)*time.Millisecond)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		sample.Err = err.Error()
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	req.ContentLength = int64(len(body))
	prepareOutboundHeaders(req.Header, http.Header{}, up)
	resp, err := a.transport.RoundTrip(req)
	if err != nil {
		sample.Err = err.Error()
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		sample.Err = fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}

	// 流式读: 首字节时刻 = ttfb, 流末时刻 - 首字节 = 解码段。
	var firstAt, lastAt time.Time
	var usage *Usage
	acc := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			now := time.Now()
			if firstAt.IsZero() {
				firstAt = now
			}
			lastAt = now
			if len(acc) < 64*1024 { // 探测响应很小, 截断只为兜底
				acc = append(acc, buf[:n]...)
			}
		}
		if rerr != nil {
			break
		}
	}
	if firstAt.IsZero() {
		sample.Err = "no response body"
		a.recordProbeSample(up.ID, model, sample, c.Window)
		return
	}
	if u := extractUsage(acc); u != nil {
		usage = u
	}
	sample.OK = true
	probeOK = true
	sample.TtfbMs = msCeil(firstAt.Sub(start))
	if usage != nil {
		sample.Tokens = usage.Completion
		if d := lastAt.Sub(firstAt); d > 0 && usage.Completion > 0 {
			sample.Tps = float64(usage.Completion) / d.Seconds()
		}
	}
	a.recordProbeSample(up.ID, model, sample, c.Window)
	// 探测流量如实写请求日志 (source=probe): 它真的消耗配额, 用量交给外部程序分析。
	a.logReq(logRec{
		Ts: time.Now().Format(time.RFC3339Nano), Logical: alias, Upstream: up.ID, Model: model,
		Path: "/v1/probe", Stream: true, Status: http.StatusOK, Usage: usage,
		Missing: usage == nil, Source: "probe", TtfbMs: sample.TtfbMs, Tps: sample.Tps,
		DurationMs: msCeil(time.Since(start)),
	})
}
