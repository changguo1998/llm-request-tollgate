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
}

// relayStream 按行透传 SSE, 期间提取流末 usage。
// 流未以 [DONE] 正常结束时, 发一个 SSE error 事件后关流, 不伪造正常结束。
// live 每约 200ms 回写一次已收到的输出 token 数与首字节时刻, 可为 nil。
// 返回提取到的 usage 与是否缺失。
func relayStream(w http.ResponseWriter, body io.Reader, logical, realModel string, start time.Time, isResponses bool, live func(tokens int, firstAt time.Time)) relayOut {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var usage *Usage
	est := 0
	done := false
	var firstAt, lastAt time.Time // 首字节 / 末字节时刻 (被动时延采样)
	var lastLive time.Time        // 上次回写实时吞吐的时刻 (节流用)

	write := func(s string) {
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
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
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				done = true
			} else if data != "" {
				var chunk struct {
					Usage *openaiUsage `json:"usage"`
					Type  string       `json:"type"`
					// /v1/responses 的事件把 usage 与 model 都装在 response 对象里。
					Response *struct {
						Usage *openaiUsage `json:"usage"`
					} `json:"response"`
					Delta   json.RawMessage `json:"delta"`
					Choices []struct {
						Delta struct {
							Content          string `json:"content"`
							ReasoningContent string `json:"reasoning_content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if usage == nil {
						switch {
						case chunk.Usage != nil:
							usage = toUsage(chunk.Usage)
						case chunk.Response != nil && chunk.Response.Usage != nil:
							usage = toUsage(chunk.Response.Usage)
						}
					}
					if isResponses {
						// Responses API 走语义事件流, 终态是 response.completed (不保证发
						// [DONE]); failed/incomplete 是上游自己报的终态, 也算正常收尾 ——
						// 否则每条正常流都会被补一个 error 事件。
						switch chunk.Type {
						case "response.completed", "response.failed", "response.incomplete":
							done = true
						case "response.output_text.delta", "response.reasoning_text.delta",
							"response.reasoning_summary_text.delta":
							var s string
							if json.Unmarshal(chunk.Delta, &s) == nil {
								est += estimateTokens(s)
							}
						}
					}
					for _, c := range chunk.Choices {
						// 思考增量也算输出: 纯思考模型在 content 里一个字都没有, 只计
						// content 会让接收中的吞吐一直显示不出来, 兜底的 completion 也会是 0。
						est += estimateTokens(c.Delta.Content)
						est += estimateTokens(c.Delta.ReasoningContent)
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
				}
			}
		}
		write(line + "\n")
		// 实时吞吐: 最多 200ms 回写一次, 不每行都去抢 flowMu。
		if live != nil && !firstAt.IsZero() && now.Sub(lastLive) >= 200*time.Millisecond {
			lastLive = now
			live(est, firstAt)
		}
	}
	if !done {
		// 流未正常结束: 用流内约定形态表达错误。
		write("event: error\n")
		write(`data: {"error":{"message":"upstream stream ended unexpectedly","type":"upstream_error"}}` + "\n\n")
	}
	missing := usage == nil
	if missing {
		usage = &Usage{Completion: est, Total: est}
	}
	out := relayOut{Usage: usage, Missing: missing}
	if !firstAt.IsZero() {
		out.TtfbMs = msCeil(firstAt.Sub(start))
		if !lastAt.IsZero() {
			out.DecodeMs = msCeil(lastAt.Sub(firstAt))
		}
	}
	return out
}
