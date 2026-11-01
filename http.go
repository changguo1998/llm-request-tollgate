// http.go: HTTP 通用件 —— 头处理、错误响应与 /v1/models。

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// 客户端可能带来的认证头一律删除, 换成上游配置的凭据。
var clientAuthHeaders = []string{
	"Authorization", "Api-Key", "X-Api-Key", "api-key", "X-Goog-Api-Key", "Proxy-Authorization",
}

// shouldFallback: 该响应是否进候选分支 (换下一个候选或按类上锁)。只有 200 算服务成功;
// 其余状态码 (含 3xx / 4xx / 429 / 5xx) 一律交给 classifyFailure 按上游规则判类,
// 免得额度类的 400 / 402 / 403 / 422 被当成成功直接透传、既不上锁也不计数。
func shouldFallback(status int) bool {
	return status != http.StatusOK
}

// buildAttemptPayload 按候选上游生成转发请求体: 真实模型 id + 按上游开关注入
// include_usage。每次从原始 payload 复制, 不跨尝试共享可变状态。
func buildAttemptPayload(payload map[string]any, model string, includeUsage, stream bool) ([]byte, error) {
	p := make(map[string]any, len(payload))
	for k, v := range payload {
		p[k] = v
	}
	if stream {
		var so map[string]any
		if orig, ok := p["stream_options"].(map[string]any); ok {
			so = make(map[string]any, len(orig)+1)
			for k, v := range orig {
				so[k] = v
			}
		} else if includeUsage {
			so = map[string]any{}
		}
		if so != nil {
			if includeUsage {
				so["include_usage"] = true
			}
			p["stream_options"] = so
		}
	}
	p["model"] = model
	return json.Marshal(p)
}

// keyPrefix 给出认证头里的 scheme: 显式配置优先; 缺省 Authorization 用 "Bearer ",
// 其余头 (api-key / x-api-key 这类) 直接放裸 key。
func (u *UpstreamCfg) keyPrefix() string {
	if u.KeyPrefix != nil {
		return *u.KeyPrefix
	}
	if strings.EqualFold(u.AuthHeader, "Authorization") {
		return "Bearer "
	}
	return ""
}

// prepareOutboundHeaders 拷贝客户端头, 替换认证, 删 hop-by-hop 与 Accept-Encoding。
func prepareOutboundHeaders(dst, src http.Header, up *UpstreamCfg) {
	for k, vv := range src {
		dst[k] = append([]string(nil), vv...)
	}
	for _, k := range clientAuthHeaders {
		dst.Del(k)
	}
	for _, k := range hopHeaders {
		dst.Del(k)
	}
	dst.Del("Accept-Encoding")
	if up.AuthHeader != "" {
		dst.Set(up.AuthHeader, up.keyPrefix()+up.Key)
	}
	for k, v := range up.Headers {
		dst.Set(k, v)
	}
}

func isHopHeader(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": code, "code": code},
	})
}

// serveModels 返回别名表里的逻辑模型清单。只列对外名字, 不暴露上游真实 id。
func (a *App) serveModels(w http.ResponseWriter) {
	a.mu.RLock()
	names := make([]string, 0, len(a.aliases))
	for n := range a.aliases {
		names = append(names, n)
	}
	a.mu.RUnlock()
	sort.Strings(names)
	data := make([]map[string]string, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]string{"id": n, "object": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}
