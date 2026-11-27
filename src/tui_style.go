// tui_style.go: 配色与列间距。

package main

import (
	"strconv"
	"strings"
	"time"
)

const (
	tuiReset = "\x1b[0m"
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
	// tuiEditBg 是第 2 页输入态 (f / t 筛选) 整行铺底的强调背景色 (蓝底), 区别于普通反显;
	// 设置页的 "=" 已改原位输入, 不再用这个底色 (只有输入缓冲本身反显)。
	tuiEditBg = "\x1b[44m"
)

// tuiGrayByTheme 是说明性文字与数值小数位的颜色查表: 下标 0 = 深色主题用亮黑 (90,
// 中性灰), 1 = 浅色主题用浅蓝 (94, 亮蓝)。两个槽都随终端主题变明度, 但亮黑在浅色背景上
// 偏淡、深白 (37) 更淡, 所以浅色主题改用蓝系 —— 蓝在浅背景上压得住, 又不至于像纯黑那样
// 和正文抢。按主题查表取定值, 不按前景/背景色值计算。
// 不用 SGR 2 暗显: 部分终端不实现 dim, 整数与小数就分不出颜色。
var tuiGrayByTheme = [2]string{"\x1b[90m", "\x1b[94m"}

// tuiLightTheme 是本次运行是否浅色主题: settings 页的主题行切换, 只在内存里 ——
// 不写配置、不进 store, 重启回到深色。
var tuiLightTheme bool

// tuiGray 是当前主题的说明文字色 (查表结果, 深色是亮黑灰 / 浅色是浅蓝): 全 TUI 的
// 说明性文字与小数位共用, 切主题只换这一处。名字沿用历史的 gray, 不按灰/蓝分开叫。
var tuiGray = tuiGrayByTheme[0]

// setTuiTheme 切主题: 记住状态, 并按表换上说明文字色。
func setTuiTheme(light bool) {
	tuiLightTheme = light
	idx := 0
	if light {
		idx = 1
	}
	tuiGray = tuiGrayByTheme[idx]
}

// themeCN 是当前主题的中文名 (settings 页主题行显示用)。
func themeCN() string {
	if tuiLightTheme {
		return "浅色"
	}
	return "深色"
}

// upPalette 是上游的专属色: 状态用红 / 黄 / 绿的正色 (31 / 32 / 33), 这里用青 / 品红 /
// 蓝系 (含亮色变体)、默认前景色与亮红 / 亮绿 / 亮黄, 按上游在第 1 页上游表的固定顺序
// 轮流分配, 各页同色。默认前景色在蓝 (34) 与亮青 (96) 之间。
// 这一格写 SGR 39 (恢复默认前景) 而不是空串: upColors 里空串表示「没分到色」, 上色处
// 会跳过它 (见 tui_events.go 的 upName)。
var upPalette = []string{
	"\x1b[36m", "\x1b[35m", "\x1b[34m",
	"\x1b[39m", "\x1b[96m", "\x1b[95m", "\x1b[94m",
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
//	黄   = 等待与降级 (事件正文的 switch / retry / 退避 / 低性能锁 / 排队 / 超发试探 / 顶部错误提示;
//	      以及触发低性能阈值时的警示上色: 首字节时延上在阶段链 send 格, 解码时长上在 recv 格,
//	      命中低吞吐绝对/相对门槛时上在实时性能数字 —— 只警示, 不触发锁)
//	灰   = 说明 (标题 / 单位 / 位置行 / 空; 深色主题是亮黑, 浅色主题是浅蓝, 见 tuiGrayByTheme)
//	青   = 接收中这一段的实时性能 (第 1 页阶段链尾; 无状态含义, 只表示正在收;
//	      命中低吞吐绝对/相对门槛时改黄, 见 flow.SlowPerf)
//	前景色 = 事件正文的其余文字
//	专属色 = 逻辑名与供应商名 (逻辑名: events 标识列 / 请求表 / 模型表分组标题;
//	          供应商名: 各页的 上游/模型 单元格与事件正文)
//	反显 = 当前选中行 / 设置页原位输入的输入缓冲 (当光标)。
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
