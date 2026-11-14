// tui_run.go: 状态台主循环与按键处理。
//
// 状态台以观测为主, 不参与转发、调度与记账; settings 页可写 (改配置源头), 第 1 页可
// 解锁与置粘性偏好 (都写运行中的服务)。观测数据看三件事:
//  1. active  正在活跃的请求 + 它此刻走的是哪个后端 (上游 + 真实模型), 以及每个上游的
//     队列 / 并发上限 / 锁 / 退避。数据来自服务的内存观测面: 同进程看内存 (-tui),
//     另进程看 GET /state (-tui-state [base])。
//  2. routes  按 逻辑名 x 上游 x 真实模型 聚合的路由历史 (请求数 / 错误 / fallback /
//     token / 首字节 / 吞吐), 直接读 JSONL 日志 —— 日志是唯一事实来源。
//  3. events  事件流: 请求阶段战报 + 加锁 / 探测 / 解锁, 倒序。
//
// 用法:
//
//	llm-request-tollgate -tui                  # 同进程起代理 + 状态台 (q 退出)
//	llm-request-tollgate -tui-state             # 只看已常驻的服务 (默认 127.0.0.1:8787)
//	llm-request-tollgate -tui-state <base>      # 只看指定地址上的服务
//
// 依赖: 只用标准库 + Linux ioctl (syscall), 与代理一样零第三方依赖。

package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func runTUI(cfgPath, stateURL string) {
	m := newTUIModel(cfgPath)
	m.loadCfg()
	m.stateURL = stateURL

	var app *App
	if stateURL != "" {
		m.stateURL = strings.TrimSuffix(strings.TrimRight(stateURL, "/"), "/")
	} else {
		app = startTUIProxy(cfgPath, m)
	}

	// 日志 (含跨天归档) 在主循环里读: 单 goroutine 独占 routes/events, 不用加锁。
	// 终端: 原始模式 + 备用屏 + 藏光标; 退出时逐条还原 (含 panic)。
	leave, ok := enterTUI()
	if !ok {
		if app == nil {
			return
		}
		// 已经起了代理: 退回普通常驻, 别把服务带走。
		fmt.Fprintln(os.Stderr, "已退回普通模式 (Ctrl-C 退出)")
		select {}
	}
	defer leave()
	runTUILoop(m, app)
}

// startTUIProxy 是同进程模式: 起代理 (日志写文件) + 观测推送 + 热加载钩子
// (settings 页的 h 键与 SIGHUP 同一条路), 返回 App 供主循环轮询。
func startTUIProxy(cfgPath string, m *tuiModel) *App {
	log.SetOutput(io.Discard) // 标准 logger 会踩画面
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config invalid:", err)
		os.Exit(1)
	}
	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	if app.logPath() != "" {
		app.startPulseLoop(pulsePathFor(app.logPath()))
	}
	m.perfIdx = app.perfIndex // 性能列用同一个每小时重算的参数表
	// 先显式 bind: 端口被占要立刻说清楚, 不能画着一个没起来的服务的状态。
	ln, lerr := net.Listen("tcp", cfg.Listen)
	if lerr != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", lerr)
		os.Exit(1)
	}
	go func() {
		if err := http.Serve(ln, app); err != nil {
			m.errf("服务退出: %v", err)
		}
	}()
	app.evAdd("start", "", "", "", "start "+cfg.Listen)
	// 热加载: settings 页的 h 键与 SIGHUP 走同一条路 (同进程不需要 systemd)。
	m.reloadFn = func() error {
		c, err := loadConfig(cfgPath)
		if err != nil {
			app.evAdd("reload", "", "", "", "reload fail: "+err.Error())
			return err
		}
		app.applyConfig(c)
		app.evAdd("reload", "", "", "", "reload ok: "+cfgPath)
		return nil
	}
	// SIGHUP 热加载别名与上游 (与常驻模式同一语义)。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGHUP)
		for range ch {
			_ = m.reloadFn()
		}
	}()
	return app
}

// enterTUI 进原始模式 + 备用屏 + 藏光标, 返回退出时的还原函数。
// 非交互终端返回 ok=false (已打印原因), 由调用方决定是否退回普通常驻。
func enterTUI() (leave func(), ok bool) {
	old, err := tuiRaw(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "TUI 需要交互式终端:", err)
		return nil, false
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")
	return func() {
		os.Stdout.WriteString(tuiReset + "\x1b[?25h\x1b[?1049l")
		tuiRestore(os.Stdin.Fd(), old)
	}, true
}

// runTUILoop 是主循环: 每秒刷新 + 按键处理 + 窗口变化重绘, 直到 q / EOF。
func runTUILoop(m *tuiModel, app *App) {
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	keys := make(chan tuiKey, 64)
	go readKeys(keys)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var since uint64
	out := bufio.NewWriter(os.Stdout)
	pending := []byte{} // 未处理完的转义序列 (方向键是 3 字节)

	render := func() {
		out.WriteString(m.draw())
		out.Flush()
	}
	refresh := func(force bool) {
		if !force && time.Since(m.refreshed) < 300*time.Millisecond {
			return
		}
		m.refreshed = time.Now()
		m.pollLog()
		m.sweepRoutes() // 顺手裁掉已超出回看窗口的路由聚合, 不用等按 r 才消失
		since = m.pollState(app, since)
	}
	refresh(true)
	render()

	for {
		select {
		case <-tick.C:
			refresh(false)
			render()
		case <-resize:
			render()
		case k, ok := <-keys:
			if !ok {
				return
			}
			if quit := m.handleKeyEv(k, &pending); quit {
				return
			}
			refresh(false)
			render()
		}
	}
}

// tuiKey 是一个按键事件: b 是读到的字节; esc 为真表示单独按下的 Esc (后面没跟着转义
// 序列的字节)。两者开头都是 0x1b, 只能靠 readKeys 等一下来分辨。
type tuiKey struct {
	b   byte
	esc bool
}

// tuiEscWait 是「单独按下的 Esc」的判定时长: 方向键 / PgUp / PgDn 这些转义序列的后续
// 字节几乎紧跟 0x1b 到达, 等这么久还没来就当成单独按下的 Esc。
const tuiEscWait = 30 * time.Millisecond

// readKeys 把标准输入的字节逐个送进 keys; 读到 EOF 就关闭通道。读到 0x1b 时先等一会儿
// 看后面有没有字节: 有就是转义序列的开头 (照常送进去攒 pending), 没有就是单独按下的 Esc
// (带 esc 标记送进去, 不留在 pending 里等一个永远不来的序列)。
func readKeys(keys chan<- tuiKey) {
	r := bufio.NewReader(os.Stdin)
	for {
		b, err := r.ReadByte()
		if err != nil {
			close(keys)
			return
		}
		if b == 0x1b && r.Buffered() == 0 && !tuiInputReady(os.Stdin.Fd(), tuiEscWait) {
			keys <- tuiKey{esc: true}
			continue
		}
		keys <- tuiKey{b: b}
	}
}

// handleKey 处理一个输入字节; 返回 true = 退出。
//
// settings 页把编辑键留给自己 (s = 保存并重生成, h = 热加载, m/p/g = 换面板), events 页
// 把筛选键留给自己 (f / t / c), 第 1 页把 h/j/k/l 留给行光标 (u / Enter / c 是行操作);
// 切页统一走 1-3 / 左右方向键 / Tab。
func (m *tuiModel) handleKey(b byte, pending *[]byte) bool {
	e := m.prio
	if m.tab == tabSettings && e != nil && e.prompt != nil {
		*pending = nil // 输入态: 整字节喂给输入框, 别把 Esc 当转义序列攒
		e.promptKey(b)
		return false
	}
	if m.tab == tabEvents && m.evPrompt != nil {
		*pending = nil // 输入态: 整字节喂给输入框, 别把 Esc 当转义序列攒
		m.evPromptKey(b)
		return false
	}
	*pending = append(*pending, b)
	if p := *pending; p[0] == 0x1b {
		quit, complete := m.escapeKey(p)
		if complete {
			*pending = nil
		}
		return quit
	}
	*pending = nil
	switch {
	case m.tab == tabSettings && e != nil:
		return m.settingsKey(b, e)
	case m.tab == tabEvents:
		if quit, handled := m.eventsKey(b); handled {
			return quit
		}
	case m.tab == tabActive:
		if quit, handled := m.flowKey(b); handled {
			return quit
		}
	}
	return m.commonKey(b)
}

// handleKeyEv 分派一个按键事件: 单独按下的 Esc 走 escKey, 其余字节照旧走 handleKey。
func (m *tuiModel) handleKeyEv(k tuiKey, pending *[]byte) bool {
	if k.esc {
		*pending = nil // 单独的 Esc: 没收全的转义序列作废, 不跟它拼
		return m.escKey()
	}
	return m.handleKey(k.b, pending)
}

// escKey 是单独按下的 Esc: 输入态里取消输入 (与 Esc 字节进输入框同一条路), 第 1 页清掉
// 行光标 (上方两张表全都不选中); 其余页不动。Esc 从不退出。
func (m *tuiModel) escKey() bool {
	e := m.prio
	switch {
	case m.tab == tabSettings && e != nil && e.prompt != nil:
		e.promptKey(0x1b)
	case m.tab == tabEvents && m.evPrompt != nil:
		m.evPromptKey(0x1b)
	case m.tab == tabActive:
		m.clearFlowCursor()
	}
	return false
}

// flowKey 是第 1 页的行操作键: h/j/k/l 移动光标 (方向键仍走通用表滚动 / 切页), u 解锁
// 光标处的锁, Enter 把光标所在模型行置为粘性偏好, c 清除该逻辑名的偏好。
func (m *tuiModel) flowKey(b byte) (quit, handled bool) {
	switch b {
	case 'h':
		m.flowCol(-1)
	case 'l':
		m.flowCol(1)
	case 'j':
		m.flowMove(1)
	case 'k':
		m.flowMove(-1)
	case 'u':
		m.flowUnlock()
	case '\r', '\n':
		m.flowStickySet()
	case 'c':
		m.flowStickyClear()
	default:
		return false, false
	}
	return false, true
}

// escapeKey 处理转义序列 (方向键 / PgUp / PgDn): 字节没收全时 complete=false,
// 继续留在 pending 里等下一拍。转义键从不退出。
func (m *tuiModel) escapeKey(p []byte) (quit, complete bool) {
	if len(p) < 3 {
		return false, false // 方向键是 3 字节, 等收全
	}
	code := p[2]
	if code == '5' || code == '6' { // PgUp / PgDn 是 4 字节
		if len(p) < 4 {
			return false, false
		}
		code += 32 // '5'->'U'(上翻页), '6'->'V'(下翻页); 下面按语义处理
	}
	var d int
	switch code {
	case 'A', 'U':
		d = -1
	case 'B', 'V':
		d = 1
	case 'C':
		m.switchTab(1)
		return false, true
	case 'D':
		m.switchTab(-1)
		return false, true
	}
	if code == 'U' {
		d -= 10
	}
	if code == 'V' {
		d += 10
	}
	switch {
	case m.tab == tabSettings && m.prio != nil:
		m.prio.moveFocus(d)
	default:
		m.move(d)
	}
	return false, true
}

// settingsKey 是 settings 页的按键: 先给编辑器, 没接住再处理本页专属键
// (s 保存 / h 热加载 / l 切页 / 数字跳页)。
func (m *tuiModel) settingsKey(b byte, e *tuiEditor) bool {
	if quit, handled := e.key(b); handled {
		return quit
	}
	switch b {
	case 's':
		m.saveSettings()
	case 'h':
		m.prioHup()
	case 'l', '\t':
		m.switchTab(1)
	case '1', '2', '3':
		m.switchTabTo(int(b - '1'))
	}
	return false
}

// eventsKey 是事件页的筛选键: f 文本筛选, t 时间范围, c 清除全部筛选。
func (m *tuiModel) eventsKey(b byte) (quit, handled bool) {
	switch b {
	case 'f':
		m.startEvPrompt(true)
	case 't':
		m.startEvPrompt(false)
	case 'c':
		m.clearEvFilter()
	default:
		return false, false
	}
	return false, true
}

// commonKey 是各页共用的按键: 退出 / 切页 / 滚动 / 重读日志。
func (m *tuiModel) commonKey(b byte) bool {
	switch b {
	case 'q', 0x03, 0x04: // q / Ctrl-C / Ctrl-D
		return true
	case '1', '2', '3':
		m.switchTabTo(int(b - '1'))
	case 'h', 0x1b:
		m.switchTab(-1)
	case 'l', '\t':
		m.switchTab(1)
	case 'j':
		m.move(1)
	case 'k':
		m.move(-1)
	case 'g':
		m.scroll[m.tab] = 0
	case 'G':
		m.scroll[m.tab] = 1 << 30
	case 'r': // 重读全部日志 (换窗口 / 想看全量时用)
		m.logPos, m.logRest, m.scannedFiles = map[string]int64{}, map[string][]byte{}, map[string]bool{}
		m.routes = map[string]*routeRow{}
		m.nameEnds = map[string][]time.Time{} // 配色权重也从头重算, 免得重复累加
		m.events = nil
		m.scanned = 0
		m.ev.resetScan()          // 时间范围回读也从头扫
		m.refreshed = time.Time{} // 让下一拍立刻重读
	case ' ':
		m.move(m.rows - 6)
	}
	return false
}

// switchTab 左右切页 (方向键 / h / l / Tab)。
func (m *tuiModel) switchTab(dir int) {
	m.switchTabTo((m.tab + dir + len(tuiTabs)) % len(tuiTabs))
}

// switchTabTo 跳到第 i 个页签; 首次进 settings 页才去读 store 与配置 (那都是真文件)。
func (m *tuiModel) switchTabTo(i int) {
	if i < 0 || i >= len(tuiTabs) {
		return
	}
	m.tab = i
	if m.tab == tabSettings && m.prio == nil {
		m.prio = newTuiEditor(m.cfgPath)
	}
}

// move 滚动当前 tab; 上限在下一次 draw 里按实际行数夹紧。
func (m *tuiModel) move(d int) {
	m.scroll[m.tab] += d
	if m.scroll[m.tab] < 0 {
		m.scroll[m.tab] = 0
	}
	if m.scroll[m.tab] > 1<<20 {
		m.scroll[m.tab] = 1 << 20
	}
}

// pulsePathFor / gatePathFor: 与被观测服务同目录的状态文件 (var/)。
func pulsePathFor(logPath string) string {
	if logPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(logPath), "tui-pulse.json")
}

func gatePathFor(logPath string) string {
	if logPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(logPath), "gate-state.json")
}

func (a *App) logPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.cfg == nil {
		return ""
	}
	return a.cfg.LogPath
}
