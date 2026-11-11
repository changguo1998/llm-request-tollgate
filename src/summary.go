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
	files, err := summaryFiles(path)
	if err != nil {
		log.Fatalf("summary: %v", err)
	}
	rows, order, err := summarize(files)
	if err != nil {
		log.Fatalf("summary: %v", err)
	}
	printSummary(rows, order)
}

// summaryFiles 是 -summary 要读的文件: 目录取其中全部 *.jsonl (跨天归档), 文件就它自己。
func summaryFiles(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return []string{path}, nil
	}
	ms, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
	if err != nil || len(ms) == 0 {
		return nil, fmt.Errorf("no *.jsonl under %s", path)
	}
	sort.Strings(ms)
	return ms, nil
}

// summarize 逐文件累计分组行: 按 逻辑名+来源 分组 (来源取 source, 缺省用 ua),
// order 是首次出现顺序, 之后按 total 降序排。
func summarize(files []string) (map[string]*sumRow, []string, error) {
	rows := map[string]*sumRow{}
	var order []string
	for _, fp := range files {
		f, err := os.Open(fp)
		if err != nil {
			return nil, nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			order = addSumRecord(rows, order, sc.Bytes())
		}
		if err := sc.Err(); err != nil {
			f.Close()
			return nil, nil, err
		}
		f.Close()
	}
	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].T > rows[order[j]].T
	})
	return rows, order, nil
}

// addSumRecord 把一行日志累计进分组表 (按 逻辑名+来源); 返回可能追加了 key 的 order。
func addSumRecord(rows map[string]*sumRow, order []string, line []byte) []string {
	var rec logRec
	if err := json.Unmarshal(line, &rec); err != nil || rec.Logical == "" {
		return order
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
	return order
}

// printSummary 打印表头 / 各行 / TOTAL。
func printSummary(rows map[string]*sumRow, order []string) {
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
	entries := loadUsageEntries(logFilesFor(path, maxWindowOf(windows)))
	now := time.Now()
	groups := append(append([]string{}, windows...), "")
	for _, w := range groups {
		printUsageWindow(w, entries, now)
	}
	fmt.Println("提示: 服务只写日志, 不做预算与折算; 额度分析交给独立的分析程序 (不在本仓库)")
}

// maxWindowOf 取窗口参数里最长的跨度 (决定要读哪些历史归档)。
func maxWindowOf(windows []string) time.Duration {
	maxWindow := time.Duration(0)
	for _, w := range windows {
		if d, ok := parseDur(w); ok && d > maxWindow {
			maxWindow = d
		}
	}
	return maxWindow
}

// usageEntry 是一条带 usage 的日志 (按时间窗口汇总用)。
type usageEntry struct {
	ts    time.Time
	up    string
	model string
	usage Usage
	miss  bool
}

// loadUsageEntries 读日志文件里的带 usage 记录 (没 usage / 时间解析失败的跳过)。
func loadUsageEntries(files []string) []usageEntry {
	var entries []usageEntry
	for _, fp := range files {
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
			entries = append(entries, usageEntry{ts: t, up: rec.Upstream, model: rec.Model, usage: *rec.Usage, miss: rec.Missing})
		}
		f.Close()
	}
	return entries
}

// printUsageWindow 打印一个窗口的分组表: 上游/模型 按 total 降序; 空 w = 全部。
func printUsageWindow(w string, entries []usageEntry, now time.Time) {
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
