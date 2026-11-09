// config.go: 配置结构与加载 (别名 / 上游 / 门控参数)。

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type Timeouts struct {
	ConnectMs        int `json:"connect_ms"`
	ResponseHeaderMs int `json:"response_header_ms"`
	StreamIdleMs     int `json:"stream_idle_ms"`
}

// ModelCfg 是上游声明的一个真实模型。Billing 是它的额度组名: 同一上游里同名的
// 模型属于同一组, 额度锁只锁组; 不写组名 = 该上游的默认组 (编译期常量 defaultGroup)。
// Alias 是逻辑名 (别名的别名); Priority 是候选权重 (缺省 0, 负值 = 停用).
type ModelCfg struct {
	ID       string `json:"id"`
	Billing  string `json:"billing,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

type UpstreamCfg struct {
	ID           string            `json:"id"`
	BaseURL      string            `json:"base_url"`
	AuthHeader   string            `json:"auth_header"`
	Key          string            `json:"key"`
	KeyEnv       string            `json:"key_env,omitempty"`
	KeyPrefix    *string           `json:"key_prefix,omitempty"`
	Headers      map[string]string `json:"headers"`
	IncludeUsage bool              `json:"include_usage"`
	// Priority 是 provider 权重: 越大越先用, 缺省 0; 负值 = 整家停用。
	Priority int `json:"priority,omitempty"`
	// Models 声明该上游提供哪些真实模型以及各自的逻辑名 / 额度组。可选:
	// 不写 = 别名引用到的模型全归默认组, 与加组之前的配置行为一致。
	Models []ModelCfg `json:"models,omitempty"`
	// Errors 是 的报错分类规则: 每个上游一份「状态码 + body 关键词」,
	// 用来把报错分成并发 / 额度 / 其他三类; 缺省空表 = 全部走通用兜底。
	Errors *ErrRules `json:"errors,omitempty"`
}

// Config 是所有扁平顶层键的配置: timeouts 与 gate 参数不再嵌套对象, 全部平铺在顶层。
// 时间字段在配置文件里以「秒」表达 (`*_sec`, 允许小数); 代码内部改用毫秒, 经
// gate()/timeouts() 转换 (×1000 取整)。
type Config struct {
	Listen  string `json:"listen"`
	LogPath string `json:"log_path"`
	// timeouts (扁平, 秒, 可小数)
	ConnectSec        float64 `json:"connect_sec"`
	ResponseHeaderSec float64 `json:"response_header_sec"`
	StreamIdleSec     float64 `json:"stream_idle_sec"`
	// gate 门控参数 (扁平; 时间为秒可小数, 非时间项原单位)
	IntervalSec        float64 `json:"interval_sec"`
	SemWaitSec         float64 `json:"sem_wait_sec"`
	QueueWaitSec       float64 `json:"queue_wait_sec"`
	ConcurrencyLockSec float64 `json:"concurrency_lock_sec"`
	QuotaLockSec       float64 `json:"quota_lock_sec"`
	BreakerLockSec     float64 `json:"breaker_lock_sec"`
	LowPerfWindowSec   float64 `json:"lowperf_window_sec"`
	LowPerfPeakMax     int     `json:"lowperf_peak_max"`     // tokens/s, 非时间
	LowPerfAvgMax      int     `json:"lowperf_avg_max"`      // tokens/s, 非时间
	LowPerfTtfbMaxSec  float64 `json:"lowperf_ttfb_max_sec"` // 高延迟阈值 (秒); 0 = 关闭
	LowPerfLockSec     float64 `json:"lowperf_lock_sec"`
	// LowPerfExempt 是低性能锁豁免名单: 名单内的真实模型 id 禁止触发低性能锁。
	LowPerfExempt   []string  `json:"lowperf_exempt,omitempty"`
	RetryBackoffSec []float64 `json:"retry_backoff_sec"`
	// TUI 显示窗口 (秒, 可小数)
	RateWindowSec  float64 `json:"rate_window_sec"`
	BlockWindowSec float64 `json:"block_window_sec"`
	// 锁恢复探测: 指针区分"未写"(缺省 true) 与显式 false。
	ProbeEnabled *bool         `json:"probe_enabled"`
	Upstreams    []UpstreamCfg `json:"upstreams"`
	// Aliases 不再从配置读取 (顶层 aliases 键废弃): loadConfigOpt 读取后由
	// rebuildAliases 从上游 models 的 alias 字段重建, 等价于旧顶层候选表。
	Aliases map[string][][2]string `json:"-"`
	// Groups 是别名的别名: 组名 -> 已注册逻辑名列表 (顺序即展开候选的优先级)。
	// 请求写组名时, 候选链 = 组成员候选链按序拼接 (去重). 组名对客户端与逻辑名
	// 无差别; 禁止嵌套 (组成员只能是逻辑名).
	Groups map[string][]string `json:"groups"`
}

// probeEnabled 是否开启锁恢复探测: 缺省 true (未写即开), 显式 false 关闭。
func (c *Config) probeEnabled() bool {
	return c.ProbeEnabled == nil || *c.ProbeEnabled
}

// rebuildAliases 从上游 models 重建候选表 (逻辑名 -> 候选链), 等价于旧顶层
// aliases 键: 逻辑名 = 模型条目的 Alias (缺省 = 模型 id); 候选顺序 = 模型
// priority 降序 -> provider priority 降序 -> 上游配置顺序 -> 模型 id,
// 与生成器 / TUI 预期同口径; 模型或整家负优先级停用, 不进候选。
// loadConfigOpt 读取配置后立即调用, 之后 applyConfig / 展开 / TUI 全部沿用
// c.Aliases, 数据流程不变。
func (c *Config) rebuildAliases() {
	type item struct {
		name            string
		up, mid         string
		modelPrio       int
		provPrio        int
		upOrder, midOrd int
	}
	items := make([]item, 0)
	for ui, u := range c.Upstreams {
		for mi, m := range u.Models {
			if u.Priority < 0 || m.Priority < 0 {
				continue // provider 或模型负优先级: 停用, 不进候选
			}
			items = append(items, item{m.Alias, u.ID, m.ID, m.Priority, u.Priority, ui, mi})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.modelPrio != b.modelPrio {
			return a.modelPrio > b.modelPrio
		}
		if a.provPrio != b.provPrio {
			return a.provPrio > b.provPrio
		}
		if a.upOrder != b.upOrder {
			return a.upOrder < b.upOrder
		}
		if a.midOrd != b.midOrd {
			return a.midOrd < b.midOrd
		}
		return a.mid < b.mid
	})
	out := make(map[string][][2]string)
	for _, it := range items {
		name := it.name
		if name == "" {
			name = it.mid // 缺省逻辑名 = 模型 id
		}
		out[name] = append(out[name], [2]string{it.up, it.mid})
	}
	c.Aliases = out
}

// expandedAliases 返回别名表 + 组展开后的完整候选映射 (逻辑名 -> 候选链)。
// 组名是别名的别名: 候选链 = 组成员候选链按序拼接, 跨成员去重 (同一 上游/模型
// 只留第一次出现)。组名自身相当于一个普通逻辑名, 粘性 / fallback / 记账 / 观测
// 全部复用逻辑名的既有路径。校验由 loadConfigOpt 完成, 这里假定成员都存在。
func (c *Config) expandedAliases() map[string][][2]string {
	out := make(map[string][][2]string, len(c.Aliases)+len(c.Groups))
	for name, list := range c.Aliases {
		out[name] = list
	}
	for gname, members := range c.Groups {
		var chain [][2]string
		seen := map[[2]string]bool{}
		for _, m := range members {
			for _, tgt := range out[m] {
				if seen[tgt] {
					continue
				}
				seen[tgt] = true
				chain = append(chain, tgt)
			}
		}
		out[gname] = chain
	}
	return out
}

// 秒 <-> 毫秒换算 (配置文件为秒可小数, 代码内部毫秒取整)。
func secToMs(s float64) int {
	return int(s*1000 + 0.5)
}
func msToSec(ms int) float64 {
	return float64(ms) / 1000
}
func secListToMs(ls []float64) []int {
	out := make([]int, len(ls))
	for i, v := range ls {
		out[i] = secToMs(v)
	}
	return out
}
func msListToSec(ls []int) []float64 {
	out := make([]float64, len(ls))
	for i, v := range ls {
		out[i] = msToSec(v)
	}
	return out
}

// timeouts 把扁平秒字段装成内部 Timeouts (毫秒, 仅逻辑用)。
func (c *Config) timeouts() Timeouts {
	return Timeouts{
		ConnectMs: secToMs(c.ConnectSec), ResponseHeaderMs: secToMs(c.ResponseHeaderSec),
		StreamIdleMs: secToMs(c.StreamIdleSec),
	}
}

// setTimeouts 把内部 Timeouts 写回扁平秒字段。
func (c *Config) setTimeouts(t Timeouts) {
	c.ConnectSec, c.ResponseHeaderSec, c.StreamIdleSec = msToSec(t.ConnectMs), msToSec(t.ResponseHeaderMs), msToSec(t.StreamIdleMs)
}

// gate 把扁平门控字段装成内部 GateCfg (毫秒)。
func (c *Config) gate() GateCfg {
	return GateCfg{
		IntervalMs: secToMs(c.IntervalSec), SemWaitMs: secToMs(c.SemWaitSec), QueueWaitMs: secToMs(c.QueueWaitSec),
		ConcurrencyLockMs: secToMs(c.ConcurrencyLockSec), QuotaLockMs: secToMs(c.QuotaLockSec), BreakerLockMs: secToMs(c.BreakerLockSec),
		LowPerfWindowMs: secToMs(c.LowPerfWindowSec), LowPerfPeakMax: c.LowPerfPeakMax, LowPerfAvgMax: c.LowPerfAvgMax,
		LowPerfLockMs: secToMs(c.LowPerfLockSec), LowPerfTtfbMaxMs: secToMs(c.LowPerfTtfbMaxSec),
		LowPerfExempt:  c.LowPerfExempt,
		RetryBackoffMs: secListToMs(c.RetryBackoffSec),
	}
}

// setGate 把内部 GateCfg 写回扁平秒字段。
func (c *Config) setGate(g GateCfg) {
	c.IntervalSec, c.SemWaitSec, c.QueueWaitSec = msToSec(g.IntervalMs), msToSec(g.SemWaitMs), msToSec(g.QueueWaitMs)
	c.ConcurrencyLockSec, c.QuotaLockSec, c.BreakerLockSec = msToSec(g.ConcurrencyLockMs), msToSec(g.QuotaLockMs), msToSec(g.BreakerLockMs)
	c.LowPerfWindowSec = msToSec(g.LowPerfWindowMs)
	c.LowPerfPeakMax, c.LowPerfAvgMax = g.LowPerfPeakMax, g.LowPerfAvgMax
	c.LowPerfLockSec, c.LowPerfTtfbMaxSec = msToSec(g.LowPerfLockMs), msToSec(g.LowPerfTtfbMaxMs)
	c.LowPerfExempt = g.LowPerfExempt
	c.RetryBackoffSec = msListToSec(g.RetryBackoffMs)
}

// GateCfg 是所有上游共用的门控参数。
type GateCfg struct {
	IntervalMs        int `json:"interval_ms"`
	SemWaitMs         int `json:"sem_wait_ms"`
	QueueWaitMs       int `json:"queue_wait_ms"`
	ConcurrencyLockMs int `json:"concurrency_lock_ms"`
	QuotaLockMs       int `json:"quota_lock_ms"`
	BreakerLockMs     int `json:"breaker_lock_ms"`
	// 低性能锁: 覆盖两种「慢到没有实用价值」—— 低吞吐 (窗口内解码吞吐的峰值与平均都
	// 低于门槛) 或 高延迟 (窗口内平均首字节时延超过阈值), 任一命中即锁该 上游/模型。
	LowPerfWindowMs  int `json:"lowperf_window_ms"`
	LowPerfPeakMax   int `json:"lowperf_peak_max"`    // 低吞吐峰值上限 (tokens/s)
	LowPerfAvgMax    int `json:"lowperf_avg_max"`     // 低吞吐平均上限 (tokens/s)
	LowPerfTtfbMaxMs int `json:"lowperf_ttfb_max_ms"` // 高延迟阈值 (平均首字节时延, ms); 0 = 关闭该项
	LowPerfLockMs    int `json:"lowperf_lock_ms"`
	// LowPerfExempt 是低性能锁豁免名单: 名单内的模型禁止触发低性能锁 (按真实模型 id 匹配)。
	LowPerfExempt  []string `json:"lowperf_exempt,omitempty"`
	RetryBackoffMs []int    `json:"retry_backoff_ms"`
}

// gateIntervalMs 是出队节拍的缺省值。
const gateIntervalMs = 500

// normGate 按缺省值补齐门控参数。
func normGate(g GateCfg) GateCfg {
	if g.IntervalMs <= 0 {
		g.IntervalMs = gateIntervalMs
	}
	if g.SemWaitMs <= 0 {
		g.SemWaitMs = 5000
	}
	if g.QueueWaitMs <= 0 {
		g.QueueWaitMs = 10000
	}
	if g.ConcurrencyLockMs <= 0 {
		g.ConcurrencyLockMs = 3600000
	}
	if g.QuotaLockMs <= 0 {
		g.QuotaLockMs = 600000
	}
	if g.BreakerLockMs <= 0 {
		g.BreakerLockMs = 300000
	}
	if g.LowPerfWindowMs <= 0 {
		g.LowPerfWindowMs = 600000
	}
	if g.LowPerfPeakMax <= 0 {
		g.LowPerfPeakMax = 5
	}
	if g.LowPerfAvgMax <= 0 {
		g.LowPerfAvgMax = 1
	}
	if g.LowPerfLockMs <= 0 {
		g.LowPerfLockMs = 300000
	}
	// LowPerfTtfbMaxMs 缺省 0 = 高延迟判定关闭, 只保留低吞吐逻辑。
	if len(g.RetryBackoffMs) == 0 {
		g.RetryBackoffMs = []int{10000, 20000, 30000}
	}
	return g
}

// 配置里写的是毫秒, 调度里用 Duration。
func (g GateCfg) interval() time.Duration { return time.Duration(g.IntervalMs) * time.Millisecond }
func (g GateCfg) semWait() time.Duration  { return time.Duration(g.SemWaitMs) * time.Millisecond }
func (g GateCfg) queueWait() time.Duration {
	return time.Duration(g.QueueWaitMs) * time.Millisecond
}
func (g GateCfg) concurrencyLock() time.Duration {
	return time.Duration(g.ConcurrencyLockMs) * time.Millisecond
}
func (g GateCfg) quotaLock() time.Duration {
	return time.Duration(g.QuotaLockMs) * time.Millisecond
}
func (g GateCfg) breakerLock() time.Duration {
	return time.Duration(g.BreakerLockMs) * time.Millisecond
}

// 低性能锁的窗口 / 门槛 / 高延迟阈值 / 锁时长。
func (g GateCfg) lowPerfWindow() time.Duration {
	return time.Duration(g.LowPerfWindowMs) * time.Millisecond
}
func (g GateCfg) lowPerfLock() time.Duration {
	return time.Duration(g.LowPerfLockMs) * time.Millisecond
}

// lowPerfTtfbMax 是高延迟阈值; 0 = 关闭该项。
func (g GateCfg) lowPerfTtfbMax() time.Duration {
	return time.Duration(g.LowPerfTtfbMaxMs) * time.Millisecond
}

// backoff 返回第 n 次连续失败该暂停多久; 超出序列长度返回 0 (那一次该进熔断)。
func (g GateCfg) backoff(n int) time.Duration {
	if n < 1 || n > len(g.RetryBackoffMs) {
		return 0
	}
	return time.Duration(g.RetryBackoffMs[n-1]) * time.Millisecond
}

// ErrRule 是一条错误分类规则: 状态码在 Status 里 (缺省 = 任意状态码), 且匹配文本里出现
// Contains 里任一子串, 两者都满足才算命中 (docs/providers-api.md §1.2)。
// 状态码只用来收敛候选集, 定类必须落到 body 的结构化字段或关键词 —— 多家的 429 是
// 「限流与额度共用」的同一个码, 只看状态码就锁候选会误判。
func loadConfig(path string) (*Config, error) {
	return loadConfigOpt(path, false)
}

// loadConfigOpt 读配置。lenient=true 时缺 key 环境变量不报错 (settings 页离线编辑
// 配置时不需要真 key, 只关心上游 id 与别名结构)。
func loadConfigOpt(path string, lenient bool) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.LogPath == "" {
		c.LogPath = "var/requests.jsonl"
	}
	// TUI 显示窗口缺省: 速度窗 1h, active 分块采样时间尺度 35s (rowStatTau0)。
	if c.RateWindowSec <= 0 {
		c.RateWindowSec = 3600 // 1h
	}
	if c.BlockWindowSec <= 0 {
		c.BlockWindowSec = 35 // rowStatTau0 兜底
	}
	// 三项超时缺省: 旧配置/生成器可能没给扁平 `*_sec` (早期是嵌套 timeouts)。0 在
	// idleConn 里会退化成"每次读写立即超时", 所以这里补回历史缺省, 不能留 0。
	if c.ConnectSec <= 0 {
		c.ConnectSec = 10
	}
	if c.ResponseHeaderSec <= 0 {
		c.ResponseHeaderSec = 120
	}
	if c.StreamIdleSec <= 0 {
		c.StreamIdleSec = 240
	}
	// 额度组: 同一上游里模型 id 唯一; 声明了 models 的上游, 模型条目清一下空白
	// (id / billing / alias), 缺省逻辑名留空由 rebuildAliases 用模型 id 兜底。
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		ids := make(map[string]bool, len(u.Models))
		for j := range u.Models {
			m := &u.Models[j]
			m.ID = strings.TrimSpace(m.ID)
			m.Billing = strings.TrimSpace(m.Billing)
			m.Alias = strings.TrimSpace(m.Alias)
			if m.ID == "" {
				return nil, fmt.Errorf("upstream %s: model entry without id", u.ID)
			}
			if ids[m.ID] {
				return nil, fmt.Errorf("upstream %s: duplicate model %s", u.ID, m.ID)
			}
			ids[m.ID] = true
		}
	}
	// 候选表重建: 顶层 aliases 键已废弃, 逻辑名与候选链全部从上游 models 的
	// alias / priority 重建, 之后 applyConfig 与组校验拿到的就是这张表。
	c.rebuildAliases()
	// 组 (别名的别名): 组名不能与逻辑名重名 (否则展开后互相覆盖无从区分);
	// 成员必须都是已注册逻辑名 (禁止嵌套: 组成员只能是逻辑名, 不能是别的组名)。
	// 成员带多余空白时剔除 (与逻辑名同一的 TrimSpace 口径), 免得校验过了展开时查空表。
	for gname, members := range c.Groups {
		gname = strings.TrimSpace(gname)
		if gname == "" {
			return nil, fmt.Errorf("group: empty group name")
		}
		if _, dup := c.Aliases[gname]; dup {
			return nil, fmt.Errorf("group %s: name collides with an alias", gname)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("group %s: no members", gname)
		}
		clean := members[:0]
		for _, m := range members {
			m = strings.TrimSpace(m)
			if m == "" {
				return nil, fmt.Errorf("group %s: empty member name", gname)
			}
			if _, ok := c.Aliases[m]; !ok {
				return nil, fmt.Errorf("group %s: member %s is not a registered alias", gname, m)
			}
			clean = append(clean, m)
		}
		c.Groups[gname] = clean
	}
	// 低性能豁免名单: 按真实模型 id 匹配, 手写配置容易带多余空白, 去空白并剔除空串。
	exempt := c.LowPerfExempt[:0]
	for _, m := range c.LowPerfExempt {
		if m = strings.TrimSpace(m); m != "" {
			exempt = append(exempt, m)
		}
	}
	c.LowPerfExempt = exempt
	// key_env: 配置只存环境变量名, 真 key 启动/热加载时从环境取, 不落盘。
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.KeyEnv != "" {
			v, ok := os.LookupEnv(u.KeyEnv)
			if !ok || v == "" {
				if lenient {
					continue
				}
				return nil, fmt.Errorf("upstream %s: env %s not set", u.ID, u.KeyEnv)
			}
			u.Key = v
		}
	}
	return &c, nil
}
