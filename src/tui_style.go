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
	// tuiRedBright 是额度锁的浅红 (亮红): 与熔断锁的正红同属错误类, 用明度区分「钱」
	// (额度) 与「坏」(熔断), 与 91 系亮色变体保持同一套风格。
	tuiRedBright = "\x1b[91m"
	// tuiTitle 是 settings 页各区标题的颜色 (亮青, 区别于红/黄/绿的语义色)。
	tuiTitle = "\x1b[36m"
	// tuiLive 是接收中实时性能的颜色 (青): 只标出「这一段正在收」, 不参与状态含义,
	// 所以不用承载健康 / 错误 / 等待的红黄绿。
	tuiLive = "\x1b[36m"
	// tuiEditBg 是 settings 页进入输入编辑 (=) 时的强调背景色 (蓝底), 区别于普通反显。
	tuiEditBg = "\x1b[44m"
)

// upPalette 是上游的专属色: 状态用红 / 黄 / 绿的正色 (31 / 32 / 33), 这里用青 / 品红 /
// 蓝系 (含亮色变体) 与亮红 / 亮绿 / 亮黄, 按上游在第 1 页上游表的固定顺序轮流分配, 各页同色。
var upPalette = []string{
	"\x1b[36m", "\x1b[35m", "\x1b[34m",
	"\x1b[96m", "\x1b[95m", "\x1b[94m",
	"\x1b[91m", "\x1b[92m", "\x1b[93m",
}

// logicalPalette 是逻辑名的专属色: 先用标准色 (蓝 / 品红 / 青), 标准色用完了再上浅色
// (亮蓝 / 亮品红 / 亮青, 最后才亮红 / 亮绿 / 亮黄), 按配置里的逻辑名字典序轮流分配。
var logicalPalette = []string{
	"\x1b[34m", "\x1b[35m", "\x1b[36m",
	"\x1b[94m", "\x1b[95m", "\x1b[96m",
	"\x1b[91m", "\x1b[92m", "\x1b[93m",
}

// 颜色标准 (全 TUI 一套, 新增画面前先对照这张表):
//
//	绿   = 健康 (live / 事件正文的 ok(200) / 错误数 0)
//	红   = 错误与锁 (事件正文的 fail(...) / 错误 / 熔断锁 / 额度锁 / 并发锁 / 连续错误 > 0)
//	黄   = 等待与降级 (事件正文的 switch / retry / 退避 / 低性能锁 / 排队 / 超发试探 / 顶部错误提示)
//	灰   = 说明 (标题 / 单位 / 位置行 / 空)
//	青   = 接收中这一段的实时性能 (第 1 页阶段链尾; 无状态含义, 只表示正在收)
//	前景色 = 事件正文的其余文字
//	加粗+专属色 = 逻辑名 (events 标识列; 标准色先用、用完再上浅色)
//	专属色 = 供应商名 (各页的 上游/模型 单元格与事件正文)
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
