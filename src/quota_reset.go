// quota_reset.go: 从错误响应的响应体 / 响应头里解析「恢复 / 重置时刻」, 用它设定定时锁的
// 倒计时而不是固定时长: 额度错误 -> 额度锁 (quota_lock_ms 是回退值), 通用兜底的错误 ->
// 熔断锁 (breaker_lock_ms 是回退值)。解析来源按优先级:
//   1. 响应体里的绝对时间 (Limit resets at ... / It will reset at ...);
//   2. 响应头里的绝对时间 (Reset_at / x-ratelimit-reset-*), 已知键按固定顺序判;
//   3. Retry-After (相对秒数或 HTTP-date) 与 reset 头的时长形态 (如 "6m0s")。
// 全部解析不到时才回退配置的固定时长。上下限保护 (minLockDur / maxLockDur) 防止脏数据
// 把锁设得太短或太长 (已过的、太近的、远超一周的都不认)。

package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// minLockDur / maxLockDur 是解析结果可接受的范围: 至少锁 minLockDur, 封顶 maxLockDur。
// 周额度重置约 3 天后, 7 天足够; 更长的重置时间视为异常, 回退固定时长。
const (
	minLockDur = time.Second
	maxLockDur = 7 * 24 * time.Hour
)

// bodyScanCap 是被动扫描响应体的长度上限: 重置时间总在错误文案开头, 只扫前 8KB
// (与 errMatchText 的匹配上限同口径)。
const bodyScanCap = 8 * 1024

// resetTimeRe 匹配带明确时区的日期时间 (数字偏移和/或缩写); 不带时区的不匹配, 免得把
// 本地时区的文本错当 UTC。匹配形态: "2026-11-07 13:36:10 UTC" /
// "2026-11-09 00:00:00 +0800 CST" / RFC3339。
var resetTimeRe = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[ ]+[+-]\d{2}:?\d{2})?(?:[ ]+[A-Za-z]{2,5})?`)

// resetKwRe 找「重置 / 到期」关键字后紧跟的日期时间, 命中才优先采用 —— 免得正文里
// 先出现的无关时间 (如请求发起时刻) 被错当重置时刻。第 1 组捕获时间戳本体。
// 关键词覆盖各家实测文案 (resets at / will reset at / expires at / until)。
var resetKwRe = regexp.MustCompile(
	`(?i)(?:resets?|expires?|will reset|until|renews?)[^0-9]{0,80}?(` + resetTimeRe.String() + `)`)

// resetTimeLayouts 是各时区形态可用的解析布局, 从最具体到最通用。数字偏移优先于缩写:
// 缩写 "CST" 在 Go 里默认是美国中部时区, 显式偏移 (如 +0800) 才可靠。
var resetTimeLayouts = []string{
	"2006-01-02 15:04:05 -0700 MST",
	"2006-01-02 15:04:05 -07:00 MST",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 -07:00",
	"2006-01-02 15:04:05 MST",
	time.RFC3339Nano,
	time.RFC3339,
}

// resetHeaderKeys 是已知的重置时间响应头, 按固定优先级判 (绝对时间形态先于时长形态),
// 避免 map 遍历序造成的取源不确定; 值形态交给 resetDur 统一判。
var resetHeaderKeys = []string{"Reset_at", "X-RateLimit-Reset", "RateLimit-Reset", "X-Ratelimit-Reset"}

// recoveryDur 从错误响应的响应体与响应头解析恢复倒计时 (到某个恢复 / 重置时刻的距离);
// 解析不到返回 0。额度与通用兜底两条设锁路径共用, 用来源按顺序取第一个落在边界内的结果。
func recoveryDur(body []byte, hdr http.Header, now time.Time) time.Duration {
	if len(body) > bodyScanCap {
		body = body[:bodyScanCap]
	}
	// 1. 响应体里的绝对时间: 优先关键字后紧跟的, 退而求其次取第一个带时区的时间戳。
	if sm := resetKwRe.FindSubmatch(body); len(sm) == 2 {
		if d, ok := resetDur(string(sm[1]), now); ok {
			return d
		}
	}
	if m := resetTimeRe.Find(body); m != nil {
		if d, ok := resetDur(string(m), now); ok {
			return d
		}
	}
	// 2. 响应头里的重置时间: 已知键固定顺序优先, 其余带 reset 的键兜底扫一遍。
	for _, k := range resetHeaderKeys {
		if v := hdr.Get(k); v != "" {
			if d, ok := resetDur(v, now); ok {
				return d
			}
		}
	}
	for k, vv := range hdr {
		if !strings.Contains(strings.ToLower(k), "reset") {
			continue
		}
		for _, v := range vv {
			if d, ok := resetDur(v, now); ok {
				return d
			}
		}
	}
	// 3. Retry-After: 整数秒, 或 HTTP-date (RFC1123)。
	if v := hdr.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			if d := clampResetDur(time.Duration(n) * time.Second); d > 0 {
				return d
			}
		} else if t, err := time.Parse(http.TimeFormat, strings.TrimSpace(v)); err == nil {
			if d := clampResetDur(t.Sub(now)); d > 0 {
				return d
			}
		}
	}
	return 0
}

// resetDur 解析一段文本为「到某个时刻为止的倒计时」: 先按带显式时区的日期时间解析
// (无时区布局不在列表里), 再退到时长形态 (如 "6m0s" / "60s", 视作相对到期时间)。
// 结果须落在 [minLockDur, maxLockDur] 内, 否则判无效。
func resetDur(text string, now time.Time) (time.Duration, bool) {
	text = strings.TrimSpace(text)
	for _, l := range resetTimeLayouts {
		t, err := time.Parse(l, text)
		if err != nil {
			continue
		}
		d := clampResetDur(t.Sub(now))
		return d, d > 0
	}
	if d, err := time.ParseDuration(text); err == nil {
		if c := clampResetDur(d); c > 0 {
			return c, true
		}
	}
	return 0, false
}

// clampResetDur 把倒计时按上下限收口; 收完还在范围内就返回它, 否则无效 (0)。
func clampResetDur(d time.Duration) time.Duration {
	if d < minLockDur || d > maxLockDur {
		return 0
	}
	return d
}
