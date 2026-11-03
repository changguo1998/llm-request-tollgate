// tui_log.go: 日志增量读取与逐笔吸收。

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

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
	// 平均停留时长按全部能读到的请求算 (不按回看窗口), 只用于第 1 页分块行数的权重。
	if r.DurationMs > 0 {
		m.durSum += r.DurationMs
		m.durN++
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
		now := time.Now()
		if r.DurationMs > 0 {
			if r.Usage.Prompt+r.Usage.Completion > 0 {
				row.TotalRate = addTok(row.TotalRate,
					tokPoint{ts: ts, tok: r.Usage.Prompt + r.Usage.Completion, dur: r.DurationMs}, now, m.rateWin)
			}
			if r.Bytes > 0 {
				row.ByteRate = addTok(row.ByteRate,
					tokPoint{ts: ts, tok: int(r.Bytes), dur: r.DurationMs}, now, m.rateWin)
			}
		}
		if r.DecodeMs > 0 && r.Usage.Completion > 0 {
			row.DecodeRate = addTok(row.DecodeRate,
				tokPoint{ts: ts, tok: r.Usage.Completion, dur: r.DecodeMs}, now, m.rateWin)
		}
	}
	if r.TtfbMs > 0 {
		row.TtfbSum += r.TtfbMs
		row.TtfbN++
	}
	if row.First.IsZero() || ts.Before(row.First) {
		row.First = ts
	}
	if ts.After(row.Last) {
		row.Last = ts
	}

	// 事件: 只挑改变了程序状态的那些 (闸门挡下 / 错误 / 换候选 / 排队); 仅仅首字节慢
	// 而没有触发任何锁的请求不留事件, 低性能锁由锁事件自己记。
	cand := ""
	if c := candName(r.Upstream, r.Model); c != "" {
		cand = c + " " // 第 2 页没有单独的 上游/模型 列, 候选写进文案
	}
	switch {
	case r.Gate != "":
		m.addEvent(routeEvent{Ts: ts, Req: r.Req, Kind: "gate", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: cand + "被挡下: " + gateReason(r.Gate) + " (排队 " + itoa64(r.QueueMs) + "ms, 等并发 " + itoa64(r.SemMs) + "ms)"})
	case r.Status >= 400 || r.Err != "":
		msg := cand + "状态码 " + strconv.Itoa(r.Status)
		if r.Err != "" {
			msg += " " + tuiTrunc(r.Err, 60)
		}
		if r.LockSkips > 0 {
			msg += " (跳过锁 " + strconv.Itoa(r.LockSkips) + " 次)"
		}
		if r.BackoffSkips > 0 {
			msg += " (跳过退避 " + strconv.Itoa(r.BackoffSkips) + " 次)"
		}
		m.addEvent(routeEvent{Ts: ts, Req: r.Req, Kind: "error", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: msg})
	case r.Fallbacks > 0:
		m.addEvent(routeEvent{Ts: ts, Req: r.Req, Kind: "fallback", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status,
			Msg: cand + "换了 " + strconv.Itoa(r.Fallbacks) + " 次候选" +
				lockNote(r.LockSkips) + backoffNote(r.BackoffSkips)})
	case r.Queued:
		m.addEvent(routeEvent{Ts: ts, Req: r.Req, Kind: "queue", Logical: r.Logical, Upstream: r.Upstream,
			Model: r.Model, Status: r.Status, Msg: cand + "排队 " + itoa64(r.QueueMs) + "ms"})
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
