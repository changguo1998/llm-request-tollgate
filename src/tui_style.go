// tui_style.go: 配色与列间距。

package main

import (
	"strconv"
	"strings"
	"time"
)

const (
	tuiReset = "\x1b[0m"
	tuiBold  = "\x1b[1m"
	tuiDim   = "\x1b[2m"
	tuiRev   = "\x1b[7m"
	tuiRed   = "\x1b[31m"
	tuiGreen = "\x1b[32m"
	tuiYell  = "\x1b[33m"
	// tuiTitle 是 settings 页各区标题的颜色 (亮青, 区别于红/黄/绿的语义色)。
	tuiTitle = "\x1b[36m"
)

// upPalette 是上游的专属色: 状态用红 / 黄 / 绿的正色 (31 / 32 / 33), 这里用青 / 品红 /
// 蓝系 (含亮色变体) 与亮红 / 亮绿 / 亮黄, 按上游在第 1 页上游表的固定顺序轮流分配, 各页同色。
var upPalette = []string{
	"\x1b[36m", "\x1b[35m", "\x1b[34m",
	"\x1b[96m", "\x1b[95m", "\x1b[94m",
	"\x1b[91m", "\x1b[92m", "\x1b[93m",
}

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

// tuiColGap 是表格列与列之间留几格 (所有表统一); tuiColSep 是对应的分隔串。
const tuiColGap = 1

var tuiColSep = strings.Repeat(" ", tuiColGap)
