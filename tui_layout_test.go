package main

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// 帧必须刚好铺满终端高度, 且键位提示贴最后一行 —— 曾经踩过的坑:
// 补空行的循环条件里放了 countLines(), 每轮它都在长, 条件自己收紧, 循环提前退出,
// 于是提示浮在屏幕中间, 下面留一片空白。
func TestDrawFillsTerminalHeight(t *testing.T) {
	// 借一个 pty, 让 tuiSize() 拿到指定行列
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	defer m.Close()
	var n uint32
	ioctlCall(m.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n))
	var zero int32
	ioctlCall(m.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&zero))
	sl, err := os.OpenFile("/dev/pts/"+itoa(int(n)), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	defer sl.Close()

	old := os.Stdout
	os.Stdout = sl
	defer func() { os.Stdout = old }()

	// 排空 master, 否则写满缓冲区会卡住
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			if _, err := m.Read(buf); err != nil {
				return
			}
		}
	}()

	for _, rows := range []int{45, 24, 60} {
		ioctlCall(m.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&tuiWinsize{Row: uint16(rows), Col: 120}))
		mo := newTUIModel("cfg")
		mo.pulse.Cands = []candView{{ID: "ustc", SoftCap: 1}}
		for _, tab := range []int{0, 1, 2, 3} {
			mo.tab = tab
			out := mo.draw()
			got := strings.Count(out, "\r\n")
			// 键位提示贴底。settings 页的提示里没有"切页", 所以按各页都有的 "q 退出" 定位。
			hintRow := strings.Count(out[:strings.Index(out, "q 退出")], "\r\n") + 1
			t.Logf("rows=%d tab=%d 帧行数(CRLF)=%d 键位行=%d", rows, tab, got, hintRow)
			if hintRow != rows {
				t.Errorf("rows=%d tab=%d: 键位应贴底 (第 %d 行), 实际第 %d 行", rows, tab, rows, hintRow)
			}
			// 顶端只留页签, 不再画上游状态 (上游状态在 models 页看)。
			if strings.Contains(out, "ustc 2/3") {
				t.Errorf("rows=%d tab=%d: 顶端不该出现上游状态", rows, tab)
			}
			// 左边界的缩进统一: 每个非空行的可见文本都从 tuiIndent 起。
			for i, line := range strings.Split(out, "\r\n") {
				vis := stripANSI(line)
				if vis == "" {
					continue
				}
				if !strings.HasPrefix(vis, tuiIndent) {
					t.Errorf("rows=%d tab=%d 第 %d 行左边界不是 %d 空格: %q",
						rows, tab, i+1, len(tuiIndent), vis)
				}
				if strings.HasPrefix(strings.TrimPrefix(vis, tuiIndent), "|") {
					t.Errorf("rows=%d tab=%d 第 %d 行行首多了竖线: %q", rows, tab, i+1, vis)
				}
			}
			// 键位提示用正常前景色: 不带 dim。
			for _, line := range strings.Split(out, "\r\n") {
				if strings.Contains(line, "q 退出") && strings.Contains(line, tuiDim) {
					t.Errorf("rows=%d tab=%d: 键位提示不该是 dim 色", rows, tab)
				}
			}
		}
	}
}

// stripANSI 去掉 CSI 序列, 只留可见文本 (缩进断言要看可见列)。
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if n := ansiSpan(s, i); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func ioctlCall(fd uintptr, req uintptr, arg unsafe.Pointer) {
	syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
}

// openTestPTY 把 stdout 指到一块 rows x cols 的 pty, 让 draw() 量到指定行列
// (测试结束复原)。第 1 页的分块撑到 30 行以上, 要断言整页就得给高一点的窗口。
func openTestPTY(t *testing.T, rows, cols int) {
	t.Helper()
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	var n uint32
	ioctlCall(f.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n))
	var zero int32
	ioctlCall(f.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&zero))
	sl, err := os.OpenFile("/dev/pts/"+itoa(int(n)), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	old := os.Stdout
	os.Stdout = sl
	t.Cleanup(func() {
		os.Stdout = old
		sl.Close()
		f.Close()
	})
	// 排空 master, 否则写满缓冲区会卡住
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	ioctlCall(f.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&tuiWinsize{Row: uint16(rows), Col: uint16(cols)}))
}

// autoCols 的分配规则: 列宽按内容涨 (下限 w, 上限是终端宽), 余量先补齐差得少的列;
// 余量不够时是内容被截断, 而不是整列消失。
func TestAutoColsAllocatesByContent(t *testing.T) {
	cols := []tuiCol{
		{"编号", 4, true, true, 1},
		{"逻辑名", 10, true, false, 0},
		{"路径", 12, true, false, 3},
	}
	rows := [][]string{{"12345", "claude-sonnet-4", "/v1/chat/completions"}}
	// 基准宽 26 + 列间距 2 + 缩进 2 = 30, 内容宽 40 + 间距 2 + 缩进 2 = 44
	// (编号 5 / 逻辑名 15 / 路径 20)。
	cases := []struct {
		width int
		want  [3]int
		why   string
	}{
		{200, [3]int{5, 15, 20}, "宽终端: 各列涨到内容宽"},
		{38, [3]int{5, 15, 14}, "余量 8: 先补齐编号 (差 1), 再喂逻辑名 (差 5), 剩下 2 给路径"},
		{33, [3]int{5, 12, 12}, "余量 3: 补完编号 (差 1) 再补逻辑名 2"},
		{20, [3]int{4, 10, 12}, "连基准宽都放不下: 原样返回, 交给 fitCols 丢列"},
	}
	for _, c := range cases {
		got := autoCols(cols, rows, c.width)
		if got[0].w != c.want[0] || got[1].w != c.want[1] || got[2].w != c.want[2] {
			t.Errorf("width=%d (%s): 期望 %v, 实际 %d/%d/%d",
				c.width, c.why, c.want, got[0].w, got[1].w, got[2].w)
		}
	}
}

// 列宽不写死上限: 长内容一路涨到终端右边界, 顶满之后才截断; 内容都放得下就停在内容宽。
func TestAutoColsFillsToBoundary(t *testing.T) {
	cols := []tuiCol{
		{"编号", 4, true, true, 1},
		{"逻辑名", 10, true, false, 0},
		{"路径", 12, true, false, 3},
	}
	rows := [][]string{{"1", "m1", strings.Repeat("p", 67)}}

	// 80 列: 余量 50 全给路径列 (67 列内容放不下), 表右边界正好顶到 80。
	got := autoCols(cols, rows, 80)
	if w := tableWidth(got); w != 80 {
		t.Errorf("长行应顶到右边界 80, 实际表宽 %d", w)
	}
	if got[2].w != 62 {
		t.Errorf("路径列应拿走余量 (62), 实际 %d", got[2].w)
	}
	// 200 列: 内容都放得下, 各列停在内容宽, 不再多占。
	if w := tableWidth(autoCols(cols, rows, 200)); w != 2+4+tuiColGap+10+tuiColGap+67 {
		t.Errorf("内容放得下时不该再涨, 实际表宽 %d", w)
	}
}

// active 页主表在 120 列终端上不该再丢列: 用截断次要列换全部 8 列可见。
func TestActiveTableKeepsAllColsAt120(t *testing.T) {
	rows := [][]string{{
		"12345", "1m02s", "claude-3-5-sonnet", "group/claude-sonnet-4-5-20250929",
		"是", "claude-code", "/v1/messages", "额度锁 09:12",
	}}
	cols := autoCols(activeCols, rows, 120)
	if w := tableWidth(cols); w > 120 {
		t.Errorf("按内容分配后仍超出终端: %d > 120", w)
	}
	for i, ok := range fitCols(cols, 120) {
		if !ok {
			t.Errorf("120 列终端下 %q 列不该被丢", activeCols[i].title)
		}
	}
}

// 回归: 中间列整列为空被 colKeep 丢掉 (如全部请求都没有来源) 之后, 后面的
// 路径列不能再按错位的 cell 算宽 —— 曾经列宽停在基准值, 长路径被提前截断,
// 终端明明够宽也打出 ~。现在行列按同一份 keep 对齐, 宽终端下路径应完整显示。
func TestFlowDroppedColKeepsLaterColWidth(t *testing.T) {
	openTestPTY(t, 40, 160)
	m := newTUIModel("cfg")
	m.tab = tabActive
	now := time.Now()
	path := strings.Repeat("p", 60)
	m.pulse.Active = []flow{{
		ID: 7, Logical: "claude-3-5-sonnet", Up: "group", Model: "claude-sonnet-4-5-20250929",
		Stage: stageSend, StartTime: now, Path: path,
	}}
	out := stripANSI(m.draw())
	if strings.Contains(out, "~") {
		t.Errorf("宽终端下第 1 页不该出现截断符 ~:\n%v", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("长路径应完整显示, 实际被截断:\n%v", out)
	}
}

// 路由表按最后使用时间降序 (最近用过的排最上面), 同一时刻按请求数兜底。
func TestRouteRowsSortByLastUse(t *testing.T) {
	now := time.Now()
	m := newTUIModel("cfg")
	m.routes = map[string]*routeRow{
		"old":  {Logical: "a", Upstream: "u1", Model: "m-old", Reqs: 99, Last: now.Add(-2 * time.Hour)},
		"new":  {Logical: "b", Upstream: "u2", Model: "m-new", Reqs: 1, Last: now.Add(-time.Minute)},
		"mid":  {Logical: "c", Upstream: "u3", Model: "m-mid", Reqs: 5, Last: now.Add(-30 * time.Minute)},
		"same": {Logical: "d", Upstream: "u4", Model: "m-same", Reqs: 7, Last: now.Add(-time.Minute)},
	}
	got := m.routeRows()
	want := []string{"m-same", "m-new", "m-mid", "m-old"} // 同一时刻: 请求数 7 > 1
	for i, w := range want {
		if got[i].Model != w {
			t.Fatalf("第 %d 行应为 %s, 实际顺序 %v", i+1, w, []string{
				got[0].Model, got[1].Model, got[2].Model, got[3].Model})
		}
	}
}

// 上游与真实模型合成一格: 两边都在写 上游/模型, 缺一边不留斜杠 (锁 / 闸门事件没有模型)。
func TestUpModelJoin(t *testing.T) {
	cases := []struct{ up, model, want string }{
		{"ustc", "gpt-5-codex", "ustc/gpt-5-codex"},
		{"", "gpt-5-codex", "gpt-5-codex"},
		{"ustc", "", "ustc"},
		{"", "", ""},
	}
	m := newTUIModel("cfg") // 没分到颜色时, 上游名原样输出
	for _, c := range cases {
		if got := m.upModel(c.up, c.model); got != c.want {
			t.Errorf("upModel(%q, %q) = %q, 期望 %q", c.up, c.model, got, c.want)
		}
	}
}

// 每张表都有标题行 (标题在表头之上); events 页滚动到底不再画成空白 (G 键把滚动设成很大)。
func TestTableTitlesAndEventsScroll(t *testing.T) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	defer f.Close()
	var n uint32
	ioctlCall(f.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n))
	var zero int32
	ioctlCall(f.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&zero))
	sl, err := os.OpenFile("/dev/pts/"+itoa(int(n)), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	defer sl.Close()
	old := os.Stdout
	os.Stdout = sl
	defer func() { os.Stdout = old }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()

	ioctlCall(f.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&tuiWinsize{Row: 40, Col: 140}))
	m := newTUIModel("cfg")
	m.pulse.Cands = []candView{{ID: "ustc", SoftCap: 1}}
	m.tab = tabActive
	out := m.draw()
	for _, title := range []string{"并发池", "频率队列", "状态锁", "已发送", "接收中", "已失败"} {
		if !strings.Contains(stripANSI(out), title) {
			t.Errorf("active 页缺少分块 %q", title)
		}
	}
	m.tab = tabModels
	out = m.draw()
	for _, title := range []string{"模型队列", "上游状态", "路由聚合"} {
		if !strings.Contains(stripANSI(out), title) {
			t.Errorf("models 页缺少标题 %q", title)
		}
	}
	if strings.Index(out, "上游状态") > strings.Index(out, "模型队列") {
		t.Errorf("上游状态应排在模型队列之前")
	}

	// 事件多于窗口, 再把滚动位置设成 G 键那种大数: 必须显出最后一屏, 不能空白。
	m.tab = tabEvents
	for i := 0; i < 200; i++ {
		m.events = append(m.events, routeEvent{
			Ts: time.Now().Add(-time.Duration(i) * time.Second), Kind: "ok",
			Logical: "m1", Upstream: "ustc", Model: "real-m1",
			Msg: "状态码 200 #" + itoa(i), // 各不相同, 别被折成一行 xN
		})
	}
	m.scroll[tabEvents] = 1 << 30
	out = stripANSI(m.draw())
	if !strings.Contains(out, "事件流") {
		t.Error("events 页缺少标题")
	}
	if !strings.Contains(out, "状态码 200 #") {
		t.Error("滚动到底应显出最后几行事件, 实际画成空白")
	}
	if !strings.Contains(out, "200 行, 当前 ") {
		t.Error("事件多于窗口时应显示当前窗口位置")
	}
}

// 第 1 页按流程阶段分块: 顺序与调度器的真实步骤一致, 每笔请求落在自己的块里。
func TestFlowBlocksByStage(t *testing.T) {
	openTestPTY(t, 50, 140)
	m := newTUIModel("cfg")
	now := time.Now()
	m.pulse.Active = []flow{
		{ID: 1, Logical: "parse-me", Up: "ustc", Model: "m1", Stage: stageParse, StartTime: now},
		{ID: 2, Logical: "sem-me", Up: "ustc", Model: "m1", Stage: stageSem, StartTime: now},
		{ID: 3, Logical: "queue-me", Up: "ustc", Model: "m2", Stage: stageQueue, StartTime: now},
		{ID: 4, Logical: "model-me", Up: "group", Model: "m3", Stage: stageModelWait, StartTime: now},
		{ID: 5, Logical: "header-me", Up: "group", Model: "m3", Stage: stageSend, StartTime: now},
		{ID: 6, Logical: "recv-me", Up: "group", Model: "m3", Stage: stageRecv, StartTime: now},
	}
	out := stripANSI(m.draw())
	for _, s := range []string{"并发池 (2)", "频率队列 (1)",
		"状态锁 (1)", "已发送 (1)", "接收中 (1)", "已失败 (0)"} {
		if !strings.Contains(out, s) {
			t.Errorf("缺少分块 %q", s)
		}
	}
	for _, s := range []string{"parse-me", "sem-me", "queue-me", "model-me", "header-me", "recv-me"} {
		if !strings.Contains(out, s) {
			t.Errorf("活动连接 %q 没画出来", s)
		}
	}
	// 块序 = 流程顺序。
	order := []string{"并发池", "频率队列", "状态锁", "已发送", "接收中", "已失败"}
	last := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i < last {
			t.Errorf("分块顺序不对: %q 出现在前一阶段之前", s)
		}
		last = i
	}
}

// flowTitleRows 找出第 1 页各分块标题所在的行号 (下标对应 flowBlocks 的顺序)。
func flowTitleRows(t *testing.T, out string) []int {
	t.Helper()
	lines := strings.Split(stripANSI(out), "\r\n")
	rows := make([]int, len(flowBlocks))
	for i := range rows {
		rows[i] = -1
	}
	for n, ln := range lines {
		text := strings.TrimPrefix(ln, tuiIndent)
		for i, b := range flowBlocks {
			// 只认行首的标题, 别把「状态」列里同名的阶段 (如 接收中) 当成标题。
			if strings.HasPrefix(text, b.title) {
				rows[i] = n
			}
		}
	}
	for i, row := range rows {
		if row < 0 {
			t.Fatalf("分块 %q 没画出来", flowBlocks[i].title)
		}
	}
	return rows
}

// 分块标题之间至少隔 2 行: 数据行区按最小行数补齐, 块之间再空一行。
func TestFlowBlockTitleSpacing(t *testing.T) {
	openTestPTY(t, 50, 140)
	m := newTUIModel("cfg")
	m.tab = tabActive
	rows := flowTitleRows(t, m.draw())
	for i := 1; i < len(rows); i++ {
		if gap := rows[i] - rows[i-1] - 1; gap < 2 {
			t.Errorf("分块 %q 与 %q 之间只隔 %d 行, 至少要 2 行",
				flowBlocks[i-1].title, flowBlocks[i].title, gap)
		}
	}
}

// 有内容的块自带行与补齐的空行, 标题之间同样不会贴在一起。
func TestFlowBlockTitleSpacingWithRows(t *testing.T) {
	openTestPTY(t, 50, 140)
	m := newTUIModel("cfg")
	m.tab = tabActive
	now := time.Now()
	m.pulse.Active = []flow{
		{ID: 1, Logical: "queue-me", Up: "ustc", Model: "m1", Stage: stageQueue, StartTime: now},
		{ID: 2, Logical: "recv-me", Up: "ustc", Model: "m1", Stage: stageRecv, StartTime: now},
	}
	rows := flowTitleRows(t, m.draw())
	for i := 1; i < len(rows); i++ {
		if gap := rows[i] - rows[i-1] - 1; gap < 2 {
			t.Errorf("分块 %q 与 %q 之间只隔 %d 行, 至少要 2 行",
				flowBlocks[i-1].title, flowBlocks[i].title, gap)
		}
	}
}

// 第 1 页的版式: 列名整页只画一次 (在页首), 块与块之间空一行; 每块的数据行区正好
// 留 defaultMinRows[i] 行 (没有历史停留时长时), 空块也照留。表后不再有单位说明行。
func TestFlowSingleHeaderAndMinRows(t *testing.T) {
	openTestPTY(t, 60, 160)
	m := newTUIModel("cfg")
	m.tab = tabActive
	m.pulse.Active = []flow{
		{ID: 1, Logical: "queue-me", Up: "ustc", Model: "m1", Stage: stageQueue, StartTime: time.Now()},
	}
	out := stripANSI(m.draw())

	if n := strings.Count(out, "上游/模型"); n != 1 {
		t.Errorf("列名应整页只画一次, 实际 %d 次", n)
	}
	if strings.Index(out, "上游/模型") > strings.Index(out, "并发池") {
		t.Error("列名应排在第一块标题之前 (页首)")
	}
	if strings.Contains(out, "单位:") {
		t.Error("表格后面不该再画单位说明")
	}

	lines := strings.Split(out, "\r\n")
	// 页首: 空一行再画列名 (列名不顶在页签下面), 且列名在第一块标题之前。
	headRow := -1
	for i, ln := range lines {
		if strings.Contains(ln, "编号") {
			headRow = i
			break
		}
	}
	if headRow < 0 {
		t.Fatal("没画出列名行")
	}
	if headRow == 0 || strings.TrimSpace(lines[headRow-1]) != "" {
		t.Errorf("列名上一行应是空行, 实际 %q", lines[headRow-1])
	}
	// 块间距 = 上一块标题 + 1 + 数据行区 defaultMinRows + 空行; 空块也照留, 所以位置固定。
	titles := flowTitleRows(t, out)
	for i := 1; i < len(titles); i++ {
		want := titles[i-1] + 1 + defaultMinRows[i-1] + 1
		if titles[i] != want {
			t.Errorf("分块 %q 起始行 %d, 应为 %d (上一块 %d 行数据 + 空行)",
				flowBlocks[i].title, titles[i], want, defaultMinRows[i-1])
		}
	}
}

// 没有进行中的请求时, 第 1 页列名也照常显示 (不能因为整列为空把列名一起收掉)。
func TestFlowHeaderAlwaysShown(t *testing.T) {
	openTestPTY(t, 24, 120)
	m := newTUIModel("cfg")
	m.tab = tabActive
	out := stripANSI(m.draw())
	for _, name := range []string{"编号", "耗时", "逻辑名", "上游/模型", "流式", "来源", "路径"} {
		if !strings.Contains(out, name) {
			t.Errorf("空表也应显示列名 %q", name)
		}
	}
}

// 第 1 页列名固定在页签下方: 滚动分块时列名不跟着滚走。
func TestFlowHeaderPinned(t *testing.T) {
	openTestPTY(t, 24, 100)
	m := newTUIModel("cfg")
	m.tab = tabActive
	for i := 1; i <= 6; i++ {
		m.pulse.Active = append(m.pulse.Active, flow{
			ID: uint64(i), Logical: "m", Up: "ustc", Model: "m1",
			Stage: stageQueue, StartTime: time.Now(),
		})
	}
	m.scroll[tabActive] = 5
	out := stripANSI(m.draw())
	lines := strings.Split(out, "\r\n")
	headRow := -1
	for i, ln := range lines {
		if strings.Contains(ln, "编号") {
			headRow = i
			break
		}
	}
	if headRow < 0 {
		t.Fatal("滚动后列名不见了")
	}
	if headRow > 4 {
		t.Errorf("列名应固定在页签下方, 实际在第 %d 行", headRow+1)
	}
	if strings.Contains(out, "并发池") {
		t.Error("滚动后分块内容应已上移 (第一块不在窗口里)")
	}
}

// 第 2 页从模型视角画: 每个真实模型的队列 / 退避 / 熔断锁, 以及上游按记账组铺开的锁列。
func TestModelsPage(t *testing.T) {
	m := newTUIModel("cfg")
	m.tab = tabModels
	m.pulse.Cands = []candView{{
		ID: "ustc", InFlight: 2, SoftCap: 3, Queue: 4, NextMs: 12000, LockMs: 60000, LockWhy: "quota",
		Groups: []candGroup{{Name: "g1", LockMs: 60000}, {Name: "g2"}},
		Models: []candModelLock{
			{Model: "deepseek-flash", Queue: 2},
			{Model: "glm-5.3", PausedMs: 20000, Fails: 3},
			{Model: "kimi-k3", LeftMs: 300000, Why: "breaker"},
		},
	}}
	out := stripANSI(m.draw())
	for _, s := range []string{"模型队列", "上游/真实模型", "ustc/deepseek-flash", "ustc/glm-5.3",
		"ustc/kimi-k3", "上游状态", "记账", "g2", "4[/12s]", "01:00", "路由聚合"} {
		if !strings.Contains(out, s) {
			t.Errorf("models 页缺少 %q", s)
		}
	}
	// 锁中的组用倒计时占位, 组名不再出现。
	if strings.Contains(out, "g1") {
		t.Error("锁中的记账组应显示倒计时, 不该再显示组名 g1")
	}
	// 上游状态表与下面的汇总 / 模型队列之间空一行 (表后没有单位行了)。
	lines := strings.Split(out, "\r\n")
	for i, ln := range lines {
		if !strings.Contains(ln, "ustc  2(+1)/3") {
			continue
		}
		if i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "" {
			t.Errorf("上游状态表后面应空一行: %q", ln)
		}
	}
	if strings.Contains(out, "单位:") {
		t.Error("表格后面不该再画单位说明")
	}
}

// 第 2 页合并表的列按类别分组: 调度状态 / 历史计数 / 流量 / 时间, 同类相邻;
// 原来的 30 分钟小方条那列已去掉。
func TestModelsColumnGroups(t *testing.T) {
	got := make([]string, 0, len(modelCols()))
	for _, c := range modelCols() {
		got = append(got, c.title)
	}
	want := []string{
		"上游/模型",
		"排队", "退避", "熔断", "连错",
		"请求", "错误", "换候选",
		"token", "缓存", "首字节", "吞吐",
		"最近",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("列序应按类别分组:\n得到 %v\n期望 %v", got, want)
	}
}

// 第 2 页: 队列与路由聚合成一张表, 行按 上游/真实模型 合并 (同模型多逻辑名求和),
// 表里不出现逻辑名。
func TestModelsMergedRow(t *testing.T) {
	m := newTUIModel("cfg")
	now := time.Now()
	m.pulse.Cands = []candView{{
		ID: "ustc", Models: []candModelLock{{Model: "m1", Queue: 4, Fails: 1}},
	}}
	m.routes = map[string]*routeRow{
		"alias-a\x00ustc\x00m1": {
			Logical: "alias-a", Upstream: "ustc", Model: "m1", Reqs: 2, Total: 100,
			TtfbSum: 200, TtfbN: 2, Last: now.Add(-time.Minute),
		},
		"alias-b\x00ustc\x00m1": {
			Logical: "alias-b", Upstream: "ustc", Model: "m1", Reqs: 3, Total: 50,
			TtfbSum: 100, TtfbN: 2, Last: now,
		},
	}
	rows := m.modelRows()
	if len(rows) != 1 {
		t.Fatalf("同上游/模型应合成一行, 得到 %d 行", len(rows))
	}
	r := rows[0]
	if r.Reqs != 5 || r.Total != 150 || r.Queue != 4 || r.Fails != 1 || r.TtfbN != 4 {
		t.Errorf("合并结果不对: %+v", r)
	}
	m.tab = tabModels
	out := stripANSI(m.draw())
	if !strings.Contains(out, "ustc/m1") {
		t.Errorf("合并表里应有 ustc/m1")
	}
	for _, s := range []string{"alias-a", "alias-b"} {
		if strings.Contains(out, s) {
			t.Errorf("第 2 页不该出现逻辑名 %q", s)
		}
	}
}

// 颜色标准: 锁与错误红, 退避/试探黄, 错误数 0 绿; 表头不加色。
// 用宽终端画, 免得窄屏按 hide 档位丢列, 把要断言的单元格裁掉。
func TestColorStandard(t *testing.T) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	defer f.Close()
	var n uint32
	ioctlCall(f.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n))
	var zero int32
	ioctlCall(f.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&zero))
	sl, err := os.OpenFile("/dev/pts/"+itoa(int(n)), os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open pts: %v", err)
	}
	defer sl.Close()
	old := os.Stdout
	os.Stdout = sl
	defer func() { os.Stdout = old }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	ioctlCall(f.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&tuiWinsize{Row: 40, Col: 160}))

	m := newTUIModel("cfg")
	m.tab = tabModels
	m.pulse.Cands = []candView{{
		ID: "ustc", LockMs: 600000, Probing: true,
		Groups: []candGroup{{Name: "g1", LockMs: 600000}, {Name: "g2"}},
		Models: []candModelLock{{Model: "m1", PausedMs: 8000, LeftMs: 300000, Fails: 2}},
	}}
	m.routes = map[string]*routeRow{
		"k": {Upstream: "ustc", Model: "m1", Reqs: 1, Errs: 1, TtfbSum: 100, TtfbN: 1, Last: time.Now()},
	}
	raw := m.draw()
	for _, want := range []string{
		tuiRed + "10:00" + tuiReset, // 记账组的额度锁
		tuiGreen + "g2" + tuiReset,  // 没锁的记账组: 绿色组名
		tuiYell + "8s" + tuiReset,   // 退避
		tuiRed + "05:00" + tuiReset, // 熔断锁
		tuiRed + "1" + tuiReset,     // 错误数
		tuiRed + "2" + tuiReset,     // 连续错误
		tuiYell + "(+1)" + tuiReset, // 超发试探
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("缺少按标准上色的单元格 %q", want)
		}
	}
}
