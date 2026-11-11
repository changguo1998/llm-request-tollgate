// relay_test.go: SSE 流式透传的口径 (拆分 relayStream 前的特征测试)。

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 流末 usage 提取、model 改回逻辑名、以 [DONE] 正常结束时不补 error 事件。
func TestRelayStreamUsageAndRewrite(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"model":"real-1","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"model":"real-1","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		"",
	}, "\n"))
	rec := httptest.NewRecorder()
	ro := relayStream(rec, body, "m1", "real-1", time.Now(), false, nil)
	if ro.Missing || ro.Usage == nil || ro.Usage.Total != 15 ||
		ro.Usage.Prompt != 10 || ro.Usage.Completion != 5 {
		t.Fatalf("应提取流末 usage: %+v", ro)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"model":"real-1"`) || !strings.Contains(out, `"model":"m1"`) {
		t.Errorf("流内 model 应改回逻辑名:\n%s", out)
	}
	if strings.Contains(out, "event: error") {
		t.Errorf("正常 [DONE] 收尾不该补 error 事件:\n%s", out)
	}
}

// /v1/responses: response.completed 是终态 (不需要 [DONE]), usage 在 response 对象里,
// 所有 *.delta 都计入输出估算。
func TestRelayStreamResponsesCompleted(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":{"text":"hello"}}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"real-1","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
		"",
	}, "\n"))
	rec := httptest.NewRecorder()
	ro := relayStream(rec, body, "m1", "real-1", time.Now(), true, nil)
	if ro.Missing || ro.Usage == nil || ro.Usage.Total != 5 {
		t.Fatalf("response.completed 应算正常收尾并取 response.usage: %+v", ro)
	}
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("responses 正常终态不该补 error 事件:\n%s", rec.Body.String())
	}
}

// 流没有正常结束: 追一个 SSE error 事件, 用量缺失时按估算兜底 (只算 completion)。
func TestRelayStreamUnexpectedEnd(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"abc\"}}]}\n")
	rec := httptest.NewRecorder()
	ro := relayStream(rec, body, "m1", "real-1", time.Now(), false, nil)
	if !ro.Missing {
		t.Error("没有 usage 时应标 Missing")
	}
	if ro.Usage == nil || ro.Usage.Completion <= 0 || ro.Usage.Total != ro.Usage.Completion {
		t.Errorf("缺失时估算应只写 completion: %+v", ro)
	}
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("异常结束应补 error 事件:\n%s", rec.Body.String())
	}
	if ro.Bytes <= 0 || ro.TtfbMs < 0 {
		t.Errorf("字节数与首字节时延应统计: %+v", ro)
	}
}
