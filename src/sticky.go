// sticky.go: 逻辑名级上游偏好 —— 记住最近成功服务的候选, 让同一逻辑名的请求粘在
// 同一上游, 保持那家的缓存热度。偏好只在偏好候选被长期锁 (额度 / 低性能 / 熔断) 挡下、
// 备用顶上并成功时才迁移; 单次失败与锁 / 退避 / 排队超时造成的临时顶班都不计 (候选恢复
// 后流量自动回流)。真实失败不单独数次数: 连续失败会自己触发熔断锁, 一上锁即迁移。
// 另有一个失效窗口 (gate.sticky_ttl_sec, 缺省 5min): 逻辑名空闲超过它, 偏好作废, 下笔
// 按配置优先级重选后重新建立。

package main

import (
	"sync"
	"time"
)

// stickyLongLock 是「直接视为候选失效, 顶班即迁移」的锁原因: 这类锁要么整组、要么
// 好几分钟起跳, 期间不该让每笔请求都先撞它一次再走备用; 熔断锁是连续失败到阈值才上,
// 一上锁就迁走偏好, 免得它一解锁又把流量全吸回去再坏一轮。
func stickyLongLock(reason string) bool {
	return reason == "quota" || reason == lowPerfReason || reason == "breaker"
}

// stickyPref 是一个逻辑名的偏好状态。
type stickyPref struct {
	pref  int       // 偏好候选在 alias 列表里的下标 (0 = 配置第一候选; -1 = 失效待重选)
	moved time.Time // 最近一次偏好迁移/重置时刻 (目前只留作展示)
	last  time.Time // 最近一次请求结束时刻 (失效窗口由此计时)
}

// sticky 是全部逻辑名的偏好表; 独立互斥, 不占调度热路径的主锁, 也不阻塞热加载。
type sticky struct {
	mu    sync.Mutex
	prefs map[string]*stickyPref
}

func newSticky() *sticky {
	return &sticky{prefs: map[string]*stickyPref{}}
}

// prefIdx 返回该逻辑名的偏好下标与是否刚过期 (无记录 / 越界 / 已作废时返回 (0, false))。
// 记录在失效窗口内 (从上次请求结束起 < ttl) 返回偏好; 超窗则作废偏好 (pref = -1) 并返回
// (0, true), 让这笔请求按配置优先级选候选, 成功后由 served 重新建立偏好。
func (s *sticky) prefIdx(logical string, n int, ttl time.Duration, now time.Time) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		return 0, false
	}
	if p.pref < 0 || p.pref >= n {
		return 0, false
	}
	if ttl > 0 && now.After(p.last.Add(ttl)) {
		p.pref = -1 // 失效: 作废偏好, 下笔成功后按实际服务候选重设
		return 0, true
	}
	return p.pref, false
}

// served 一笔请求成功服务后记录偏好。servedIdx 是成功候选下标, prefIdx 是本笔开头的偏好
// 下标, longLock 表示偏好候选被长期锁顶班 (应视为候选失效), expired 表示本笔开头时偏好
// 已过失效窗口 (prefIdx 返回过 true)。每笔成功都刷新 last (窗口从请求结束起重新计时)。
// 规则:
//   - 成功在偏好候选上: 不改偏好, 只刷新窗口
//   - 偏好候选被长期锁顶班后成功别处: 偏好直接迁移, 不再连撞
//   - 失效窗口后的第一笔成功请求 / 已作废 (pref < 0) 的记录: 按本次服务的候选重置
//   - 其余顶班 (临时锁 / 退避 / 排队超时 / 真实失败): 偏好不动
func (s *sticky) served(logical string, servedIdx, prefIdx int, longLock, expired bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		p = &stickyPref{}
		s.prefs[logical] = p
	}
	p.last = now
	if expired || p.pref < 0 {
		p.pref, p.moved = servedIdx, now
		return
	}
	if servedIdx == prefIdx || servedIdx == p.pref {
		return
	}
	if longLock {
		p.pref, p.moved = servedIdx, now
	}
}

// finish 每笔请求结束 (成功或失败) 时刷新失效窗口起点, 并结算过期后的归零:
// 失效窗口后的第一笔请求没能服务成功时, 作废偏好 (下一笔成功后按实际服务候选重设)。
func (s *sticky) finish(logical string, served, expired bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		return // 从没服务过: 没有可维护的窗口
	}
	if expired && !served {
		p.pref = -1 // 过期后的第一笔请求没成功: 作废, 下笔按优先级重选
	}
	p.last = now
}

// set 人工置一次偏好 (纯内存, 不落盘): 立即生效, 但自动迁移规则照旧 —— 之后偏好候选
// 被长期锁 (额度 / 低性能 / 熔断) 挡下时, 仍会迁到顶班候选。下标由调用方按当前 alias 解析。
func (s *sticky) set(logical string, idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prefs[logical]
	if !ok {
		p = &stickyPref{}
		s.prefs[logical] = p
	}
	p.pref, p.moved, p.last = idx, time.Now(), time.Now()
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

// pairs 返回全部逻辑名的当前偏好候选 (逻辑名 -> 上游/真实模型) 与剩余保持时长 (ms),
// 供观测面展示 (第 1 页模型表的分组标题倒计时)。已过失效窗口的偏好不返回 (倒计时归零即
// 摘除, 星号与倒计时随之消失)。aliases 由调用方按 a.mu -> s.mu 的锁序传入 (与
// 逻辑名倒计时保持设定值: 请求进行期间窗口不启动, 请求结束才从满值开始倒。
func (s *sticky) pairs(aliases map[string][]aliasTarget, ttl time.Duration, now time.Time, inflight map[string]bool) map[string]stickyView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]stickyView{}
	for logical, p := range s.prefs {
		list, ok := aliases[logical]
		if !ok || p.pref < 0 || p.pref >= len(list) {
			continue
		}
		at := list[p.pref]
		v := stickyView{Upstream: at.Upstream, Model: at.Model}
		if ttl > 0 {
			if inflight[logical] {
				v.RemainingMs = int64(ttl / time.Millisecond) // 进行中: 窗口从请求结束才开始
			} else {
				rem := ttl - now.Sub(p.last)
				if rem <= 0 {
					continue // 已过失效窗口: 不作当前偏好展示 (无倒计时, 也无星号)
				}
				v.RemainingMs = int64(rem / time.Millisecond)
			}
		}
		out[logical] = v
	}
	return out
}
