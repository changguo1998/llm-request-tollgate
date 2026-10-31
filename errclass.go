// errclass.go: 上游报错分类规则 (并发 / 额度 / 其他)。

package main

import (
	"encoding/json"
	"strings"
)

type ErrRule struct {
	Status   []int    `json:"status,omitempty"`
	Contains []string `json:"contains,omitempty"`
}

// ErrRules 是每个上游一份的错误分类规则, 形状见 docs/providers-api.md §3.1。
// 只保留两档会触发特殊动作的类: 额度 (锁候选 10min) 与并发 (锁超发试探 1h);
// 调研文档里的可选 `rate` 档动作与通用兜底一致, 所以不单独建类 (配了也不生效)。
type ErrRules struct {
	Quota       []ErrRule `json:"quota,omitempty"`
	Concurrency []ErrRule `json:"concurrency,omitempty"`
}

// errClass 是三类特殊限制加一类通用兜底。
type errClass int

const (
	classOther       errClass = iota // 通用兜底: 退避与熔断
	classConcurrency                 // 并发限制: 锁掉超发试探
	classQuota                       // 额度限制: 锁整个候选
)

// errMatchText 拼出关键词匹配用的文本: error.type + error.code + error.message 转小写;
// body 不是 JSON, 或没有 error 对象 (有的上游回纯文本) 时退回整段 body
// (docs/providers-api.md §1.2 第 2/3 步 —— 不同上游的关键词落在不同字段上)。
func errMatchText(body []byte) string {
	if len(body) > 8*1024 {
		body = body[:8*1024] // 只用于匹配, 截断到 8KB
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil &&
		(env.Error.Message != "" || env.Error.Type != "" || len(env.Error.Code) > 0) {
		// code 有的是字符串 ("QuotaExceeded.X"), 有的是数字; 去掉引号后当文本拼进去。
		code := strings.Trim(string(env.Error.Code), `"`)
		return strings.ToLower(strings.Join(
			[]string{env.Type, env.Error.Type, code, env.Error.Message}, " "))
	}
	return strings.ToLower(string(body))
}

// match 判定一条规则是否命中 (status 与 contains 是与关系); 空规则恒不命中。
func (r ErrRule) match(status int, text string) bool {
	if len(r.Status) == 0 && len(r.Contains) == 0 {
		return false // 空规则: 别当成"命中一切"
	}
	if len(r.Status) > 0 {
		hit := false
		for _, s := range r.Status {
			if s == status {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(r.Contains) == 0 {
		return true // 只写状态码的规则: 语义明确, 但额度类不该这么配
	}
	for _, kw := range r.Contains {
		if kw != "" && strings.Contains(text, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// classify 按该上游自己的规则给报错归类; 没配规则就是通用兜底。
// 额度类先判: 它不可重试, 必须先于并发/限流类 (docs/providers-api.md §3.2 第 1 条)。
func (u *UpstreamCfg) classify(status int, body []byte) errClass {
	if u.Errors != nil {
		text := errMatchText(body)
		for _, r := range u.Errors.Quota {
			if r.match(status, text) {
				return classQuota
			}
		}
		for _, r := range u.Errors.Concurrency {
			if r.match(status, text) {
				return classConcurrency
			}
		}
	}
	return classOther
}

// bodySnippet 取 body 前 n 个字符用于日志, 去掉换行与回车免得把日志行撑破。
func bodySnippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	s = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}
