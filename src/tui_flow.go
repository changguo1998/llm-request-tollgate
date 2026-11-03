// tui_flow.go: active 页的列定义与耗时单元格。

package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// 列宽在放得下时按内容涨, 顶到终端右边界为止。上游与真实模型合成一格, 写法 "上游/模型"
// (与 -usage 的 key 一致, 如 ustc/gpt-5-codex)。列序按流程从左到右: 编号 → 逻辑名 →
// 流式 → 上游 → 耗时 → 状态; 路径与来源不进表 (路径太长, 又对辨认一笔请求没帮助)。
var activeCols = []tuiCol{
	{"编号", 4, true, true, 1}, {"逻辑名", 10, true, false, 0},
	{"流式", 4, false, false, 2}, {"上游/模型", 18, true, false, 0},
	{"耗时", 14, true, true, 0}, {"状态", 10, true, false, 0},
}

// flowSegs 是耗时列括号里的分段: 与第 1 页分块同序, 只去掉「已失败」(它是结局不是
// 阶段)。并发池把解析与取信号量合为一段, 与分块口径一致。
var flowSegs = [][]int{
	{stageParse, stageSem},
	{stageQueue},
	{stageModelWait},
	{stageSend},
	{stageRecv},
}

// segMergeMax 是合并前段 (并发池 / 频率队列 / 状态锁) 的门槛: 三段合计不超过它时,
// 发出去之前的等待不值得逐段看, 并成一段「发送前」, 耗时列只写 发送前 / 等待中 /
// 接收中 三段; 超过它才按 5 段摊开, 让人一眼看出时间耗在哪一步。
const segMergeMax = 4 * time.Second

// segOf 是阶段所属的分段下标 (-1 = 不在任何分段里, 如已失败)。
func segOf(stage int) int {
	for i, seg := range flowSegs {
		if stageIn(stage, seg) {
			return i
		}
	}
	return -1
}

// flowSegDurs 是耗时列括号里各段的秒数: 从第一段写到手边这一段, 没到过的阶段不补 0。
// 已结算的段读 Segs (服务端在离开阶段时入账), 当前段用 CurAt 现算 —— 对端每秒才刷新
// 一次, 直接读 Segs 会让正在走的这一段晚一拍。
// 前三段 (并发池 / 频率队列 / 状态锁) 合计不超过 segMergeMax 时并成一段「发送前」,
// 剩下的依次是「等待中」(等响应头) 与「接收中」。
func flowSegDurs(f flow) []float64 {
	last := segOf(f.MaxStage)
	if last < 0 {
		return nil
	}
	out := make([]float64, last+1)
	for i := 0; i <= last && i < len(flowSegs); i++ {
		for _, st := range flowSegs[i] {
			if st < len(f.Segs) {
				out[i] += f.Segs[st]
			}
		}
	}
	if cur := segOf(f.Stage); cur >= 0 && cur <= last && f.CurAt > 0 {
		out[cur] += time.Since(time.UnixMilli(f.CurAt)).Seconds()
	}
	return mergePreSend(out)
}

// mergePreSend 把前三段 (并发池 / 频率队列 / 状态锁) 并进一段「发送前」: 三段合计
// (按屏幕上的 0.1s 精度) 不超过 segMergeMax 才并, 超过就原样返回。
func mergePreSend(segs []float64) []float64 {
	pre := 0.0
	for i := 0; i < len(segs) && i < 3; i++ {
		pre += segs[i]
	}
	if math.Round(pre*10)/10 > segMergeMax.Seconds() {
		return segs
	}
	out := append([]float64{pre}, segs[min(3, len(segs)):]...)
	return out
}

// durCell 是耗时格: 总耗时, 括号里是各段耗时 (如 3s(0.1s+0.1s+0s+2.8s))。总耗时与分段
// 同样按 0.1s 取整, 不然括号里的和会比总数差一截 (如 3.7s 印成 3s)。
func (m *tuiModel) durCell(f flow) string {
	el := time.Since(f.StartTime)
	segs := flowSegDurs(f)
	if len(segs) == 0 {
		return durStr(el)
	}
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = segStr(s)
	}
	return segStr(el.Seconds()) + "(" + strings.Join(parts, "+") + ")"
}

// segStr 写一段耗时: 按 0.1s 四舍五入 (整秒不写小数点, 如 2.8s / 3s / 0s), 超过一分钟
// 走 durStr 的 mm:ss 写法。
func segStr(sec float64) string {
	if sec < 60 {
		v := math.Round(max(0, sec)*10) / 10
		if v == math.Trunc(v) {
			return strconv.Itoa(int(v)) + "s"
		}
		return strconv.FormatFloat(v, 'f', 1, 64) + "s"
	}
	return durStr(time.Duration(sec * float64(time.Second)))
}
