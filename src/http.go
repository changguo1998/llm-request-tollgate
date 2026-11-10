// http.go: HTTP 通用件 —— 头处理、错误响应与 /v1/models。

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
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

// serveSticky 处理 POST /sticky (人工改粘性偏好), query 参数:
//   - logical: 逻辑名; 清全部时可省略
//   - upstream: 目标上游; 省略 = 清除偏好 (回落配置首候选)
//   - model: 目标真实模型; 省略时取该上游在候选链里的第一个
//
// 只置一次: 立即生效, 自动迁移规则照旧 (之后真实失败 / 长期锁仍可迁走); 纯内存不落盘。
func (a *App) serveSticky(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST /sticky")
		return
	}
	if a.sticky == nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "preference store is not initialized")
		return
	}
	q := r.URL.Query()
	logical, upstream, model := q.Get("logical"), q.Get("upstream"), q.Get("model")
	if upstream == "" {
		n := a.sticky.reset(logical)
		a.evAdd("sticky", logical, "", "", "人工清除偏好 "+strconv.Itoa(n)+" 条")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"cleared": n})
		return
	}
	if logical == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"logical is required when upstream is set")
		return
	}
	// 在 a.mu 下解析并写入 (锁序 a.mu -> sticky.mu, 与 applyConfig 一致), 免得解析出的
	// 下标被并发热加载挪走。
	a.mu.RLock()
	list, ok := a.aliases[logical]
	idx := -1
	if ok {
		for i, at := range list {
			if at.Upstream == upstream && (model == "" || at.Model == model) {
				idx = i
				break
			}
		}
	}
	if idx >= 0 {
		a.sticky.set(logical, idx)
	}
	a.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found", "unknown model: "+logical)
		return
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "not_found",
			"candidate not in alias chain: "+upstream+"/"+model)
		return
	}
	at := list[idx]
	a.evAdd("sticky", logical, at.Upstream, at.Model, "人工置偏好 (置一次, 自动迁移照旧)")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"logical": logical, "upstream": at.Upstream, "model": at.Model,
	})
}

// forceUnlock 强制解锁 (调试用): 清掉全部候选 (或指定 upstream) 的锁与连续错误计数, 落盘。
// model / group 非空时按它们过滤。返回清掉的锁数。
func (a *App) forceUnlock(upID, model, group string) int {
	a.mu.RLock()
	cands := make([]*cand, 0, len(a.cands))
	if upID != "" {
		if c, ok := a.cands[upID]; ok {
			cands = append(cands, c)
		}
	} else {
		for _, c := range a.cands {
			cands = append(cands, c)
		}
	}
	a.mu.RUnlock()
	total := 0
	for _, c := range cands {
		total += c.clearLocks(model, group)
	}
	a.saveGateState()
	a.evAdd("unlock", "", upID, model, "unlock(manual "+strconv.Itoa(total)+")")
	return total
}

// serveUnlock 处理 POST /unlock (调试用强制解锁)。可用 query 过滤:
// upstream / model / group; 都不给 = 清掉所有候选的所有锁与退避。
func (a *App) serveUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST /unlock")
		return
	}
	q := r.URL.Query()
	total := a.forceUnlock(q.Get("upstream"), q.Get("model"), q.Get("group"))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"cleared": total})
}
