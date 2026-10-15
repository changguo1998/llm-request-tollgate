package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 观测面: 进行中的请求进出、事件序号只取增量、快照字段齐全 (TUI 就靠这些字段画表)。
func TestViewFlowAndEvents(t *testing.T) {
	a, err := newApp(&Config{Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl")})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()

	id := a.flowStart("m1", "agent-x", "/v1/chat/completions", true)
	a.flowStage(id, "up1", "real-m1", stageSend, "退避 3s", 2)
	act := a.flowActives()
	if len(act) != 1 {
		t.Fatalf("进行中的请求应有一条, 得到 %d", len(act))
	}
	f := act[0]
	if f.ID != id || f.Logical != "m1" || f.Source != "agent-x" || !f.Stream {
		t.Errorf("进行中请求的字段不对: %+v", f)
	}
	if f.Up != "up1" || f.Model != "real-m1" || f.Attempt != 2 || stageName(f) != "退避 3s" {
		t.Errorf("候选状态不对: %+v (stage=%q)", f, stageName(f))
	}

	// 事件游标: 只取增量, 且序号单调。
	a.evAdd("lock", "", "up1", "", "锁 quota 10m0s")
	since := a.snapshot(0).MaxSeq
	if since != 1 {
		t.Fatalf("游标应为 1, 得到 %d", since)
	}
	a.evAdd("error", "m1", "up1", "", "status 500")
	evs, max := a.eventsSince(since)
	if len(evs) != 1 || evs[0].Kind != "error" || max != 2 {
		t.Fatalf("增量事件不对: %+v max=%d", evs, max)
	}
	if n, _ := a.eventsSince(max); len(n) != 0 {
		t.Fatalf("游标之后不该再有事件: %+v", n)
	}

	// 结束即出表, 事件流不受影响。
	a.flowEnd(id)
	if got := len(a.flowActives()); got != 0 {
		t.Fatalf("结束后进行中应为 0, 得到 %d", got)
	}
}

// 候选快照: 队列 / 锁 / 退避都能被 TUI 看到 (只读, 不改调度状态)。
func TestCandViewSnapshot(t *testing.T) {
	a, err := newApp(&Config{
		Listen: "127.0.0.1:0", LogPath: filepath.Join(t.TempDir(), "req.jsonl"),
		Upstreams: []UpstreamCfg{{ID: "up1", BaseURL: "http://127.0.0.1:1/v1"}},
	})
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer func() {
		if a.logW != nil {
			a.logW.Close()
		}
	}()
	c := a.cands["up1"]
	if c == nil {
		t.Fatal("上游没有调度器")
	}
	c.lock(lockAll, "quota", 10*time.Minute)
	c.mu.Lock()
	c.inFlight, c.softCap = 2, 3
	// 两层队列各放一笔: 观测面的 Queue 是两段之和。
	c.enqueueLocked(&ticket{c: c, model: "real-m1"}, queueFlow)
	c.enqueueLocked(&ticket{c: c, model: "real-m1"}, queueModel)
	c.mu.Unlock()

	vs := a.candViews()
	if len(vs) != 1 {
		t.Fatalf("应有 1 个候选, 得到 %d", len(vs))
	}
	v := vs[0]
	if v.ID != "up1" || v.InFlight != 2 || v.SoftCap != 3 || v.Queue != 2 {
		t.Errorf("候选计数不对: %+v", v)
	}
	if v.LockMs <= 0 || v.LockWhy != "quota" {
		t.Errorf("整家锁没被看到: %+v", v)
	}
}

// 终端宽度: ANSI 序列不占列, CJK 占两列 (窄终端丢列与表格对齐都靠它)。
func TestTUIWidthAndTrunc(t *testing.T) {
	if got := tuiWidth("\x1b[32m3\x1b[0m"); got != 1 {
		t.Errorf("ANSI 序列应不占列, 得到 %d", got)
	}
	if got := tuiWidth("正常"); got != 4 {
		t.Errorf("中文应按两列算, 得到 %d", got)
	}
	if got := tuiTrunc("\x1b[31m25\x1b[0m", 4); got != "\x1b[31m25\x1b[0m" {
		t.Errorf("4 列够放 2 字符 + 颜色, 不该截断: %q", got)
	}
	if got := tuiTrunc("abcdef", 4); got != "abc~" {
		t.Errorf("超长应截断并留 ~: %q", got)
	}
	// 行总宽不超过画布: 这是"中文不再挤歪表"的回归点。
	c := &tuiCanvas{cols: 20}
	c.line("\x1b[1m" + strings.Repeat("宽", 30) + "\x1b[0m")
	if w := tuiWidth(strings.SplitN(c.b.String(), "\r\n", 2)[0]); w > 20 {
		t.Errorf("画布行宽超限: %d", w)
	}
}

// routes/events 两页的数据: 归档 + 当前日志一起读, 窗口外的记录不算。
func TestTUIPollLogAcrossArchive(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "req.jsonl")
	arch := filepath.Join(dir, "req-2020-01-01.jsonl")
	now := time.Now()
	line := func(up, model string, status, ttfb int, ts time.Time) string {
		return `{"ts":"` + ts.Format(time.RFC3339Nano) + `","logical":"m1","upstream":"` + up +
			`","model":"` + model + `","path":"/v1/chat/completions","stream":true,"status":` +
			itoa(status) + `,"fallbacks":1,"ttfb_ms":` + itoa(ttfb) + `,"client":"127.0.0.1:1"}` + "\n"
	}
	if err := os.WriteFile(cur, []byte(line("up1", "real1", 200, 10, now)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arch, []byte(line("up2", "real2", 500, 12000, now.Add(-time.Hour))), 0o644); err != nil {
		t.Fatal(err)
	}
	// 窗口外: 不该进表
	old := line("up3", "real3", 200, 10, now.Add(-48*time.Hour))
	f, _ := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(old)
	f.Close()

	m := newTUIModel("config.json")
	m.logPath, m.window = cur, 6*time.Hour
	m.scannedFiles = map[string]bool{}
	m.logPos, m.logRest = map[string]int64{}, map[string][]byte{}
	m.pollLog()

	if len(m.routes) != 2 {
		t.Fatalf("应有 2 条路由 (当前 + 归档), 得到 %d: %+v", len(m.routes), m.routes)
	}
	var gotArch, gotCur bool
	for _, r := range m.routes {
		switch r.Upstream {
		case "up2":
			gotArch = r.Errs == 1 && r.TtfbN == 1
		case "up1":
			gotCur = r.Upstream == "up1" && r.Model == "real1"
		}
	}
	if !gotArch || !gotCur {
		t.Errorf("聚合不对: arch=%v cur=%v", gotArch, gotCur)
	}
	// 事件: 归档里的 500 与慢请求都要在
	var hasErr, hasSlow bool
	for _, e := range m.events {
		hasErr = hasErr || e.Kind == "error"
		hasSlow = hasSlow || e.Kind == "slow"
	}
	if !hasErr || !hasSlow {
		t.Errorf("事件流缺少 error/slow: %+v", m.events)
	}
	// 增量: 再追加一行, 只应新增一条记录 (不重复计旧行)
	f, _ = os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(line("up1", "real1", 200, 10, now))
	f.Close()
	before := m.scanned
	m.pollLog()
	if m.scanned != before+1 {
		t.Errorf("增量读应只多 1 条, 从 %d 到 %d", before, m.scanned)
	}
}

func itoa(n int) string { return itoa64(int64(n)) }
