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
		c.mu.Lock()
		v := candView{
			ID: c.id, Queue: c.queued(), InFlight: c.inFlight, SoftCap: c.softCap,
			Probing: c.probing,
		}
		for _, g := range c.modelGates {
			if g.fails > v.ErrStrk {
				v.ErrStrk = g.fails
			}
			if left := g.pausedUntil.Sub(now); left > 0 && msCeil(left) > v.PausedMs {
				v.PausedMs = msCeil(left)
			}
		}
		// 额度锁按组记, 上游行给出最久的那一把 (老消费方还看这个汇总值)。
		for _, l := range c.groupLocks {
			if left := l.left(now); left > 0 && msCeil(left) > v.LockMs {
				v.LockMs, v.LockWhy = msCeil(left), l.reason
			}
		}
		// 记账列: 声明过的组都列出来 (没锁也列, 组名要当表头), 无 models 的上游只有默认组。
		names := order[c.id]
		if len(names) == 0 {
			names = []string{defaultGroup}
		}
		for _, name := range names {
			g := candGroup{Name: name}
			if l, ok := c.groupLocks[name]; ok {
				g.LockMs = msCeil(l.left(now))
			}
			v.Groups = append(v.Groups, g)
		}
		// 模型视角: 只列真正进过队列的 上游/模型 (队列已空但曾经有过请求的也算),
		// 不按逻辑名或配置里的候选清单铺开。
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
		for _, x := range models {
			v.Models = append(v.Models, *x)
		}
		sort.Slice(v.Models, func(i, j int) bool { return v.Models[i].Model < v.Models[j].Model })
		if left := c.concLock.left(now); left > 0 {
			v.ConcMs = msCeil(left)
		}
		if left := c.nextSend.Sub(now); left > 0 {
			v.NextMs = msCeil(left)
		}
		c.mu.Unlock()
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
func (a *App) stickySnapshot() map[string]stickyView {
	a.mu.RLock()
	aliases := a.aliases
	a.mu.RUnlock()
	if a.sticky == nil {
		return nil
	}
	return a.sticky.pairs(aliases)
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
