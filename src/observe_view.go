// observe_view.go: 观测面快照 (GET /state) 与推送循环。

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"
)

// flowActives 拷贝当前进行中的请求 (按开始时间升序): 每段尝试的 Segs 已是秒, 已结算;
// 当前阶段的进入时刻填进 CurAt (JSON 里的 stageAt 是内部记账, 不出原始时刻)。
func (a *App) flowActives() []flow {
	a.flowMu.Lock()
	out := make([]flow, 0, len(a.flows))
	for _, f := range a.flows {
		v := *f
		v.Tries = make([]flowTry, len(f.Tries))
		copy(v.Tries, f.Tries)
		for i := range v.Tries {
			v.Tries[i].Segs = append([]float64(nil), f.Tries[i].Segs...)
		}
		if n := len(v.Tries); n > 0 && v.Tries[n-1].Cur >= 0 && !f.stageAt.IsZero() {
			v.Tries[n-1].CurAt = f.stageAt.UnixMilli()
		}
		out = append(out, v)
	}
	a.flowMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime.Before(out[j].StartTime) })
	return out
}

// candViews 读每个候选上游的瞬时调度状态 (逐个短锁, 不嵌套)。
func (a *App) candViews() []candView {
	a.mu.RLock()
	cands := make([]*cand, 0, len(a.cands))
	for _, c := range a.cands {
		cands = append(cands, c)
	}
	// groupOrder 整体重建 (applyConfig 换新 map), 拿引用后在锁外读是安全的。
	order := a.groupOrder
	a.mu.RUnlock()
	now := time.Now()
	out := make([]candView, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.view(order[c.id], now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// view 锁住一个候选读出瞬时状态: 队列计数 + 汇总锁 + 记账组 + 模型视角。
func (c *cand) view(groups []string, now time.Time) candView {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := candView{
		ID: c.id, Queue: c.queued(), InFlight: c.inFlight, SoftCap: c.softCap,
		Probing: c.probing,
	}
	c.aggLocks(&v, now)
	v.Groups = c.groupView(groups, now)
	v.Models = c.modelView(now)
	if left := c.concLock.left(now); left > 0 {
		v.ConcMs = msCeil(left)
	}
	if left := c.nextSend.Sub(now); left > 0 {
		v.NextMs = msCeil(left)
	}
	return v
}

// aggLocks 汇总该上游最坏的连错 / 退避 / 组额度锁; 额度锁按组记, 上游行给出最久的
// 那一把 (老消费方还看这个汇总值)。
func (c *cand) aggLocks(v *candView, now time.Time) {
	for _, g := range c.modelGates {
		if g.fails > v.ErrStrk {
			v.ErrStrk = g.fails
		}
		if left := g.pausedUntil.Sub(now); left > 0 && msCeil(left) > v.PausedMs {
			v.PausedMs = msCeil(left)
		}
	}
	for _, l := range c.groupLocks {
		if left := l.left(now); left > 0 && msCeil(left) > v.LockMs {
			v.LockMs, v.LockWhy = msCeil(left), l.reason
		}
	}
}

// groupView 是记账列的快照: 声明过的组都列出来 (没锁也列, 组名要当表头), 无 models
// 的上游只有默认组。
func (c *cand) groupView(names []string, now time.Time) []candGroup {
	if len(names) == 0 {
		names = []string{defaultGroup}
	}
	out := make([]candGroup, 0, len(names))
	for _, name := range names {
		g := candGroup{Name: name}
		if l, ok := c.groupLocks[name]; ok {
			g.LockMs = msCeil(l.left(now))
		}
		out = append(out, g)
	}
	return out
}

// modelView 是模型视角的快照: 只列真正进过队列的 上游/模型 (队列已空但曾经有过请求的
// 也算), 不按逻辑名或配置里的候选清单铺开; 按模型 id 排序。
func (c *cand) modelView(now time.Time) []candModelLock {
	models := map[string]*candModelLock{}
	ensure := func(m string) *candModelLock {
		if x := models[m]; x != nil {
			return x
		}
		x := &candModelLock{Model: m}
		models[m] = x
		return x
	}
	for m := range c.seen {
		ensure(m)
	}
	for m, q := range c.modelQ {
		ensure(m).Queue = len(q)
	}
	for model, g := range c.modelGates {
		x := ensure(model)
		x.Fails = g.fails
		if left := g.pausedUntil.Sub(now); left > 0 {
			x.PausedMs = msCeil(left)
		}
		if left := g.lock.left(now); left > 0 {
			x.LeftMs, x.Why = msCeil(left), g.lock.reason
		}
	}
	out := make([]candModelLock, 0, len(models))
	for _, x := range models {
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

func (a *App) listenAddr() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cfg == nil {
		return ""
	}
	return a.cfg.Listen
}

// stickySnapshot 返回各逻辑名当前的粘性偏好候选 (观测面展示用, TUI 第 1 页标记
// 每组的 *)。aliases 取引用后在锁外读 (applyConfig 整体换新 map), 与 candViews 同口径。
// 有请求进行中的逻辑名倒计时按设定值满显 (窗口从请求结束才起算); 锁序: flowMu 与
// a.mu 先后各自取用不嵌套, 再进 sticky 的锁, 与 applyConfig 的 a.mu -> sticky.mu 一致。
func (a *App) stickySnapshot() map[string]stickyView {
	a.mu.RLock()
	aliases := a.aliases
	a.mu.RUnlock()
	if a.sticky == nil {
		return nil
	}
	return a.sticky.pairs(aliases, a.gate.stickyTTL(), time.Now(), a.inflightLogicals())
}

// inflightLogicals 返回当前有进行中请求的逻辑名集合: 这些逻辑名的粘性失效窗口还没开始
// (最近一次请求尚未结束), pairs 里它们的倒计时按设定值 (TTL) 满显, 不动也不递减。
func (a *App) inflightLogicals() map[string]bool {
	a.flowMu.Lock()
	defer a.flowMu.Unlock()
	out := make(map[string]bool, len(a.flows))
	for _, f := range a.flows {
		out[f.Logical] = true
	}
	return out
}

// snapshot 组装一份观测载荷 (since = TUI 上次拿到的 MaxSeq)。
func (a *App) snapshot(since uint64) pulse {
	evs, max := a.eventsSince(since)
	now := time.Now()
	return pulse{
		Ver:    viewVer,
		Listen: a.listenAddr(), Now: now.Format(time.RFC3339Nano),
		Active: a.flowActives(), Cands: a.candViews(), Events: evs, MaxSeq: max,
		Sticky: a.stickySnapshot(),
	}
}

// serveState 是只读观测面 (GET /state): 进行中的请求 + 候选调度状态 + 新增事件。
// 不给客户端用, 给运维面 (TUI) 看。
func (a *App) serveState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET /state")
		return
	}
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(a.snapshot(since))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	w.Write(b)
}

// startPulseLoop 每秒把观测载荷写到日志同目录的 tui-pulse.json: -tui 同进程时
// TUI 直接读文件, 不起 HTTP 轮询。原子替换 (临时文件 + rename), 读侧永远看到完整 JSON。
func (a *App) startPulseLoop(path string) {
	if path == "" {
		return
	}
	go func() {
		var last uint64
		seen := map[string]bool{} // 候选 -> 上一拍是否处于锁/退避中 (检测恢复用)
		for {
			p := a.snapshot(last)
			if b, err := json.Marshal(p); err == nil {
				tmp := path + ".tmp"
				if os.WriteFile(tmp, b, 0o644) == nil {
					os.Rename(tmp, path)
				}
			}
			// 锁/退避到期是"没有请求也可能发生"的状态变化, 只能靠轮询发现。
			// seen 里存的是"上一拍是否处于锁/退避中", 从有到无=恢复。
			for _, c := range p.Cands {
				busy := c.LockMs > 0 || c.PausedMs > 0
				if prev, ok := seen[c.ID]; ok && prev && !busy {
					a.evAdd("unlock", "", c.ID, "", "unlock")
				}
				seen[c.ID] = busy
			}
			last = p.MaxSeq
			time.Sleep(time.Second)
		}
	}()
}
