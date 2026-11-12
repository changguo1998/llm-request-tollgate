// perfref.go: 速度参考 (speed reference) 参数加载与 r* 指标计算.
//
// 定稿口径见 docs/speed-metric.md §6: r* = (c/d) * (c/b)^g, g 每模型一个.
// 参数来自 analysis/plot_speedref.py 生成的 JSON (analysis/figs/speedref_params.json),
// 键为 "upstream/model", 每个条目至少含 g (其余字段本文件不消费).
//
// 本文件只提供参数加载与纯计算, 不接入观察面 / TUI / 调度 / 告警.

package main

import (
	"encoding/json"
	"math"
	"os"
)

// perfParam 是一条 上游/模型 的速度参考参数 (JSON 条目, 只消费 g).
// 其余字段 (a/ref_hours/n/frac/flatness) 由 json.Unmarshal 自然忽略.
type perfParam struct {
	G float64 `json:"g"`
}

// PerfRef 是所有模型的速度参考参数表: "upstream/model" -> g.
type PerfRef struct {
	byModel map[string]float64
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
	p := &PerfRef{byModel: make(map[string]float64, len(raw))}
	for key, pr := range raw {
		p.byModel[key] = pr.G
	}
	return p, nil
}

// g 取指定 上游/模型 的指数; 参数表里没有该模型时退回 0 (即 r* 退化为原始速率 c/d).
func (p *PerfRef) g(up, model string) float64 {
	if p == nil {
		return 0
	}
	return p.byModel[up+"/"+model]
}

// index 算一笔请求的速度参考指标: r* = (c/d) * (c/b)^g.
// c/d 用解码吞吐 (c 输出 token, d 毫秒), (c/b) 用字节密度的倒数.
// 任一输入无效 (c<=0 / d<=0 / b<=0) 时返回 0, 调用方应保证先行过滤.
func (p *PerfRef) index(up, model string, c, d, b int64) float64 {
	if c <= 0 || d <= 0 || b <= 0 {
		return 0
	}
	rate := float64(c) / (float64(d) / 1000) // tokens/s
	g := p.g(up, model)
	if g == 0 {
		return rate
	}
	// (c/b)^g; c/b 恒为正, 无定义域问题
	return rate * math.Pow(float64(c)/float64(b), g)
}
