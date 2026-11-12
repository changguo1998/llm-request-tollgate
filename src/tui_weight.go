// tui_weight.go: 逻辑名配色权重 —— 按请求结束时刻做 10min 尺度的高斯衰减求和。

package main

import (
	"math"
	"sort"
	"time"
)

// nameWeightScale 是配色权重的时间尺度: 请求结束时刻距当前越远, 这一笔贡献越小。
// 固定 10min, 不做配置项。
const nameWeightScale = 10 * time.Minute

// addNameEnd 记一笔请求的结束时刻 (配色权重用); 窗口外的时刻由 weightOrder 清掉。
func (m *tuiModel) addNameEnd(name string, ts time.Time) {
	if name == "" {
		return
	}
	if m.nameEnds == nil {
		m.nameEnds = map[string][]time.Time{}
	}
	m.nameEnds[name] = append(m.nameEnds[name], ts)
}

// weightOrder 按权重降序给逻辑名排位, 同权重按字典序; names 原顺序不动。
// 权重 = 该逻辑名窗口内各请求的 exp(-((结束时刻-now)/10min)^2) 之和 (求和, 不取平均)。
// 同时丢掉窗口外的结束时刻, 内存不随运行时长增长。
func (m *tuiModel) weightOrder(names []string, now time.Time) []string {
	scale := nameWeightScale.Seconds()
	weights := make(map[string]float64, len(m.nameEnds))
	for name, ends := range m.nameEnds {
		keep := ends[:0]
		sum := 0.0
		for _, t := range ends {
			age := now.Sub(t)
			if age > m.window {
				continue
			}
			keep = append(keep, t)
			x := age.Seconds() / scale
			sum += math.Exp(-x * x)
		}
		m.nameEnds[name] = keep
		weights[name] = sum
	}
	out := append([]string(nil), names...)
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := weights[out[i]], weights[out[j]]
		if wi != wj {
			return wi > wj
		}
		return out[i] < out[j]
	})
	return out
}
