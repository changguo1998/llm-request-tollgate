// perfref_runtime.go: 速度参考参数的周期重算 (运行时状态)。
//
// 每小时用全部日志重算一次 (recomputePerfRef), 结果驻 App 内存; 只更新参数表,
// 不接观察面 / TUI / 调度 / 告警。重算失败保留旧表, 仅记日志。

package main

import (
	"log"
	"path/filepath"
	"strings"
	"time"
)

// perfRecomputeInterval 是速度参考参数的周期重算间隔 (每小时一次)。
const perfRecomputeInterval = time.Hour

// startPerfRecompute 启动周期重算: 先立即算一次, 之后每 perfRecomputeInterval 重算。
func (a *App) startPerfRecompute() {
	go func() {
		a.recomputePerf()
		t := time.NewTicker(perfRecomputeInterval)
		defer t.Stop()
		for range t.C {
			a.recomputePerf()
		}
	}()
}

// recomputePerf 读日志目录重算参数并替换内存表; 失败只记日志, 保留旧表。
func (a *App) recomputePerf() {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	if cfg == nil || cfg.LogPath == "" {
		return
	}
	p, skipped, err := recomputePerfRef(filepath.Dir(cfg.LogPath), nil)
	if err != nil {
		log.Printf("perf recompute failed: %v", err)
		return
	}
	a.perfRefMu.Lock()
	a.perfRef = p
	a.perfRefMu.Unlock()
	note := ""
	if len(skipped) > 0 {
		note = " (跳过 " + strings.Join(skipped, ", ") + ")"
	}
	log.Printf("perf recompute: %d model(s)%s", len(p.byModel), note)
}

// perfIndex 是速度参考指标的线程安全读取口: perf = (c/d)*(c/b)^(-g); 无参数表时
// 退回原始速率 c/d (tokens/s)。
func (a *App) perfIndex(up, model string, c, d, b int64) float64 {
	a.perfRefMu.RLock()
	p := a.perfRef
	a.perfRefMu.RUnlock()
	if p == nil {
		return float64(c) / float64(d) * 1000
	}
	return p.index(up, model, c, d, b)
}
