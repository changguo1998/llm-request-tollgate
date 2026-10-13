package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testGate 是调度测试用的门控参数: 节拍与等待都调小, 免得测试跑几秒。
func testGate() GateCfg {
	return normGate(GateCfg{IntervalMs: 20, SemWaitMs: 300, QueueWaitMs: 300})
}

// TestNormGateDefaults 核对缺省值逐项等于设计 §5.5.4。
func TestNormGateDefaults(t *testing.T) {
	g := normGate(GateCfg{})
	if g.IntervalMs != 500 || g.SemWaitMs != 20000 || g.QueueWaitMs != 10000 ||
		g.ConcurrencyLockMs != 3600000 || g.QuotaLockMs != 600000 || g.BreakerLockMs != 300000 {
		t.Fatalf("缺省值不符: %+v", g)
	}
	want := []int{10000, 20000, 30000}
	if len(g.RetryBackoffMs) != len(want) {
		t.Fatalf("retry_backoff_ms 缺省值不符: %v", g.RetryBackoffMs)
	}
	for i, v := range want {
		if g.RetryBackoffMs[i] != v {
			t.Fatalf("retry_backoff_ms 缺省值不符: %v", g.RetryBackoffMs)
		}
	}
	// 配置里写了的字段不能被缺省值盖掉。
	g2 := normGate(GateCfg{IntervalMs: 20, RetryBackoffMs: []int{1}})
	if g2.IntervalMs != 20 || len(g2.RetryBackoffMs) != 1 || g2.RetryBackoffMs[0] != 1 {
		t.Fatalf("已有值被覆盖: %+v", g2)
	}
}

// TestQueueFIFOAndPacing 核对按到达顺序放行, 且两笔放行的间隔不低于节拍。
func TestQueueFIFOAndPacing(t *testing.T) {
	c := newCand("u1")
	g := testGate()
	g.IntervalMs = 50
	c.softCap = 8 // 这一案只看队列, 不让并发上限挡路

	type res struct {
		n  int
		at time.Time
		ok bool
		wh string
	}
	ch := make(chan res, 3)
	for n := 1; n <= 3; n++ {
		go func(n int) {
			time.Sleep(time.Duration(n) * 15 * time.Millisecond) // 错开入队, 让顺序确定
			_, why, _, _, ok := c.acquire(context.Background(), g)
			ch <- res{n, time.Now(), ok, why}
		}(n)
	}
	got := make([]res, 0, 3)
	for i := 0; i < 3; i++ {
		select {
		case r := <-ch:
			got = append(got, r)
		case <-time.After(2 * time.Second):
			t.Fatal("等放行超时")
		}
	}
	for i, r := range got {
		if !r.ok {
			t.Fatalf("第 %d 笔没拿到发送资格: %s", i+1, r.wh)
		}
		if r.n != i+1 {
			t.Fatalf("没有按到达顺序放行: %+v", got)
		}
	}
	for i := 1; i < len(got); i++ {
		if gap := got[i].at.Sub(got[i-1].at); gap < 45*time.Millisecond {
			t.Fatalf("放行间隔 %s 低于节拍 50ms", gap)
		}
	}
}

// TestSoftCapStartProbeAndGrow 核对并发上限起点 1, 满了先试探, 试探成功 +1,
// 且同一时刻只允许一笔试探在飞。
func TestSoftCapStartProbeAndGrow(t *testing.T) {
	c := newCand("u1")
	g := testGate()
	if c.softCap != 1 {
		t.Fatalf("并发上限起点应为 1, 实为 %d", c.softCap)
	}
	if _, why, _, _, ok := c.acquire(context.Background(), g); !ok {
		t.Fatalf("第一笔应拿到槽位: %s", why)
	}
	kind, why, _, _, ok := c.acquire(context.Background(), g)
	if !ok || kind != slotProbe {
		t.Fatalf("并发满时应试探: kind=%v ok=%v why=%s", kind, ok, why)
	}
	if _, why3, _, _, ok3 := c.acquire(context.Background(), g); ok3 || why3 != "sem_timeout" {
		t.Fatalf("试探在飞时不该再试探, 应等信号量: ok=%v why=%s", ok3, why3)
	}
	c.finish(slotProbe, true) // 试探成功
	if c.softCap != 2 {
		t.Fatalf("试探成功后上限应为 2, 实为 %d", c.softCap)
	}
	kind, why, _, _, ok = c.acquire(context.Background(), g)
	if !ok || kind != slotCapped {
		t.Fatalf("上限放宽后应能正常拿到槽位: kind=%v ok=%v why=%s", kind, ok, why)
	}
}

// TestConcurrencyLockBlocksProbe 核对并发锁只锁试探: 锁内并发满就等信号量。
func TestConcurrencyLockBlocksProbe(t *testing.T) {
	c := newCand("u1")
	g := testGate()
	if _, _, _, _, ok := c.acquire(context.Background(), g); !ok {
		t.Fatal("第一笔应拿到槽位")
	}
	c.lockConcurrency("concurrency", 500*time.Millisecond)
	if _, why, _, _, ok := c.acquire(context.Background(), g); ok || why != "sem_timeout" {
		t.Fatalf("并发锁生效时不该试探: ok=%v why=%s", ok, why)
	}
	if lock, left, _ := func() (bool, time.Duration, string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.concLock.active(time.Now()), c.concLock.left(time.Now()), c.concLock.reason
	}(); !lock || left <= 0 {
		t.Fatal("并发锁应处于生效状态")
	}
}

// TestClassifyRules 核对状态码与关键词都能命中, 且大小写不敏感; 没规则一律兜底。
func TestClassifyRules(t *testing.T) {
	if got := (&UpstreamCfg{}).classify(429, []byte("rate limited")); got != classOther {
		t.Fatalf("空规则表应走通用兜底, 实为 %v", got)
	}
	u := &UpstreamCfg{Errors: &ErrRules{
		Concurrency: &ErrRule{Status: []int{429}, Keywords: []string{"concurrency limit"}},
		Quota:       &ErrRule{Keywords: []string{"insufficient balance"}},
	}}
	if got := u.classify(429, nil); got != classConcurrency {
		t.Fatalf("状态码命中应判并发, 实为 %v", got)
	}
	if got := u.classify(400, []byte("Concurrency Limit Exceeded")); got != classConcurrency {
		t.Fatalf("关键词不区分大小写应判并发, 实为 %v", got)
	}
	if got := u.classify(403, []byte("insufficient balance")); got != classQuota {
		t.Fatalf("关键词命中应判额度, 实为 %v", got)
	}
	if got := u.classify(500, []byte("slow down")); got != classOther {
		t.Fatalf("都不命中应走通用兜底, 实为 %v", got)
	}
}

// TestProviderLockKeepsLaterExpiry 核对额度与熔断先后触发时取更晚的到期时间。
func TestProviderLockKeepsLaterExpiry(t *testing.T) {
	c := newCand("u1")
	c.lockProvider("quota", 300*time.Millisecond)
	c.lockProvider("breaker", 50*time.Millisecond)
	locked, left, reason := c.locked()
	if !locked || reason != "quota" || left <= 200*time.Millisecond {
		t.Fatalf("应保留更晚的到期时间与原因: locked=%v left=%s reason=%s", locked, left, reason)
	}
	c.lockProvider("breaker", time.Second)
	locked, left, reason = c.locked()
	if !locked || reason != "breaker" || left <= 900*time.Millisecond {
		t.Fatalf("更晚的锁应生效: locked=%v left=%s reason=%s", locked, left, reason)
	}
}

// TestProviderLockReleasesQueued 核对加锁时排队者会退出队列 (由上层换下一个候选)。
func TestProviderLockReleasesQueued(t *testing.T) {
	c := newCand("u1")
	g := normGate(GateCfg{IntervalMs: 500, SemWaitMs: 300, QueueWaitMs: 2000})
	if _, _, _, _, ok := c.acquire(context.Background(), g); !ok {
		t.Fatal("第一笔应拿到槽位")
	}
	done := make(chan string, 1)
	go func() {
		_, why, _, _, ok := c.acquire(context.Background(), g)
		if ok {
			done <- "ok"
			return
		}
		done <- why
	}()
	time.Sleep(30 * time.Millisecond) // 让它进队列并卡在节拍上
	start := time.Now()
	c.lockProvider("quota", 200*time.Millisecond)
	select {
	case why := <-done:
		if why != "locked" {
			t.Fatalf("排队者应在加锁后退出队列, 实为 %s", why)
		}
	case <-time.After(time.Second):
		t.Fatal("加锁后排队者没有被释放")
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("排队者退出太慢: %s", el)
	}
}

// TestBackoffSequence 核对连续失败按次数逐级加长暂停, 第 4 次熔断, 成功后清零。
func TestBackoffSequence(t *testing.T) {
	c := newCand("u1")
	g := testGate() // retry_backoff_ms 未给, 走缺省 [10000, 20000, 30000]
	prev := time.Duration(0)
	for n := 1; n <= 3; n++ {
		if breaker := c.recordOther(g); breaker {
			t.Fatalf("第 %d 次连续失败不该进熔断", n)
		}
		c.mu.Lock()
		pause := time.Until(c.pausedUntil)
		c.mu.Unlock()
		if want := g.backoff(n); pause < want-100*time.Millisecond || pause > want {
			t.Fatalf("第 %d 次失败的暂停 %s 与配置 %s 不符", n, pause, want)
		}
		if pause <= prev {
			t.Fatalf("暂停没有逐级加长: %s -> %s", prev, pause)
		}
		prev = pause
	}
	if !c.recordOther(g) {
		t.Fatal("第 4 次连续失败应进熔断")
	}
	c.recordSuccess()
	if c.errStreak != 0 {
		t.Fatalf("成功后连续错误计数应清零, 实为 %d", c.errStreak)
	}
}

// TestGateStateRoundTrip 核对供应商锁落盘后能在重启时恢复, 过期或未知上游的不恢复。
func TestGateStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	a := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	a.cands["u1"].lockProvider("quota", 10*time.Minute)
	a.saveGateState()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("锁状态没有落盘: %v", err)
	}

	restarted := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	restarted.loadGateState()
	locked, left, reason := restarted.cands["u1"].locked()
	if !locked || reason != "quota" || left < 9*time.Minute {
		t.Fatalf("重启后应恢复未到期的锁: locked=%v left=%s reason=%s", locked, left, reason)
	}

	stale := gateState{Locks: map[string]gateLockRec{
		"u1":    {UntilMs: time.Now().Add(-time.Minute).UnixMilli(), Reason: "breaker"},
		"ghost": {UntilMs: time.Now().Add(time.Hour).UnixMilli(), Reason: "quota"},
	}}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	again := &App{cands: map[string]*cand{"u1": newCand("u1")}, gatePath: path}
	again.loadGateState()
	if locked, _, _ := again.cands["u1"].locked(); locked {
		t.Fatal("已过期的锁不该恢复")
	}
}
