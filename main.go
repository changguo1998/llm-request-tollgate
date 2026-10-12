// llm-request-tollgate: 自研单文件 OpenAI 兼容代理 (设计文档 docs/design.md)。
//
// 单进程内含七件事 (设计 §4.1):
//  1. 别名表 (白名单 + 路由): 逻辑模型名 -> [(上游, 真实模型 id), ...],
//     未命中的模型名直接返 404, 不猜上游 (D2)。
//  2. 按上游分组 (v1 一个上游一把 key, "上游+凭据" 等价于上游记录) 的
//     令牌桶 (速率 + 突发) 与信号量 (并发), 桶空排队, 超 queue_wait_ms 返
//     429 带 Retry-After (D3, F3)。
//  3. 手动转发: 认证头替换 (D1), 删 Accept-Encoding 让 Transport 协商并
//     透明解压 (设计 §4.9.1), 不加 X-Forwarded-For (F9)。
//  4. 用量提取: 强写 stream_options.include_usage, 流末 usage chunk / 非流式
//     响应体 usage; 缺 usage 按 F7 字符口径兜底并计数。
//  5. 预算判定点: 第一版恒放行只留位置 (D5)。
//  6. 响应整形: 非流 / 流式 chunk 里的 model 字段改回请求的逻辑名 (O9)。
//  7. 对外面: GET /v1/models 返回逻辑名清单 (O5); 日志记 source 标识 (O3);
//     -summary 汇总命令 (O2); -check 配置自检 + deploy/ 常驻与自检脚本 (O1)。
//
// 配置: config.local.json (gitignored, 软链到 ~/fff/config), SIGHUP 热加载别名与上游 (F1);
// key: 支持 key_env 字段只存环境变量名, 真 key 启动/热加载时从环境变量取, 不落盘。
// 日志: JSONL 追加写, 逐笔明细 (F5, F8)。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// 客户端打在代理上的路径 (D4: 第一版只做 /v1/chat/completions)。
	clientPath = "/v1/chat/completions"
	// 上游路径: base_url 自带 /v1 前缀 (设计 §4.8), 不能再拼一次。
	upstreamPath = "/chat/completions"
)

// ---------- 配置 ----------

type Timeouts struct {
	ConnectMs        int `json:"connect_ms"`
	ResponseHeaderMs int `json:"response_header_ms"`
	StreamIdleMs     int `json:"stream_idle_ms"`
	QueueWaitMs      int `json:"queue_wait_ms"`
}

type Limits struct {
	RatePerSec  float64 `json:"rate_per_sec"`
	Burst       float64 `json:"burst"`
	Concurrency int     `json:"concurrency"`
}

type UpstreamCfg struct {
	ID           string            `json:"id"`
	BaseURL      string            `json:"base_url"`
	AuthHeader   string            `json:"auth_header"`
	Key          string            `json:"key"`
	KeyEnv       string            `json:"key_env,omitempty"`
	Headers      map[string]string `json:"headers"`
	IncludeUsage bool              `json:"include_usage"`
	Limits       Limits            `json:"limits"`
}

type Config struct {
	Listen    string                 `json:"listen"`
	LogPath   string                 `json:"log_path"`
	Timeouts  Timeouts               `json:"timeouts"`
	Upstreams []UpstreamCfg          `json:"upstreams"`
	Aliases   map[string][][2]string `json:"aliases"`
}

// ---------- 限速: 令牌桶 + 信号量, 按上游分组 ----------

type Limiter struct {
	rate, burst, tokens float64
	last                time.Time
	sem                 chan struct{}
	mu                  sync.Mutex
}

func newLimiter(l Limits) *Limiter {
	cap := l.Concurrency
	if cap < 1 {
		cap = 1
	}
	burst := l.Burst
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:   math.Max(0, l.RatePerSec),
		burst:  burst,
		tokens: burst,
		sem:    make(chan struct{}, cap),
	}
}

// takeToken 在 ctx 超时内拿到一个令牌, 拿不到返回 false。
func (l *Limiter) takeToken(ctx context.Context) bool {
	for {
		l.mu.Lock()
		now := time.Now()
		if l.last.IsZero() {
			l.last = now
		}
		l.tokens = math.Min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return true
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		t := time.NewTimer(need)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

func (l *Limiter) acquireSem(ctx context.Context) bool {
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *Limiter) releaseSem() { <-l.sem }

// ---------- 用量 ----------

type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// openaiUsage 是上游响应里的原始字段名 (OpenAI 兼容口径)。
type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func toUsage(u *openaiUsage) *Usage {
	if u == nil {
		return nil
	}
	return &Usage{Prompt: u.PromptTokens, Completion: u.CompletionTokens, Total: u.TotalTokens}
}

// estimateTokens 按 F7 口径粗估: 非 ASCII 1 token/字符, 其余 4 字符 1 token。
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

// estimateUsageFromBody 取响应体生成文本, 用于缺 usage 时的 F7 兜底估算。
func estimateUsageFromBody(buf []byte) int {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return 0
	}
	n := 0
	for _, c := range resp.Choices {
		n += estimateTokens(c.Message.Content)
	}
	return n
}

// rewriteModel 把 JSON 里顶层 "model" 字段的值改成 logical, 其余字节原样保留 (O9)。
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

// ---------- 日志 (JSONL) ----------

type logRec struct {
	Ts          string `json:"ts"`
	Logical     string `json:"logical"`
	Upstream    string `json:"upstream"`
	Model       string `json:"model"`
	Path        string `json:"path"`
	Stream      bool   `json:"stream"`
	Status      int    `json:"status"`
	Usage       *Usage `json:"usage,omitempty"`
	Missing     bool   `json:"missing_usage"`
	Queued      bool   `json:"queued"`
	QueueMs     int64  `json:"queue_ms"`
	Client      string `json:"client"`
	UA          string `json:"ua,omitempty"`
	Source      string `json:"source,omitempty"`
	Err         string `json:"err,omitempty"`
	BudgetAllow bool   `json:"budget_allow"`
}

// ---------- 代理主体 ----------

type App struct {
	mu        sync.RWMutex
	cfg       *Config
	upstreams map[string]*UpstreamCfg
	aliases   map[string][]aliasTarget
	limiters  map[string]*Limiter
	transport *http.Transport
	streamIdl time.Duration

	logMu  sync.Mutex
	logW   *os.File
	logger *log.Logger
}

type aliasTarget struct {
	Upstream string
	Model    string
}

func newApp(c *Config) (*App, error) {
	a := &App{
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		limiters:  map[string]*Limiter{},
	}
	a.applyConfig(c)
	if err := a.openLog(); err != nil {
		return nil, err
	}
	return a, nil
}

// applyConfig 重建上游表与别名表; 限速器按上游 id 保留, 不因热加载被重置。
func (a *App) applyConfig(c *Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = c
	a.streamIdl = time.Duration(c.Timeouts.StreamIdleMs) * time.Millisecond
	upstreams := map[string]*UpstreamCfg{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		upstreams[u.ID] = u
		if _, ok := a.limiters[u.ID]; !ok {
			a.limiters[u.ID] = newLimiter(u.Limits)
		}
	}
	a.upstreams = upstreams
	aliases := map[string][]aliasTarget{}
	for name, list := range c.Aliases {
		for _, pair := range list {
			aliases[name] = append(aliases[name], aliasTarget{pair[0], pair[1]})
		}
	}
	a.aliases = aliases
	// 超时参数启动时一次性固化为 Transport (热加载不重建连接池)。
	if a.transport == nil {
		connect := time.Duration(c.Timeouts.ConnectMs) * time.Millisecond
		a.transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				d := net.Dialer{Timeout: connect}
				conn, err := d.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				// 每次读都重设 idle 死线, 实现流空闲超时 (F2)。
				return &idleConn{Conn: conn, idle: a.streamIdl}, nil
			},
			ResponseHeaderTimeout: time.Duration(c.Timeouts.ResponseHeaderMs) * time.Millisecond,
		}
	}
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

func (a *App) openLog() error {
	path := a.cfg.LogPath
	if path == "" {
		return nil
	}
	if i := strings.LastIndex(path, "/"); i >= 0 && i > 0 {
		if err := os.MkdirAll(path[:i], 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	a.logW = f
	a.logger = log.New(f, "", 0)
	return nil
}

func (a *App) logReq(rec logRec) {
	if a.logger == nil {
		return
	}
	b, _ := json.Marshal(rec)
	a.logMu.Lock()
	defer a.logMu.Unlock()
	a.logger.Println(string(b))
}

// ---------- HTTP 处理 ----------

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// 客户端可能带来的认证头一律删除, 换成上游配置的凭据 (D1)。
var clientAuthHeaders = []string{
	"Authorization", "Api-Key", "X-Api-Key", "api-key", "X-Goog-Api-Key", "Proxy-Authorization",
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"only GET /v1/models")
			return
		}
		a.serveModels(w)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != clientPath {
		writeError(w, http.StatusNotFound, "not_found",
			"unsupported path/method: "+r.Method+" "+r.URL.Path)
		return
	}
	// 请求体整体缓冲, 才能改写 model 与 stream_options (设计 §4.9.4)。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"cannot read request body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"request body is not valid JSON")
		return
	}
	logical, _ := payload["model"].(string)
	if logical == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing model")
		return
	}
	stream := false
	if s, ok := payload["stream"].(bool); ok {
		stream = s
	}

	// 别名解析 -> 选上游 -> 限速器 -> 排队预算, 一把 RLock 拿齐快照。
	a.mu.RLock()
	alias, aliasOK := a.aliases[logical]
	var chosen *UpstreamCfg
	realModel := ""
	for _, cand := range alias {
		if u, ok := a.upstreams[cand.Upstream]; ok {
			chosen = u
			realModel = cand.Model
			break
		}
	}
	var lim *Limiter
	queueWait := time.Duration(a.cfg.Timeouts.QueueWaitMs) * time.Millisecond
	if chosen != nil {
		lim = a.limiters[chosen.ID]
	}
	a.mu.RUnlock()

	if !aliasOK {
		writeError(w, http.StatusNotFound, "model_not_found", "unknown model: "+logical)
		return
	}
	if chosen == nil {
		writeError(w, http.StatusNotFound, "model_not_found",
			"model "+logical+" has no configured upstream")
		return
	}

	// 预算判定点: 第一版恒放行 (D5), 只留位置与日志字段。
	budgetAllow := true

	// 限速闸: 信号量 + 令牌桶, 共用 queue_wait 排队预算 (F3/F4)。
	// 注意: 信号量一旦拿到就必须释放, 否则后面请求永远排队 (429 泄漏)。
	gateCtx, cancel := context.WithTimeout(r.Context(), queueWait)
	defer cancel()
	gateStart := time.Now()
	if !lim.acquireSem(gateCtx) {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"upstream busy: queue wait exceeded "+queueWait.String())
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: chosen.ID, Model: realModel, Path: r.URL.Path, Stream: stream,
			Status: http.StatusTooManyRequests, Missing: true, Client: r.RemoteAddr,
			UA: r.Header.Get("User-Agent"), Source: r.Header.Get("X-Tollgate-Source"),
			Err: "rate_limited", BudgetAllow: budgetAllow,
		})
		return
	}
	defer lim.releaseSem()
	if !lim.takeToken(gateCtx) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(queueWait/time.Second))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error",
			"upstream busy: queue wait exceeded "+queueWait.String())
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: chosen.ID, Model: realModel, Path: r.URL.Path, Stream: stream,
			Status: http.StatusTooManyRequests, Missing: true, Client: r.RemoteAddr,
			UA: r.Header.Get("User-Agent"), Source: r.Header.Get("X-Tollgate-Source"),
			Err: "rate_limited", BudgetAllow: budgetAllow,
		})
		return
	}
	queueMs := time.Since(gateStart).Milliseconds()
	queued := queueMs >= 5

	// 改写请求体: 真实模型 id + 强制 include_usage (设计 §4.3)。
	payload["model"] = realModel
	if chosen.IncludeUsage && stream {
		so, _ := payload["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
			payload["stream_options"] = so
		}
		so["include_usage"] = true
	}
	newBody, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "rewrite body failed")
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method,
		chosen.BaseURL+upstreamPath, bytes.NewReader(newBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	outReq.ContentLength = int64(len(newBody))
	prepareOutboundHeaders(outReq.Header, r.Header, chosen)

	resp, err := a.transport.RoundTrip(outReq)
	if err != nil {
		// 首字节之前失败: 真实错误原样交给 agent (设计 §2.2)。
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		a.logReq(logRec{
			Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
			Upstream: chosen.ID, Model: realModel, Path: r.URL.Path, Stream: stream,
			Status: http.StatusBadGateway, Missing: true, Queued: queued, QueueMs: queueMs,
			Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"),
			Source: r.Header.Get("X-Tollgate-Source"),
			Err:    err.Error(), BudgetAllow: budgetAllow,
		})
		return
	}
	defer resp.Body.Close()

	// 转发响应头 (F9): 除 hop-by-hop 外原样透传。
	dst := w.Header()
	for k, vv := range resp.Header {
		if isHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	var usage *Usage
	missing := true
	status := resp.StatusCode
	if stream {
		w.WriteHeader(status)
		dst.Del("Content-Length")
		usage, missing = relayStream(w, resp.Body, logical)
	} else {
		buf, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			status = http.StatusBadGateway
			buf = []byte(`{"error":{"message":` + strconv.Quote(rerr.Error()) + `}}`)
			dst.Set("Content-Type", "application/json")
		}
		buf = rewriteModel(buf, logical)
		usage = extractUsage(buf)
		missing = usage == nil
		if missing {
			est := estimateUsageFromBody(buf)
			usage = &Usage{Completion: est, Total: est}
		}
		dst.Set("Content-Length", strconv.Itoa(len(buf)))
		w.WriteHeader(status)
		w.Write(buf)
	}

	a.logReq(logRec{
		Ts: time.Now().Format(time.RFC3339Nano), Logical: logical,
		Upstream: chosen.ID, Model: realModel, Path: r.URL.Path, Stream: stream,
		Status: status, Usage: usage, Missing: missing, Queued: queued, QueueMs: queueMs,
		Client: r.RemoteAddr, UA: r.Header.Get("User-Agent"), Source: r.Header.Get("X-Tollgate-Source"), BudgetAllow: budgetAllow,
	})
}

// relayStream 按行透传 SSE, 期间提取流末 usage (设计 §4.9.2)。
// 流未以 [DONE] 正常结束时, 发一个 SSE error 事件后关流, 不伪造正常结束 (F6)。
// 返回提取到的 usage 与是否缺失 (配合 F7 兜底)。
func relayStream(w http.ResponseWriter, body io.Reader, logical string) (*Usage, bool) {
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var usage *Usage
	est := 0
	done := false

	write := func(s string) {
		if _, err := io.WriteString(w, s); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				done = true
			} else if data != "" {
				var chunk struct {
					Usage   *openaiUsage `json:"usage"`
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if usage == nil && chunk.Usage != nil {
						usage = toUsage(chunk.Usage)
					}
					for _, c := range chunk.Choices {
						est += estimateTokens(c.Delta.Content)
					}
					// O9: 块内 model 改回逻辑名 (只改该字段, 其余字节原样)。
					if data2 := rewriteModel([]byte(data), logical); string(data2) != data {
						line = "data: " + string(data2)
					}
				}
			}
		}
		write(line + "\n")
	}
	if !done {
		// 流未正常结束: 用流内约定形态表达错误 (设计 §2.2 / F6)。
		write("event: error\n")
		write(`data: {"error":{"message":"upstream stream ended unexpectedly","type":"upstream_error"}}` + "\n\n")
	}
	missing := usage == nil
	if missing {
		usage = &Usage{Completion: est, Total: est}
	}
	return usage, missing
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
		dst.Set(up.AuthHeader, up.Key)
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

// serveModels 返回别名表里的逻辑模型清单 (O5)。只列对外名字, 不暴露上游真实 id。
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

// ---------- 入口 ----------

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.LogPath == "" {
		c.LogPath = "var/requests.jsonl"
	}
	// key_env: 配置只存环境变量名, 真 key 启动/热加载时从环境取, 不落盘。
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.KeyEnv != "" {
			v, ok := os.LookupEnv(u.KeyEnv)
			if !ok || v == "" {
				return nil, fmt.Errorf("upstream %s: env %s not set", u.ID, u.KeyEnv)
			}
			u.Key = v
		}
	}
	return &c, nil
}

// sumRow 是 -summary 的每行聚合 (O2)。
type sumRow struct {
	Key     string
	Up      string
	Reqs    int
	Errs    int
	Queued  int
	Missing int
	P, C, T int
}

// runSummary 汇总 JSONL 请求日志 (O2): 按 逻辑名+来源 分组, 来源取 source, 缺省用 ua。
func runSummary(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("summary: %v", err)
	}
	defer f.Close()
	rows := map[string]*sumRow{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec logRec
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Logical == "" {
			continue
		}
		src := rec.Source
		if src == "" {
			src = rec.UA
		}
		key := rec.Logical + "\x00" + src
		r, ok := rows[key]
		if !ok {
			r = &sumRow{Key: rec.Logical + " / " + src}
			rows[key] = r
			order = append(order, key)
		}
		if r.Up == "" {
			r.Up = rec.Upstream
		}
		r.Reqs++
		if rec.Status >= 400 {
			r.Errs++
		}
		if rec.Queued {
			r.Queued++
		}
		if rec.Missing {
			r.Missing++
		}
		if rec.Usage != nil {
			r.P += rec.Usage.Prompt
			r.C += rec.Usage.Completion
			r.T += rec.Usage.Total
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatalf("summary: %v", err)
	}
	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].T > rows[order[j]].T
	})
	fmt.Printf("%-34s %-10s %6s %5s %5s %5s %8s %8s %8s\n",
		"logical/source", "upstream", "reqs", "err", "queued", "miss", "prompt", "completion", "total")
	tot := &sumRow{Key: "TOTAL"}
	for _, k := range order {
		r := rows[k]
		fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
			r.Key, r.Up, r.Reqs, r.Errs, r.Queued, r.Missing, r.P, r.C, r.T)
		tot.Reqs += r.Reqs
		tot.Errs += r.Errs
		tot.Queued += r.Queued
		tot.Missing += r.Missing
		tot.P += r.P
		tot.C += r.C
		tot.T += r.T
	}
	fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
		tot.Key, "", tot.Reqs, tot.Errs, tot.Queued, tot.Missing, tot.P, tot.C, tot.T)
}

func main() {
	cfgPath := flag.String("config", "config.local.json", "config file path")
	summary := flag.String("summary", "", "aggregate JSONL request log and exit")
	check := flag.Bool("check", false, "validate config and exit")
	flag.Parse()

	if *summary != "" {
		runSummary(*summary)
		return
	}
	if *check {
		cfg, err := loadConfig(*cfgPath)
		if err != nil {
			log.Fatalf("config invalid: %v", err)
		}
		log.Printf("config OK: listen=%s log_path=%s upstreams=%d aliases=%d",
			cfg.Listen, cfg.LogPath, len(cfg.Upstreams), len(cfg.Aliases))
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	app, err := newApp(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	// SIGHUP 热加载别名与上游 (F1)。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		for range ch {
			c, err := loadConfig(*cfgPath)
			if err != nil {
				log.Printf("reload failed: %v", err)
				continue
			}
			app.applyConfig(c)
			log.Printf("config reloaded from %s", *cfgPath)
		}
	}()

	log.Printf("llm-request-tollgate listening on %s (config %s)", cfg.Listen, *cfgPath)
	if err := http.ListenAndServe(cfg.Listen, app); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
