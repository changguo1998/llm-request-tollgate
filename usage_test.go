package main

import (
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
