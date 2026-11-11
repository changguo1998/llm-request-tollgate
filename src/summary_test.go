// summary_test.go: -summary / -usage 汇总口径的回归 (拆分 runSummary / runUsage 前).

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStdout 跑 fn 并取回它打到 stdout 的全部输出。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// writeLogs 把若干条日志写成目录下的一个 JSONL 文件。
func writeLogs(t *testing.T, dir string, recs []logRec) string {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// summary: 按 逻辑名+来源 分组 (source 缺省用 ua), 按 total 降序, 末行是 TOTAL。
func TestRunSummaryOutput(t *testing.T) {
	dir := t.TempDir()
	writeLogs(t, dir, []logRec{
		{Logical: "m1", Source: "agent", Upstream: "u1", Status: 200, Queued: true,
			Usage: &Usage{Prompt: 10, Completion: 5, Total: 15}},
		{Logical: "m1", UA: "cli", Upstream: "u1", Status: 500,
			Usage: &Usage{Prompt: 20, Completion: 10, Total: 30}},
		{Logical: "m2", Source: "agent", Upstream: "u2", Status: 200},
		{Logical: "", Source: "skip"}, // 没逻辑名: 跳过
	})
	out := captureStdout(t, func() { runSummary(dir) })
	for _, want := range []string{
		"m1 / agent", "m1 / cli", "m2 / agent",
		"TOTAL", "45", // 总 token 数
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary 输出缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "skip") {
		t.Errorf("没有逻辑名的记录不该入表:\n%s", out)
	}
	// total 大的组在前: m1 / cli (30) 在 m1 / agent (15) 之前。
	if strings.Index(out, "m1 / cli") > strings.Index(out, "m1 / agent") {
		t.Errorf("应按 total 降序:\n%s", out)
	}
}

// usage: 每个窗口一段, 只统计带 usage 的记录, 窗口外的按时间过滤; 末行是提示。
func TestRunUsageWindows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeLogs(t, dir, []logRec{
		{Ts: now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
			Upstream: "u1", Model: "real-1", Usage: &Usage{Prompt: 10, Completion: 5, Total: 15}},
		{Ts: now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			Upstream: "u1", Model: "real-1", Usage: &Usage{Prompt: 20, Completion: 10, Total: 30}},
		{Ts: now.Add(-3 * time.Hour).Format(time.RFC3339Nano),
			Upstream: "u2", Model: "real-2"}, // 没有 usage: 跳过
	})
	out := captureStdout(t, func() { runUsage(dir, []string{"1h", ""}) })
	for _, want := range []string{
		"== 窗口 1h", "== 窗口 全部", "u1/real-1", "提示: 服务只写日志",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage 输出缺少 %q:\n%s", want, out)
		}
	}
	// 1h 窗口只算第一笔 (total 15); 全部算两笔 (45)。
	part1 := out[strings.Index(out, "== 窗口 1h"):strings.Index(out, "== 窗口 全部")]
	if !strings.Contains(part1, "15") || strings.Contains(part1, "45") {
		t.Errorf("1h 窗口应只算最近一笔:\n%s", part1)
	}
	partAll := out[strings.Index(out, "== 窗口 全部"):]
	if !strings.Contains(partAll, "45") {
		t.Errorf("全部窗口应合计 45:\n%s", partAll)
	}
}
