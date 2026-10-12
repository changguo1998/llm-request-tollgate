// fakeupstream: 本地假上游, 为自测回放 OpenAI 兼容响应 (设计 §4.7 起步方式)。
//
// 行为:
//   - POST /v1/chat/completions: 根据请求的 stream 返回 SSE 或 JSON。
//   - SSE 回放序列: 思考块 -> 文本 -> 工具调用 -> (include_usage 时) usage -> [DONE]。
//   - 头 x-abort-after=N: 发出 N 个 data 块后直接关流 (不回 [DONE]),
//     用于验证代理的 F6 行为 (流中失败发 SSE error 事件)。
//   - usage 确定性生成: prompt_tokens = 7 + runeCount(body)/4, completion = 42,
//     测试脚本用同一公式核对代理日志。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19000", "listen addr")
	delayMs := flag.Int("delay", 20, "SSE chunk delay ms")
	flag.Parse()

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		var req struct {
			Model         string `json:"model"`
			Stream        bool   `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.Unmarshal(body, &req)
		if req.Model == "" {
			req.Model = "fake"
		}
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		// 确定性用量: 测试脚本按同一组数值核对代理日志。
		const promptTokens = 7
		const completionTokens = 42

		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"id": "chatcmpl-fake", "object": "chat.completion", "created": time.Now().Unix(),
				"model": req.Model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "Hello world!"},
				}},
				// 非流式响应按真实 provider 行为总是带 usage。
				"usage": map[string]any{
					"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
					"total_tokens": promptTokens + completionTokens,
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		// 流式: 逐块回放。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		delay := time.Duration(*delayMs) * time.Millisecond

		abortAfter := -1
		if v := r.Header.Get("X-Abort-After"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				abortAfter = n
			}
		}

		send := func(data string) bool {
			// 返回 false 表示该块是应被截断的块之后的块 (abort 触发)。
			if abortAfter == 0 {
				return false
			}
			if abortAfter > 0 {
				abortAfter--
			}
			if _, err := w.Write([]byte("data: " + data + "\n\n")); err != nil {
				return false
			}
			flusher.Flush()
			time.Sleep(delay)
			return true
		}

		chunks := []string{
			`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"` + req.Model + `","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking step one"}}]}`,
			`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"` + req.Model + `","choices":[{"index":0,"delta":{"content":"Hello "}}]}`,
			`{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"` + req.Model + `","choices":[{"index":0,"delta":{"content":"world!","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]}}]}`,
		}
		for _, c := range chunks {
			if !send(c) {
				return
			}
		}
		if includeUsage {
			usage := `{"id":"chatcmpl-fake","object":"chat.completion.chunk","model":"` + req.Model + `","choices":[],"usage":{"prompt_tokens":` +
				strconv.Itoa(promptTokens) + `,"completion_tokens":` + strconv.Itoa(completionTokens) +
				`,"total_tokens":` + strconv.Itoa(promptTokens+completionTokens) + `}}`
			if !send(usage) {
				return
			}
		}
		// [DONE] 也受 abort 截断影响: abort 触发时直接关流 (F6 验证)。
		if abortAfter != 0 {
			w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		}
	})

	// 供别名表与模型清单探测用。
	http.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "deepseek-v4.1-flash", "object": "model"}},
		})
	})

	srv := &http.Server{Addr: *addr, Handler: nil}
	log.Printf("fake upstream listening on %s (delay %dms)", *addr, *delayMs)
	if err := srv.ListenAndServe(); err != nil && !strings.Contains(err.Error(), "closed") {
		log.Fatal(err)
	}
}
