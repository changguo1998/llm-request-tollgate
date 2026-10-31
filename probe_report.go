// probe_report.go: 探测报表 (GET /v1/probe) 与命令行报表。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"
)

// probeRow 报告里的一行: 某别名的某个候选及其指标与建议。
type probeRow struct {
	Alias     string  `json:"alias"`
	Role      string  `json:"role"` // primary | next | candidate
	Upstream  string  `json:"upstream"`
	Model     string  `json:"model"`
	TtfbMs    float64 `json:"ttfb_ms"`
	Tps       float64 `json:"tps"`
	Samples   int     `json:"samples"`
	OK        int     `json:"ok"`
	Fail      int     `json:"fail"`
	Active    int     `json:"active"`
	Passive   int     `json:"passive"`
	Meets     bool    `json:"meets_targets"`
	Advantage bool    `json:"advantage"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

// probeReport 生成只读报告: 每别名按 fallback 顺序列出候选指标, 并给出
// "是否达到标准" 与 "是否相对主候选有明显优势" 的建议值 —— 仅供决策, 不切换。
func (a *App) probeReport() map[string]any {
	c := a.probeCfg()
	if c == nil {
		c = probeDefaults(&ProbeCfg{})
	}
	a.mu.RLock()
	names := make([]string, 0, len(a.aliases))
	for name := range a.aliases {
		names = append(names, name)
	}
	aliases := make(map[string][]aliasTarget, len(a.aliases))
	for k, v := range a.aliases {
		aliases[k] = v
	}
	ups := make(map[string]*UpstreamCfg, len(a.upstreams))
	for k, v := range a.upstreams {
		ups[k] = v
	}
	a.mu.RUnlock()
	sort.Strings(names)

	a.probeMu.Lock()
	stats := make(map[string]probeStat, len(a.probe))
	for k, v := range a.probe {
		stats[k] = *v
	}
	a.probeMu.Unlock()

	meets := func(st probeStat) bool {
		if st.OK == 0 {
			return false
		}
		if c.TargetTtfbMs > 0 && st.EwmaTtfbMs > float64(c.TargetTtfbMs) {
			return false
		}
		if c.TargetTps > 0 && st.EwmaTps < c.TargetTps {
			return false
		}
		return c.TargetTtfbMs > 0 || c.TargetTps > 0
	}
	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339Nano),
		"standards": map[string]any{
			"target_ttfb_ms": c.TargetTtfbMs,
			"target_tps":     c.TargetTps,
			"switch_margin":  c.SwitchMargin,
		},
		"probe": map[string]any{
			"enabled": c.Enabled, "scope": c.Scope, "interval_sec": c.IntervalSec,
		},
	}
	rows := map[string][]probeRow{}
	for _, name := range names {
		cands := aliases[name]
		list := make([]probeRow, 0, len(cands))
		for i, cand := range cands {
			role := "candidate"
			switch {
			case i == 0:
				role = "primary"
			case i == 1:
				role = "next"
			}
			row := probeRow{Alias: name, Role: role, Upstream: cand.Upstream, Model: cand.Model}
			if st, ok := stats[probeKey(cand.Upstream, cand.Model)]; ok {
				row.TtfbMs, row.Tps = st.EwmaTtfbMs, st.EwmaTps
				row.Samples, row.OK, row.Fail = len(st.Samples), st.OK, st.Fail
				row.Active, row.Passive, row.UpdatedAt = st.Active, st.Passive, st.UpdatedAt
				row.Meets = meets(st)
			}
			// 建议: 下一个候选达标、且相对主候选有 margin 以上的优势 (两个标准任一即可)。
			if row.Role == "next" && row.Meets && len(list) > 0 {
				p := list[0]
				if p.TtfbMs > 0 && row.TtfbMs > 0 {
					if float64(row.TtfbMs) <= float64(p.TtfbMs)*(1-c.SwitchMargin) {
						row.Advantage = true
					}
				}
				if !row.Advantage && p.Tps > 0 && row.Tps > 0 {
					if row.Tps >= p.Tps*(1+c.SwitchMargin) {
						row.Advantage = true
					}
				}
			}
			list = append(list, row)
		}
		rows[name] = list
	}
	out["aliases"] = rows
	return out
}

// serveProbe GET /v1/probe: 只读报告 (标准判定仅建议; 不改变任何路由行为)。
func (a *App) serveProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(a.probeReport())
}

// runProbeReport -probe: 从配置文件与状态文件离线打印同一个报告。
func runProbeReport(cfgPath string) {
	cfg, err := loadConfigOpt(cfgPath, true)
	if err != nil {
		log.Fatalf("config invalid: %v", err)
	}
	a := &App{
		upstreams: map[string]*UpstreamCfg{},
		aliases:   map[string][]aliasTarget{},
		probe:     map[string]*probeStat{},
	}
	a.applyConfig(cfg)
	a.probePath = probePathFor(cfg)
	a.loadProbeState()
	rep := a.probeReport()
	std := rep["standards"].(map[string]any)
	fmt.Printf("标准: ttfb<=%v ms, tps>=%v, margin=%v\n", std["target_ttfb_ms"], std["target_tps"], std["switch_margin"])
	rows := rep["aliases"].(map[string][]probeRow)
	names := make([]string, 0, len(rows))
	for k := range rows {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		list := rows[name]
		if len(list) < 2 {
			continue
		}
		fmt.Printf("%s\n", name)
		for _, row := range list {
			flag := " "
			if row.Advantage {
				flag = "*" // * = 建议切换 (仅建议)
			}
			fmt.Printf("  %s %-8s %-6s/%-28s ttfb=%6.0fms tps=%6.1f 样本=%d(主%v/探%v) 失败=%d 达标=%v\n",
				flag, row.Role, row.Upstream, row.Model, row.TtfbMs, row.Tps,
				row.Samples, row.Passive, row.Active, row.Fail, row.Meets)
		}
	}
}
