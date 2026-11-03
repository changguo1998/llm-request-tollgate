// app_test.go: applyConfig 热加载超时三项值。

package main

import (
	"testing"
	"time"
)

// TestReloadTimeouts 验证三项超时随 applyConfig 热更新 (transport 不重建):
// connect / stream_idle 走原子值, response_header 写回 transport 字段。
func TestReloadTimeouts(t *testing.T) {
	app := newTestAppCfg(t, &Config{
		Timeouts: Timeouts{ConnectMs: 1000, ResponseHeaderMs: 2000, StreamIdleMs: 3000},
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://127.0.0.1:1/v1",
			AuthHeader: "Authorization", Key: "k"}},
	})
	want := func(connectNs, streamNs int64, rh time.Duration) {
		t.Helper()
		if got := app.connectNs.Load(); got != connectNs {
			t.Fatalf("connectNs = %d, want %d", got, connectNs)
		}
		if got := app.streamIdlNs.Load(); got != streamNs {
			t.Fatalf("streamIdlNs = %d, want %d", got, streamNs)
		}
		if got := app.transport.ResponseHeaderTimeout; got != rh {
			t.Fatalf("response_header = %v, want %v", got, rh)
		}
	}
	want(1000*int64(time.Millisecond), 3000*int64(time.Millisecond), 2000*time.Millisecond)

	// 热加载换三值: transport 指针不变 (连接池复用), 三个值即更新。
	oldRT := app.transport
	app.applyConfig(&Config{
		Timeouts: Timeouts{ConnectMs: 5000, ResponseHeaderMs: 6000, StreamIdleMs: 7000},
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://127.0.0.1:1/v1",
			AuthHeader: "Authorization", Key: "k"}},
		Aliases: map[string][][2]string{"m1": {{"u1", "real-1"}}},
	})
	if app.transport != oldRT {
		t.Fatalf("热加载不应重建 transport")
	}
	want(5000*int64(time.Millisecond), 7000*int64(time.Millisecond), 6000*time.Millisecond)
}
