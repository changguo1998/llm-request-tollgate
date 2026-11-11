// relay.go: SSE 透传与流式用量提取。

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type relayOut struct {
	Usage    *Usage
	Missing  bool
	TtfbMs   int64
	DecodeMs int64
	Bytes    int64 // 转发给客户端的响应体字节数 (SSE 行 + 补发的 error 事件)
}

// relayStream 按行透传 SSE, 期间提取流末 usage 并统计已转发的响应字节数。
// 流未以 [DONE] 正常结束时, 发一个 SSE error 事件后关流, 不伪造正常结束。
// live 每约 200ms 回写一次已收到的输出 token 数、响应字节数与首字节时刻, 可为 nil。
// 返回提取到的 usage 与是否缺失。
func relayStream(w http.ResponseWriter, body io.Reader, logical, realModel string, start time.Time, isResponses bool, live func(tokens int, bytes int64, firstAt time.Time)) relayOut {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var bytesOut int64
	acc := relayAcc{}
	var firstAt, lastAt time.Time // 首字节 / 末字节时刻 (被动时延采样)
	var lastLive time.Time        // 上次回写实时吞吐的时刻 (节流用)

	write := func(s string) {
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
		bytesOut += int64(len(s))
		if flusher != nil {
			flusher.Flush()
		}
	}
	for sc.Scan() {
		now := time.Now()
		if firstAt.IsZero() {
			firstAt = now
		}
		lastAt = now
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			line = acc.feed(line, isResponses, logical, realModel)
		}
		write(line + "\n")
		// 实时吞吐: 最多 200ms 回写一次, 不每行都去抢 flowMu。
		if live != nil && !firstAt.IsZero() && now.Sub(lastLive) >= 200*time.Millisecond {
			lastLive = now
			live(acc.est, bytesOut, firstAt)
		}
	}
	if !acc.done {
		// 流未正常结束: 用流内约定形态表达错误。
		write("event: error\n")
		write(`data: {"error":{"message":"upstream stream ended unexpectedly","type":"upstream_error"}}` + "\n\n")
	}
	return relayResult(acc.usage, acc.est, firstAt, lastAt, start, bytesOut)
}

// relayAcc 累计一次流式转发的用量与终态 (逐行喂入)。
type relayAcc struct {
	usage *Usage
	est   int
	done  bool
}

// feed 处理一行 data: 负载, 返回实际转发的那一行 (model 改写 / [DONE] 原样)。
func (a *relayAcc) feed(line string, isResponses bool, logical, realModel string) string {
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return line
	}
	out, done, tokens, usage := parseSSEData(data, isResponses, logical, realModel)
	a.done = a.done || done
	a.est += tokens
	if a.usage == nil {
		a.usage = usage
	}
	return out
}

// parseSSEData 处理一行 data: 负载: 返回要转发的行 (可能改写过 model)、是否到终态、
// 输出 token 增量与块内 usage。
func parseSSEData(data string, isResponses bool, logical, realModel string) (line string, done bool, tokens int, usage *Usage) {
	line = "data: " + data
	if data == "[DONE]" {
		return line, true, 0, nil
	}
	usage, done, tokens, ok := parseSSEChunk(data, isResponses)
	if !ok {
		return line, false, 0, nil
	}
	// 块内 model 改回逻辑名 (只改该字段, 其余字节原样)。
	data2 := rewriteModel([]byte(data), logical)
	if isResponses {
		// 事件里的 model 嵌在 response 对象下, 顶层改写够不着。
		data2 = rewriteModelKnown(data2, realModel, logical)
	}
	if string(data2) != data {
		line = "data: " + string(data2)
	}
	return line, done, tokens, usage
}

// parseSSEChunk 解析一个 SSE 数据块: 提取 usage / 终态 / 输出 token 增量。
func parseSSEChunk(data string, isResponses bool) (usage *Usage, done bool, tokens int, ok bool) {
	var chunk struct {
		Usage *openaiUsage `json:"usage"`
		Type  string       `json:"type"`
		// /v1/responses 的事件把 usage 与 model 都装在 response 对象里。
		Response *struct {
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Delta json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return nil, false, 0, false
	}
	switch {
	case chunk.Usage != nil:
		usage = toUsage(chunk.Usage)
	case chunk.Response != nil && chunk.Response.Usage != nil:
		usage = toUsage(chunk.Response.Usage)
	}
	if isResponses {
		// Responses API 走语义事件流, 终态是 response.completed (不保证发
		// [DONE]); failed/incomplete 是上游自己报的终态, 也算正常收尾 ——
		// 否则每条正常流都会被补一个 error 事件。
		switch chunk.Type {
		case "response.completed", "response.failed", "response.incomplete":
			done = true
		}
		// 所有带内容的 *.delta 都计入 (文本 / 思考 / 工具参数 / 音频等多模态),
		// 只看 type 后缀, 结构事件与元数据不算。
		if strings.HasSuffix(chunk.Type, ".delta") {
			tokens += countOutputJSON(chunk.Delta)
		}
	}
	for _, c := range chunk.Choices {
		// 整个 delta 交给同一份口径: content / reasoning_content / 工具调用
		// (name 与 arguments) / 多模态容器都算输出, role / finish_reason 这类
		// 结构字段不算。思考增量必须计入 —— 纯思考模型 content 里一个字都
		// 没有, 只数 content 会让接收中的吞吐一直显示不出来。
		tokens += countOutputJSON(c.Delta)
	}
	return usage, done, tokens, true
}

// relayResult 汇总 relayOut: 缺 usage 时按输出估算兜底, 首字节 / 解码段只在有首字节时算。
func relayResult(usage *Usage, est int, firstAt, lastAt, start time.Time, bytesOut int64) relayOut {
	missing := usage == nil
	if missing {
		usage = &Usage{Completion: est, Total: est}
	}
	out := relayOut{Usage: usage, Missing: missing, Bytes: bytesOut}
	if !firstAt.IsZero() {
		out.TtfbMs = msCeil(firstAt.Sub(start))
		if !lastAt.IsZero() {
			out.DecodeMs = msCeil(lastAt.Sub(firstAt))
		}
	}
	return out
}
