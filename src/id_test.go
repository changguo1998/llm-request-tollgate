// id_test.go: 跨重启唯一的请求编号 —— 基准时刻 / 分钟数 / 分配规则。

package main

import (
	"testing"
	"time"
)

// monthBefore03: 月初 03:00 之后基准是当月 1 日 03:00, 之前基准是上月 1 日 03:00。
func TestMonthBefore03(t *testing.T) {
	loc := time.Local
	cases := []struct {
		now, want time.Time
	}{
		{time.Date(2026, 3, 1, 2, 59, 59, 0, loc), time.Date(2026, 2, 1, 3, 0, 0, 0, loc)},
		{time.Date(2026, 3, 1, 3, 0, 0, 0, loc), time.Date(2026, 3, 1, 3, 0, 0, 0, loc)},
		{time.Date(2026, 3, 31, 23, 59, 59, 0, loc), time.Date(2026, 3, 1, 3, 0, 0, 0, loc)},
		{time.Date(2026, 2, 1, 3, 0, 0, 0, loc), time.Date(2026, 2, 1, 3, 0, 0, 0, loc)},
		{time.Date(2026, 2, 1, 2, 59, 59, 0, loc), time.Date(2026, 1, 1, 3, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := monthBefore03(c.now); !got.Equal(c.want) {
			t.Errorf("monthBefore03(%v) = %v, 期望 %v", c.now, got, c.want)
		}
	}
}

func TestMinsSinceBase(t *testing.T) {
	loc := time.Local
	base := time.Date(2026, 3, 1, 3, 0, 0, 0, loc)
	cases := []struct {
		d    time.Duration
		want int
	}{
		{0, 0},
		{59 * time.Second, 0},
		{60 * time.Second, 1},
		{61 * time.Second, 1},
		{24 * time.Hour, 1440},
	}
	for _, c := range cases {
		if got := minsSinceBase(base.Add(c.d), base); got != c.want {
			t.Errorf("minsSinceBase(+%v) = %d, 期望 %d", c.d, got, c.want)
		}
	}
}

// flowStart 连续分配: 同一分钟内序号从 0 递增, 编号从 100 起 (不与「无编号 0」冲突),
// 且与纯函数算出的分钟数一致。
func TestFlowStartNumbering(t *testing.T) {
	a := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	now := time.Now()
	mins := minsSinceBase(now, monthBefore03(now))
	id1 := a.flowStart("m1", "", "/v1/chat/completions", true)
	id2 := a.flowStart("m1", "", "/v1/chat/completions", true)
	id3 := a.flowStart("m1", "", "/v1/chat/completions", true)
	if id1 != uint64(mins+1)*100 {
		t.Errorf("第 1 笔编号应 = (mins+1)*100 = %d, 得到 %d (mins=%d)", (mins+1)*100, id1, mins)
	}
	if id1 < 100 || id1%100 != 0 {
		t.Errorf("第 1 笔编号应从 100 起 (该分钟序号 0): %d", id1)
	}
	if id2 != id1+1 || id3 != id1+2 {
		t.Errorf("同一分钟内编号应连续 +1: %d %d %d", id1, id2, id3)
	}
	// 新进程 (重启) 重新分配: 编号仍从该分钟序号 0 起, 但数值形式一致且非零。
	a2 := newTestApp(t, "http://127.0.0.1:1", GateCfg{}, nil)
	rid := a2.flowStart("m1", "", "/v1/chat/completions", true)
	if rid < 100 || rid%100 != 0 {
		t.Errorf("新进程第 1 笔编号应从 100 起: %d", rid)
	}
}
