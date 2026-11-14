// tui_run_test.go: 主循环按键分派的回归 (拆分 handleKey / runTUI 前的特征测试)。

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 转义序列按 3 字节收全再处理: 方向键上下移动, 左右切页, PgUp / PgDn 是 4 字节;
// 收不全时保留 pending, 不误当成普通键。
func TestHandleKeyEscapeSequences(t *testing.T) {
	m := newTUIModel("cfg")
	m.tab = tabActive
	var pending []byte
	send := func(b byte) bool {
		t.Helper()
		return m.handleKey(b, &pending)
	}
	// 单个 ESC: 还在等后续字节。
	if quit := send(0x1b); quit || len(pending) != 1 {
		t.Fatalf("单个 ESC 应留在 pending: quit=%v pending=%q", quit, pending)
	}
	send('[')
	if len(pending) != 2 {
		t.Fatalf("ESC [ 应还在等, pending=%q", pending)
	}
	m.scroll[tabActive] = 5
	send('B') // 下方向键
	if len(pending) != 0 || m.scroll[tabActive] != 6 {
		t.Fatalf("下键应滚动 +1 并清空 pending: scroll=%d pending=%q", m.scroll[tabActive], pending)
	}

	// 左右切页: 不再经过普通键表。
	send(0x1b)
	send('[')
	send('C')
	if m.tab != tabEvents || len(pending) != 0 {
		t.Fatalf("右键应切到下一页: tab=%d pending=%q", m.tab, pending)
	}
	send(0x1b)
	send('[')
	send('D')
	if m.tab != tabActive {
		t.Fatalf("左键应切回上一页: tab=%d", m.tab)
	}

	// PgDn 是 ESC [ 6 ~: 第 3 字节时还不能动, 第 4 字节到了才按 11 行滚动。
	m.scroll[tabActive] = 5
	send(0x1b)
	send('[')
	send('6')
	if m.scroll[tabActive] != 5 || len(pending) != 3 {
		t.Fatalf("PgDn 收全前不该动: scroll=%d pending=%q", m.scroll[tabActive], pending)
	}
	send('~')
	if m.scroll[tabActive] != 16 || len(pending) != 0 {
		t.Fatalf("PgDn 应滚动 +11: scroll=%d pending=%q", m.scroll[tabActive], pending)
	}
}

// 普通键在 events 页交给筛选, 其余页仍按通用表: r 清空聚合与事件, G 跳到底, q 退出。
func TestHandleKeyCommonKeys(t *testing.T) {
	m := newTUIModel("cfg")
	var pending []byte
	m.routes = map[string]*routeRow{"k": {Reqs: 1}}
	m.events = []routeEvent{{Msg: "x"}}
	m.handleKey('r', &pending)
	if len(m.routes) != 0 || len(m.events) != 0 || m.scanned != 0 {
		t.Errorf("r 应清空聚合与事件: routes=%d events=%d scanned=%d", len(m.routes), len(m.events), m.scanned)
	}
	m.handleKey('G', &pending)
	if m.scroll[m.tab] != 1<<30 {
		t.Errorf("G 应跳到底: %d", m.scroll[m.tab])
	}
	if quit := m.handleKey('q', &pending); !quit {
		t.Error("q 应退出")
	}
}

// 单独按下的 Esc (readKeys 用等待分辨出来后带 esc 标记送来) 在第 1 页清掉行光标:
// 两张表全都不选中 (没有反显格), 再按 j / k 重新选起 (j 第一行, k 最后一行)。
func TestEscClearsFlowCursor(t *testing.T) {
	m := flowFixture(t)
	var pending []byte
	m.cur = flowCursor{table: 0, up: "u1", name: true}
	raw := m.draw()
	if !strings.Contains(raw, tuiRev+m.upColors["u1"]+"u1") {
		t.Fatalf("选中时上游名应反显: %q", raw)
	}

	if quit := m.handleKeyEv(tuiKey{esc: true}, &pending); quit {
		t.Fatalf("Esc 不该退出")
	}
	if !m.cur.off {
		t.Fatalf("Esc 应清掉光标: %+v", m.cur)
	}
	raw = m.draw()
	for _, id := range []string{"u1", "u2"} {
		c := m.upColors[id]
		if strings.Contains(raw, tuiRev+c+id) || strings.Contains(raw, tuiRev+" "+c+id) {
			t.Errorf("Esc 后不该还有反显的光标格 (%s)", id)
		}
	}

	// 没选中行时 u 只提示先选一行, 不去发请求。
	m.flowUnlock()
	if !m.flowErr || !strings.Contains(m.flowMsg, "j/k") {
		t.Errorf("没选中行时解锁应提示先选一行: %q", m.flowMsg)
	}

	// j 重新选起 = 第一行; 再 Esc 后 k = 最后一行。
	if quit := m.handleKey('j', &pending); quit {
		t.Fatalf("j 不该退出")
	}
	if m.cur.off || m.cur.table != 0 || m.cur.up != "u1" || !m.cur.name {
		t.Fatalf("Esc 后 j 应选中第一行: %+v", m.cur)
	}
	m.handleKeyEv(tuiKey{esc: true}, &pending)
	m.handleKey('k', &pending)
	if m.cur.off || m.cur.table != 1 || m.cur.logical != "m2" || m.cur.model != "real-2" {
		t.Fatalf("Esc 后 k 应选中最后一行: %+v", m.cur)
	}
}

// Esc 在输入态里取消输入 (settings 页的行内输入 / 第 2 页的筛选框), 不把半个转义序列
// 留在 pending 里, 也不去动第 1 页的光标。
func TestEscCancelsPrompt(t *testing.T) {
	m := flowFixture(t)
	var pending []byte
	m.tab = tabSettings
	m.prio = &tuiEditor{prompt: &tuiPrompt{label: "新的优先级数值:"}}
	if quit := m.handleKeyEv(tuiKey{esc: true}, &pending); quit || m.prio.prompt != nil {
		t.Fatalf("settings 页 Esc 应取消输入: quit=%v prompt=%v", quit, m.prio.prompt)
	}
	m.tab = tabEvents
	m.startEvPrompt(true)
	if quit := m.handleKeyEv(tuiKey{esc: true}, &pending); quit || m.evPrompt != nil {
		t.Fatalf("events 页 Esc 应取消输入: quit=%v prompt=%v", quit, m.evPrompt)
	}
	if m.cur.off {
		t.Errorf("events 页的 Esc 不该清第 1 页光标")
	}
}

// tuiInputReady 是分辨 Esc 的探针: 没字节就等满超时返回 false, 有字节立刻返回 true。
func TestInputReady(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if tuiInputReady(r.Fd(), 10*time.Millisecond) {
		t.Error("空管道不该报有输入")
	}
	if _, err := w.Write([]byte("[")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !tuiInputReady(r.Fd(), 500*time.Millisecond) {
		t.Error("管道里有字节时应报有输入")
	}
}
