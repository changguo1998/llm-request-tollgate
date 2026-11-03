// unlock_test.go: 调试用强制解锁 (POST /v1/unlock)。

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestForceUnlock(t *testing.T) {
	app := newTestAppCfg(t, &Config{
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://example.invalid/v1", AuthHeader: "Authorization", Key: "k"}},
		Aliases:   map[string][][2]string{"m1": {{"u1", "real-1"}}},
	})
	c := app.cands["u1"]
	c.lockModel("real-1", "breaker", time.Minute)
	c.lockGroup("g1", "quota", time.Minute)
	c.lockConcurrency("concurrency", time.Minute)
	if locked, _, _ := c.locked("real-1", "g1"); !locked {
		t.Fatal("先应处于锁中")
	}

	n := app.forceUnlock("", "", "")
	if n < 3 {
		t.Fatalf("应清掉 3 把锁 (模型/组/并发), 得到 %d", n)
	}
	if locked, _, _ := c.locked("real-1", "g1"); locked {
		t.Fatal("强制解锁后不应再锁")
	}
	c.mu.Lock()
	left := len(c.modelGates["real-1"].lock.reason) + len(c.groupLocks) + len(c.concLock.reason)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("清锁后内部不应残留锁状态")
	}

	// 落盘里也不应再有锁。
	b, err := os.ReadFile(app.gatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"reason"`) {
		t.Fatalf("gate-state 不应再有锁: %s", b)
	}
}

func TestForceUnlockFilter(t *testing.T) {
	app := newTestAppCfg(t, &Config{
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://example.invalid/v1", AuthHeader: "Authorization", Key: "k"}},
		Aliases:   map[string][][2]string{"m1": {{"u1", "real-1"}}},
	})
	c := app.cands["u1"]
	c.lockModel("real-1", "breaker", time.Minute)
	c.lockModel("real-2", "breaker", time.Minute)
	if n := app.forceUnlock("u1", "real-1", ""); n != 1 {
		t.Fatalf("只清 real-1 应为 1, 得到 %d", n)
	}
	if locked, _, _ := c.locked("real-1", ""); locked {
		t.Fatal("real-1 应已解锁")
	}
	if locked, _, _ := c.locked("real-2", ""); !locked {
		t.Fatal("real-2 应仍锁着")
	}
}
