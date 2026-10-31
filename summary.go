// summary.go: -summary 与 -usage 汇总命令。

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// sumRow 是 -summary 的每行聚合。
type sumRow struct {
	Key     string
	Up      string
	Reqs    int
	Errs    int
	Queued  int
	Missing int
	P, C, T int
}

// runSummary 汇总 JSONL 请求日志: 按 逻辑名+来源 分组, 来源取 source, 缺省用 ua。
// path 可以是单个日志文件, 也可以是一个目录 (汇总其中全部 *.jsonl, 用于跨天归档)。
func runSummary(path string) {
	files := []string{path}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		ms, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
		if err != nil || len(ms) == 0 {
			log.Fatalf("summary: no *.jsonl under %s", path)
		}
		sort.Strings(ms)
		files = ms
	}
	rows := map[string]*sumRow{}
	var order []string
	for _, fp := range files {
		f, err := os.Open(fp)
		if err != nil {
			log.Fatalf("summary: %v", err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec logRec
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Logical == "" {
				continue
			}
			src := rec.Source
			if src == "" {
				src = rec.UA
			}
			key := rec.Logical + "\x00" + src
			r, ok := rows[key]
			if !ok {
				r = &sumRow{Key: rec.Logical + " / " + src}
				rows[key] = r
				order = append(order, key)
			}
			if r.Up == "" {
				r.Up = rec.Upstream
			}
			r.Reqs++
			if rec.Status >= 400 {
				r.Errs++
			}
			if rec.Queued {
				r.Queued++
			}
			if rec.Missing {
				r.Missing++
			}
			if rec.Usage != nil {
				r.P += rec.Usage.Prompt
				r.C += rec.Usage.Completion
				r.T += rec.Usage.Total
			}
		}
		if err := sc.Err(); err != nil {
			f.Close()
			log.Fatalf("summary: %v", err)
		}
		f.Close()
	}
	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].T > rows[order[j]].T
	})
	fmt.Printf("%-34s %-10s %6s %5s %5s %5s %8s %8s %8s\n",
		"logical/source", "upstream", "reqs", "err", "queued", "miss", "prompt", "completion", "total")
	tot := &sumRow{Key: "TOTAL"}
	for _, k := range order {
		r := rows[k]
		fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
			r.Key, r.Up, r.Reqs, r.Errs, r.Queued, r.Missing, r.P, r.C, r.T)
		tot.Reqs += r.Reqs
		tot.Errs += r.Errs
		tot.Queued += r.Queued
		tot.Missing += r.Missing
		tot.P += r.P
		tot.C += r.C
		tot.T += r.T
	}
	fmt.Printf("%-34s %-10s %6d %5d %5d %5d %8d %8d %8d\n",
		tot.Key, "", tot.Reqs, tot.Errs, tot.Queued, tot.Missing, tot.P, tot.C, tot.T)
}

// logFilesFor 列出要读的日志文件: 传目录则目录下全部 *.jsonl (按名排序),
// 传文件则它本身加最近归档 (覆盖 maxWindow 跨度)。
func logFilesFor(path string, maxWindow time.Duration) []string {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		ms, _ := filepath.Glob(filepath.Join(path, "*.jsonl"))
		sort.Strings(ms)
		return ms
	}
	return logFilesForScan(path, maxWindow)
}

// usageRow 是 -usage 的每行: 上游 + 真实模型 的 token 分口径累计。
type usageRow struct {
	Key                               string
	Reqs, Miss                        int
	Prompt, Completion, Cached, Total int
}

// parseDur 解析 "3h" / "12h" / "90m" / "7d" 这类窗口时长 (-usage 的窗口参数用)。
func parseDur(s string) (time.Duration, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch s[i:] {
	case "s":
		return time.Duration(n) * time.Second, true
	case "m", "min":
		return time.Duration(n) * time.Minute, true
	case "h":
		return time.Duration(n) * time.Hour, true
	case "d":
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// runUsage 按 上游+真实模型 列出各窗口的 token 用量 (服务只写日志, 分析在服务之外)。
func runUsage(path string, windows []string) {
	maxWindow := time.Duration(0)
	for _, w := range windows {
		if d, ok := parseDur(w); ok && d > maxWindow {
			maxWindow = d
		}
	}
	type entry struct {
		ts    time.Time
		up    string
		model string
		usage Usage
		miss  bool
	}
	var entries []entry
	for _, fp := range logFilesFor(path, maxWindow) {
		f, err := os.Open(fp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec logRec
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.Usage == nil {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, rec.Ts)
			if err != nil {
				continue
			}
			entries = append(entries, entry{ts: t, up: rec.Upstream, model: rec.Model, usage: *rec.Usage, miss: rec.Missing})
		}
		f.Close()
	}

	now := time.Now()
	groups := append(append([]string{}, windows...), "")
	for _, w := range groups {
		label, span := "全部", time.Duration(0)
		if w != "" {
			label = w
			span, _ = parseDur(w)
		}
		rows := map[string]*usageRow{}
		var order []string
		for _, e := range entries {
			if w != "" && now.Sub(e.ts) > span {
				continue
			}
			key := e.up + "/" + e.model
			r, ok := rows[key]
			if !ok {
				r = &usageRow{Key: key}
				rows[key] = r
				order = append(order, key)
			}
			r.Reqs++
			if e.miss {
				r.Miss++
			}
			r.Prompt += e.usage.Prompt
			r.Completion += e.usage.Completion
			r.Cached += e.usage.Cached
			r.Total += e.usage.Total
		}
		sort.Slice(order, func(i, j int) bool { return rows[order[i]].Total > rows[order[j]].Total })
		fmt.Printf("== 窗口 %s (仅含有 usage 的记录)\n", label)
		fmt.Printf("%-40s %6s %6s %9s %10s %9s %9s\n",
			"upstream/model", "reqs", "miss", "prompt", "completion", "cached", "total")
		for _, k := range order {
			r := rows[k]
			fmt.Printf("%-40s %6d %6d %9d %10d %9d %9d\n",
				r.Key, r.Reqs, r.Miss, r.Prompt, r.Completion, r.Cached, r.Total)
		}
	}
	fmt.Println("提示: 服务只写日志, 不做预算与折算; 额度分析交给独立的分析程序 (不在本仓库)")
}
