// tui_run_test.go: 主循环按键分派的回归 (拆分 handleKey / runTUI 前的特征测试)。

package main

import "testing"

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
