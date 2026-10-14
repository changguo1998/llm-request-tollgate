// tui.go: 实时状态台 (TUI) —— 只读观测面, 不参与转发、调度与记账。
//
// 看三件事:
//  1. active  正在活跃的请求 + 它此刻走的是哪个后端 (上游 + 真实模型), 以及每个上游的
//     队列 / 并发上限 / 锁 / 退避。数据来自服务的内存观测面: 同进程看内存 (-tui),
//     另进程看 GET /v1/state (-tui-state <base>)。
//  2. routes  按 逻辑名 x 上游 x 真实模型 聚合的路由历史 (请求数 / 错误 / fallback /
//     token / 首字节 / 吞吐), 直接读 JSONL 日志 —— 日志是唯一事实来源 (设计 D12)。
//  3. events  事件流: 锁 / 熔断 / 退避 / fallback / 错误 / 慢请求, 倒序。
//
// 用法:
//
//	llm-request-tollgate -tui                  # 同进程起代理 + 状态台 (q 退出)
//	llm-request-tollgate -tui -tui-state http://127.0.0.1:8787   # 只看已常驻的服务
//
// 依赖: 只用标准库 + Linux ioctl (syscall); 与代理一样零第三方依赖。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

// ---------- 终端: 原始模式与尺寸 (Linux ioctl, 无第三方依赖) ----------

const (
	tuiTCGETS     = 0x5401
	tuiTCSETS     = 0x5402
	tuiTIOCGWINSZ = 0x5413
	tuiFlagIflag  = 0x1 // BRKINT
	tuiFlagIcrnl  = 0x100
	tuiFlagOpost  = 0x1
	tuiFlagEcho   = 0x8
	tuiFlagIcanon = 0x2
	tuiFlagIsig   = 0x1
	tuiFlagIexten = 0x8000
	tuiFlagVmin   = 6
	tuiFlagVtime  = 5
)

type tuiTermios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Line   uint8
	Cc     [19]uint8
	Ispeed uint32
	Ospeed uint32
}

type tuiWinsize struct{ Row, Col, Xpixel, Ypixel uint16 }

func tuiIoctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// tuiRaw 关掉行缓冲 / 回显 / 信号键, 读满 1 字节即返回 (VMIN=1)。
func tuiRaw(fd uintptr) (*tuiTermios, error) {
	var old tuiTermios
	if err := tuiIoctl(fd, tuiTCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= tuiFlagIcrnl | tuiFlagIflag
	raw.Oflag &^= tuiFlagOpost
	raw.Lflag &^= tuiFlagEcho | tuiFlagIcanon | tuiFlagIsig | tuiFlagIexten
	raw.Cc[tuiFlagVmin], raw.Cc[tuiFlagVtime] = 1, 0
	if err := tuiIoctl(fd, tuiTCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return &old, nil
}

func tuiRestore(fd uintptr, t *tuiTermios) {
	if t != nil {
		tuiIoctl(fd, tuiTCSETS, unsafe.Pointer(t))
	}
}

// tuiSize 取终端行列; 失败时给 100x30, 保证还能画出东西。
func tuiSize() (int, int) {
	ws := tuiWinsize{}
	if tuiIoctl(os.Stdout.Fd(), tuiTIOCGWINSZ, unsafe.Pointer(&ws)) == nil && ws.Col > 0 {
		return int(ws.Col), int(ws.Row)
	}
	return 100, 30
}

// ---------- 画布: 逐行拼装, 按显示宽度截断 (ASCII 宽度 = 字节数) ----------

type tuiCanvas struct {
	b    bytes.Buffer
	cols int
}

func (c *tuiCanvas) line(s string) {
	if tuiWidth(s) > c.cols {
		s = tuiTrunc(s, c.cols)
	}
	c.b.WriteString(s)
	c.b.WriteString("\x1b[K\r\n")
}

// ansiSpan 从 s[i] (ESC) 起量出一个 ANSI 序列的长度; 不是序列就返回 0。
// 只处理 CSI (ESC [ ... 终止字节 @-~), 本文件画面上用的全是这种。
func ansiSpan(s string, i int) int {
	if s[i] != 0x1b || i+1 >= len(s) || s[i+1] != '[' {
		return 0
	}
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j >= len(s) {
		return len(s) - i
	}
	return j + 1 - i
}

// tuiWidth 是显示宽度: ASCII 1 列, CJK 全角 2 列, ANSI 序列 0 列。
// 不这么做的话, 中文列会把表头挤歪半行。
func tuiWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := ansiSpan(s, i); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			i++
			w++
			continue
		}
		w += runeWidth(r)
		i += size
	}
	return w
}

// runeWidth 只区分"窄/宽"两档, 够表格对齐用 (不引入 wcwidth 依赖)。
func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0xa4cf, // CJK 部首 / 假名 / 注音
		r >= 0xac00 && r <= 0xd7a3, // Hangul 音节
		r >= 0xf900 && r <= 0xfaff, // CJK 兼容汉字
		r >= 0xfe30 && r <= 0xfe6f, // CJK 兼容形式
		r >= 0xff00 && r <= 0xff60, // 全角 ASCII
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x20000 && r <= 0x3fffd: // 扩展区汉字
		return 2
	}
	return 1
}

// tuiPad 补到 w 列 (不截断; 需要截断先 tuiTrunc)。
func tuiPad(s string, w int) string {
	if n := tuiWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// tuiTrunc 截到 w 列以内 (只看可见字符, ANSI 序列不占列宽); 截断时末尾留 ~。
func tuiTrunc(s string, w int) string {
	if tuiWidth(s) <= w {
		return s
	}
	limit := w
	if w > 1 {
		limit = w - 1
	}
	var b strings.Builder
	used, cut := 0, false
	for i := 0; i < len(s); {
		if n := ansiSpan(s, i); n > 0 { // 序列原样带走, 不计宽
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runeWidth(r)
		if used+rw > limit {
			cut = true
			break
		}
		b.WriteString(s[i : i+size])
		used += rw
		i += size
	}
	if cut && w > 1 {
		b.WriteString("~")
	}
	return b.String()
}

const (
	tuiReset = "\x1b[0m"
	tuiBold  = "\x1b[1m"
	tuiDim   = "\x1b[2m"
	tuiRev   = "\x1b[7m"
	tuiRed   = "\x1b[31m"
	tuiGreen = "\x1b[32m"
	tuiYell  = "\x1b[33m"
	tuiCyan  = "\x1b[36m"
)

// tuiBuckets 是路由表右侧"每分钟请求数"小方条的分钟数; tuiBlocks 是它的 9 档灰度。
// tuiWindow 是默认回看窗口: 聚合只算这么久以内的日志 (与 -summary 的窗口口径一致)。
const (
	tuiBuckets    = 30
	tuiWindow     = 6 * time.Hour
	tuiScanWindow = 24 * time.Hour
)

var tuiBlocks = [9]byte{' ', '_', '.', ':', '-', '=', '+', '*', '#'}

// ---------- 视图模型 ----------

// routeRow 是「逻辑名 x 上游 x 真实模型」的一行聚合 (来自日志)。
type routeRow struct {
	Logical, Upstream, Model string
	Reqs, Errs, Fallbacks    int
	Prompt, Cached, Total    int
	TtfbSum                  int64
	TtfbN                    int
	TpsSum                   float64
	TpsN                     int
	First, Last              time.Time
	bucket                   [tuiBuckets]uint8 // 最近 N 分钟每分钟的请求数 (封顶 255)
}

// routeEvent 是路由历史里值得单独看一眼的一条 (从日志逐行还原)。
type routeEvent struct {
	Ts       time.Time
	Kind     string // error | fallback | gate | slow | ok
	Logical  string
	Upstream string
	Model    string
	Status   int
	Msg      string
}

type tuiModel struct {
	cfgPath  string
	logPath  string
	listen   string
	stateURL string // 空 = 同进程直接读内存

	start   time.Time
	pulse   pulse
	live    bool
	lastErr string
	mu      sync.Mutex        // 保护 lastErr (监听失败可能来自别的 goroutine)
	gates   map[string]string // 候选 -> 当前锁 (gate-state.json 里的锁, 用作补充)

	routes map[string]*routeRow
	events []routeEvent

	window       time.Duration     // 回看窗口 (行只算窗口内的记录)
	logPos       map[string]int64  // 每个日志文件读到哪
	logRest      map[string][]byte // 每文件各自的半行缓冲 (读到一半的行)
	scannedFiles map[string]bool   // 归档已整读, 不再重复扫
	scanned      int

	tab       int
	scroll    [3]int
	cols      int
	rows      int
	refreshed time.Time // 上次重读日志/取观测的时刻 (按键连发时别每次都重读)
}

// errf 记下最近一次取数失败 (可能来自别的 goroutine, 加锁)。
func (m *tuiModel) errf(format string, args ...any) {
	m.mu.Lock()
	m.lastErr = fmt.Sprintf(format, args...)
	m.mu.Unlock()
}

func (m *tuiModel) lastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

func newTUIModel(cfgPath string) *tuiModel {
	m := &tuiModel{
		cfgPath: cfgPath, start: time.Now(),
		routes: map[string]*routeRow{}, gates: map[string]string{},
		window: tuiWindow,
		logPos: map[string]int64{}, logRest: map[string][]byte{}, scannedFiles: map[string]bool{},
	}
	return m
}

// loadCfg 从配置里取 监听地址 / 日志路径 (TUI 只读这两个字段)。
func (m *tuiModel) loadCfg() {
	c, err := loadConfigOpt(m.cfgPath, true)
	if err != nil {
		m.lastErr = "配置读不到: " + err.Error()
		return
	}
	m.listen, m.logPath = c.Listen, c.LogPath
}

// ---------- 日志: 增量读 + 聚合 ----------

// pollLog 读日志: 归档文件 (跨天) 整体扫一遍, 当前文件增量读。
// 与 -summary / -usage 取同样的文件范围 (logFilesFor 的窗口), 这样看到的不是"从本次启动起"。
func (m *tuiModel) pollLog() {
	if m.logPath == "" {
		return
	}
	for _, f := range logFilesFor(m.logPath, tuiScanWindow) {
		current := filepath.Clean(f) == filepath.Clean(m.logPath)
		if !current {
			if m.scannedFiles[filepath.Clean(f)] { // 归档不会再变, 扫过就跳过
				continue
			}
		}
		m.tailFile(f, current)
	}
}

// tailFile 读一个文件的新增部分 (整文件读完后记下来, 归档不再重复扫)。
func (m *tuiModel) tailFile(path string, incremental bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	pos := m.logPos[path]
	if st.Size() < pos { // 轮转/截断: 从头读
		pos, m.logRest[path] = 0, nil
	}
	if st.Size() == pos {
		return
	}
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, st.Size()-pos)
	n, _ := io.ReadFull(f, buf)
	m.logPos[path] = pos + int64(n)
	m.consume(buf[:n], path)
	if !incremental && m.logPos[path] >= st.Size() {
		m.scannedFiles[filepath.Clean(path)] = true
	}
}

func (m *tuiModel) consume(b []byte, key string) {
	data := b
	if rest := m.logRest[key]; len(rest) > 0 {
		data = append(rest, b...)
		m.logRest[key] = nil
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i]
		data = data[i+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			m.absorb(line)
		}
	}
	if len(data) > 0 {
		if len(data) > 1<<20 { // 异常超长行: 丢掉, 别把内存拖爆
			return
		}
		m.logRest[key] = append([]byte(nil), data...)
	}
}

// absorb 吸收一条日志: 更新聚合行 + 生成事件 (倒序展示用)。
func (m *tuiModel) absorb(line []byte) {
	var r logRec
	if err := json.Unmarshal(line, &r); err != nil {
		return
	}
	m.scanned++
	ts, _ := time.Parse(time.RFC3339Nano, r.Ts)
	if ts.IsZero() {
		ts = time.Now()
	}
	if age := time.Since(ts); age > m.window {
		return // 窗口外的记录不算 (日志文件按 mtime 已粗筛过, 这里再按时间精筛)
	}
	key := r.Logical + "\x00" + r.Upstream + "\x00" + r.Model
	row := m.routes[key]
	if row == nil {
		row = &routeRow{Logical: r.Logical, Upstream: r.Upstream, Model: r.Model}
		m.routes[key] = row
	}
	row.Reqs++
	if r.Status >= 400 || r.Err != "" {
		row.Errs++
	}
	row.Fallbacks += r.Fallbacks
	if r.Usage != nil {
		total := r.Usage.Total
		if total == 0 {
			total = r.Usage.Prompt + r.Usage.Completion
		}
		row.Prompt += r.Usage.Prompt
		row.Cached += r.Usage.Cached
		row.Total += total
	}
	if r.TtfbMs > 0 {
		row.TtfbSum += r.TtfbMs
		row.TtfbN++
	}
	if r.Tps > 0 {
		row.TpsSum += r.Tps
		row.TpsN++
	}
	if row.First.IsZero() || ts.Before(row.First) {
		row.First = ts
	}
	if ts.After(row.Last) {
		row.Last = ts
	}
	if age := time.Since(ts); age >= 0 && age < tuiBuckets*time.Minute {
		i := tuiBuckets - 1 - int(age/time.Minute)
		if i >= 0 && i < tuiBuckets && row.bucket[i] < 255 {
			row.bucket[i]++
		}
	}

	// 事件: 只挑值得看的 (闸门挡下 / 错误 / fallback / 锁跳过 / 慢 / 极慢的 ok)。
	switch {
	case r.Gate != "":
		m.addEvent(routeEvent{Ts: ts, Kind: "gate", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: "被挡下: " + gateReason(r.Gate) + " (排队 " + itoa64(r.QueueMs) + "ms, 等并发 " + itoa64(r.SemMs) + "ms)"})
	case r.Status >= 400 || r.Err != "":
		msg := "状态码 " + strconv.Itoa(r.Status)
		if r.Err != "" {
			msg += " " + tuiTrunc(r.Err, 60)
		}
		if r.LockSkips > 0 {
			msg += " (跳过锁 " + strconv.Itoa(r.LockSkips) + " 次)"
		}
		m.addEvent(routeEvent{Ts: ts, Kind: "error", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: msg})
	case r.Fallbacks > 0:
		m.addEvent(routeEvent{Ts: ts, Kind: "fallback", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: "换了 " + strconv.Itoa(r.Fallbacks) + " 次候选, 最后落到 " + r.Upstream +
				lockNote(r.LockSkips)})
	case r.Queued:
		m.addEvent(routeEvent{Ts: ts, Kind: "queue", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: "排队 " + itoa64(r.QueueMs) + "ms"})
	}
	if r.TtfbMs >= 10000 {
		m.addEvent(routeEvent{Ts: ts, Kind: "slow", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: "首字节 " + itoa64(r.TtfbMs) + "ms, 吞吐 " + ftoa(r.Tps, 1) + " token/s"})
	}
}

func lockNote(n int) string {
	if n == 0 {
		return ""
	}
	return " (跳过 " + strconv.Itoa(n) + " 个已加锁的上游)"
}

// gateReason 把日志里的 gate 值换成中文短语。
func gateReason(g string) string {
	switch g {
	case "locked":
		return "候选都在锁里"
	case "sem_timeout":
		return "等并发槽位超时"
	case "queue_timeout":
		return "排队超时"
	case "canceled":
		return "客户端断开"
	}
	return g
}

func (m *tuiModel) addEvent(e routeEvent) {
	m.events = append(m.events, e)
	if len(m.events) > 4000 { // 只留最近的, 够翻就行
		m.events = append(m.events[:0], m.events[len(m.events)-2000:]...)
	}
}

// gateLocks 读 gate-state.json: 常驻服务启动时恢复的锁在这里, 内存观测面看不到。
func (m *tuiModel) gateLocks() {
	if m.logPath == "" {
		return
	}
	b, err := os.ReadFile(gatePathFor(m.logPath))
	if err != nil {
		return
	}
	var st gateState
	if json.Unmarshal(b, &st) != nil {
		return
	}
	now := time.Now()
	out := map[string]string{}
	for id, l := range st.Locks {
		if left := time.UnixMilli(l.UntilMs).Sub(now); left > 0 {
			out[id] = lockReason(l.Reason) + " " + durText(left)
		}
	}
	m.gates = out
}

// ---------- 轮询观测面 ----------

// pollState 同进程直接读内存; 另进程走 GET /v1/state?since=。
func (m *tuiModel) pollState(app *App, since uint64) uint64 {
	if app != nil {
		p := app.snapshot(since)
		m.pulse, m.live, m.lastErr = p, true, ""
		m.mergeEvents(p.Events)
		return p.MaxSeq
	}
	url := m.stateURL + "/v1/state?since=" + strconv.FormatUint(since, 10)
	cl := http.Client{Timeout: 2 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		m.live, m.lastErr = false, "观测面不可达: "+err.Error()
		return since
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var p pulse
	if err := json.Unmarshal(b, &p); err != nil {
		m.live, m.lastErr = false, "观测面返回异常: "+err.Error()
		return since
	}
	if p.Ver != viewVer { // 老二进制没有 /v1/state, 404 之外的路径别当成"没数据"
		m.live = false
		m.errf("对端没有观测面接口 (ver=%q): 那一边需要换成带 %s 的二进制并重启", p.Ver, viewVer)
		m.pulse = pulse{}
		return since
	}
	m.pulse, m.live, m.lastErr = p, true, ""
	m.mergeEvents(p.Events)
	return p.MaxSeq
}

// mergeEvents 把服务侧事件 (锁/恢复/请求结局) 并进事件流。
func (m *tuiModel) mergeEvents(evs []viewEvent) {
	for _, e := range evs {
		ts, _ := time.Parse(time.RFC3339Nano, e.Ts)
		m.addEvent(routeEvent{Ts: ts, Kind: e.Kind, Logical: e.Logical, Upstream: e.Upstream,
			Model: e.Model, Msg: e.Msg})
	}
}

// durText 是给句子用的时长写法 (窗口、倒计时): 6h0m -> 6 小时, 10m0s -> 10 分 0 秒。
// 表格里的窄列仍用 durStr 的紧凑写法 (8s / 1.2m 这种)。
func durText(d time.Duration) string {
	if d <= 0 {
		return "0 秒"
	}
	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + " 毫秒"
	case d < time.Minute:
		return ftoa(d.Seconds(), 1) + " 秒"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " 分 " + strconv.Itoa(int(d.Seconds())%60) + " 秒"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return strconv.Itoa(h) + " 小时"
	}
	return strconv.Itoa(h) + " 小时 " + strconv.Itoa(m) + " 分"
}

// ---------- 渲染 ----------

var tuiTabs = []string{"active", "routes", "events"}

func (m *tuiModel) draw() string {
	m.cols, m.rows = tuiSize()
	if m.cols < 60 {
		m.cols = 60
	}
	c := &tuiCanvas{cols: m.cols}
	// 版式: 标题 / 状态 / 空行 / 页签 / 空行 / 内容 / ... / 键位 (贴底)。
	// 固定占 7 行 (标题、状态、3 个空行、页签、键位), 其余全给内容。
	m.drawHeader(c)
	m.drawTabs(c)
	body := m.rows - 7
	if body < 3 {
		body = 3
	}
	switch m.tab {
	case 0:
		m.drawActive(c, body)
	case 1:
		m.drawRoutes(c, body)
	default:
		m.drawEvents(c, body)
	}
	// 补空行把键位提示压到最后一行。注意别把 countLines() 写进循环条件:
	// 它每轮都在长, 条件会自己收紧, 循环提前结束 (屏幕上就是"提示浮在中间")。
	for c.countLines() < m.rows-1 {
		c.line("")
	}
	c.last(tuiDim + m.hint() + tuiReset)
	// 万一体内容本身高过终端 (极小的窗口), 按行裁掉尾部, 别让提示被顶出屏幕。
	return "\x1b[H" + c.clip(m.rows)
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
	base := "  1/2/3 或 Tab 切页   up/down/PgUp/PgDn 滚动   r 重读日志   q 退出"
	switch m.tab {
	case 0:
		base += "   (active: 进行中的请求 + 上游队列状态)"
	case 1:
		base += "   (routes: 按 逻辑名 x 上游 x 真实模型 聚合, 窗口 " + durText(m.window) + ")"
	default:
		base += "   (events: 最新的在最上面)"
	}
	return base
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
	c.line(tuiBold + "tollgate tui" + tuiReset + "  " + tuiDim + "观测 " + src +
		"   监听 " + m.listen + "   日志 " + m.logPath + tuiReset)
	c.line("")
	act := len(m.pulse.Active)
	line := "状态 " + live + "   进行中 " + strconv.Itoa(act) +
		"   已读日志 " + strconv.Itoa(m.scanned) + " 条   运行 " + durText(time.Since(m.start))
	if e := m.lastError(); e != "" {
		line += "   " + tuiYell + e + tuiReset
	}
	c.line(line)
	c.line("") // 标题、状态与页签之间各留一行
}

func (m *tuiModel) drawTabs(c *tuiCanvas) {
	var b strings.Builder
	b.WriteString(" ")
	for i, name := range tuiTabs {
		if i == m.tab {
			b.WriteString(tuiRev + tuiBold + " " + name + " " + tuiReset)
		} else {
			b.WriteString(tuiDim + " " + strconv.Itoa(i+1) + " " + name + " " + tuiReset)
		}
		b.WriteString(" ")
	}
	// 每个上游一句话: 队列 / 上限 / 锁
	for _, cand := range m.pulse.Cands {
		state := tuiGreen + "正常" + tuiReset
		if s := candState(cand, m.gates[cand.ID]); s != "" {
			state = tuiRed + s + tuiReset
		}
		b.WriteString(tuiDim + "| " + tuiReset + cand.ID + " " +
			strconv.Itoa(cand.InFlight) + "/" + strconv.Itoa(cand.SoftCap) +
			" 排队" + strconv.Itoa(cand.Queue) + " " + state + " ")
	}
	c.line(b.String())
	c.line("") // 页签与内容之间留一行
}

// candState 拼候选的异常状态; 正常时返回空串。
func candState(c candView, gateLock string) string {
	var parts []string
	if c.LockMs > 0 {
		why := "额度锁"
		if c.LockWhy == "breaker" {
			why = "熔断锁"
		}
		parts = append(parts, why+" "+durText(time.Duration(c.LockMs)*time.Millisecond))
	} else if gateLock != "" {
		parts = append(parts, "锁 "+gateLock)
	}
	if c.PausedMs > 0 {
		parts = append(parts, "退避 "+durText(time.Duration(c.PausedMs)*time.Millisecond))
	}
	if c.ConcMs > 0 {
		parts = append(parts, "并发锁")
	}
	if c.Probing {
		parts = append(parts, "试探中")
	}
	if c.ErrStrk > 0 {
		parts = append(parts, "连续错误 "+strconv.Itoa(c.ErrStrk))
	}
	return strings.Join(parts, " ")
}

// tuiTable 是一张定宽表: 每列 标题 + 宽度 (+ 右对齐)。
// hide 是被隐藏的先后 (只统计可见列): 0 = 最后才砍, 越大越先砍; 4 = 永不砍。
type tuiCol struct {
	title string
	w     int
	right bool
	hide  int
}

func drawTable(c *tuiCanvas, cols []tuiCol, rows [][]string, scroll, limit int) {
	vis := fitCols(cols, c.cols)
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = tuiTrunc(col.title, col.w)
	}
	c.line(tuiBold + tuiReset + " " + strings.Join(alignRow(cols, head, vis), "  ") + tuiReset)
	if len(rows) == 0 {
		c.line(tuiDim + "  (空)" + tuiReset)
		return
	}
	end := scroll + limit
	if end > len(rows) {
		end = len(rows)
	}
	for i := scroll; i < end; i++ {
		c.line(" " + strings.Join(alignRow(cols, rows[i], vis), "  "))
	}
	if len(rows) > limit {
		c.line(tuiDim + "  ... 共 " + strconv.Itoa(len(rows)) + " 行, 当前 " +
			strconv.Itoa(scroll+1) + "-" + strconv.Itoa(end) + tuiReset)
	}
}

// fitCols 在窄终端上按 hide 档位丢列 (0 = 最后丢, 4 = 永不丢), 至少留 3 列。
func fitCols(cols []tuiCol, width int) []bool {
	vis := make([]bool, len(cols))
	for i := range vis {
		vis[i] = true
	}
	total := func() int {
		n, shown := 1, 0
		for i, col := range cols {
			if !vis[i] {
				continue
			}
			n += col.w + 2
			shown++
		}
		if shown > 1 {
			n -= 2
		}
		return n
	}
	for drop := 1; drop <= 3; drop++ {
		for i, col := range cols {
			if total() <= width {
				return vis
			}
			if col.hide == drop {
				vis[i] = false
			}
		}
	}
	return vis
}

// alignRow 只输出 vis 为真的列 (与 fitCols 一起决定窄终端的表宽)。
func alignRow(cols []tuiCol, cells []string, vis []bool) []string {
	out := make([]string, 0, len(cols))
	for i, col := range cols {
		if !vis[i] {
			continue
		}
		s := ""
		if i < len(cells) {
			s = cells[i]
		}
		s = tuiTrunc(s, col.w)
		if col.right {
			out = append(out, strings.Repeat(" ", max(0, col.w-tuiWidth(s)))+s)
		} else {
			out = append(out, tuiPad(s, col.w))
		}
	}
	return out
}

// drawActive: 上半是进行中的请求, 下半是上游队列与锁。
func (m *tuiModel) drawActive(c *tuiCanvas, body int) {
	rows := make([][]string, 0, len(m.pulse.Active))
	for _, f := range m.pulse.Active {
		tag := "非流式"
		if f.Stream {
			tag = "流式"
		}
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10),
			durStr(time.Since(f.StartTime)),
			f.Logical,
			f.Up,
			f.Model,
			tag,
			stageName(f),
			f.Source,
			f.Path,
		})
	}
	half := body / 2
	if half < 3 {
		half = 3
	}
	drawTable(c, []tuiCol{
		{"编号", 4, true, 1}, {"已耗时", 8, true, 0}, {"逻辑名", 22, false, 0},
		{"上游", 8, false, 0}, {"真实模型", 24, false, 0}, {"方式", 8, false, 2},
		{"当前状态", 18, false, 0}, {"来源", 8, false, 1}, {"路径", 18, false, 3},
	}, rows, m.scroll[0], half)

	c.line("")
	c.line(tuiBold + " 上游队列 (进行中/并发上限, 排队, 状态)" + tuiReset)
	crows := make([][]string, 0, len(m.pulse.Cands))
	for _, cand := range m.pulse.Cands {
		state := tuiGreen + "正常" + tuiReset
		if s := candState(cand, m.gates[cand.ID]); s != "" {
			state = tuiYell + s + tuiReset
		}
		next := "-"
		if cand.NextMs > 0 {
			next = durStr(time.Duration(cand.NextMs) * time.Millisecond)
		}
		crows = append(crows, []string{
			cand.ID,
			strconv.Itoa(cand.InFlight) + "/" + strconv.Itoa(cand.SoftCap),
			strconv.Itoa(cand.Queue),
			next,
			strconv.Itoa(cand.ErrStrk),
			state,
		})
	}
	drawTable(c, []tuiCol{
		{"上游", 10, false, 0}, {"进行中/上限", 11, true, 1}, {"排队", 4, true, 0},
		{"下次出队", 8, true, 2}, {"连续错误", 8, true, 2}, {"状态", 30, false, 0},
	}, crows, 0, len(crows))
}

// drawRoutes: 路由聚合表 + 顶部窗口汇总。
func (m *tuiModel) drawRoutes(c *tuiCanvas, body int) {
	rows := m.routeRows()
	c.line(tuiDim + " " + m.routeSummary(rows) + tuiReset)
	c.line(tuiDim + " " + m.sparkline() + tuiReset)
	c.line("")
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		errs := tuiGreen + "0" + tuiReset
		if r.Errs > 0 {
			errs = tuiRed + strconv.Itoa(r.Errs) + tuiReset
		}
		ttfb, tps := "-", "-"
		if r.TtfbN > 0 {
			ttfb = durStr(time.Duration(r.TtfbSum/int64(r.TtfbN)) * time.Millisecond)
		}
		if r.TpsN > 0 {
			tps = ftoa(r.TpsSum/float64(r.TpsN), 1)
		}
		cells = append(cells, []string{
			r.Logical, r.Upstream, r.Model,
			strconv.Itoa(r.Reqs), errs, strconv.Itoa(r.Fallbacks),
			humanTokens(r.Total), humanTokens(r.Cached),
			ttfb, tps, mages(r.Last), bucketBar(r.bucket),
		})
	}
	drawTable(c, []tuiCol{
		{"逻辑名", 22, false, 0}, {"上游", 8, false, 0}, {"真实模型", 24, false, 1},
		{"请求", 6, true, 0}, {"错误", 4, true, 0}, {"换候选", 7, true, 1},
		{"总 token", 9, true, 1}, {"缓存命中", 8, true, 3}, {"首字节", 8, true, 2},
		{"吞吐", 7, true, 2}, {"最近一次", 9, true, 2}, {strconv.Itoa(tuiBuckets) + " 分钟", tuiBuckets, false, 3},
	}, cells, m.scroll[1], body-3)
}

// drawEvents: 事件流 (最新在前); 连续同类事件折成一行 xN, 免得刷屏。
func (m *tuiModel) drawEvents(c *tuiCanvas, body int) {
	evs := make([]routeEvent, 0, len(m.events))
	evs = append(evs, m.events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].Ts.After(evs[j].Ts) })
	rows := make([][]string, 0, len(evs))
	for i := 0; i < len(evs); {
		e := evs[i]
		reps := 1
		for i+reps < len(evs) && sameEvent(e, evs[i+reps]) {
			reps++
		}
		msg := e.Msg
		if reps > 1 {
			msg += "  x" + strconv.Itoa(reps)
		}
		rows = append(rows, []string{
			e.Ts.Format("15:04:05"),
			tuiEventTag(e.Kind),
			e.Logical,
			e.Upstream,
			e.Model,
			msg,
		})
		i += reps
	}
	drawTable(c, []tuiCol{
		{"时间", 8, false, 0}, {"类型", 10, false, 0}, {"逻辑名", 20, false, 1},
		{"上游", 8, false, 1}, {"真实模型", 22, false, 2}, {"说明", 60, false, 0},
	}, rows, m.scroll[2], body)
}

// sameEvent 判断两条事件能不能折叠成一行 (同类 + 同路由 + 同文案)。
func sameEvent(a, b routeEvent) bool {
	return a.Kind == b.Kind && a.Logical == b.Logical && a.Upstream == b.Upstream &&
		a.Model == b.Model && a.Msg == b.Msg
}

// tuiEventKinds 把内部事件名换成屏幕上给的中文 (内部名字保持英文, 便于对日志)。
var tuiEventKinds = map[string]string{
	"start": "启动", "reload": "重载", "ok": "正常", "error": "错误",
	"fallback": "换候选", "gate": "闸门", "queue": "排队", "slow": "慢",
	"lock": "加锁", "unlock": "解锁",
}

// tuiEventTag 给事件类型上色; 窄终端不依赖颜色也能读。
func tuiEventTag(kind string) string {
	name, ok := tuiEventKinds[kind]
	if !ok {
		name = kind
	}
	switch kind {
	case "error", "gate", "lock":
		return tuiRed + name + tuiReset
	case "fallback", "slow", "queue":
		return tuiYell + name + tuiReset
	case "ok", "unlock":
		return tuiGreen + name + tuiReset
	}
	return tuiDim + name + tuiReset
}

// routeRows 按请求数降序 (最近有流量的排前面)。
func (m *tuiModel) routeRows() []*routeRow {
	out := make([]*routeRow, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reqs != out[j].Reqs {
			return out[i].Reqs > out[j].Reqs
		}
		return out[i].Last.After(out[j].Last)
	})
	return out
}

func (m *tuiModel) routeSummary(rows []*routeRow) string {
	var reqs, errs, fb, total, cached, ttfbN int
	var ttfbSum int64
	var first, last time.Time
	for _, r := range rows {
		reqs += r.Reqs
		errs += r.Errs
		fb += r.Fallbacks
		total += r.Total
		cached += r.Cached
		ttfbSum += r.TtfbSum
		ttfbN += r.TtfbN
		if r.First.IsZero() {
			continue
		}
		if first.IsZero() || r.First.Before(first) {
			first = r.First
		}
		if last.IsZero() || r.Last.After(last) {
			last = r.Last
		}
	}
	span := "n/a"
	if !first.IsZero() {
		span = durText(last.Sub(first).Round(time.Minute))
	}
	cache := "-"
	if total > 0 {
		cache = ftoa(float64(cached)*100/float64(total), 0) + "%"
	}
	ttfb := "-"
	if ttfbN > 0 {
		ttfb = durStr(time.Duration(ttfbSum/int64(ttfbN)) * time.Millisecond)
	}
	return "窗口 " + durText(m.window) + " (实际 " + span + ")   " + strconv.Itoa(len(rows)) + " 条路由   " +
		strconv.Itoa(reqs) + " 请求   " + strconv.Itoa(errs) + " 错误   " +
		strconv.Itoa(fb) + " 次换候选   " + humanTokens(total) + " token (缓存 " + cache +
		")   首字节平均 " + ttfb
}

// sparkline 是最近 N 分钟每分钟的请求数 (只看最近两条路由桶太粗, 这里全量累加)。
func (m *tuiModel) sparkline() string {
	var b [tuiBuckets]int
	for _, r := range m.routes {
		for i, v := range r.bucket {
			b[i] += int(v)
		}
	}
	max := 1
	for _, v := range b {
		if v > max {
			max = v
		}
	}
	var sb strings.Builder
	sb.WriteString("每分钟请求数 ")
	for _, v := range b {
		idx := 0
		if v > 0 {
			idx = 1 + v*(len(tuiBlocks)-2)/max
		}
		sb.WriteString(string(tuiBlocks[idx]))
	}
	sb.WriteString("  (峰值 " + strconv.Itoa(max) + "/min)")
	return sb.String()
}

func bucketBar(b [tuiBuckets]uint8) string {
	out := make([]byte, 0, tuiBuckets)
	for _, v := range b {
		// 5 档: 空白 = 没有请求, 之后按量级升档 (这是"热不热"的粗看, 不是精确刻度)。
		idx := 0
		switch {
		case v == 0:
			idx = 0
		case v < 5:
			idx = 1
		case v < 20:
			idx = 3
		case v < 50:
			idx = 5
		default:
			idx = 7
		}
		out = append(out, tuiBlocks[idx])
	}
	return string(out)
}

func mages(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s前"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + "m前"
	}
	return strconv.Itoa(int(d.Hours())) + "h前"
}

func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return ftoa(float64(n)/1e6, 1) + "M"
	case n >= 1_000:
		return ftoa(float64(n)/1e3, 1) + "k"
	}
	return strconv.Itoa(n)
}

func ftoa(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// ---------- 事件循环 ----------

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
		// SIGHUP 热加载别名与上游 (与常驻模式同一语义)。
		go func() {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGHUP)
			for range ch {
				c, err := loadConfig(cfgPath)
				if err != nil {
					app.evAdd("reload", "", "", "", "热加载失败: "+err.Error())
					continue
				}
				app.applyConfig(c)
				app.evAdd("reload", "", "", "", "热加载成功: "+cfgPath)
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
		m.gateLocks()
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
func (m *tuiModel) handleKey(b byte, pending *[]byte) bool {
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
		switch code {
		case 'A', 'U':
			m.move(-1)
		case 'B', 'V':
			m.move(1)
		case 'C':
			m.tab = (m.tab + 1) % len(tuiTabs)
		case 'D':
			m.tab = (m.tab + len(tuiTabs) - 1) % len(tuiTabs)
		}
		if code == 'U' {
			m.move(-10)
		}
		if code == 'V' {
			m.move(10)
		}
		return false
	}
	*pending = nil
	switch b {
	case 'q', 0x03, 0x04: // q / Ctrl-C / Ctrl-D
		return true
	case '1', '2', '3':
		m.tab = int(b - '1')
	case 'h', 0x1b:
		m.tab = (m.tab + len(tuiTabs) - 1) % len(tuiTabs)
	case 'l':
		m.tab = (m.tab + 1) % len(tuiTabs)
	case '\t':
		m.tab = (m.tab + 1) % len(tuiTabs)
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

// lockReason 把 gate-state.json 里的原因换成中文。
func lockReason(r string) string {
	if r == "breaker" {
		return "熔断"
	}
	return "额度"
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
