// tui_log_test.go: 日志吸收的聚合口径 (拆分 absorb 前的特征测试)。

package main

import (
	"fmt"
	"testing"
	"time"
)

// 一条日志同时进聚合行与事件流: 请求 / 错误 / 换候选 / token 总量 (total 缺省时
// prompt+completion 兜底) / cached / 首字节 / 三种速率样本 / 首末时间。
func TestAbsorbAggregatesRoute(t *testing.T) {
	m := newTUIModel("cfg")
	m.window, m.rateWin = 3*time.Hour, time.Hour
	ts := time.Now().Add(-time.Minute)
	line := fmt.Sprintf(`{"ts":%q,"logical":"lg","upstream":"u1","model":"m1",
	  "path":"/v1/chat/completions","status":502,"err":"boom","fallbacks":2,
	  "usage":{"prompt":100,"completion":50,"cached":30},
	  "duration_ms":2000,"bytes":1000,"decode_ms":500,"ttfb_ms":120}`,
		ts.Format(time.RFC3339Nano))
	m.absorb([]byte(line))
	row := m.routes["lg\x00u1\x00m1"]
	if row == nil {
		t.Fatalf("应生成聚合行: %+v", m.routes)
	}
	if row.Reqs != 1 || row.Errs != 1 || row.Fallbacks != 2 {
		t.Errorf("计数不对: %+v", row)
	}
	if row.Prompt != 100 || row.Cached != 30 || row.Total != 150 {
		t.Errorf("token 汇总不对 (total 应兜底成 prompt+completion): %+v", row)
	}
	if len(row.TtfbPts) != 1 || len(row.DecodeRate) != 1 ||
		len(row.TotalRate) != 1 || len(row.ByteRate) != 1 {
		t.Errorf("四类样本应各记一条: %+v", row)
	}
	if row.First.IsZero() || row.Last.IsZero() || row.First.After(row.Last) {
		t.Errorf("首末时间应填上: first=%v last=%v", row.First, row.Last)
	}
	if len(m.events) != 1 {
		t.Errorf("同一行日志应生成一条事件, 得到 %d", len(m.events))
	}

	// 坏 JSON / 空行不计数, 也不建行、不生成事件。
	m2 := newTUIModel("cfg")
	m2.absorb([]byte("{"))
	if m2.scanned != 0 || len(m2.routes) != 0 || len(m2.events) != 0 {
		t.Errorf("坏 JSON 应原样忽略: scanned=%d routes=%v events=%d",
			m2.scanned, m2.routes, len(m2.events))
	}
}
