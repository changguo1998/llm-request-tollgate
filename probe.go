// probe.go: 探测配置、样本积累与状态落盘。

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

type ProbeCfg struct {
	Enabled      bool     `json:"enabled,omitempty"`
	Scope        string   `json:"scope,omitempty"`          // next (默认) | all
	IntervalSec  int      `json:"interval_sec,omitempty"`   // 默认 900
	MaxTokens    int      `json:"max_tokens,omitempty"`     // 每次探测的生成上限, 默认 8
	TimeoutMs    int      `json:"timeout_ms,omitempty"`     // 单次探测超时, 默认 30000
	Window       int      `json:"window,omitempty"`         // 每候选保留样本数, 默认 5
	Targets      []string `json:"targets,omitempty"`        // 只探这些别名; 空 = 全部多候选别名
	TargetTtfbMs int      `json:"target_ttfb_ms,omitempty"` // 标准: 首字节时延上限 (仅报告判定)
	TargetTps    float64  `json:"target_tps,omitempty"`     // 标准: 吞吐下限 (仅报告判定)
	SwitchMargin float64  `json:"switch_margin,omitempty"`  // 明显优势阈值 (仅报告判定)
}

// probeSample 一次采样: 主动探测 (Passive=false) 或真实请求 (Passive=true)。
type probeSample struct {
	Ts      string  `json:"ts"`
	TtfbMs  int64   `json:"ttfb_ms"`
	Tps     float64 `json:"tps,omitempty"`
	Tokens  int     `json:"tokens,omitempty"`
	OK      bool    `json:"ok"`
	Err     string  `json:"err,omitempty"`
	Passive bool    `json:"passive,omitempty"`
}

// probeStat 某 (上游, 模型) 的滚动统计: EWMA (α=0.3) + 最近 Window 个样本。
type probeStat struct {
	Upstream   string        `json:"upstream"`
	Model      string        `json:"model"`
	Samples    []probeSample `json:"samples"`
	EwmaTtfbMs float64       `json:"ewma_ttfb_ms"`
	EwmaTps    float64       `json:"ewma_tps"`
	OK         int           `json:"ok"`
	Fail       int           `json:"fail"`
	Active     int           `json:"active"`
	Passive    int           `json:"passive"`
	UpdatedAt  string        `json:"updated_at"`
}

// probeState 状态文件 (与 gate-state.json 同目录, 重启不丢)。
type probeState struct {
	Stats     map[string]probeStat `json:"stats"`
	UpdatedAt string               `json:"updated_at"`
}

// relayOut relayStream 的产出: 用量 + 时延 (首字节与解码段)。
// probeDefaults 填默认值: 只探 fallback 顺序的下一个候选, 间隔 900s,
// 每次生成上限 8 token, 单次超时 30s, 样本窗 5。
func probeDefaults(c *ProbeCfg) *ProbeCfg {
	out := *c
	if out.Scope == "" {
		out.Scope = "next"
	}
	if out.IntervalSec <= 0 {
		out.IntervalSec = 900
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 8
	}
	if out.TimeoutMs <= 0 {
		out.TimeoutMs = 30000
	}
	if out.Window <= 0 {
		out.Window = 5
	}
	return &out
}

func probeKey(upID, model string) string { return upID + "|" + model }

// msCeil 毫秒向上取整: 亚毫秒的响应记成 1ms, 不记 0 (0 会被当成"没测到")。
func msCeil(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// probePathFor 指标状态文件与 gate-state.json 同目录 (var/)。
func probePathFor(c *Config) string {
	if c == nil || c.LogPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.LogPath), "probe.json")
}

// probeCfg 取当前 (已填默认值的) 探测配置; 未配置或未开启时返回 nil。
func (a *App) probeCfg() *ProbeCfg {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cfg == nil || a.cfg.Probe == nil {
		return nil
	}
	return probeDefaults(a.cfg.Probe)
}

// recordProbeSample 追加样本并更新 EWMA (α=0.3); 只失败也记, 便于看可用率。
func (a *App) recordProbeSample(upID, model string, s probeSample, window int) {
	if a.probe == nil {
		return
	}
	if window <= 0 {
		window = 5
	}
	k := probeKey(upID, model)
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	st := a.probe[k]
	if st == nil {
		st = &probeStat{Upstream: upID, Model: model}
		a.probe[k] = st
	}
	if s.Passive {
		st.Passive++
	} else {
		st.Active++
	}
	if s.OK {
		st.OK++
		if st.EwmaTtfbMs <= 0 {
			st.EwmaTtfbMs = float64(s.TtfbMs)
		} else {
			st.EwmaTtfbMs = 0.7*st.EwmaTtfbMs + 0.3*float64(s.TtfbMs)
		}
		if s.Tps > 0 {
			if st.EwmaTps <= 0 {
				st.EwmaTps = s.Tps
			} else {
				st.EwmaTps = 0.7*st.EwmaTps + 0.3*s.Tps
			}
		}
	} else {
		st.Fail++
	}
	st.UpdatedAt = s.Ts
	st.Samples = append(st.Samples, s)
	if len(st.Samples) > window {
		st.Samples = st.Samples[len(st.Samples)-window:]
	}
}

func (a *App) loadProbeState() {
	if a.probePath == "" {
		return
	}
	b, err := os.ReadFile(a.probePath)
	if err != nil {
		return
	}
	var st probeState
	if json.Unmarshal(b, &st) != nil || st.Stats == nil {
		return
	}
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	for k := range st.Stats {
		s := st.Stats[k]
		a.probe[k] = &s
	}
	log.Printf("probe: loaded %d candidate stats from %s", len(st.Stats), a.probePath)
}

// saveProbeState 原子写 (临时文件 + rename), 与锁状态同一套路。
func (a *App) saveProbeState() {
	path := a.probePath
	if path == "" {
		return
	}
	a.probeMu.Lock()
	st := probeState{Stats: map[string]probeStat{}, UpdatedAt: time.Now().Format(time.RFC3339Nano)}
	for k, v := range a.probe {
		st.Stats[k] = *v
	}
	a.probeMu.Unlock()

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		log.Printf("probe state write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("probe state rename failed: %v", err)
	}
}

// startProbeLoop 起探测循环; 未开启时什么都不做 (默认关闭)。
// 探测参数启动时固化, 改配置要重启 (与 limits / timeouts 一致)。
func (a *App) startProbeLoop() {
	c := a.probeCfg()
	if c == nil || !c.Enabled {
		return
	}
	log.Printf("probe: enabled scope=%s interval=%ds max_tokens=%d targets=%d window=%d",
		c.Scope, c.IntervalSec, c.MaxTokens, len(c.Targets), c.Window)
	go func() {
		a.probeOnce(c)
		t := time.NewTicker(time.Duration(c.IntervalSec) * time.Second)
		defer t.Stop()
		for range t.C {
			a.probeOnce(c)
		}
	}()
}

// probeOnce 跑一轮: 对每个目标候选发一次小请求并落状态。
func (a *App) probeOnce(c *ProbeCfg) {
	targets := a.probeTargets(c)
	for _, t := range targets {
		a.probeCandidate(t.alias, t.up, t.model, c)
	}
	if len(targets) > 0 {
		a.saveProbeState()
	}
}
