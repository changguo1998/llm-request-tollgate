// unlock_test.go: 调试用强制解锁 (POST /unlock)。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	c.lockModel("real-1", "breaker", time.Minute, "")
	c.lockGroup("g1", "quota", time.Minute, "")
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
	c.lockModel("real-1", "breaker", time.Minute, "")
	c.lockModel("real-2", "breaker", time.Minute, "")
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

// 解锁单个记账组 (第 1 页上游表的记账格): 只清那一把额度锁, 模型锁 / 并发锁 / 其它组
// 都不动; group=def 是默认组。不给 group 时 (forceUnlock 的旧路径) 仍清整条上游。
func TestUnlockSingleGroup(t *testing.T) {
	app := newTestAppCfg(t, &Config{
		Upstreams: []UpstreamCfg{{ID: "u1", BaseURL: "http://example.invalid/v1", AuthHeader: "Authorization", Key: "k"}},
		Aliases:   map[string][][2]string{"m1": {{"u1", "real-1"}}},
	})
	c := app.cands["u1"]
	c.lockModel("real-1", "breaker", time.Minute, "")
	c.lockGroup("g1", "quota", time.Minute, "")
	c.lockGroup(defaultGroup, "quota", time.Minute, "")
	c.lockConcurrency("concurrency", time.Minute)

	post := func(q string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/unlock"+q, nil)
		app.serveUnlock(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200, 得到 %d: %s", rec.Code, rec.Body.String())
		}
		var v struct {
			Cleared int `json:"cleared"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
		return v.Cleared
	}
	groupLocked := func(name string) bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, ok := c.groupLocks[name]
		return ok
	}

	if n := post("?upstream=u1&group=g1"); n != 1 {
		t.Fatalf("只清 g1 应为 1, 得到 %d", n)
	}
	if groupLocked("g1") {
		t.Error("g1 应已解锁")
	}
	if !groupLocked(defaultGroup) {
		t.Error("默认组不该被 g1 的解锁清掉")
	}
	if locked, _, _ := c.locked("real-1", ""); !locked {
		t.Error("模型锁不该被组解锁清掉")
	}
	c.mu.Lock()
	conc := c.concLock.reason
	c.mu.Unlock()
	if conc == "" {
		t.Error("并发试探锁不该被组解锁清掉")
	}

	if n := post("?upstream=u1&group=def"); n != 1 {
		t.Fatalf("group=def 应清默认组 (1), 得到 %d", n)
	}
	if groupLocked(defaultGroup) {
		t.Error("默认组应已解锁")
	}
	if locked, _, _ := c.locked("real-1", ""); !locked {
		t.Error("默认组解锁也不该动模型锁")
	}

	// group 单独给: 不知道哪个上游, 应拒绝而不是退化成清全部。
	rec := httptest.NewRecorder()
	app.serveUnlock(rec, httptest.NewRequest(http.MethodPost, "/unlock?group=g1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("只给 group 应 400, 得到 %d", rec.Code)
	}
	if locked, _, _ := c.locked("real-1", ""); !locked {
		t.Error("400 不该动任何锁")
	}

	if n := post("?upstream=u1"); n < 2 {
		t.Fatalf("不给 group 时仍应清整条上游 (模型锁 + 并发锁), 得到 %d", n)
	}
	if locked, _, _ := c.locked("real-1", ""); locked {
		t.Error("整条上游解锁后模型锁应没了")
	}
}
