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
	// 版式: 标题 / 状态 (含路由聚合汇总) / 页签 / 内容 / ... / 键位 (贴底);
	// 上游状态并进第 1 页正文。上游专属色按上游表的固定顺序分配, 各页同色; 红黄绿的正色
	// 留给状态, 上游用的是它们的亮色变体。
	m.upColors = make(map[string]string, len(m.pulse.Cands))
	for i, cand := range m.sortedCands() {
		m.upColors[cand.ID] = upPalette[i%len(upPalette)]
	}
	m.drawHeader(c)
	m.drawTabs(c)
	body := m.bodyRows()
	switch m.tab {
	case tabActive:
		m.drawFlow(c, body)
	case tabEvents:
		m.drawEvents(c, body)
	default:
		m.drawSettings(c, body)
	}
	// 补空行把键位提示压到最后一行。注意别把 countLines() 写进循环条件:
	// 它每轮都在长, 条件会自己收紧, 循环提前结束 (屏幕上就是"提示浮在中间")。
	for c.countLines() < m.rows-1 {
		c.line("")
	}
	c.last(m.hint())
	// 万一体内容本身高过终端 (极小的窗口), 按行裁掉尾部, 别让提示被顶出屏幕。
	return "\x1b[H" + c.clip(m.rows)
}

// bodyRows 是内容区行数: 固定区占 标题 + 状态 (可能折行) + 页签 + 键位, 其余全部给内容。
// 布局不用空行做分隔, 屏幕上每一行都是内容或键位。
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
	if m.tab == tabSettings {
		return "  m/p/g 换面板   j/k 移动   +/- 调值   = 输入   s 保存并生成" +
			"   h 热加载   q 退出   (" + m.prioHint() + ")"
	}
	base := "  1-3 或 Tab 切页   up/down/PgUp/PgDn 滚动   r 重读日志   q 退出"
	switch m.tab {
	case tabActive:
		// 窗口信息放最前: 键位行本来就长, 放尾巴会被窄终端截掉。
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
	default:
		base += "   (events: 最新的在最上面)"
	}
	return base
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
	act := len(m.pulse.Active)
	// 各段自带前导分隔: 换行到下一行时由 packPieces 去掉, 段间间距保持原样。
	pieces := []string{
		"状态 " + live,
		"   进行中 " + strconv.Itoa(act),
		"   已读日志 " + strconv.Itoa(m.scanned) + " 条",
		"   运行 " + durStr(time.Since(m.start)),
	}
	if e := m.lastError(); e != "" {
		pieces = append(pieces, "   "+tuiYell+e+tuiReset)
	}
	// 路由聚合的整体状态 (窗口 / 请求 / 错误 / 换候选 / token / 首字节) 接在状态行末尾;
	// 排在错误提示之后, 免得长行先把报错截掉。按空格切段, 太长时在词边界换行。
	for i, f := range strings.Fields(m.routeSummary(m.routeRows())) {
		if i == 0 {
			pieces = append(pieces, "   "+f)
			continue
		}
		pieces = append(pieces, " "+f)
	}
	lines := packPieces(pieces, m.cols-len(tuiIndent))
	for _, ln := range lines {
		c.line(tuiIndent + ln)
	}
	m.statusRows = len(lines)
}

// packPieces 把带前导分隔的片段按可见宽度拼成若干行: 放不下就换行并去掉该片段的前导
// 空格; 单段自己就超宽时至多占一行 (截断), 段内不拆, 免得切断词或 ANSI 序列。
func packPieces(pieces []string, width int) []string {
	var lines []string
	cur, curW := "", 0
	for _, p := range pieces {
		if p == "" {
			continue
		}
		w := tuiWidth(p)
		if cur != "" && width > 0 && curW+w > width {
			lines = append(lines, cur)
			p, cur, curW = strings.TrimLeft(p, " "), "", 0
			w = tuiWidth(p)
		}
		if width > 0 && w > width {
			p, w = tuiTrunc(p, width), width
		}
		cur += p
		curW += w
	}
	if cur != "" || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
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
