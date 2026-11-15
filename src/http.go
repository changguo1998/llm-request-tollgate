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

// modelMeta 是 /v1/models 条目里除 id 外下发的模型参数, 字段名与 fff store 一致;
// 没有任何候选声明时整段省略。
type modelMeta struct {
	Reasoning        bool               `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	ContextWindow    int                `json:"contextWindow,omitempty"`
	MaxTokens        int                `json:"maxTokens,omitempty"`
}

// modelEntry 是 /v1/models 的一个条目: 标准 OpenAI 的 id / object / created /
// owned_by, 再加按候选聚合的模型参数 (非标准扩展键, 客户端通常会忽略)。
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	modelMeta
}

// aggregateMeta 把一个逻辑名的候选聚合成对外模型参数: reasoning 任一候选声明即 true;
// thinkingLevelMap 按等级并集合并 (每个候选先补齐, 再逐级取同名映射, 没有同名取最低
// effort; 全候选都显式 null 的等级保持 null); contextWindow / maxTokens 取最小值 ——
// 请求可能落到任一候选, 只有保守值才不会被上游拒绝 (与
// scripts/upsert_tollgate_provider.py 同一口径)。调用方须持有 a.mu。
func (a *App) aggregateMeta(chain []aliasTarget) modelMeta {
	var meta modelMeta
	var maps []map[string]*string
	for _, t := range chain {
		up := a.upstreams[t.Upstream]
		if up == nil {
			continue
		}
		for i := range up.Models {
			m := &up.Models[i]
			if m.ID != t.Model {
				continue
			}
			if m.Reasoning {
				meta.Reasoning = true
			}
			// 没有等级表的候选也参与合并: 它按同名映射补齐 (与 upsert 脚本一致)。
			maps = append(maps, m.ThinkingLevelMap)
			if m.ContextWindow > 0 && (meta.ContextWindow == 0 || m.ContextWindow < meta.ContextWindow) {
				meta.ContextWindow = m.ContextWindow
			}
			if m.MaxTokens > 0 && (meta.MaxTokens == 0 || m.MaxTokens < meta.MaxTokens) {
				meta.MaxTokens = m.MaxTokens
			}
			break
		}
	}
	meta.ThinkingLevelMap = mergeLevelMaps(maps)
	return meta
}

// 推理等级从 off (不开启, 0) 到 max (最高); effort 名从低到高 (none = 不开启)。
var levelOrder = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func orderRank(order []string, name string) int {
	for i, item := range order {
		if item == name {
			return i
		}
	}
	return len(order) // 表里没有的名字排最后 (effort 未知按最低处理, 见 effortRank)
}

func levelRank(level string) int { return orderRank(levelOrder, level) }

// effortRank 把未知 effort 名当最低 (返回 -1), 合并取最低时更保守。
func effortRank(effort string) int {
	for i, item := range effortOrder {
		if item == effort {
			return i
		}
	}
	return -1
}

// filledLevelMap 把一张等级表补齐到 levels: 缺的等级指向本表更高的最低等级,
// 没有更高等级时指向本表最高等级; 完全没有表时按同名映射。
func filledLevelMap(levelMap map[string]*string, levels []string) map[string]*string {
	declared := map[string]string{}
	for level, effort := range levelMap {
		if effort != nil && *effort != "" {
			declared[level] = *effort
		}
	}
	if len(declared) == 0 {
		filled := make(map[string]*string, len(levels))
		for _, level := range levels {
			name := level
			filled[level] = &name
		}
		return filled
	}
	highest := ""
	for level := range declared {
		if highest == "" || levelRank(level) > levelRank(highest) {
			highest = level
		}
	}
	filled := make(map[string]*string, len(levels))
	for _, level := range levels {
		if effort, ok := declared[level]; ok {
			value := effort
			filled[level] = &value
			continue
		}
		higher := ""
		for name := range declared {
			if levelRank(name) <= levelRank(level) {
				continue
			}
			if higher == "" || levelRank(name) < levelRank(higher) {
				higher = name
			}
		}
		source := declared[highest]
		if higher != "" {
			source = declared[higher]
		}
		value := source
		filled[level] = &value
	}
	return filled
}

// mergeLevelMaps 按等级并集合并多张表: 候选有同名映射就用它, 否则取最低 effort;
// 全候选都显式写 null 的等级保持 null, 不补。
func mergeLevelMaps(maps []map[string]*string) map[string]*string {
	levelSet := map[string]bool{}
	for _, one := range maps {
		for level := range one {
			levelSet[level] = true
		}
	}
	if len(levelSet) == 0 {
		return nil
	}
	levels := make([]string, 0, len(levelSet))
	for level := range levelSet {
		levels = append(levels, level)
	}
	sort.Slice(levels, func(i, j int) bool { return levelRank(levels[i]) < levelRank(levels[j]) })

	filled := make([]map[string]*string, 0, len(maps))
	for _, one := range maps {
		filled = append(filled, filledLevelMap(one, levels))
	}
	out := make(map[string]*string, len(levels))
	for _, level := range levels {
		allNull := true
		for _, one := range maps {
			effort, ok := one[level]
			if !ok || effort != nil && *effort != "" {
				allNull = false
				break
			}
		}
		if allNull {
			out[level] = nil
			continue
		}
		sameNamed := false
		lowest := ""
		for _, one := range filled {
			effort := one[level]
			if effort == nil || *effort == "" {
				continue
			}
			if *effort == level {
				sameNamed = true
				break
			}
			if lowest == "" || effortRank(*effort) < effortRank(lowest) {
				lowest = *effort
			}
		}
		if sameNamed {
			name := level
			out[level] = &name
			continue
		}
		if lowest == "" {
			out[level] = nil
			continue
		}
		value := lowest
		out[level] = &value
	}
	return out
}

// serveModels 返回别名表里的逻辑模型清单。只列对外名字, 不暴露上游真实 id;
// 每个名字带上按候选聚合的模型参数 (推理等级 / 上下文 / 输出上限)。
func (a *App) serveModels(w http.ResponseWriter) {
	a.mu.RLock()
	names := make([]string, 0, len(a.aliases))
	for n := range a.aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	data := make([]modelEntry, 0, len(names))
	for _, n := range names {
		data = append(data, modelEntry{ID: n, Object: "model", OwnedBy: "tollgate", modelMeta: a.aggregateMeta(a.aliases[n])})
	}
	a.mu.RUnlock()
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
		a.stickyClear(w, logical)
		return
	}
	a.stickySet(w, logical, upstream, model)
}

// stickyClear 清除某逻辑名的偏好 (不带 upstream 时的路径)。
func (a *App) stickyClear(w http.ResponseWriter, logical string) {
	n := a.sticky.reset(logical)
	a.evAdd("sticky", logical, "", "", "sticky clr "+strconv.Itoa(n))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"cleared": n})
}

// stickySet 在候选链里定位 (上游, 模型) 并写入偏好下标。
func (a *App) stickySet(w http.ResponseWriter, logical, upstream, model string) {
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
	a.evAdd("sticky", logical, at.Upstream, at.Model, "sticky set")
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

// forceUnlockGroup 只清某上游一个记账组的额度锁 (group 写作 def 时是默认组): 不动
// 该上游的模型锁与并发试探锁。上游不存在或该组没锁时返回 0。同样落盘并记一条人工
// 解锁事件。
func (a *App) forceUnlockGroup(upID, group string) int {
	a.mu.RLock()
	c := a.cands[upID]
	a.mu.RUnlock()
	if c == nil {
		return 0
	}
	if group == "def" {
		group = defaultGroup
	}
	n := c.clearGroupLock(group)
	a.saveGateState()
	a.evAdd("unlock", "", upID, "", "unlock(manual "+strconv.Itoa(n)+")")
	return n
}

// serveUnlock 处理 POST /unlock (调试用强制解锁)。可用 query 过滤:
//   - upstream + model: 只清该 上游/模型 的熔断锁与退避;
//   - upstream + group: 只清该上游该记账组的额度锁 (`def` = 默认组), 不动模型锁;
//   - upstream 单独: 清该上游的全部锁 (记账组 + 模型 + 并发试探);
//   - 都不给: 清掉所有候选的所有锁与退避。
//
// group 单独给没有意义 (不知道哪个上游), 直接拒绝, 不要退化成清全部。
func (a *App) serveUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST /unlock")
		return
	}
	q := r.URL.Query()
	up, model, group := q.Get("upstream"), q.Get("model"), q.Get("group")
	var total int
	switch {
	case up == "" && model == "" && group != "":
		writeError(w, http.StatusBadRequest, "invalid_request_error", "group requires upstream or model")
		return
	case up != "" && model == "" && group != "":
		total = a.forceUnlockGroup(up, group)
	default:
		total = a.forceUnlock(up, model, group)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"cleared": total})
}
