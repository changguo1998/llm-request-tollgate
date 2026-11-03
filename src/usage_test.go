package main

import (
	"strings"
	"testing"
)

// 非流式缺 usage 的兜底估算与流式同口径: 思考内容也算输出。
// "thinking" 8 字符 -> 2, "Hello world!" 12 字符 -> 3, 合计 5。
func TestEstimateUsageFromBodyCountsReasoning(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"Hello world!",
		"reasoning_content":"thinking"}}]}`)
	if got := estimateUsageFromBody(body); got != 5 {
		t.Fatalf("chat 非流式兜底应把思考算进去 (2+3), 得到 %d", got)
	}
}

// /v1/responses: 生成文本在 output[].content[].text, reasoning 项的思考在 summary[].text,
// 两处都要算; "Hello " 6 字符 -> 2, "thinking" -> 2, "world!" -> 2。
func TestEstimateUsageFromBodyResponsesReasoning(t *testing.T) {
	body := []byte(`{"output":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
		{"type":"message","content":[{"type":"output_text","text":"Hello "},
			{"type":"output_text","text":"world!"}]}]}`)
	if got := estimateUsageFromBody(body); got != 6 {
		t.Fatalf("responses 非流式兜底应算上 summary 里的思考 (2+2+2), 得到 %d", got)
	}
}

// 中文按 1 token/字符, 与流式口径同一函数: "思考" -> 2。
func TestEstimateUsageFromBodyCJKReasoning(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"reasoning_content":"思考","content":""}}]}`)
	if got := estimateUsageFromBody(body); got != 2 {
		t.Fatalf("中文思考应按 1 token/字符, 得到 %d", got)
	}
}

// 工具调用也算输出: name "get_weather" 11 字符 -> 3, arguments 18 字符 -> 5,
// content "ok" -> 1; role / id / type 这些结构字段不计, 合计 9。
func TestEstimateUsageFromBodyCountsToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok",
		"tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]}}]}`)
	if got := estimateUsageFromBody(body); got != 9 {
		t.Fatalf("工具调用的 name 与 arguments 应计入输出, 得到 %d", got)
	}
}

// 多模态段: base64 长串与 URL 每段按 1 token, 不按字符放大; 文本仍按字符口径。
func TestEstimateSegmentBinary(t *testing.T) {
	b64 := strings.Repeat("QUJD", 200) // 800 字符的 base64 片段
	if got := estimateSegment(b64); got != 1 {
		t.Fatalf("base64 段应记 1 token, 得到 %d", got)
	}
	if got := estimateSegment("https://example.com/a.png"); got != 1 {
		t.Fatalf("URL 段应记 1 token, 得到 %d", got)
	}
	if got := estimateSegment("hello world"); got != 3 {
		t.Fatalf("文本仍按字符口径 (11 字符 -> 3), 得到 %d", got)
	}
}

// 元数据不进吞吐: role / finish_reason / index / id / type 都不计, 只计内容字段。
func TestCountOutputSkipsMeta(t *testing.T) {
	delta := []byte(`{"role":"assistant","finish_reason":"stop","content":"Hello world!",
		"tool_calls":[{"index":0,"id":"call_1","type":"function",
			"function":{"name":"f","arguments":"{}"}}]}`)
	// content 12 -> 3, name "f" -> 1, arguments "{}" -> 1, 合计 5。
	if got := countOutputJSON(delta); got != 5 {
		t.Fatalf("结构字段不该计入, 得到 %d", got)
	}
}
