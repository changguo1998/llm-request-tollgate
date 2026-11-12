// perfref.go: 速度参考 (speed reference) 参数加载与 r* 指标计算.
//
// 定稿口径见 docs/speed-metric.md §6: r* = (c/d) * (c/b)^(-g).
// 参数来自 analysis/plot_speedref.py 生成的 JSON (analysis/figs/speedref_params.json),
// 键为 "upstream/model", 每个条目含 g/a; 顶层 _meta 元数据被忽略.
//
// 本文件只提供参数加载与纯计算, 不接入观察面 / TUI / 调度 / 告警.

package main

import (
	"encoding/json"
	"math"
	"os"
)

// perfParam 是一条 上游/模型 的速度参考参数: g 长度指数, a 参考基准 (log 空间).
type perfParam struct {
	G float64 `json:"g"`
	A float64 `json:"a"`
}

// perfMetaKey 是参数文件顶层的元数据键 (_meta: 版本/时间戳), 加载时跳过.
const perfMetaKey = "_meta"

// PerfRef 是所有模型的速度参考参数表: "upstream/model" -> (g, a).
type PerfRef struct {
	byModel map[string]perfParam
}

// loadPerfRef 从 JSON 文件加载速度参考参数; 文件缺失或格式错误时返回错误.
func loadPerfRef(path string) (*PerfRef, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]perfParam
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	p := &PerfRef{byModel: make(map[string]perfParam, len(raw))}
	for key, pr := range raw {
		if key == perfMetaKey {
			continue
		}
		p.byModel[key] = pr
	}
	return p, nil
}

// param 取指定 上游/模型 的参数; 参数表里没有该模型时返回 (零值, false).
func (p *PerfRef) param(up, model string) (perfParam, bool) {
	if p == nil {
		return perfParam{}, false
	}
	pr, ok := p.byModel[up+"/"+model]
	return pr, ok
}

// g 取指定 上游/模型 的指数; 参数表里没有该模型时退回 0.
func (p *PerfRef) g(up, model string) float64 {
	pr, _ := p.param(up, model)
	return pr.G
}

// index 算一笔请求的速度参考指标 r* (性能): (c/d) * (c/b)^(-g).
// c/d 用解码吞吐 (c 输出 token, d 毫秒), (c/b) 用字节密度的倒数. 为什么是 -g: 归一目标是
// 让 r* 与密度无关 — 拟合 log(c/d) = a + g*log(c/b) 给出 g 后, log r* = log(c/d) - g*log(c/b)
// 沿拟合线恒等于 a, 即任一密度的请求归一后都落回该模型典型速率 e^a (典型状态附近数值即该
// 模型典型速率, 约 e^a 量级); a 仅作参考不参与计算. 任一输入无效 (c<=0 / d<=0 / b<=0) 时
// 返回 0, 调用方应保证先行过滤; 参数表里没有该模型时退回原始速率 c/d.
func (p *PerfRef) index(up, model string, c, d, b int64) float64 {
	if c <= 0 || d <= 0 || b <= 0 {
		return 0
	}
	rate := float64(c) / float64(d) * 1000 // tokens/s (先毫秒运算, 最后转 /s)
	pr, ok := p.param(up, model)
	if !ok {
		return rate // 无参考参数: 退回原始速率
	}
	// perf (性能, r*): (c/d) * (c/b)^(-g); c/b 恒为正, 无定义域问题
	perf := rate * math.Pow(float64(c)/float64(b), -pr.G)
	return perf
}
