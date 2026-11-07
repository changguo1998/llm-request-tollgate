// tui_rows.go: active 页分块行数 (高斯加权采样)。

package main

import (
	"math"
	"time"
)

// flowBlocks 是第 1 页的分块: 顺序就是请求在调度器里的流程顺序。
// 解析与并发池合成第一块 (解析是瞬时状态, 单独一块只会闪一下)。
// 各块的数据行区高度由 flowMinRows 定: 先按窗口内各块在飞笔数的累计占比分配「总并发峰值」,
// 得到每块的参考行数, 再用此刻的笔数校准; 块的位置只随统计变, 不随单笔请求进出走。
var flowBlocks = []struct {
	title  string
	stages []int
}{
	{"并发池", []int{stageParse, stageSem}},
	{"频率队列", []int{stageQueue}},
	{"状态锁", []int{stageModelWait}},
	{"已发送", []int{stageSend}},
	{"接收中", []int{stageRecv}},
	{"已失败", []int{stageFailed}},
}

// defaultMinRows 是各块数据行区的缺省档: 统计窗口里还没有样本时用这一档 (每块 1 行)。
var defaultMinRows = []int{1, 1, 1, 1, 1, 1}

// 行数统计的权重与样本上限 (取观测面每秒一拍; 样本按高斯权重衰减, 2h 的样本留在内存里)。
// 高斯的时间尺度 t0 是实测的平均停留时长: 停留这么久的样本权重 0.9, 2 倍处 0.66, 3 倍处
// 0.39 (公式 0.9^((age/t0)²)); 权重低于 gaussKeep 的样本 (约 8.1 倍 t0 之前) 不再参与统计。
// 还没测到停留时长时用 rowStatTau0 兜底。
const (
	rowStatTau0 = 35 * time.Second
	gaussAtTau  = 0.9
	gaussKeep   = 1e-3
	rowHistMax  = 7200
)

// rowSample 是一次采样: 各块的在飞笔数, 以及这些请求的耗时合计与笔数 (求平均停留时长)。
type rowSample struct {
	at  time.Time
	cnt []int
}

// flowMinRows 给每个分块定数据行区高度: 参考行数来自一段时间的统计, 最后用此刻的笔数校准。
// 参考行数 (blockRefRows): 每个样本先算各块笔数占该样本在飞笔数的比例, 再按高斯权重对各
// 样本的比例取加权平均 (近的重、远的轻, 于是某一段时间挂着很多笔不会独占统计), 乘以「同时在飞
// 笔数峰值」(总并发数), 四舍五入成参考行数, 至少 1 行 (不设上限)。
// 校准: 行数 = max(此刻笔数, 参考行数, 1)。笔数超过参考值时按实际留, 有空行的块也不把
// 请求藏起来; 空块最少留 1 行撑住版式。
func (m *tuiModel) flowMinRows() []int {
	cur := m.blockCounts()
	ref, ok := m.blockRefRows(time.Now())
	if !ok {
		ref = defaultMinRows
	}
	out := make([]int, len(flowBlocks))
	for i := range out {
		out[i] = max(cur[i], ref[i], 1)
	}
	return out
}

// blockCounts 是各块此刻的请求数 (校准用)。
func (m *tuiModel) blockCounts() []int {
	out := make([]int, len(flowBlocks))
	for _, f := range m.pulse.Active {
		if b := blockOfStage(f.Stage); b >= 0 {
			out[b]++
		}
	}
	return out
}

// blockOfStage 返回该阶段属于哪个分块 (不属于任何块时 -1)。
func blockOfStage(stage int) int {
	for i, b := range flowBlocks {
		if stageIn(stage, b.stages) {
			return i
		}
	}
	return -1
}

// noteRows 采一次样 (每次取到观测面后调用), 顺手丢掉太老的样本。
func (m *tuiModel) noteRows(now time.Time) {
	s := rowSample{at: now, cnt: make([]int, len(flowBlocks))}
	for _, f := range m.pulse.Active {
		if b := blockOfStage(f.Stage); b >= 0 {
			s.cnt[b]++
		}
	}
	m.rowHist = append(m.rowHist, s)
	if len(m.rowHist) > rowHistMax {
		m.rowHist = append([]rowSample(nil), m.rowHist[len(m.rowHist)-rowHistMax:]...)
	}
}

// blockRefRows 用最近一段的样本算参考行数: 每个样本先算各块笔数占该样本在飞笔数的比例,
// 再按高斯权重 (离参看时刻越远越轻) 对各样本的比例取加权平均, 最后乘「同时在飞笔数峰值」
// (总并发数), 四舍五入成参考行数。
// 权重的时间尺度 t0 = 实测平均停留时长 (从开始请求信号量到请求结束): 停留 t0 那么久的样本
// 权重 0.9, 见 gaussWeight。
// 参看时刻: 此刻有进行中的请求就用当前时间, 没有就用最后一次请求结束的时刻 (最后一个非空
// 样本), 于是空闲一阵之后仍然沿用上一段有请求的数据, 版式不塌回缺省档。权重低于 gaussKeep
// 的样本直接跳过, 没有硬截断, 只是越远的样本越轻。第二个返回值 = 有没有可用的样本。
func (m *tuiModel) blockRefRows(now time.Time) ([]int, bool) {
	k := len(flowBlocks)
	refAt, end := now, len(m.rowHist) // end = 可以用来算权重的样本个数 (到参看时刻为止)
	if blockTotal(m.blockCounts()) == 0 {
		end, refAt = -1, time.Time{}
		for i := len(m.rowHist) - 1; i >= 0; i-- {
			if blockTotal(m.rowHist[i].cnt) > 0 && !m.rowHist[i].at.After(now) {
				end, refAt = i+1, m.rowHist[i].at
				break
			}
		}
		if end < 0 {
			return nil, false
		}
	}
	tau := m.rowTau()
	share := make([]float64, k)
	weight, peak := 0.0, 0.0
	for _, s := range m.rowHist[:end] {
		n := blockTotal(s.cnt)
		if n <= 0 {
			continue // 空样本没有占比可言, 不参与加权
		}
		w := gaussWeight(s.at, refAt, tau)
		if w < gaussKeep {
			continue
		}
		weight += w
		peak = math.Max(peak, float64(n))
		for i := 0; i < k && i < len(s.cnt); i++ {
			share[i] += w * float64(s.cnt[i]) / float64(n)
		}
	}
	if weight <= 0 || peak <= 0 {
		return nil, false
	}
	out := make([]int, k)
	for i := 0; i < k; i++ {
		out[i] = clampRows(int(math.Round(share[i] / weight * peak)))
	}
	return out, true
}

// rowTau 是全部能读到的请求的平均耗时 (从开始请求信号量到请求结束), 作高斯权重的时间尺度;
// 日志里还没有请求时用 block_window_ms 兜底 (缺省 35s)。
func (m *tuiModel) rowTau() time.Duration {
	if m.durN <= 0 || m.durSum <= 0 {
		return m.blockWin
	}
	return time.Duration(m.durSum/m.durN) * time.Millisecond
}

// gaussWeight 是样本的高斯权重: 停留 t0 那么久时为 0.9, 越远越轻, 写作 0.9^((age/t0)²)
// (指数的底换成 e: exp((age/t0)² · ln 0.9))。
func gaussWeight(at, ref time.Time, tau time.Duration) float64 {
	t := tau.Seconds()
	if t <= 0 {
		return 1
	}
	d := at.Sub(ref).Seconds() / t
	return math.Exp(d * d * math.Log(gaussAtTau))
}

// blockTotal 是一个样本里各块笔数之和 (同时在飞的笔数)。
func blockTotal(cnt []int) int {
	n := 0
	for _, v := range cnt {
		n += v
	}
	return n
}

// clampRows 保证行数至少 1 行撑版式, 不设上限。
func clampRows(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func stageIn(st int, list []int) bool {
	for _, v := range list {
		if v == st {
			return true
		}
	}
	return false
}
