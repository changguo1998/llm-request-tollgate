// usage.go: 用量提取与兜底估算, 响应里的 model 字段改写。

package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	// Cached 是命中缓存的输入 token (各家字段名不同, 见 openaiUsage);
	// 平台按缓存价计费 (约为输入价 1/10), 系数标定要用到。
	Cached int `json:"cached,omitempty"`
}

// openaiUsage 是上游响应里的原始字段名 (OpenAI 兼容口径)。
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Responses API (/v1/responses) 口径: 同名语义用 input/output_tokens 表达。
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// 四种缓存口径: OpenAI/vLLM 的 prompt_tokens_details.cached_tokens,
	// DeepSeek 的 prompt_cache_hit_tokens, Anthropic 风格的 cache_read_input_tokens,
	// Responses API 的 input_tokens_details.cached_tokens。
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

func toUsage(u *openaiUsage) *Usage {
	if u == nil {
		return nil
	}
	// chat/completions 用 prompt/completion_tokens, Responses API 用 input/output_tokens;
	// 两者归一成同一份 Usage, 下游记账与汇总不必区分接口。
	prompt, completion := u.PromptTokens, u.CompletionTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	if completion == 0 {
		completion = u.OutputTokens
	}
	cached := u.PromptCacheHitTokens
	if cached == 0 {
		cached = u.CacheReadInputTokens
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		cached = u.InputTokensDetails.CachedTokens
	}
	return &Usage{
		Prompt: prompt, Completion: completion,
		Total: u.TotalTokens, Cached: cached,
	}
}

// estimateTokens 按字符口径粗估: 非 ASCII 1 token/字符, 其余 4 字符 1 token。
// 流式请求由 relayStream 逐块累加, 非流式由 estimateUsageFromBody 读响应体; 两路都
// 把思考内容算输出。
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return other + (ascii+3)/4
}

func extractUsage(buf []byte) *Usage {
	var resp struct {
		Usage *openaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return toUsage(resp.Usage)
}

// estimateUsageFromBody 取响应体里的生成文本与思考内容 (message.content /
// message.reasoning_content / output[].content[].text / output[].summary[].text), 用于
// 非流式缺 usage 时的兜底估算; 口径与流式逐块累加的那一路一致 —— 思考也算输出。
func estimateUsageFromBody(buf []byte) int {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		// /v1/responses: 生成文本在 output[].content[].text, reasoning 项的可见思考
		// 在 output[].summary[].text。
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return 0
	}
	n := 0
	for _, c := range resp.Choices {
		n += estimateTokens(c.Message.Content)
		n += estimateTokens(c.Message.ReasoningContent)
	}
	for _, o := range resp.Output {
		for _, c := range o.Content {
			n += estimateTokens(c.Text)
		}
		for _, s := range o.Summary {
			n += estimateTokens(s.Text)
		}
	}
	return n
}

// replaceModelValue 把 data 里值为 raw 的 "model" 字段换成 enc, 其余字节原样保留。
func replaceModelValue(data, raw, enc []byte) []byte {
	key := []byte(`"model":`)
	for from := 0; ; {
		i := bytes.Index(data[from:], key)
		if i < 0 {
			return data
		}
		i += from
		j := i + len(key)
		for j < len(data) && (data[j] == ' ' || data[j] == '\t') {
			j++
		}
		if j+len(raw) <= len(data) && bytes.Equal(data[j:j+len(raw)], raw) {
			out := make([]byte, 0, len(data)+(len(enc)-len(raw)))
			out = append(out, data[:j]...)
			out = append(out, enc...)
			out = append(out, data[j+len(raw):]...)
			return out
		}
		from = i + 1
	}
}

// rewriteModel 把 JSON 里顶层 "model" 字段的值改成 logical, 其余字节原样保留。
// 仅处理值为不含量转义的简单字符串, 否则原样返回。
func rewriteModel(data []byte, logical string) []byte {
	if logical == "" {
		return data
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	raw, ok := m["model"]
	if !ok || len(raw) < 2 || raw[0] != '"' || bytes.IndexByte(raw, '\\') >= 0 {
		return data
	}
	var cur string
	if json.Unmarshal(raw, &cur) != nil || cur == logical {
		return data
	}
	enc, _ := json.Marshal(logical)
	return replaceModelValue(data, raw, enc)
}

// rewriteModelKnown 同 rewriteModel, 但用于 /v1/responses 事件里嵌在 response
// 对象下的 model (顶层没有 model 字段可解析): 真实 id 由路由已知, 直接按字面量换。
func rewriteModelKnown(data []byte, real, logical string) []byte {
	if real == "" || logical == "" || real == logical || strings.ContainsAny(real, `\"`) {
		return data
	}
	raw, err := json.Marshal(real)
	if err != nil {
		return data
	}
	enc, _ := json.Marshal(logical)
	return replaceModelValue(data, raw, enc)
}
