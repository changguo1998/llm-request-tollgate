// log.go: JSONL 逐笔日志、按天轮转与归档读取。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type logRec struct {
	Ts           string  `json:"ts"`
	Logical      string  `json:"logical"`
	Upstream     string  `json:"upstream"`
	Model        string  `json:"model"`
	Path         string  `json:"path"`
	Stream       bool    `json:"stream"`
	Status       int     `json:"status"`
	Usage        *Usage  `json:"usage,omitempty"`
	Missing      bool    `json:"missing_usage"` // 上游没回 usage, 走了字符粗估
	Queued       bool    `json:"queued"`
	QueueMs      int64   `json:"queue_ms"`
	SemMs        int64   `json:"sem_ms,omitempty"` // 等并发信号量的时长
	Client       string  `json:"client"`
	UA           string  `json:"ua,omitempty"`
	Source       string  `json:"source,omitempty"`
	Fallbacks    int     `json:"fallbacks,omitempty"`
	Err          string  `json:"err,omitempty"`
	Gate         string  `json:"gate,omitempty"` // 被闸门挡下没发出去时: locked | sem_timeout | queue_timeout | backoff_timeout | canceled
	LockSkips    int     `json:"lock_skips,omitempty"`
	BackoffSkips int     `json:"backoff_skips,omitempty"`
	TtfbMs       int64   `json:"ttfb_ms,omitempty"`     // 首字节时延 (被动采样)
	Tps          float64 `json:"tps,omitempty"`         // 解码吞吐 tokens/s (流式才有)
	Bytes        int64   `json:"bytes,omitempty"`       // 转发给客户端的响应体字节数
	Bps          float64 `json:"bps,omitempty"`         // 接收的字节吞吐 bytes/s (流式才有)
	DurationMs   int64   `json:"duration_ms,omitempty"` // 请求耗时 (收到请求到写完响应), 供非空闲吞吐
}

// logDayOf 返回时间所在本地日历天 (轮转边界按本地日期算)。
// 不动失败计数与锁, 也不打断已建立的流 (切换只可能发生在下次请求开始时)。
func logDayOf(t time.Time) string { return t.Format("2006-01-02") }

// archivePath 给出归档文件名: var/requests.jsonl -> var/requests-2026-10-12.jsonl。
// 目标已存在时追加 -1/-2 序号, 不覆盖既有归档。
func archivePath(path, day string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	if _, err := os.Stat(base + "-" + day + ext); err != nil {
		return base + "-" + day + ext
	}
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s-%s-%d%s", base, day, i, ext)
		if _, err := os.Stat(p); err != nil {
			return p
		}
	}
}

// rotateLog 把当前日志归档为带日期的文件, 再新开一份当前日志。
// 调用方须持有 logMu。prevDay 是这段日志所属的日期。
func (a *App) rotateLog(prevDay string) {
	path := a.cfg.LogPath
	if a.logW != nil {
		a.logW.Close()
	}
	a.logW, a.logger = nil, nil
	if st, err := os.Stat(path); err == nil {
		if st.Size() > 0 {
			if err := os.Rename(path, archivePath(path, prevDay)); err != nil {
				log.Printf("log rotate failed: %v", err)
			}
		} else {
			os.Remove(path) // 空文件不留归档
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("log reopen failed: %v", err)
		return
	}
	a.logW = f
	a.logger = log.New(f, "", 0)
}

// logFilesForScan 返回参与聚合的日志文件: 当前日志 + 最近归档 (覆盖 maxWindow 跨度)。
// 归档的 mtime 就是它最后被写入的时间, 据此判断是否还在窗口内 (多留一天余量)。
func logFilesForScan(path string, maxWindow time.Duration) []string {
	var files []string
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		files = append(files, path)
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	ms, _ := filepath.Glob(base + "-*" + ext)
	cutoff := time.Now().Add(-maxWindow - 24*time.Hour)
	for _, m := range ms {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(cutoff) {
			files = append(files, m)
		}
	}
	return files
}

func (a *App) openLog() error {
	path := a.cfg.LogPath
	if path == "" {
		return nil
	}
	if i := strings.LastIndex(path, "/"); i >= 0 && i > 0 {
		if err := os.MkdirAll(path[:i], 0o755); err != nil {
			return err
		}
	}
	// 隔天重启: 上一段日志先归档, 当前日志从空开始。
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		if old := logDayOf(st.ModTime()); old != logDayOf(time.Now()) {
			if err := os.Rename(path, archivePath(path, old)); err != nil {
				log.Printf("log rotate on start failed: %v", err)
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	a.logW = f
	a.logger = log.New(f, "", 0)
	a.logDay = logDayOf(time.Now())
	return nil
}

func (a *App) logReq(rec logRec) {
	if a.logger == nil {
		return
	}
	b, _ := json.Marshal(rec)
	a.logMu.Lock()
	defer a.logMu.Unlock()
	// 跨过本地日期边界就轮转 (长期常驻时按天归档)。
	if day := logDayOf(time.Now()); day != a.logDay {
		a.rotateLog(a.logDay)
		a.logDay = day
		if a.logger == nil {
			return
		}
	}
	a.logger.Println(string(b))
}
