// tui_flow.go: active 页的列定义与阶段链。

package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 第 1 页一张表: 前三列是请求标识, 最后一列是阶段链 (与事件页同形, 各阶段按调度顺序
// 用 " → " 连起来, 看起来像分了子列, 但只是一条链)。换候选 / 同候选重试另起一行,
// 前三列留空, 链从并发池重新开始 (与上一行的链首左对齐)。
// 列宽下限: 编号 7 / 逻辑名 6 / 流式 1, 阶段链沿用事件列的 40; 有内容或有空间时再按内容涨。
var activeCols = []tuiCol{
	{"编号", 7, true, true, 2}, {"逻辑名", 6, true, false, 0},
	{"流式", 1, true, false, 1},
	{"阶段", 40, true, false, 0},
}

// flowRows 把进行中的请求摊成显示行: 每笔请求的每一次尝试一行, 最新的请求在最上面,
// 同一笔请求内按尝试先后从上到下排 (回退另起一行, 前三列留空)。
func (m *tuiModel) flowRows() [][]string {
	flows := append([]flow(nil), m.pulse.Active...)
	sort.Slice(flows, func(i, j int) bool { return flows[i].ID > flows[j].ID })
	var out [][]string
	for _, f := range flows {
		tries := f.Tries
		if len(tries) == 0 { // 对端没给每尝试数据时, 按当前阶段画一行
			tries = []flowTry{{Up: f.Up, Model: f.Model, Cur: f.Stage, Segs: make([]float64, stageCount)}}
		}
		for i, tr := range tries {
			cells := make([]string, 0, len(activeCols))
			if i == 0 {
				tag := "否"
				if f.Stream {
					tag = "是"
				}
				// 逻辑名带专属色 (加粗), 与事件页标识列同一写法。
				cells = append(cells, strconv.FormatUint(f.ID, 10), m.logicalName(f.Logical), tag)
			} else {
				cells = append(cells, "", "", "") // 回退行: 标识列留空
			}
			cells = append(cells, m.stageChain(f, tr))
			out = append(out, cells)
		}
	}
	return out
}

// stageChain 把一次尝试写成一条阶段链 (与事件页同形): 到过的阶段按流程顺序用 " → " 连起,
// 每段写「短词 耗时」; 没到过的阶段不出现在链里。当前阶段实时走, 卡住原因 / 接收中的实时
// 吞吐 / 该段的结局词写在链尾 (链尾就是这条尝试走到的地方)。
func (m *tuiModel) stageChain(f flow, tr flowTry) string {
	chain := strings.Join(m.stageTokens(f, tr), stageSep)
	tail := m.stageTail(f, tr)
	if len(tail) == 0 {
		return chain
	}
	if chain == "" {
		return strings.Join(tail, " ")
	}
	return chain + " " + strings.Join(tail, " ")
}

// stageTokens 把到过的阶段按流程顺序拼成链首, 每段写「短词 耗时」: 当前段实时走
// (不满 1s 也写), 已结束且不满 1s 的段不写耗时; 没到过的阶段不出现。
func (m *tuiModel) stageTokens(f flow, tr flowTry) []string {
	reach := func(st int) bool {
		return tr.Cur == st || (st < len(tr.Segs) && tr.Segs[st] > 0)
	}
	secOf := func(st int) float64 {
		sec := 0.0
		if st < len(tr.Segs) {
			sec = tr.Segs[st]
		}
		if tr.Cur == st && tr.CurAt > 0 {
			sec += time.Since(time.UnixMilli(tr.CurAt)).Seconds()
		}
		return sec
	}
	dur := func(st int) string {
		sec := secOf(st)
		if tr.Cur != st && sec < 1 {
			return ""
		}
		return " " + segStr(sec)
	}
	var toks []string
	// 解析段只在还没取信号量时单独出现: 取了信号量就由 sem 那一段代表。
	if reach(stageParse) && !reach(stageSem) {
		toks = append(toks, "new("+f.Logical+")"+dur(stageParse))
	}
	if reach(stageSem) {
		toks = append(toks, "sem("+m.upName(tr.Up)+")"+dur(stageSem))
	}
	if reach(stageQueue) { // 频率队列不带参数 (哪个上游由 sem 那一段说明)
		toks = append(toks, "freq"+dur(stageQueue))
	}
	if reach(stageModelWait) { // 状态锁只写模型, 不写供应商
		w := "queue"
		if tr.Model != "" {
			w += "(" + tr.Model + ")"
		}
		toks = append(toks, w+dur(stageModelWait))
	}
	if reach(stageSend) {
		toks = append(toks, "send"+dur(stageSend))
	}
	if reach(stageRecv) {
		toks = append(toks, "recv"+dur(stageRecv))
	}
	return toks
}

// stageTail 是链尾 (这条尝试走到的地方): 卡住原因 / 接收中的实时吞吐 (青色) / 结局词。
func (m *tuiModel) stageTail(f flow, tr flowTry) []string {
	var tail []string
	if tr.Cur >= 0 && isGateDetail(f.Gate) {
		tail = append(tail, f.Gate)
	}
	if tr.Cur == stageRecv {
		if s := liveTps(f); s != "" {
			tail = append(tail, tuiLive+s+tuiReset)
		}
	}
	if tr.Fail != "" {
		tail = append(tail, colorTryFail(tr.Fail))
	}
	return tail
}

// liveTps 是接收中这一笔的实时吞吐, 只写一个: 优先解码 (已收到的输出 token / 首字节
// 到达至今), 其次总 (同一份输出 token / 请求总耗时), 最后下载 (已转发的响应字节 /
// 首字节到达至今), 都不加前缀。还没收到东西时为空。
func liveTps(f flow) string {
	if f.Stage != stageRecv || f.RecvStart <= 0 || (f.RecvTok <= 0 && f.RecvBytes <= 0) {
		return ""
	}
	el := time.Since(time.UnixMilli(f.RecvStart)).Seconds()
	// 解码: 接收窗口够长才算 (窗口刚开始时速率会虚高, 也无法除)。
	if f.RecvTok > 0 && el > 0 {
		return strings.TrimLeft(humanTPS(float64(f.RecvTok)/el), " ") + " t/s"
	}
	// 总: 解码不可用时退回按请求总耗时算。
	if tot := time.Since(f.StartTime).Seconds(); f.RecvTok > 0 && tot > 0 {
		return strings.TrimLeft(humanTPS(float64(f.RecvTok)/tot), " ") + " t/s"
	}
	// 下载: 没有 token 可计时 (非流式 / 只有思考块以外的字节) 时写字节速率。
	if f.RecvBytes > 0 && el > 0 {
		return humanRate(float64(f.RecvBytes) / el)
	}
	return ""
}

// colorTryFail 给链尾的结局词上色: fail(...) 红字 (失败), reject(...) 黄字 (被挡)。
func colorTryFail(s string) string {
	if strings.HasPrefix(s, "fail") {
		return tuiRed + s + tuiReset
	}
	return tuiYell + s + tuiReset
}

// segStr 只决定耗时怎么显示 (内部仍按秒的小数算): 不足 1s 精确到 0.1s (整 0 写 0s),
// 1s 及以上精确到 1s (四舍五入), 超过一分钟走 durStr 的 mm:ss 写法。
func segStr(sec float64) string {
	if sec < 60 {
		if sec < 1 {
			v := math.Round(max(0, sec)*10) / 10
			if v == math.Trunc(v) {
				return strconv.Itoa(int(v)) + "s"
			}
			return strconv.FormatFloat(v, 'f', 1, 64) + "s"
		}
		return strconv.Itoa(int(math.Round(sec))) + "s"
	}
	return durStr(time.Duration(sec * float64(time.Second)))
}
