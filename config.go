// config.go: 配置结构与加载 (别名 / 上游 / 门控参数)。

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type Timeouts struct {
	ConnectMs        int `json:"connect_ms"`
	ResponseHeaderMs int `json:"response_header_ms"`
	StreamIdleMs     int `json:"stream_idle_ms"`
}

// ModelCfg 是上游声明的一个真实模型。BillingGroup 是它的额度组名: 同一上游里同名的
// 模型属于同一组, 额度锁只锁组; 不写组名 = 该上游的默认组 (编译期常量 defaultGroup)。
type ModelCfg struct {
	ID           string `json:"id"`
	BillingGroup string `json:"billing_group,omitempty"`
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
	// Models 声明该上游提供哪些真实模型以及各自的额度组。可选:
	// 不写 = 别名引用到的模型全归默认组, 与加组之前的配置行为一致。
	Models []ModelCfg `json:"models,omitempty"`
	// Errors 是 的报错分类规则: 每个上游一份「状态码 + body 关键词」,
	// 用来把报错分成并发 / 额度 / 其他三类; 缺省空表 = 全部走通用兜底。
	Errors *ErrRules `json:"errors,omitempty"`
}

type Config struct {
	Listen    string                 `json:"listen"`
	LogPath   string                 `json:"log_path"`
	Timeouts  Timeouts               `json:"timeouts"`
	Gate      GateCfg                `json:"gate"`
	Upstreams []UpstreamCfg          `json:"upstreams"`
	Aliases   map[string][][2]string `json:"aliases"`
}

// GateCfg 是所有上游共用的门控参数。
type GateCfg struct {
	IntervalMs        int `json:"interval_ms"`
	SemWaitMs         int `json:"sem_wait_ms"`
	QueueWaitMs       int `json:"queue_wait_ms"`
	ConcurrencyLockMs int `json:"concurrency_lock_ms"`
	QuotaLockMs       int `json:"quota_lock_ms"`
	BreakerLockMs     int `json:"breaker_lock_ms"`
	// 低流量锁: 窗口内解码吞吐的峰值与平均都低于阈值时锁住该模型。
	LowTpsWindowMs int   `json:"lowtps_window_ms"`
	LowTpsPeakMax  int   `json:"lowtps_peak_max"` // 峰值上限 (tokens/s)
	LowTpsAvgMax   int   `json:"lowtps_avg_max"`  // 平均上限 (tokens/s)
	LowTpsLockMs   int   `json:"lowtps_lock_ms"`
	RetryBackoffMs []int `json:"retry_backoff_ms"`
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
	if g.LowTpsWindowMs <= 0 {
		g.LowTpsWindowMs = 600000
	}
	if g.LowTpsPeakMax <= 0 {
		g.LowTpsPeakMax = 5
	}
	if g.LowTpsAvgMax <= 0 {
		g.LowTpsAvgMax = 1
	}
	if g.LowTpsLockMs <= 0 {
		g.LowTpsLockMs = 300000
	}
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

// 低流量锁的窗口 / 门槛 / 锁时长 (门槛是整数 tokens/s)。
func (g GateCfg) lowTpsWindow() time.Duration {
	return time.Duration(g.LowTpsWindowMs) * time.Millisecond
}
func (g GateCfg) lowTpsLock() time.Duration {
	return time.Duration(g.LowTpsLockMs) * time.Millisecond
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
	// 额度组: 同一上游里模型 id 唯一; 声明了 models 的上游, 别名引用的模型必须在表内
	// (没列出的模型属于哪个组无从谈起, 早报错好过静默落进默认组)。
	declared := map[string]map[string]bool{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		ids := make(map[string]bool, len(u.Models))
		for j := range u.Models {
			m := &u.Models[j]
			m.ID = strings.TrimSpace(m.ID)
			m.BillingGroup = strings.TrimSpace(m.BillingGroup)
			if m.ID == "" {
				return nil, fmt.Errorf("upstream %s: model entry without id", u.ID)
			}
			if ids[m.ID] {
				return nil, fmt.Errorf("upstream %s: duplicate model %s", u.ID, m.ID)
			}
			ids[m.ID] = true
		}
		declared[u.ID] = ids
	}
	for name, list := range c.Aliases {
		for _, pair := range list {
			ids := declared[pair[0]]
			if len(ids) == 0 {
				continue // 未知上游运行期本来就跳过; 不写 models 的上游不做这项校验
			}
			if !ids[pair[1]] {
				return nil, fmt.Errorf("alias %s: model %s is not declared by upstream %s",
					name, pair[1], pair[0])
			}
		}
	}
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
