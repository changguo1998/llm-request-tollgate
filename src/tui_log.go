// tui_log.go: 日志增量读取与逐笔吸收。

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// pollLog 读日志: 归档文件 (跨天) 整体扫一遍, 当前文件增量读。
// 与 -summary / -usage 取同样的文件范围 (logFilesFor 的窗口), 这样看到的不是"从本次启动起"。
// 文件按名字排序后再读: 归档 (<base>-<日期>.jsonl) 排在当前文件 (<base>.jsonl) 之前,
// 于是按时间先后入账; 事件环只留最近若干条, 读入顺序决定留下的那批是不是最新的。
func (m *tuiModel) pollLog() {
	if m.logPath == "" {
		return
	}
	files := logFilesFor(m.logPath, tuiScanWindow)
	sort.Strings(files)
	for _, f := range files {
		current := filepath.Clean(f) == filepath.Clean(m.logPath)
		if !current {
			if m.scannedFiles[filepath.Clean(f)] { // 归档不会再变, 扫过就跳过
				continue
			}
		}
		m.tailFile(f, current)
	}
	m.pollEventFilter()
}

// tailInto 从一个文件读到末尾: pos 是上次读到哪, rest 是各文件的半行缓冲, each 逐行接收。
// 返回是否读到了当时的文件末尾 (调用方据此判断归档是否整读完毕)。
func tailInto(path string, pos map[string]int64, rest map[string][]byte, each func([]byte)) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	p := pos[path]
	if st.Size() < p { // 轮转/截断: 从头读
		p, rest[path] = 0, nil
	}
	if st.Size() == p {
		return true
	}
	if _, err := f.Seek(p, io.SeekStart); err != nil {
		return false
	}
	buf := make([]byte, st.Size()-p)
	n, _ := io.ReadFull(f, buf)
	pos[path] = p + int64(n)
	consumeLines(rest, path, buf[:n], each)
	return true
}

// consumeLines 按行切开新增内容交给 each; 末尾的半行留在 rest[key] 等下一拍补齐。
func consumeLines(rest map[string][]byte, key string, b []byte, each func([]byte)) {
	data := b
	if prev := rest[key]; len(prev) > 0 {
		data = append(prev, b...)
		rest[key] = nil
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i]
		data = data[i+1:]
		if len(bytes.TrimSpace(line)) > 0 {
			each(line)
		}
	}
	if len(data) > 0 {
		if len(data) > 1<<20 { // 异常超长行: 丢掉, 别把内存拖爆
			return
		}
		rest[key] = append([]byte(nil), data...)
	}
}

// tailFile 读一个日志文件的新增部分 (归档整读完后记下来, 不再重复扫)。
func (m *tuiModel) tailFile(path string, incremental bool) {
	done := tailInto(path, m.logPos, m.logRest, m.absorb)
	if !incremental && done {
		m.scannedFiles[filepath.Clean(path)] = true
	}
}

// absorb 吸收一条日志: 更新聚合行 + 生成事件 (倒序展示用)。
func (m *tuiModel) absorb(line []byte) {
	var r logRec
	if err := json.Unmarshal(line, &r); err != nil {
		return
	}
	m.scanned++
	ts := logRecTS(r)
	// 第 2 页的事件回放覆盖当日 + 前一日 (按本地日历日, 跨天也能看到昨天的结局);
	// 第 1 页的聚合统计仍只算回看窗口 (m.window), 与是否跨天无关。
	if ts.Before(eventReplayFrom(time.Now())) {
		return // 前一日之前的记录: 两张表都不展示
	}
	if age := time.Since(ts); age > m.window {
		m.replayEvent(r, ts)
		return
	}
	m.updateRoute(r, ts)
	m.replayEvent(r, ts)
}

// logRecTS 解析日志时间; 解析失败 (或字段缺失) 用当前时刻。
func logRecTS(r logRec) time.Time {
	ts, _ := time.Parse(time.RFC3339Nano, r.Ts)
	if ts.IsZero() {
		return time.Now()
	}
	return ts
}

// updateRoute 把一条窗口内的日志累加进路由聚合 (计数 / token / 样本 / 首末时间)。
func (m *tuiModel) updateRoute(r logRec, ts time.Time) {
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
	m.addUsage(row, r, ts)
	if r.TtfbMs > 0 {
		row.TtfbPts = addTtfb(row.TtfbPts, ttfbPoint{ts: ts, ms: r.TtfbMs}, time.Now(), m.window)
	}
	if row.First.IsZero() || ts.Before(row.First) {
		row.First = ts
	}
	if ts.After(row.Last) {
		row.Last = ts
	}
}

// addUsage 累加 token 与三类速率样本: token 总量缺省时用 prompt+completion 兜底,
// 速率样本按回看窗口 (3h) 保留 (平均看 rateWin, 峰值看整窗口)。
func (m *tuiModel) addUsage(row *routeRow, r logRec, ts time.Time) {
	if r.Usage == nil {
		return
	}
	now := time.Now()
	total := r.Usage.Total
	if total == 0 {
		total = r.Usage.Prompt + r.Usage.Completion
	}
	row.Prompt += r.Usage.Prompt
	row.Cached += r.Usage.Cached
	row.Total += total
	if r.DurationMs > 0 {
		if r.Usage.Prompt+r.Usage.Completion > 0 {
			row.TotalRate = addTok(row.TotalRate,
				tokPoint{ts: ts, tok: r.Usage.Prompt + r.Usage.Completion, dur: r.DurationMs}, now, m.window)
		}
		if r.Bytes > 0 {
			row.ByteRate = addTok(row.ByteRate,
				tokPoint{ts: ts, tok: int(r.Bytes), dur: r.DurationMs}, now, m.window)
		}
	}
	if r.DecodeMs > 0 && r.Usage.Completion > 0 {
		row.DecodeRate = addTok(row.DecodeRate,
			tokPoint{ts: ts, tok: r.Usage.Completion, dur: r.DecodeMs}, now, m.window)
	}
}

// eventReplayFrom 是第 2 页事件回放的起点: 前一日 00:00 (本地日历日), 也就是当日 + 前一日。
func eventReplayFrom(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, now.Location())
}

// replayEventOf 从一条日志记录还原一条第 2 页事件: 只挑改变了程序状态的那些 (闸门挡下 /
// 错误 / 换候选 / 排队); 文案与引擎侧结局事件同一短词表。日志一请求一行, 还原不了整条链,
// 只写结局形态。没有可还原的结局时 ok = false。
func replayEventOf(r logRec, ts time.Time) (routeEvent, bool) {
	switch {
	case r.Gate != "":
		return routeEvent{Ts: ts, Req: r.Req, Kind: "gate", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: "sem" + stageSep + "reject(" + gateWord(r.Gate) + ")"}, true
	case r.Status >= 400 || r.Err != "":
		return routeEvent{Ts: ts, Req: r.Req, Kind: "error", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: "send" + stageSep + "fail(" + replayFailParam(r) + ")"}, true
	case r.Fallbacks > 0:
		return routeEvent{Ts: ts, Req: r.Req, Kind: "fallback", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: "switch(" + candName(r.Upstream, r.Model) + ")"}, true
	case r.Queued:
		return routeEvent{Ts: ts, Req: r.Req, Kind: "queue", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: "sem(" + r.Upstream + ")" + stageSep + "freq(" + r.Upstream + ")"}, true
	}
	return routeEvent{}, false
}

// replayEvent 把一条日志记录还原成事件后并进内存事件环 (256 条上限由 addEvent 负责)。
func (m *tuiModel) replayEvent(r logRec, ts time.Time) {
	if e, ok := replayEventOf(r, ts); ok {
		m.addEvent(e)
	}
}

// gateWord 把日志里的 gate 值换成 reject 原因词 (与引擎侧同一短词表)。
func gateWord(g string) string {
	switch g {
	case "locked":
		return "locked"
	case "sem_timeout":
		return "sem-timeout"
	case "queue_timeout":
		return "queue-timeout"
	case "backoff_timeout":
		return "backoff-timeout"
	case "canceled":
		return "cancelled"
	}
	return g
}

// replayFailParam 是回放错误事件的 fail 参数: 命中归类词用归类词 (传输记 connection),
// 没归类才写状态码数字。
func replayFailParam(r logRec) string {
	switch r.Class {
	case "quota":
		return "quota"
	case "concurrency":
		return "concurrency"
	case "transport":
		return "connection"
	}
	if r.Status > 0 {
		return strconv.Itoa(r.Status)
	}
	return "error"
}
