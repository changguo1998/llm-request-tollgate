// serve.go: /v1/chat/completions 与 /v1/responses 的主链路。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.serveSide(w, r) {
		return
	}
	isResponses := r.URL.Path == clientPathResponses
	if r.Method != http.MethodPost || (!isResponses && r.URL.Path != clientPath) {
		writeError(w, http.StatusNotFound, "not_found",
			"unsupported path/method: "+r.Method+" "+r.URL.Path)
		return
	}
	s := newServeState(a, w, r, isResponses)
	if s == nil {
		return
	}
	s.run()
}

// serveSide 处理不参与转发的旁路请求, 返回是否已经应答。
func (a *App) serveSide(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/state":
		a.serveState(w, r)
	case "/v1/unlock":
		a.serveUnlock(w, r)
	case "/v1/models":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
				"only GET /v1/models")
			return true
		}
		a.serveModels(w)
	default:
		return false
	}
	return true
}

// attempt 是一个候选: 上游 + 真实模型 + 该候选自己的调度状态。
type attempt struct {
	up    *UpstreamCfg
	mod   string
	group string
	cd    *cand
}

// serveState 是一笔转发请求的全部上下文: 请求本身、观测账目, 以及候选链累积的统计。
type serveState struct {
	app      *App
	w        http.ResponseWriter
	r        *http.Request
	obs      *flowObs
	flowID   uint64
	reqStart time.Time

	logical     string
	payload     map[string]any
	stream      bool
	isResponses bool
	upPath      string
	gate        GateCfg
	attempts    []attempt

	// 候选链累积账目。
	semMs        int64         // 各候选等信号量时长之和
	queueMs      int64         // 各候选排队时长之和
	gateReject   string        // 非空 = 被闸门挡下没发出去: locked | sem_timeout | queue_timeout | canceled
	gateLeft     time.Duration // 被挡下时最长的锁剩余时间 (Retry-After 用)
	heldCand     *cand         // 已拿到资格、正在服务的那个候选
	heldKind     slotKind
	failed       int
	lockedSkips  int
	backoffSkips int

	// 最近一次失败与最终服务的候选。
	resp       *http.Response
	servedUp   *UpstreamCfg
	servedMod  string
	sentAt     time.Time // 被动时延采样: 该候选开始等待的时刻
	lastErr    error
	lastStatus int
	lastBody   []byte
	lastHdr    http.Header
	lastUpID   string
	lastMod    string

	finalStatus int
	finalTtfb   int64
}

// newServeState 读请求体并建好观测面; 请求本身不合法时直接应答并返回 nil。
func newServeState(a *App, w http.ResponseWriter, r *http.Request, isResponses bool) *serveState {
	upPath := upstreamPath
	if isResponses {
		upPath = upstreamPathResponses
	}
	// 请求体整体缓冲, 才能改写 model 与 stream_options。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"cannot read request body: "+err.Error())
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"request body is not valid JSON")
		return nil
	}
	logical, _ := payload["model"].(string)
	if logical == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing model")
		return nil
	}
	stream := false
	if st, ok := payload["stream"].(bool); ok {
		stream = st
	}
	// 观测面 (TUI): 这笔请求进"进行中"表, 每换候选/进出闸门刷一次展示状态, 结束时出表。
	reqStart := time.Now()
	flowID := a.flowStart(logical, r.Header.Get("X-Tollgate-Source"), r.URL.Path, stream)
	return &serveState{
		app: a, w: w, r: r,
		obs:         &flowObs{a: a, id: flowID, logical: logical, slowMs: 30000},
		flowID:      flowID,
		reqStart:    reqStart,
		logical:     logical,
		payload:     payload,
		stream:      stream,
		isResponses: isResponses,
		upPath:      upPath,
	}
}

// run 走完候选链: 挑候选 -> 逐候选尝试 -> 失败应答或转发响应。
func (s *serveState) run() {
	defer func() {
		// 退出顺序: 先按实际结局刷新展示状态, 再把这笔从"进行中"表里摘掉。
		s.obs.finish(s.finalStatus, s.finalTtfb)
		s.app.flowEnd(s.flowID)
	}()
	if !s.pickAttempts() {
		return
	}
	// 连接在响应发完才归还槽位 (ok 只对试探有意义)。
	defer func() {
		if s.heldCand != nil {
			s.heldCand.finish(s.heldKind, false)
		}
	}()
	s.attemptLoop()
	if s.servedUp == nil {
		s.writeFailure()
		return
	}
	s.forward()
}

// pickAttempts 一把 RLock 拿齐候选表, 循环里不再读可被热加载替换的 map。
func (s *serveState) pickAttempts() bool {
	s.app.mu.RLock()
	alias, aliasOK := s.app.aliases[s.logical]
	s.gate = s.app.gate
	s.attempts = make([]attempt, 0, len(alias))
	for _, tgt := range alias {
		u, ok := s.app.upstreams[tgt.Upstream]
		if !ok {
			continue
		}
		s.attempts = append(s.attempts, attempt{u, tgt.Model, tgt.Group, s.app.cands[u.ID]})
	}
	s.app.mu.RUnlock()
	if !aliasOK {
		writeError(s.w, http.StatusNotFound, "model_not_found", "unknown model: "+s.logical)
		return false
	}
	if len(s.attempts) == 0 {
		writeError(s.w, http.StatusNotFound, "model_not_found",
			"model "+s.logical+" has no configured upstream")
		return false
	}
	return true
}

// baseLog 填所有日志行共有的请求侧字段。
func (s *serveState) baseLog() logRec {
	return logRec{
		Ts: time.Now().Format(time.RFC3339Nano), Logical: s.logical,
		Path: s.r.URL.Path, Stream: s.stream,
		Queued: s.queueMs >= 5, QueueMs: s.queueMs, SemMs: s.semMs,
		Client: s.r.RemoteAddr, UA: s.r.Header.Get("User-Agent"),
		Source:    s.r.Header.Get("X-Tollgate-Source"),
		Fallbacks: s.failed, LockSkips: s.lockedSkips, BackoffSkips: s.backoffSkips,
	}
}
