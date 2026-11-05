// tui_run.go: 状态台主循环与按键处理。
//
// 状态台以观测为主, 不参与转发、调度与记账; settings / sticky 两页可写 (前者改配置源头,
// 后者经 POST /sticky 改运行时偏好)。观测数据看三件事:
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
		// 同进程: 起代理 (日志写文件) + 状态台读内存。标准 logger 关掉, 否则会踩画面。
		log.SetOutput(io.Discard)
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config invalid:", err)
			os.Exit(1)
		}
		app, err = newApp(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "init:", err)
			os.Exit(1)
		}
		if app.logPath() != "" {
			app.startPulseLoop(pulsePathFor(app.logPath()))
		}
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
		app.evAdd("start", "", "", "", "tollgate 启动, 监听 "+cfg.Listen)
		// 热加载: settings 页的 h 键与 SIGHUP 走同一条路 (同进程不需要 systemd)。
		m.reloadFn = func() error {
			c, err := loadConfig(cfgPath)
			if err != nil {
				app.evAdd("reload", "", "", "", "热加载失败: "+err.Error())
				return err
			}
			app.applyConfig(c)
			app.evAdd("reload", "", "", "", "热加载成功: "+cfgPath)
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
	}

	// 日志 (含跨天归档) 在主循环里读: 单 goroutine 独占 routes/events, 不用加锁。
	// 终端: 原始模式 + 备用屏 + 藏光标; 退出时逐条还原 (含 panic)。
	old, err := tuiRaw(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "TUI 需要交互式终端:", err)
		if app == nil {
			return
		}
		// 已经起了代理: 退回普通常驻, 别把服务带走。
		fmt.Fprintln(os.Stderr, "已退回普通模式 (Ctrl-C 退出)")
		select {}
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")
	defer func() {
		os.Stdout.WriteString(tuiReset + "\x1b[?25h\x1b[?1049l")
		tuiRestore(os.Stdin.Fd(), old)
	}()

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	keys := make(chan byte, 64)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(keys)
				return
			}
			keys <- b
		}
	}()

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
		case b, ok := <-keys:
			if !ok {
				return
			}
			if quit := m.handleKey(b, &pending); quit {
				return
			}
			refresh(false)
			render()
		}
	}
}

// handleKey 处理一个输入字节; 返回 true = 退出。
//
// settings 页把编辑键留给自己 (s = 保存并重生成, h = 热加载, m/p/g = 换面板), 状态页的
// h/l 仍是切页; 切页统一走 1-4 / 左右方向键 / l (settings 页的 h 被编辑键占了)。
func (m *tuiModel) handleKey(b byte, pending *[]byte) bool {
	e := m.prio
	if m.tab == tabSettings && e != nil && e.prompt != nil {
		*pending = nil // 输入态: 整字节喂给输入框, 别把 Esc 当转义序列攒
		e.promptKey(b)
		return false
	}
	*pending = append(*pending, b)
	p := *pending
	if p[0] == 0x1b {
		if len(p) < 3 {
			return false // 方向键是 3 字节, 等收全
		}
		code := p[2]
		if code == '5' || code == '6' { // PgUp / PgDn 是 4 字节
			if len(p) < 4 {
				return false
			}
			code += 32 // '5'->'U'(上翻页), '6'->'V'(下翻页); 下面按语义处理
		}
		*pending = nil
		var d int
		switch code {
		case 'A', 'U':
			d = -1
		case 'B', 'V':
			d = 1
		case 'C':
			m.switchTab(1)
			return false
		case 'D':
			m.switchTab(-1)
			return false
		}
		if code == 'U' {
			d -= 10
		}
		if code == 'V' {
			d += 10
		}
		if m.tab == tabSettings && e != nil {
			e.moveFocus(d)
		} else if m.tab == tabSticky {
			m.stickyMove(d)
		} else {
			m.move(d)
		}
		return false
	}
	*pending = nil
	if m.tab == tabSettings && e != nil {
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
		case '1', '2', '3', '4':
			m.switchTabTo(int(b - '1'))
		}
		return false
	}
	if m.tab == tabSticky {
		// sticky 页把选择键留给自己: Enter 置偏好, c 清除, j/k/g/G 移动光标。
		switch b {
		case 'j':
			m.stickyMove(1)
			return false
		case 'k':
			m.stickyMove(-1)
			return false
		case 'g':
			m.stickyJump(false)
			return false
		case 'G':
			m.stickyJump(true)
			return false
		case '\r', '\n':
			m.stickyApply()
			return false
		case 'c':
			m.stickyClear()
			return false
		}
	}
	switch b {
	case 'q', 0x03, 0x04: // q / Ctrl-C / Ctrl-D
		return true
	case '1', '2', '3', '4':
		m.switchTabTo(int(b - '1'))
	case 'h', 0x1b:
		m.switchTab(-1)
	case 'l':
		m.switchTab(1)
	case '\t':
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
		m.events = nil
		m.scanned = 0
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
