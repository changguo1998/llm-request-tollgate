// observe_view_test.go: 候选快照的汇总口径 (拆分 candViews 前的特征测试)。

package main

import (
	"path/filepath"
	"testing"
	"time"
)

// 候选快照按上游 id 排序; 汇总的锁取该上游所有记账组里最久的一把 (原因跟着它走);
// 模型视角按模型 id 排序, 每个进过队列的模型都带上自己的队列数。
func TestCandViewsLongestGroupLockAndOrder(t *testing.T) {
	a, err := newApp(&Config{
		Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl"),
		Upstreams: []UpstreamCfg{
			{ID: "u2", BaseURL: "http://127.0.0.1:1/v1",
				Models: []ModelCfg{{ID: "z-model"}}},
			{ID: "u1", BaseURL: "http://127.0.0.1:1/v1",
				Models: []ModelCfg{{ID: "real-a"}, {ID: "real-b"}}},
		},
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()
	c := a.cands["u1"]
	if c == nil {
		t.Fatal("上游没有调度器")
	}
	c.lockGroup("g1", "quota", 5*time.Minute, "")
	c.lockGroup("g2", "breaker", 10*time.Minute, "")
	c.mu.Lock()
	c.enqueueLocked(&ticket{c: c, model: "real-b"}, queueModel)
	c.enqueueLocked(&ticket{c: c, model: "real-a"}, queueModel)
	c.mu.Unlock()

	vs := a.candViews()
	if len(vs) != 2 || vs[0].ID != "u1" || vs[1].ID != "u2" {
		t.Fatalf("快照应按上游 id 排序: %+v", vs)
	}
	v := vs[0]
	if v.LockMs < int64(9*time.Minute/time.Millisecond) || v.LockWhy != "breaker" {
		t.Errorf("汇总锁应取最久的组锁 (10min / breaker): %+v", v)
	}
	// 没声明过记账组的模型: 空组列表回落默认组, 汇总锁仍按 groupLocks 算。
	if len(v.Models) != 2 || v.Models[0].Model != "real-a" || v.Models[1].Model != "real-b" {
		t.Fatalf("模型视角应按模型 id 排序: %+v", v.Models)
	}
	for _, md := range v.Models {
		if md.Queue != 1 {
			t.Errorf("%s 的队列数应为 1: %+v", md.Model, md)
		}
	}
}
