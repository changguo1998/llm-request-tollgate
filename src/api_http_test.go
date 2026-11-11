// api_http_test.go: HTTP 层的集成用例 —— 路由 (支持 / 不支持的路径与方法)、请求体校验、
// 别名解析失败、/v1/models 与 /state 观测面、/v1/responses 的路径与用量注入差异。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errOf 解出 writeError 的响应体 ({"error":{"message","type","code"}})。
func errOf(t *testing.T, body []byte) (code, msg string) {
	t.Helper()
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v (%s)", err, body)
	}
	return e.Error.Code, e.Error.Message
}

// 路由与请求体校验: 不支持的路径/方法 404, 坏 JSON 与缺 model 400, 别名解析不了 404。
func TestHTTPRouteAndPayloadErrors(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("这些请求不该打到上游: %s %s", r.Method, r.URL.Path)
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{}, nil)
	// 别名指向没有配置的上游: 与"未知模型"分开报错。
	app.mu.Lock()
	app.aliases["orphan"] = []aliasTarget{{Upstream: "no-such-upstream", Model: "real-x"}}
	app.mu.Unlock()
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
		msg    string
	}{
		{"GET 聊天接口", http.MethodGet, "/v1/chat/completions", "", http.StatusNotFound,
			"not_found", "unsupported path/method"},
		{"未知路径", http.MethodPost, "/v1/completions", `{"model":"m1"}`, http.StatusNotFound,
			"not_found", "unsupported path/method"},
		{"坏 JSON", http.MethodPost, "/v1/chat/completions", "{not json",
			http.StatusBadRequest, "invalid_request_error", "not valid JSON"},
		{"缺 model", http.MethodPost, "/v1/chat/completions", `{"stream":false}`,
			http.StatusBadRequest, "invalid_request_error", "missing model"},
		{"未知模型", http.MethodPost, "/v1/chat/completions", `{"model":"nope"}`,
			http.StatusNotFound, "model_not_found", "unknown model: nope"},
		{"别名指向不存在的上游", http.MethodPost, "/v1/chat/completions", `{"model":"orphan"}`,
			http.StatusNotFound, "model_not_found", "has no configured upstream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var resp *http.Response
			var err error
			if c.method == http.MethodGet {
				resp, err = http.Get(proxy.URL + c.path)
			} else {
				resp, err = http.Post(proxy.URL+c.path, "application/json", strings.NewReader(c.body))
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != c.status {
				t.Fatalf("%s 应返回 %d, 得到 %d (%s)", c.path, c.status, resp.StatusCode, b)
			}
			code, msg := errOf(t, b)
			if code != c.code || !strings.Contains(msg, c.msg) {
				t.Errorf("错误体应为 code=%q 且含 %q, 得到 code=%q msg=%q", c.code, c.msg, code, msg)
			}
		})
	}
}

// 模型名解析失败也要有收尾事件: 状态码 404, 不能误报 200。
func TestUnknownModelEmitsErrorEvent(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1/v1", GateCfg{}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	evs, _ := app.eventsSince(0)
	if len(evs) == 0 {
		t.Fatal("应有一条收尾事件")
	}
	last := evs[len(evs)-1]
	if last.Kind != "error" || last.Msg != "parse → fail(404)" {
		t.Errorf("未知模型应记 404 收尾事件, 得到 %+v", last)
	}
	if last.Req == 0 || last.Logical != "nope" {
		t.Errorf("收尾事件应挂请求编号与逻辑名, 得到 %+v", last)
	}
}

// 观测面走完整 HTTP 服务: /v1/models 只给逻辑名 (GET 之外 405), /state 给得出观测载荷。
func TestHTTPServeModelsAndState(t *testing.T) {
	app := newTestApp(t, "http://127.0.0.1:1/v1", GateCfg{}, nil)
	app.flowStart("m1", "agent", "/v1/chat/completions", true)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models 应返回 200, 得到 %d (%s)", resp.StatusCode, b)
	}
	var list struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("模型清单不是合法 JSON: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "m1" {
		t.Errorf("模型清单不对: %+v", list)
	}
	if strings.Contains(string(b), "real-1") {
		t.Errorf("不该暴露真实模型 id: %s", b)
	}

	// POST 不是 /v1/models 的方法。
	code, msg := postJSON(t, proxy.URL+"/v1/models", `{}`)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models 应返回 405, 得到 %d", code)
	}
	if c, m := errOf(t, msg); c != "method_not_allowed" || !strings.Contains(m, "only GET") {
		t.Errorf("405 的错误体不对: code=%q msg=%q", c, m)
	}

	// /state: 观测面 (ver + 进行中的请求)。
	resp, err = http.Get(proxy.URL + "/state?since=0")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/state 应返回 200, 得到 %d", resp.StatusCode)
	}
	var p pulse
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("观测载荷不是合法 JSON: %v", err)
	}
	if p.Ver != viewVer || len(p.Active) != 1 {
		t.Errorf("观测载荷不对: ver=%q active=%+v", p.Ver, p.Active)
	}
}

// /v1/models 的模型参数按逻辑名的候选聚合: reasoning 任一候选声明即 true,
// thinkingLevelMap 取交集 (不支持的等级给 null), contextWindow / maxTokens 取最小值。
func TestServeModelsAggregatesMeta(t *testing.T) {
	level := func(s string) *string { return &s }
	cfg := &Config{
		Upstreams: []UpstreamCfg{
			{ID: "u1", BaseURL: "http://127.0.0.1:1/v1", Key: "k", Models: []ModelCfg{{
				ID: "a", Alias: "m1", Priority: 10, Reasoning: true,
				ThinkingLevelMap: map[string]*string{"low": level("low"), "high": level("high"), "medium": nil},
				ContextWindow:    100000, MaxTokens: 8000,
			}}},
			{ID: "u2", BaseURL: "http://127.0.0.1:1/v1", Key: "k", Models: []ModelCfg{{
				ID: "b", Alias: "m1", Priority: 5,
				ThinkingLevelMap: map[string]*string{"low": level("low"), "high": nil, "medium": level("medium")},
				ContextWindow:    200000, MaxTokens: 4000,
			}}},
		},
		Aliases: map[string][][2]string{"m1": {{"u1", "a"}, {"u2", "b"}}},
	}
	app := newTestAppCfg(t, cfg)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list struct {
		Data []modelEntry `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("模型清单不是合法 JSON: %v", err)
	}
	if len(list.Data) != 1 {
		t.Fatalf("模型清单应只有 m1, 得到 %+v", list.Data)
	}
	got := list.Data[0]
	if got.ID != "m1" || !got.Reasoning || got.ContextWindow != 100000 || got.MaxTokens != 4000 {
		t.Errorf("聚合参数不对: %+v", got)
	}
	want := map[string]*string{"low": level("low"), "high": nil, "medium": nil}
	if len(got.ThinkingLevelMap) != len(want) {
		t.Fatalf("推理等级表应取交集, 得到 %+v", got.ThinkingLevelMap)
	}
	for k, v := range want {
		gv := got.ThinkingLevelMap[k]
		if (v == nil) != (gv == nil) || (v != nil && *v != *gv) {
			t.Errorf("等级 %s 应为 %v, 得到 %v", k, v, gv)
		}
	}
}

// /v1/responses 走自己的上游路径, 模型同样换回真实 id; 它没有 stream_options,
// 即便上游配了 include_usage 也不能注入。
func TestResponsesPathAndNoStreamOptions(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"response.completed","response":{"model":"real-1",`+
			`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	code, body := postJSON(t, proxy.URL+"/v1/responses",
		`{"model":"m1","input":"ping","stream":false}`)
	if code != http.StatusOK {
		t.Fatalf("转发应成功, 得到 %d (%s)", code, body)
	}
	if gotPath != upstreamPathResponses {
		t.Errorf("上游路径应为 %q, 得到 %q", upstreamPathResponses, gotPath)
	}
	if gotBody["model"] != "real-1" {
		t.Errorf("上游应收到真实模型 id, 得到 %v", gotBody["model"])
	}
	if _, ok := gotBody["stream_options"]; ok {
		t.Errorf("/v1/responses 不该注入 stream_options: %v", gotBody)
	}
	if strings.Contains(string(body), "real-1") || !strings.Contains(string(body), `"m1"`) {
		t.Errorf("响应里的模型应换回逻辑名, 得到 %s", body)
	}
}

// /v1/responses 流式: 事件里嵌在 response 下的 model 换成逻辑名, SSE 其它字节原样透传。
func TestResponsesStreamRewritesModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"model":"real-1"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"model":"real-1",`+
			`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}))
	defer up.Close()
	app := newTestApp(t, up.URL, GateCfg{}, nil)
	proxy := httptest.NewServer(app)
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m1","input":"ping","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("流式转发应成功, 得到 %d (%s)", resp.StatusCode, b)
	}
	out := string(b)
	if strings.Contains(out, "real-1") {
		t.Errorf("流式事件里不该出现真实模型 id: %s", out)
	}
	if n := strings.Count(out, `"model":"m1"`); n != 2 {
		t.Errorf("两处 model 都应换回逻辑名, 实际 %d 处: %s", n, out)
	}
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "[DONE]") &&
		!strings.Contains(out, "response.completed") {
		t.Errorf("SSE 结构应原样透传: %s", out)
	}
}
