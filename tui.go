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
)

// 颜色标准 (全 TUI 一套, 新增画面前先对照这张表):
//
//	绿   = 健康 (live / ok / 解锁 / 错误数 0)
//	红   = 错误与锁 (error / 熔断锁 / 额度锁 / 并发锁 / 连续错误 > 0)
//	黄   = 等待与降级 (退避 / 换候选 / 慢 / 超发试探 / 顶部错误提示)
//	灰   = 说明 (标题 / 单位 / 位置行 / 空)
//	加粗 = 表头; 反显 = 当前选中行。
func colorDur(ms int64, color string) string {
	if ms <= 0 {
		return ""
	}
	return color + durStr(time.Duration(ms)*time.Millisecond) + tuiReset
}

// colorCount 给错误类计数上色: 0 绿色, 大于 0 红色。
func colorCount(n int) string {
	if n > 0 {
		return tuiRed + strconv.Itoa(n) + tuiReset
	}
	return tuiGreen + "0" + tuiReset
}

// tuiIndent 是所有行的左边界缩进: 标题、状态、页签、表格与键位提示都从同一列
// 起, 各行不再参差不齐。
const tuiIndent = "  "

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
	mu      sync.Mutex // 保护 lastErr (监听失败可能来自别的 goroutine)

	routes map[string]*routeRow
	events []routeEvent

	window       time.Duration     // 回看窗口 (行只算窗口内的记录)
	logPos       map[string]int64  // 每个日志文件读到哪
	logRest      map[string][]byte // 每文件各自的半行缓冲 (读到一半的行)
	scannedFiles map[string]bool   // 归档已整读, 不再重复扫
	scanned      int

	tab       int
	scroll    [4]int // 每页各自的滚动位置: m.scroll[m.tab]
	cols      int
	rows      int
	refreshed time.Time // 上次重读日志/取观测的时刻 (按键连发时别每次都重读)
	pageTotal int       // active / models 页的总行数 (整页滚动用)
	pageTop   int       // 该页当前窗口的第一行

	prio     *tuiEditor   // settings 页 (调度参数 + 权重编辑), runTUI 里装载
	reloadFn func() error // 同进程模式的热加载回调; 空 = 走 deploy/tollgate.sh hup

	// models 页上游表的排序键 (provider 优先级), 按 store / 配置的 mtime 失效。
	provRank    map[string][2]int
	provRankMod [2]time.Time
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
		routes: map[string]*routeRow{},
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
		if r.BackoffSkips > 0 {
			msg += " (跳过退避 " + strconv.Itoa(r.BackoffSkips) + " 次)"
		}
		m.addEvent(routeEvent{Ts: ts, Kind: "error", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: msg})
	case r.Fallbacks > 0:
		m.addEvent(routeEvent{Ts: ts, Kind: "fallback", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: "换了 " + strconv.Itoa(r.Fallbacks) + " 次候选, 最后落到 " + r.Upstream +
				lockNote(r.LockSkips) + backoffNote(r.BackoffSkips)})
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

// lockNote 拼"跳过几个已加锁的上游"这句; 退避跳过的模型另说一句 (两种跳过原因不同)。
func lockNote(n int) string {
	if n == 0 {
		return ""
	}
	return " (跳过 " + strconv.Itoa(n) + " 个已加锁的上游)"
}

func backoffNote(n int) string {
	if n == 0 {
		return ""
	}
	return " (跳过 " + strconv.Itoa(n) + " 个正在退避的模型)"
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
	case "backoff_timeout":
		return "等退避结束超时"
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

// ---------- 渲染 ----------

// 页号与页签一一对应 (tuiTabs 的次序); 比较 m.tab 时用这几个名字, 别写裸数字。
const (
	tabActive   = 0 // 活动连接, 按流程阶段分块
	tabModels   = 1 // 模型队列 / 上游锁 + 路由聚合
	tabEvents   = 2
	tabSettings = 3 // 设置: gate 参数 + 优先级
)

// 时长一律走 durStr (12s / 02:03 / 01:02:03), 屏幕上不再出现"分 / 秒 / 小时"这类汉字写法。
var tuiTabs = []string{"active", "models", "events", "settings"}

func (m *tuiModel) draw() string {
	m.cols, m.rows = tuiSize()
	if m.cols < 60 {
		m.cols = 60
	}
	c := &tuiCanvas{cols: m.cols}
	// 版式: 标题 / 状态 / 页签 / 内容 / ... / 键位 (贴底); 上游状态只在 models 页里看。
	m.drawHeader(c)
	m.drawTabs(c)
	body := m.bodyRows()
	switch m.tab {
	case tabActive:
		m.drawFlow(c, body)
	case tabModels:
		m.drawModels(c, body)
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

// bodyRows 是内容区行数: 固定占 4 行 (标题、状态、页签、键位), 其余全部给内容。
// 布局不用空行做分隔, 屏幕上每一行都是内容或键位。
func (m *tuiModel) bodyRows() int {
	if body := m.rows - 4; body >= 3 {
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
		return "  m/p/g 换面板   j/k 移动   +/- 调值   = 输入   s 保存" +
			"   r 生成   h 热加载   q 退出   (" + m.prioHint() + ")"
	}
	base := "  1-4 或 Tab 切页   up/down/PgUp/PgDn 滚动   r 重读日志   q 退出"
	switch m.tab {
	case tabActive, tabModels:
		// 窗口信息放最前: 键位行本来就长, 放尾巴会被窄终端截掉。
		base += "   ("
		if m.pageTotal > 0 {
			base += "行 " + strconv.Itoa(m.pageTop+1) + "-" +
				strconv.Itoa(min(m.pageTop+m.bodyRows(), m.pageTotal)) + "/" + strconv.Itoa(m.pageTotal) + ", "
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
	line := "状态 " + live + "   进行中 " + strconv.Itoa(act) +
		"   已读日志 " + strconv.Itoa(m.scanned) + " 条   运行 " + durStr(time.Since(m.start))
	if e := m.lastError(); e != "" {
		line += "   " + tuiYell + e + tuiReset
	}
	c.line(tuiIndent + line)
}

// drawTabs 画页签行; 上游状态不放在顶端, 统一看 models 页。
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

// tuiTable 是一张表: 每列 标题 + 基准宽 + 内容自适应上限 (+ 右对齐)。
// w 是基准宽: 内容伸缩与压缩都以它为下限。
// max 是内容自适应的上限 (max <= w = 定宽列, 不随内容涨)。
// hide 是被牺牲的先后 (只统计可见列): 0 = 最后才砍, 越大越先砍; 4 = 永不砍。
type tuiCol struct {
	title string
	w     int
	max   int
	right bool
	hide  int
}

// drawTable 画一张表; units 非空时在表下一行标出各列单位 (灰字)。
// drawTable 画一张表: 标题行 + 表头 + 行 + 滚动位置 + 单位行 (标题与单位是灰字, 空表也画)。
func drawTable(c *tuiCanvas, title string, cols []tuiCol, rows [][]string, scroll, limit int, units string) {
	cols = autoCols(cols, rows, c.cols)
	vis := fitCols(cols, c.cols)
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = tuiTrunc(col.title, col.w)
	}
	if title != "" {
		c.line(tuiDim + tuiIndent + title + tuiReset)
	}
	c.line(tuiIndent + tuiBold + strings.Join(alignRow(cols, head, vis), "  ") + tuiReset)
	if len(rows) == 0 {
		c.line(tuiDim + "  (空)" + tuiReset)
		if units != "" {
			c.line(tuiDim + tuiIndent + units + tuiReset)
		}
		return
	}
	top := clampScroll(scroll, len(rows), limit)
	end := min(top+limit, len(rows))
	// 位置行画在行之前: 内容高过屏幕时画布会裁尾部, 放最后就会被裁掉 (行比位置重要,
	// 所以裁的是行, 不是它)。
	if len(rows) > limit {
		c.line(tuiDim + "  " + strconv.Itoa(len(rows)) + " 行, 当前 " +
			strconv.Itoa(top+1) + "-" + strconv.Itoa(end) + tuiReset)
	}
	for i := top; i < end; i++ {
		c.line(tuiIndent + strings.Join(alignRow(cols, rows[i], vis), "  "))
	}
	if units != "" {
		c.line(tuiDim + tuiIndent + units + tuiReset)
	}
}

// clampScroll 把滚动位置夹进 [0, rows-limit]: 空表与 G 键 (设成很大) 都靠它兜底,
// 否则会画出一片空白。整页滚动与逐表滚动共用这一处口径。
func clampScroll(top, rows, limit int) int {
	if limit < 1 {
		limit = 1
	}
	if maxTop := rows - limit; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// fitCols 在窄终端上按 hide 档位丢列 (0 = 最后丢, 4 = 永不丢), 至少留 3 列。
func fitCols(cols []tuiCol, width int) []bool {
	vis := make([]bool, len(cols))
	for i := range vis {
		vis[i] = true
	}
	total := func() int {
		n, shown := len(tuiIndent), 0
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

// tableWidth 是一行表格的显示宽度: 缩进 + 各列列宽 + 列间距 (两空格)。
func tableWidth(cols []tuiCol) int {
	n := len(tuiIndent)
	for i, col := range cols {
		if i > 0 {
			n += 2
		}
		n += col.w
	}
	return n
}

// cutRank 是列被牺牲的先后: 越小越先被牺牲 (hide 4 = 永不砍, 排最前)。
func cutRank(hide int) int {
	if hide >= 4 {
		return -1
	}
	return hide
}

// autoCols 按内容与可用宽度分配列宽: 每列先占住基准宽 (下限), 余量按"要得少的先补齐"
// 分给各列 (差得一样多时重要的先), 补到该列的内容宽 (上限 max) 为止。这样能补齐的列最多,
// 贪吃的那列 (通常是模型名) 吃剩下的余量、必要时截断; 连基准宽都放不下才原样返回,
// 交给 fitCols 按老规矩丢列。
func autoCols(cols []tuiCol, rows [][]string, width int) []tuiCol {
	out := append([]tuiCol(nil), cols...)
	base := tableWidth(out)
	if base > width {
		return out
	}
	need := make([]int, len(cols))
	for i, col := range cols {
		need[i] = col.w // 定宽列 (max <= w) 的需求就是基准宽
		if col.max <= col.w {
			continue
		}
		n := tuiWidth(col.title)
		for _, r := range rows {
			if i < len(r) && tuiWidth(r[i]) > n {
				n = tuiWidth(r[i])
			}
		}
		need[i] = min(max(n, col.w), col.max)
	}
	order := make([]int, len(cols))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		da, db := need[ia]-out[ia].w, need[ib]-out[ib].w
		if da != db {
			return da < db
		}
		return cutRank(cols[ia].hide) < cutRank(cols[ib].hide)
	})
	free := width - base
	for _, i := range order {
		if free <= 0 {
			break
		}
		give := min(free, need[i]-out[i].w)
		out[i].w += give
		free -= give
	}
	return out
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

// 更宽时按内容涨, 上限见各列 max。上游与真实模型合成一格, 写法 "上游/模型"
// (与 -usage 的 key 一致, 如 ustc/gpt-5-codex)。
var activeCols = []tuiCol{
	{"编号", 4, 6, true, 1}, {"已耗时", 6, 8, true, 0}, {"逻辑名", 10, 28, false, 0},
	{"上游/模型", 18, 48, false, 0}, {"流式", 4, 4, false, 2},
	{"状态", 10, 26, false, 0}, {"来源", 8, 14, false, 1}, {"路径", 12, 30, false, 3},
}

// flowBlocks 是第 1 页的分块: 顺序就是请求在调度器里的流程顺序。
// 解析与取信号量合成第一块 (解析是瞬时状态, 单独一块只会闪一下)。
var flowBlocks = []struct {
	title  string
	stages []int
}{
	{"获取信号量", []int{stageParse, stageSem}},
	{"排队 (等出队节拍)", []int{stageQueue}},
	{"等模型窗口 (退避 / 熔断锁)", []int{stageModelWait}},
	{"等首字节", []int{stageSend}},
	{"接收中", []int{stageRecv}},
	{"已失败", []int{stageFailed}},
}

func stageIn(st int, list []int) bool {
	for _, v := range list {
		if v == st {
			return true
		}
	}
	return false
}

// flowCells 是一笔活动连接的表格行 (列与旧版一致: 时间、逻辑名、上游/模型…) 。
func flowCells(f flow) []string {
	tag := "否"
	if f.Stream {
		tag = "是"
	}
	return []string{
		strconv.FormatUint(f.ID, 10),
		durStr(time.Since(f.StartTime)),
		f.Logical,
		upModel(f.Up, f.Model),
		tag,
		stageName(f),
		f.Source,
		f.Path,
	}
}

// drawFlow 是第 1 页: 活动连接按流程阶段分块, 整页一起滚动。
// 空块压成一行 (标题 + 空), 后面补两行空行, 让相邻两块的标题不贴在一起;
// 整列都空的时候把那一列丢掉。
func (m *tuiModel) drawFlow(c *tuiCanvas, body int) {
	grouped := make([][][]string, len(flowBlocks))
	first := -1
	for i, b := range flowBlocks {
		for _, f := range m.pulse.Active {
			if stageIn(f.Stage, b.stages) {
				grouped[i] = append(grouped[i], flowCells(f))
			}
		}
		if first < 0 && len(grouped[i]) > 0 {
			first = i
		}
	}
	full := &tuiCanvas{cols: c.cols}
	for i, b := range flowBlocks {
		rows := grouped[i]
		if len(rows) == 0 {
			full.line(tuiDim + tuiIndent + b.title + " (0)  (空)" + tuiReset)
			// 空块只占一行, 不补就和下一块的标题贴在一起; 有内容的块自带表头与行,
			// 标题之间已经隔开, 这里只补空块缺的两行。
			if i < len(flowBlocks)-1 {
				full.line("")
				full.line("")
			}
			continue
		}
		cols, rows := dropEmptyCols(activeCols, rows)
		units := ""
		if i == first {
			units = "单位: 已耗时 = 12s / 02:03 / 01:02:03"
		}
		drawTable(full, b.title+" ("+strconv.Itoa(len(rows))+")", cols, rows, 0, len(rows), units)
	}
	m.drawWindow(c, full, body)
}

// dropEmptyCols 去掉整列都是空串的列 (例如这批请求都没有来源 / 路径时)。
// 列与行一起裁, 下标保持一致。
func dropEmptyCols(cols []tuiCol, rows [][]string) ([]tuiCol, [][]string) {
	keep := make([]bool, len(cols))
	for i := range keep {
		for _, r := range rows {
			if i < len(r) && strings.TrimSpace(r[i]) != "" {
				keep[i] = true
				break
			}
		}
	}
	outCols := make([]tuiCol, 0, len(cols))
	for i, col := range cols {
		if keep[i] {
			outCols = append(outCols, col)
		}
	}
	outRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		cell := make([]string, 0, len(outCols))
		for i := 0; i < len(cols) && i < len(r); i++ {
			if keep[i] {
				cell = append(cell, r[i])
			}
		}
		outRows = append(outRows, cell)
	}
	return outCols, outRows
}

// drawWindow 把整页画布按滚动位置裁一屏贴到 c (第 1 / 2 页共用)。
// 表头会跟着内容一起滚走, 所以当前窗口写在键位提示行里 (本页 N 行, 当前 a-b)。
func (m *tuiModel) drawWindow(c *tuiCanvas, full *tuiCanvas, body int) {
	lines := full.splitLines()
	m.pageTotal = len(lines)
	top := m.scrollWindow(len(lines), body)
	m.pageTop = top
	for _, ln := range lines[top:min(top+body, len(lines))] {
		c.raw(ln)
	}
}

// sortedCands 是 models 页上游表的顺序: provider 优先级降序, 同优先级保持配置顺序
// (与 settings 页的 provider 面板同口径)。排序键按 store / 配置的 mtime 缓存, 不每帧
// 读文件; store 读不到时保持观测面原序。
func (m *tuiModel) sortedCands() []candView {
	cands := append([]candView(nil), m.pulse.Cands...)
	rank := m.provRanks()
	if len(rank) == 0 {
		return cands
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ri, iok := rank[cands[i].ID]
		rj, jok := rank[cands[j].ID]
		if iok != jok {
			return iok // 配置里有的排前面
		}
		if ri[0] != rj[0] {
			return ri[0] > rj[0]
		}
		return ri[1] < rj[1]
	})
	return cands
}

// provRanks 缓存排序键: 只有 store 或配置的文件时间变了才重读, 读不到就缓存空表,
// 免得每帧反复解析。
func (m *tuiModel) provRanks() map[string][2]int {
	sm, serr := os.Stat(storePathFor(m.cfgPath))
	cm, cerr := os.Stat(m.cfgPath)
	if serr != nil || cerr != nil {
		return nil
	}
	key := [2]time.Time{sm.ModTime(), cm.ModTime()}
	if m.provRank == nil || key != m.provRankMod {
		rank := provRank(m.cfgPath)
		if rank == nil {
			rank = map[string][2]int{}
		}
		m.provRank, m.provRankMod = rank, key
	}
	return m.provRank
}

// drawModels 是第 2 页: 先上游状态 (并发 / 锁 / 出队), 再列有过请求的 上游/模型 队列,
// 最下面是路由聚合。整页一起滚动。只出现实际上游/模型, 不按逻辑名或配置候选铺开。
func (m *tuiModel) drawModels(c *tuiCanvas, body int) {
	full := &tuiCanvas{cols: c.cols}
	var urows [][]string
	for _, cand := range m.sortedCands() {
		probe, conc, quota := "否", "-", "-"
		if cand.Probing {
			probe = tuiYell + "是" + tuiReset
		}
		if cand.ConcMs > 0 {
			conc = colorDur(cand.ConcMs, tuiRed) // 锁 = 红
		}
		if cand.LockMs > 0 {
			quota = colorDur(cand.LockMs, tuiRed) // 表头已经写明是额度锁
		}
		next := "-"
		if cand.NextMs > 0 {
			next = durStr(time.Duration(cand.NextMs) * time.Millisecond)
		}
		urows = append(urows, []string{
			cand.ID,
			strconv.Itoa(cand.InFlight) + "/" + strconv.Itoa(cand.SoftCap),
			strconv.Itoa(cand.Queue), probe, conc, quota, next, colorCount(cand.ErrStrk),
		})
	}
	drawTable(full, "上游状态 (并发 / 锁 / 出队)", []tuiCol{
		{"上游", 10, 0, false, 0}, {"进行中/上限", 11, 0, true, 1}, {"排队", 4, 0, true, 0},
		{"试探", 4, 0, false, 2}, {"并发锁", 8, 0, true, 2}, {"额度锁", 14, 0, true, 2},
		{"下次出队", 8, 0, true, 3}, {"连续错误", 8, 0, true, 3},
	}, urows, 0, len(urows),
		"单位: 进行中/上限、排队、连续错误 = 个; 并发锁、额度锁、下次出队 = 12s / 02:03 / 01:02:03")
	full.line("") // 两张表之间空一行: 上面是上游状态, 下面是汇总 + 模型队列

	// 合并表: 队列状态 (观测面) + 聚合统计 (日志), 行 = 有过请求的 上游/真实模型。
	rrows := m.routeRows()
	full.line(tuiDim + tuiIndent + m.routeSummary(rrows) + tuiReset)
	full.line(tuiDim + tuiIndent + m.sparkline() + tuiReset)

	var mrows [][]string
	for _, r := range m.modelRows() {
		// 队列的锁都画在队列表里: 退避 = 黄 (等待), 熔断锁 = 红 (锁)。
		paused := colorDur(r.PausedMs, tuiYell)
		lock := colorDur(r.LockMs, tuiRed)
		errs := colorCount(r.Errs)
		ttfb, tps := "-", "-"
		if r.TtfbN > 0 {
			ttfb = durStr(time.Duration(r.TtfbSum/int64(r.TtfbN)) * time.Millisecond)
		}
		if r.TpsN > 0 {
			tps = ftoa(r.TpsSum/float64(r.TpsN), 1)
		}
		// 与 mcols 的顺序一一对应 (dropEmptyCols 按下标裁列)。
		mrows = append(mrows, []string{
			upModel(r.up, r.model), strconv.Itoa(r.Queue),
			strconv.Itoa(r.Reqs), errs, paused, lock,
			humanTokens(r.Total), ttfb, mages(r.Last),
			colorCount(r.Fails), strconv.Itoa(r.Fallbacks), tps,
			humanTokens(r.Cached), bucketBar(r.bucket),
		})
	}
	mcols, mrows := dropEmptyCols([]tuiCol{
		{"上游/模型", 24, 60, false, 4}, {"排队", 4, 0, true, 0},
		{"请求", 6, 0, true, 0}, {"错误", 4, 0, true, 0},
		{"退避", 8, 0, true, 4}, {"熔断锁", 8, 0, true, 4},
		{"总 token", 9, 0, true, 3}, {"首字节", 8, 0, true, 3},
		{"最近一次", 9, 0, true, 2}, {"连续错误", 8, 0, true, 2},
		{"换候选", 7, 0, true, 1}, {"吞吐", 7, 0, true, 1},
		{"缓存命中", 8, 0, true, 1}, {strconv.Itoa(tuiBuckets) + "m 条", tuiBuckets, 0, false, 1},
	}, mrows)
	drawTable(full, "模型队列 + 路由聚合 (上游/真实模型, 按最近一次请求时间降序)", mcols, mrows, 0, len(mrows),
		"单位: 排队/连续错误/请求/错误/换候选 = 个; token = 个; 时长 = 12s / 02:03 / 01:02:03; "+
			"吞吐 = token/s; "+strconv.Itoa(tuiBuckets)+"m 条 = 每分钟请求数")

	m.drawWindow(c, full, body)
}

// modelRow 是第 2 页合并表的一行: 队列状态 (观测面) + 聚合统计 (日志, 按上游/模型求和,
// 不区分逻辑名)。
type modelRow struct {
	up, model        string
	Queue, Fails     int
	PausedMs, LockMs int64
	Reqs             int
	Errs             int
	Fallbacks        int
	Total, Cached    int
	TtfbSum          int64
	TtfbN            int
	TpsSum           float64
	TpsN             int
	Last             time.Time
	bucket           [tuiBuckets]uint8
}

// modelRows 合并观测面里的模型队列与日志聚合: 只保留真正有过请求的 上游/模型。
func (m *tuiModel) modelRows() []*modelRow {
	by := map[string]*modelRow{}
	get := func(up, model string) *modelRow {
		k := up + "\x00" + model
		r := by[k]
		if r == nil {
			r = &modelRow{up: up, model: model}
			by[k] = r
		}
		return r
	}
	for _, cand := range m.pulse.Cands {
		for _, md := range cand.Models {
			r := get(cand.ID, md.Model)
			r.Queue, r.Fails, r.PausedMs, r.LockMs = md.Queue, md.Fails, md.PausedMs, md.LeftMs
		}
	}
	for _, rr := range m.routes {
		if rr.Upstream == "" && rr.Model == "" {
			continue // 没路由到任何上游的记录 (如 unknown model) 不属于模型视角
		}
		r := get(rr.Upstream, rr.Model)
		r.Reqs += rr.Reqs
		r.Errs += rr.Errs
		r.Fallbacks += rr.Fallbacks
		r.Total += rr.Total
		r.Cached += rr.Cached
		r.TtfbSum += rr.TtfbSum
		r.TtfbN += rr.TtfbN
		r.TpsSum += rr.TpsSum
		r.TpsN += rr.TpsN
		if rr.Last.After(r.Last) {
			r.Last = rr.Last
		}
		for i, v := range rr.bucket {
			if int(r.bucket[i])+int(v) > 255 {
				r.bucket[i] = 255
			} else {
				r.bucket[i] += v
			}
		}
	}
	out := make([]*modelRow, 0, len(by))
	for _, r := range by {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		if out[i].Reqs != out[j].Reqs {
			return out[i].Reqs > out[j].Reqs
		}
		if out[i].up != out[j].up {
			return out[i].up < out[j].up
		}
		return out[i].model < out[j].model
	})
	return out
}

// scrollWindow 把该页的滚动位置夹进实际行数, 返回窗口第一行 —— 空表与 G 键 (设成很大)
// 都靠它兜底, 免得画出一片空白。
func (m *tuiModel) scrollWindow(rows, limit int) int {
	top := clampScroll(m.scroll[m.tab], rows, limit)
	m.scroll[m.tab] = top
	return top
}

// raw 贴一行已经画好的内容 (整页滚动取窗口用): 行尾的清除序列由内容自己带着。
func (c *tuiCanvas) raw(line string) {
	c.b.WriteString(line)
	c.b.WriteString("\r\n")
}

// splitLines 把画布切成一行行 (每行含行尾的清除序列), 供整页滚动取窗口。
func (c *tuiCanvas) splitLines() []string {
	s := strings.TrimSuffix(c.b.String(), "\r\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\r\n")
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
			upModel(e.Upstream, e.Model),
			msg,
		})
		i += reps
	}
	drawTable(c, "事件流 (最新在前)", []tuiCol{
		{"时间", 8, 0, false, 0}, {"类型", 10, 0, false, 0}, {"逻辑名", 20, 28, false, 1},
		{"上游/模型", 30, 46, false, 2}, {"说明", 60, 80, false, 0},
	}, rows, m.scrollWindow(len(rows), max(1, body-4)), max(1, body-4), "单位: 时间 = hh:mm:ss (时刻)")
}

// upModel 把上游与真实模型拼成一格 (`上游/模型`, 与活跃连接表和路由表同一写法);
// 有一边为空时不留那个斜杠。
func upModel(up, model string) string {
	switch {
	case up == "":
		return model
	case model == "":
		return up
	}
	return up + "/" + model
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
	"lock": "加锁", "unlock": "解锁", "breaker": "熔断",
}

// tuiEventTag 给事件类型上色; 窄终端不依赖颜色也能读。
func tuiEventTag(kind string) string {
	name, ok := tuiEventKinds[kind]
	if !ok {
		name = kind
	}
	switch kind {
	case "error", "gate", "lock", "breaker":
		return tuiRed + name + tuiReset
	case "fallback", "slow", "queue":
		return tuiYell + name + tuiReset
	case "ok", "unlock":
		return tuiGreen + name + tuiReset
	}
	return tuiDim + name + tuiReset
}

// routeRows 按最后使用时间降序 (最近用过的排最上面); 同一时刻的按请求数兜底。
func (m *tuiModel) routeRows() []*routeRow {
	out := make([]*routeRow, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Reqs > out[j].Reqs
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
		span = durStr(last.Sub(first))
	}
	cache := "-"
	if total > 0 {
		cache = ftoa(float64(cached)*100/float64(total), 0) + "%"
	}
	ttfb := "-"
	if ttfbN > 0 {
		ttfb = durStr(time.Duration(ttfbSum/int64(ttfbN)) * time.Millisecond)
	}
	return "窗口 " + durStr(m.window) + " (实际 " + span + ")   " + strconv.Itoa(len(rows)) + " 条路由   " +
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

// mages 是"最近一次"列的写法: 距现在多久 (与其它时长同一口径)。
func mages(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return durStr(time.Since(t))
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
// settings 页把编辑键留给自己 (h = 热加载, r = 生成配置, m/p/g = 换面板), 状态页的
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
		case 'r':
			e.regenerate()
		case 'h':
			m.prioHup()
		case 'l', '\t':
			m.switchTab(1)
		case '1', '2', '3', '4':
			m.switchTabTo(int(b - '1'))
		}
		return false
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
