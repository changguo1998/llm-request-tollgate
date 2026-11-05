// sticky.go: 逻辑名级上游偏好 —— 记住最近成功服务的候选, 让同一逻辑名的请求粘在
// 同一上游, 保持那家的缓存热度; 只有持续真实失败才迁移, 锁 / 退避 / 排队超时造成的
// 顶班不计 (那是临时不可用, 候选恢复后流量自动回流)。

package main

import (
	"sync"
	"time"
)

// stickyFailoverStreak 是「真实失败顶班」连续几次才迁移偏好。顶班 = 偏好候选真实报错
// (上游非 200 / 退避重试到熔断) 后, 备用候选顶上并成功。锁 / 退避预检 / 排队超时造成
// 的顶班不计入; 长期锁 (低性能 / 额度) 造成的顶班直接迁移。
const stickyFailoverStreak = 2

// stickyLongLock 是「直接视为候选失效, 顶班即迁移」的锁原因: 这类锁要么整组、要么
// 好几分钟起跳, 期间不该让每笔请求都先撞它一次再走备用。
func stickyLongLock(reason string) bool {
	return reason == "quota" || reason == lowPerfReason
}

// stickyPref 是一个逻辑名的偏好状态。
type stickyPref struct {
	pref   int       // 偏好候选在 alias 列表里的下标 (0 = 配置第一候选)
	streak int       // 连续真实失败顶班成功次数
	moved  time.Time // 最近一次偏好迁移时刻 (目前只留作展示)
}

// sticky 是全部逻辑名的偏好表; 独立互斥, 不占调度热路径的主锁, 也不阻塞热加载。
type sticky struct {
	mu    sync.Mutex
	prefs map[string]*stickyPref
}

func newSticky() *sticky {
	return &sticky{prefs: map[string]*stickyPref{}}
}

// prefIdx 返回该逻辑名的偏好下标; 无记录或越界时返回 0 (配置第一候选)。
func (s *sticky) prefIdx(logical string, n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok || p.pref < 0 || p.pref >= n {
		return 0
	}
	return p.pref
}

// served 一笔请求成功服务后记录偏好。
//   - servedIdx: 成功候选在 alias 列表里的下标
//   - prefIdx: 本笔请求开头的偏好下标 (即第一个被尝试的候选)
//   - blocked: 偏好候选被锁 / 退避预检 / 排队超时顶班 (临时不可用)
//   - longLock: 偏好候选被长期锁 (低性能 / 额度) 顶班 (应视为候选失效)
//
// 规则:
//   - 成功在偏好候选上: 顶班 / 失败记忆清零
//   - 偏好候选被长期锁顶班后成功别处: 偏好直接迁移, 不再连撞
//   - 偏好候选被临时不可用顶班后成功别处: 不计, 等候选恢复后流量自动回流
//   - 偏好候选真实失败顶班后成功别处: 记一次, 连续到阈才迁移
func (s *sticky) served(logical string, servedIdx, prefIdx int, blocked, longLock bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		// 无记录: 偏好隐含为 0 (配置第一候选), 首笔顶班与显式偏好同口径累计,
		// 也免得开局一笔成功就把偏好从配置序永久撬走。
		p = &stickyPref{}
		s.prefs[logical] = p
	}
	if servedIdx == prefIdx || servedIdx == p.pref {
		p.streak = 0
		return
	}
	if longLock {
		p.pref, p.streak, p.moved = servedIdx, 0, time.Now()
		return
	}
	if blocked {
		return // 临时不可用: 偏好不动, 候选恢复后流量回流
	}
	p.streak++
	if p.streak >= stickyFailoverStreak {
		p.pref, p.streak, p.moved = servedIdx, 0, time.Now()
	}
}

// set 人工置一次偏好 (纯内存, 不落盘): 立即生效, 但自动迁移规则照旧 —— 之后偏好候选
// 连续真实失败或被长期锁挡下时, 仍会迁到顶班候选。下标由调用方按当前 alias 解析。
func (s *sticky) set(logical string, idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		p = &stickyPref{}
		s.prefs[logical] = p
	}
	p.pref, p.streak, p.moved = idx, 0, time.Now()
}

// reset 清掉偏好 (logical 为空 = 清全部), 返回清掉的条数; 之后回落配置首候选。
func (s *sticky) reset(logical string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if logical != "" {
		if _, ok := s.prefs[logical]; !ok {
			return 0
		}
		delete(s.prefs, logical)
		return 1
	}
	n := len(s.prefs)
	s.prefs = map[string]*stickyPref{}
	return n
}

// prune 配置热加载后清掉失效偏好: 逻辑名没了或下标越界 (候选集变化) 一律作废,
// 让流量从配置顺序重新积累。调用方持 a.mu (applyConfig 重建 aliases 后调用)。
func (s *sticky) prune(aliases map[string][]aliasTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for logical, p := range s.prefs {
		list, ok := aliases[logical]
		if !ok || p.pref < 0 || p.pref >= len(list) {
			delete(s.prefs, logical)
		}
	}
}

// pairs 返回全部逻辑名的当前偏好候选 (逻辑名 -> 上游/真实模型), 供观测面展示。
// aliases 由调用方按 a.mu -> s.mu 的锁序传入 (与 applyConfig 一致, 防死锁);
// 热加载后仍在下标内的保留, 越界的跳过。
func (s *sticky) pairs(aliases map[string][]aliasTarget) map[string]stickyView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]stickyView{}
	for logical, p := range s.prefs {
		list, ok := aliases[logical]
		if !ok || p.pref < 0 || p.pref >= len(list) {
			continue
		}
		at := list[p.pref]
		out[logical] = stickyView{Upstream: at.Upstream, Model: at.Model}
	}
	return out
}
