// tui_draw.go: 整屏绘制、页头与页签。

package main

import (
	"bytes"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (m *tuiModel) draw() string {
	m.cols, m.rows = tuiSize()
	if m.cols < 60 {
		m.cols = 60
	}
	c := &tuiCanvas{cols: m.cols}
	// 版式: 标题 / 实时状态 / 统计状态 / 页签 / 内容 / ... / 反馈 / 键位 (贴底); 反馈行只有
	// 第 1 页有 (解锁与粘性的结果), 其余页没有反馈行。
	// 上游状态并进第 1 页正文。上游专属色按上游表的固定顺序分配, 各页同色; 红黄绿的正色
	// 留给状态, 上游用的是它们的亮色变体。
	m.upColors = make(map[string]string, len(m.pulse.Cands))
	for i, cand := range m.sortedCands() {
		m.upColors[cand.ID] = upPalette[i%len(upPalette)]
	}
	// 逻辑名专属色: 配置里的逻辑名 (含组名) 按窗口内请求的衰减权重降序分色 (同权重按
	// 字典序), 与上游的分配互不影响, 留给各页的名称单元格 (目前是 events 页的标识列)。
	names := m.weightOrder(m.cachedLogicalNames(), time.Now())
	m.logicalColors = make(map[string]string, len(names))
	for i, name := range names {
		m.logicalColors[name] = logicalPalette[i%len(logicalPalette)]
	}
	m.drawHeader(c)
	m.drawTabs(c)
	body := m.bodyRows()
	// 第 1 页的操作反馈 (解锁 / 粘性) 单独占键位提示上方一行: 屏幕底部从此是
	// 内容 / 反馈 / 键位, 反馈不挤进本来就长的键位行。有反馈时内容区让出一行。
	msg := m.flowMsgLine()
	if msg != "" {
		body = max(3, body-1)
	}
	switch m.tab {
	case tabActive:
		m.drawFlow(c, body)
	case tabEvents:
		m.drawEvents(c, body)
	case tabSettings:
		m.drawSettings(c, body)
	}
	// 补空行把键位提示压到最后一行 (有反馈行时把反馈压在键位行上面)。注意别把
	// countLines() 写进循环条件: 它每轮都在长, 条件会自己收紧, 循环提前结束 (屏幕上
	// 就是"提示浮在中间")。
	last := m.rows - 1
	if msg != "" {
		last--
	}
	for c.countLines() < last {
		c.line("")
	}
	if msg != "" {
		c.line(msg)
	}
	c.last(m.hint())
	// 万一体内容本身高过终端 (极小的窗口), 按行裁掉尾部, 别让提示被顶出屏幕。
	return "\x1b[H" + c.clip(m.rows)
}

// bodyRows 是内容区行数: 固定区占 标题 + 实时状态 + 统计状态 + 页签 + 键位 (第 1 页有
// 操作反馈时再让出一行), 其余全部给内容。布局不用空行做分隔, 屏幕上每一行都是内容、
// 反馈或键位。
func (m *tuiModel) bodyRows() int {
	if body := m.rows - 3 - m.statusRows; body >= 3 {
		return body
	}
	return 3
}

func (c *tuiCanvas) countLines() int { return bytes.Count(c.b.Bytes(), []byte("\n")) }

// clip 最多保留 n 行 (第 n 行的换行也去掉)。只在内容高过终端时才会真的裁。
func (c *tuiCanvas) clip(n int) string {
	b := c.b.Bytes()
	count, cut := 0, len(b)
	for i, ch := range b {
		if ch != '\n' {
			continue
		}
		count++
		if count == n {
			cut = i
			break
		}
	}
	if count < n {
		return string(b)
	}
	end := cut
	if end < len(b) && b[end-1] == '\r' {
		end--
	}
	return string(b[:end]) + "\x1b[K"
}

// last 画最后一行: 不带换行。整屏最后再换行会把光标顶出屏幕触发滚动, 屏幕上移一行,
// 于是贴着底部的键位提示看起来跑到了中间 (TUI 最常见的坑)。
func (c *tuiCanvas) last(s string) {
	if tuiWidth(s) > c.cols {
		s = tuiTrunc(s, c.cols)
	}
	c.b.WriteString(s)
	c.b.WriteString("\x1b[K")
}

func (m *tuiModel) hint() string {
	if m.tab == tabEvents && m.evPrompt != nil {
		return tuiEditBg + tuiPad(tuiIndent+m.evPrompt.label+" "+m.evPrompt.buffer+" ", m.cols-1) + tuiReset
	}
	if m.tab == tabSettings {
		return "  m/p/g 换面板   j/k 移动   +/- 调值   = 输入   s 保存并生成" +
			"   h 热加载   q 退出   (" + m.prioHint() + ")"
	}
	base := "  1-3 或 Tab 切页   up/down/PgUp/PgDn 滚动   r 重读日志   q 退出"
	switch m.tab {
	case tabActive:
		// 80 列终端里也要保住末尾的 q 退出, 所以用 2 空格分隔; 窗口信息放尾巴。
		base = "  hjkl 光标  u 解锁  Enter 置偏好  c 清除  ↑↓ 滚动  Tab 切页  r 重读  q 退出"
		base += "   ("
		if m.pageTotal > 0 {
			win := m.pageWin
			if win <= 0 {
				win = m.bodyRows()
			}
			base += "行 " + strconv.Itoa(m.pageTop+1) + "-" +
				strconv.Itoa(min(m.pageTop+win, m.pageTotal)) + "/" + strconv.Itoa(m.pageTotal) + ", "
		}
		base += "窗口 " + durStr(m.window) + ")"
	case tabEvents:
		base = "  f 筛选   t 时间范围   c 清除筛选   up/down/PgUp/PgDn 滚动   r 重读日志   q 退出"
		if m.evMsg != "" {
			base = tuiYell + tuiIndent + m.evMsg + tuiReset + base
		}
		base += "   (events: 最新的在最上面)"
	}
	return base
}

// flowMsgLine 是第 1 页的操作反馈行 (解锁 / 置偏好 / 清除偏好): 成功黄字, 失败红字;
// 没有反馈时返回空串 (不占行)。它单独占键位提示上方一行。
func (m *tuiModel) flowMsgLine() string {
	if m.tab != tabActive || m.flowMsg == "" {
		return ""
	}
	col := tuiYell
	if m.flowErr {
		col = tuiRed
	}
	return col + tuiIndent + m.flowMsg + tuiReset
}

// prioHint 是 settings 页提示行右侧那句: 改的是哪份 store。
func (m *tuiModel) prioHint() string {
	if m.prio == nil {
		return "未装载"
	}
	if m.prio.err != "" {
		return "store 读不到"
	}
	return filepath.Base(m.prio.path)
}

func (m *tuiModel) drawHeader(c *tuiCanvas) {
	src := "同进程"
	if m.stateURL != "" {
		src = m.stateURL
	}
	live := tuiGreen + "live" + tuiReset
	if !m.live {
		live = tuiRed + "no-state" + tuiReset
	}
	// 第 1 行整行都用前景色 (只有程序名加粗): 灰字在深色终端上太弱。
	c.line(tuiIndent + tuiBold + "tollgate tui" + tuiReset + "  观测 " + src +
		"   监听 " + m.listen + "   日志 " + m.logPath)
	// 第 2 行实时状态: 此刻正在发生什么 (连接 / 路由 / 首字节 / 字节速率 / 运行 / 窗口)。
	// 第 3 行统计状态: 窗口内的累计计数 (读日志 / 请求 / 错误 / 换候选 / token), 报错放行末。
	// 两行各固定一行, 超宽由 line 截断 (不再折行)。
	rows := m.routeRows()
	a := aggRoutes(rows)
	c.line(tuiIndent + "状态 " + live +
		"   进行中 " + strconv.Itoa(len(m.pulse.Active)) +
		"   路由 " + strconv.Itoa(len(rows)) + " 条" +
		"   首字节 " + m.ttfbCell(a) +
		"   流量 " + humanRate(m.flowRate) +
		"   运行 " + durStr(time.Since(m.start)) +
		"   窗口 " + durStr(m.window))
	stats := "已读日志 " + strconv.Itoa(m.scanned) + " 条   " +
		strconv.Itoa(a.reqs) + " 请求   " + strconv.Itoa(a.errs) + " 错误   " +
		strconv.Itoa(a.fb) + " 次换候选   " + strings.TrimLeft(humanTokens(a.total), " ") +
		" token (缓存 " + a.cache + ")"
	if e := m.lastError(); e != "" {
		stats += "   " + tuiYell + e + tuiReset
	}
	c.line(tuiIndent + stats)
	m.statusRows = 2
}

// drawTabs 画页签行; 上游状态不放在顶端, 并进第 1 页正文。
func (m *tuiModel) drawTabs(c *tuiCanvas) {
	var b strings.Builder
	b.WriteString(tuiIndent)
	for i, name := range tuiTabs {
		if i == m.tab {
			b.WriteString(tuiRev + tuiBold + " " + name + " " + tuiReset)
		} else {
			b.WriteString(tuiDim + " " + strconv.Itoa(i+1) + " " + name + " " + tuiReset)
		}
		b.WriteString(" ")
	}
	c.line(b.String())
}
