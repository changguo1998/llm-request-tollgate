// tui_events_filter.go: 第 2 页 (events) 的筛选与时间范围回读。
//
// 文本筛选 (f 键) 用同一个子串匹配 编号 / 逻辑名 / 事件 三列, 不区分大小写, 空串即清除。
// 时间范围 (t 键) 支持 10:00-11:00 / 10:00 / 30m / 2h 几种写法, 空串即清除。范围生效时
// 不再受内存里 256 条事件上限 (tuiEventKeep) 的约束: 直接从日志回读当日 + 前一日该范围内
// 会改变程序状态的记录, 结果取代默认的 256 条; 文本筛选在显示层再叠上去。日志里只有结局,
// 所以回读出来的只有结局事件 (阶段链与锁 / 探测没有), 这与事件回放 (replayEvent) 同一口径。

package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// evFilter 是第 2 页的筛选态。from 为零 = 不限时间 (只做文本筛选)。
type evFilter struct {
	text  string        // 原文 (标题行显示用)
	q     string        // 归一化后的查询串 (小写)
	from  time.Time     // 时间范围起点; 零值 = 不限
	to    time.Time     // 终点; 零值 = 到现在 (跟随)
	slide time.Duration // >0 = 相对时长 (30m / 2h), 起点每拍跟着现在走

	rows []routeEvent // 时间范围回读的事件 (取代默认的 256 条上限)
	pos  map[string]int64
	rest map[string][]byte
	done map[string]bool // 归档已整读
	seen map[evKey]bool  // 去重 (日志轮转后同一批记录可能换名再被扫到)
}

// evKey 是回读事件的去重键: 日志一记录一结局, 时间戳 + 编号就能唯一确定一条。
type evKey struct {
	ts   time.Time
	req  uint64
	kind string
	msg  string
}

// setText 换文本筛选; 空串 = 清除。
func (f *evFilter) setText(s string) {
	f.text = strings.TrimSpace(s)
	f.q = strings.ToLower(f.text)
}

// setRange 换时间范围, 并清掉上一轮的回读进度与结果 (由 pollEventFilter 重建)。
func (f *evFilter) setRange(from, to time.Time, slide time.Duration) {
	f.from, f.to, f.slide = from, to, slide
	f.resetScan()
}

// resetScan 丢掉回读进度与结果, 下一次 pollEventFilter 从头扫。
func (f *evFilter) resetScan() {
	f.rows = nil
	f.pos = nil
	f.rest = nil
	f.done = nil
	f.seen = nil
}

// clear 清除全部筛选。
func (f *evFilter) clear() {
	f.text, f.q = "", ""
	f.from, f.to, f.slide = time.Time{}, time.Time{}, 0
	f.resetScan()
}

// active 表示当前有筛选在生效。
func (f *evFilter) active() bool { return f.text != "" || !f.from.IsZero() }

// ensure 给回读用的表补上初值 (筛选清空后这些表都是 nil)。
func (f *evFilter) ensure() {
	if f.pos == nil {
		f.pos = map[string]int64{}
		f.rest = map[string][]byte{}
		f.done = map[string]bool{}
		f.seen = map[evKey]bool{}
	}
}

// inRange 判断一条原始事件的时刻是否落在时间范围内 (零值范围 = 全部)。
func (f *evFilter) inRange(ts time.Time) bool {
	if f.from.IsZero() {
		return true
	}
	if ts.Before(f.from) {
		return false
	}
	return f.to.IsZero() || !ts.After(f.to)
}

// matchText 判断一条显示行是否命中文本筛选: 编号 / 逻辑名 / 事件任一含该子串。
// 没有编号的事件 (req=0, 锁 / 探测一类) 不参与编号匹配, 免得 "0" 把所有事件都筛出来。
func (f *evFilter) matchText(r routeEvent) bool {
	if f.q == "" {
		return true
	}
	if r.Req > 0 && strings.Contains(strconv.FormatUint(r.Req, 10), f.q) {
		return true
	}
	return strings.Contains(strings.ToLower(r.Logical), f.q) ||
		strings.Contains(strings.ToLower(r.Msg), f.q)
}

// evRangeHelp 是时间范围写法不对时的提示。
const evRangeHelp = "时间范围写法不对: 用 10:00-11:00 / 10:00 / 30m / 2h"

// parseEvRange 解析时间范围输入:
//
//	30m / 2h / 90s   最近这么久, 起点每拍跟着现在走
//	10:00            最近一次 10:00 到现在
//	10:00-11:00      最近一次 10:00 到其后的 11:00 (t2 <= t1 视为跨午夜)
//	10:00-           最近一次 10:00 到现在
//
// `~` 与 `-` 等价。起点夹到事件回放窗口 (当日 + 前一日 00:00); 终点落在未来时按 "到现在"
// 处理 (持续跟进)。空串 = 清除; 写法不对时返回提示文案, 原有范围保持不变。
func parseEvRange(s string, now time.Time) (from, to time.Time, slide time.Duration, errMsg string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, time.Time{}, 0, ""
	}
	if d, ok := parseDur(s); ok {
		from = now.Add(-d)
		if lo := eventReplayFrom(now); from.Before(lo) {
			from = lo
		}
		return from, time.Time{}, d, ""
	}
	sep := "-"
	if strings.Contains(s, "~") {
		sep = "~"
	}
	parts := strings.SplitN(s, sep, 2)
	if len(parts) == 2 && strings.TrimSpace(parts[1]) == "" {
		parts = parts[:1] // "10:00-" = 从那时刻到现在
	}
	h, mi, sec, ok := parseClock(parts[0])
	if !ok {
		return time.Time{}, time.Time{}, 0, evRangeHelp
	}
	from = clockAt(now, h, mi, sec)
	if lo := eventReplayFrom(now); from.Before(lo) {
		from = lo
	}
	if len(parts) == 1 {
		return from, time.Time{}, 0, ""
	}
	h, mi, sec, ok = parseClock(parts[1])
	if !ok {
		return time.Time{}, time.Time{}, 0, evRangeHelp
	}
	to = time.Date(from.Year(), from.Month(), from.Day(), h, mi, sec, 0, now.Location())
	if to.Before(from) {
		to = to.Add(24 * time.Hour) // 跨午夜: 23:00-01:00
	}
	if to.After(now) {
		to = time.Time{} // 终点还没到: 到现在, 继续跟进
	}
	return from, to, 0, ""
}

// parseClock 解析 hh:mm 或 hh:mm:ss (小时可写一位数)。
func parseClock(s string) (h, m, sec int, ok bool) {
	fields := strings.Split(strings.TrimSpace(s), ":")
	if len(fields) < 2 || len(fields) > 3 {
		return
	}
	vals := make([]int, len(fields))
	for i, f := range fields {
		if len(f) == 0 || len(f) > 2 {
			return
		}
		for j := 0; j < len(f); j++ {
			if f[j] < '0' || f[j] > '9' {
				return
			}
		}
		vals[i], _ = strconv.Atoi(f)
	}
	h, m = vals[0], vals[1]
	if len(vals) == 3 {
		sec = vals[2]
	}
	if h > 23 || m > 59 || sec > 59 {
		return
	}
	return h, m, sec, true
}

// clockAt 把一天里的时刻锚到最近一次: 今天该时刻还没到就算昨天。
func clockAt(now time.Time, h, m, sec int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, sec, 0, now.Location())
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// evRangeLabel 把时间范围写成一行: 同一天写 "01-02 15:04-15:05", 跨天两头都写日期;
// 终点为零 = 到现在。
func evRangeLabel(from, to time.Time) string {
	if to.IsZero() {
		return from.Format("01-02 15:04") + " 起"
	}
	if from.Format("01-02") == to.Format("01-02") {
		return from.Format("01-02 15:04") + "-" + to.Format("15:04")
	}
	return from.Format("01-02 15:04") + "-" + to.Format("01-02 15:04")
}

// pollEventFilter 回读日志, 把时间范围内的结局事件补进 ev.rows: 归档整读一次 (done),
// 当前文件增量读; 相对范围 (slide > 0) 每拍把起点往前推并裁掉滑出窗口的事件。
// 时间范围生效时第 2 页改用这批回读结果, 因而不受 256 条事件上限约束。
func (m *tuiModel) pollEventFilter() {
	f := &m.ev
	if f.from.IsZero() || m.logPath == "" {
		return
	}
	f.ensure()
	now := time.Now()
	if f.slide > 0 {
		f.from = now.Add(-f.slide)
		if lo := eventReplayFrom(now); f.from.Before(lo) {
			f.from = lo
		}
	}
	// 相对范围的起点一直往前滑: 把已经滑出窗口的事件裁掉 (只裁不补)。
	if f.slide > 0 && len(f.rows) > 0 {
		keep := f.rows[:0]
		for _, e := range f.rows {
			if !e.Ts.Before(f.from) {
				keep = append(keep, e)
			}
		}
		f.rows = keep
	}
	files := logFilesFor(m.logPath, tuiScanWindow)
	sort.Strings(files)
	cur := filepath.Clean(m.logPath)
	for _, path := range files {
		if clean := filepath.Clean(path); clean != cur && f.done[clean] {
			continue
		}
		m.scanEventFile(path)
	}
}

// scanEventFile 增量读一个日志文件, 把范围内的结局事件并进 ev.rows。归档读完后记进
// done, 不再重扫; 当前文件留待下一拍接着读 (日志还在长)。
func (m *tuiModel) scanEventFile(path string) {
	f := &m.ev
	done := tailInto(path, f.pos, f.rest, func(line []byte) {
		var r logRec
		if json.Unmarshal(line, &r) != nil {
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, r.Ts)
		if err != nil || !f.inRange(ts) {
			return
		}
		e, ok := replayEventOf(r, ts)
		if !ok {
			return
		}
		k := evKey{ts: e.Ts, req: e.Req, kind: e.Kind, msg: e.Msg}
		if f.seen[k] {
			return
		}
		f.seen[k] = true
		f.rows = append(f.rows, e)
	})
	if done && filepath.Clean(path) != filepath.Clean(m.logPath) {
		f.done[filepath.Clean(path)] = true
	}
}

// startEvPrompt 打开第 2 页的行内输入: textFilter 为真 = f (编号/逻辑名/事件),
// 否则 = t (时间范围)。
func (m *tuiModel) startEvPrompt(textFilter bool) {
	m.evMsg = ""
	if textFilter {
		m.evPrompt = &tuiPrompt{label: "筛选 (编号/逻辑名/事件, 空 = 清除):", buffer: m.ev.text,
			commit: m.setEvText}
		return
	}
	m.evPrompt = &tuiPrompt{label: "时间范围 (10:00-11:00 / 10:00 / 30m / 2h, 空 = 清除):",
		commit: m.setEvRange}
}

// setEvText 是文本筛选的提交: 只改显示层, 不用回读日志。
func (m *tuiModel) setEvText(v string) {
	m.ev.setText(v)
	m.scroll[tabEvents] = 0
}

// setEvRange 是时间范围的提交: 写法不对只提示, 原范围保持不动; 换成有效范围就立刻回读。
func (m *tuiModel) setEvRange(v string) {
	from, to, slide, errMsg := parseEvRange(v, time.Now())
	if errMsg != "" {
		m.evMsg = errMsg
		return
	}
	m.evMsg = ""
	if from.IsZero() {
		m.ev.from, m.ev.to, m.ev.slide = time.Time{}, time.Time{}, 0
		m.ev.resetScan()
	} else {
		m.ev.setRange(from, to, slide)
		m.pollEventFilter() // 立刻回读, 不用等下一拍刷新
	}
	m.scroll[tabEvents] = 0
}

// evPromptKey 把一个字节喂给第 2 页的输入框。
func (m *tuiModel) evPromptKey(b byte) {
	if feedPrompt(m.evPrompt, b) {
		m.evPrompt = nil
	}
}

// clearEvFilter 清除第 2 页的全部筛选 (c 键)。
func (m *tuiModel) clearEvFilter() {
	m.ev.clear()
	m.evMsg = ""
	m.scroll[tabEvents] = 0
}

// eventsTitle 是事件流的标题: 没有筛选时保持原样, 有筛选时列出条件与命中条数。
func (m *tuiModel) eventsTitle(matched int) string {
	title := "事件流 (最新在前)"
	var parts []string
	if m.ev.text != "" {
		parts = append(parts, "筛选 \""+m.ev.text+"\"")
	}
	if !m.ev.from.IsZero() {
		parts = append(parts, evRangeLabel(m.ev.from, m.ev.to))
	}
	if len(parts) == 0 {
		return title
	}
	return title + ", " + strings.Join(parts, ", ") + ", 命中 " + strconv.Itoa(matched) + " 条"
}

// drawEventsEmpty 是筛选没有命中时的占位行。
func (m *tuiModel) drawEventsEmpty(c *tuiCanvas) {
	if m.ev.active() {
		c.line(tuiDim + "  (筛选没有命中)" + tuiReset)
		return
	}
	tableEmptyLine(c)
}
