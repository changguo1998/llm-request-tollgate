// perfref_recompute.go: 速度参考参数的重算 (移植自 analysis/plot_speedref.py).
//
// 口径与 docs/speed-metric.md §6 (参考窗选择规则) 一致:
//   - 样本前置 (成功流式, t/d/c/b>0, c>=15 且 d>=100ms)
//   - 理想小时 (med r >= p65 且 med t <= p35; 活跃小时不足 4 个退化全天)
//   - 二维密度典型区 (100x100, 3x3 平滑, frac=0.60)
//   - 典型区 OLS 拟合 log(c/d) = a + g*log(c/b)
// 只提供重算能力; 周期性触发属运行时接入, 本文件不接线.

package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// 定稿门槛 (与 plot_speedref.py 对齐).
const (
	perfThRate, perfThTTFB = 0.65, 0.35 // 理想小时分位门槛
	perfFrac               = 0.60       // 典型区密度前 60%
	perfDMin, perfCMin     = 100, 15    // 微样本剔除
	perfNX, perfNY         = 100, 100   // 密度网格
	perfMinIdeal           = 120        // 理想样本不足则跳过该模型
)

// perfModels 是参与重算的 上游/模型 列表 (与 plot_speedref.py MODELS 一致).
var perfModels = [][2]string{
	{"group", "gpt-6-sol"},
	{"ustc", "deepseek-flash"},
	{"group", "kimi-k3.1-pro"},
	{"ustc", "deepseek-v4-flash"},
	{"arkcp", "deepseek-v4-1-flash-economy"},
	{"group", "deepseek-v4.1-flash-off"},
}

// perfSample 是重算用的单请求有效样本.
type perfSample struct {
	hour          int
	ttfb, d, c, b int64
}

// perfSorted 返回升序副本.
func perfSorted(v []float64) []float64 {
	out := append([]float64(nil), v...)
	sort.Float64s(out)
	return out
}

// perfMedian 与 plot_speedref.py 的 median 一致: 排序后取 v[len/2].
func perfMedian(v []float64) float64 {
	out := perfSorted(v)
	return out[len(out)/2]
}

// perfPct 与 plot_speedref.py 的 pct 一致: v[min(n-1, int(n*q))].
func perfPct(v []float64, q float64) float64 {
	out := perfSorted(v)
	n := len(out)
	if n == 0 {
		return 0
	}
	i := int(float64(n) * q)
	if i > n-1 {
		i = n - 1
	}
	return out[i]
}

// perfHour 从 ts 取小时 (兼容 RFC3339 与无时区的本地写法).
func perfHour(ts string) (int, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.Hour(), true
		}
	}
	return 0, false
}

// perfIdealHours 挑理想小时; 活跃小时不足 4 个时退化为全天.
func perfIdealHours(rows []perfSample) []int {
	by := make(map[int][]perfSample)
	for _, r := range rows {
		by[r.hour] = append(by[r.hour], r)
	}
	rate := func(r perfSample) float64 { return float64(r.c) / float64(r.d) * 1000 } // tokens/s (毫秒运算, 最后转 /s)
	hm := make(map[int]float64, len(by))
	ht := make(map[int]float64, len(by))
	for h, lst := range by {
		rs := make([]float64, 0, len(lst))
		ts := make([]float64, 0, len(lst))
		for _, r := range lst {
			rs = append(rs, rate(r))
			ts = append(ts, float64(r.ttfb))
		}
		hm[h] = perfMedian(rs)
		ht[h] = perfMedian(ts)
	}
	if len(hm) < 4 {
		hours := make([]int, 24)
		for i := range hours {
			hours[i] = i
		}
		return hours
	}
	hvs := make([]float64, 0, len(hm))
	tvs := make([]float64, 0, len(ht))
	for _, v := range hm {
		hvs = append(hvs, v)
	}
	for _, v := range ht {
		tvs = append(tvs, v)
	}
	rq := perfPct(hvs, perfThRate)
	tq := perfPct(tvs, perfThTTFB)
	var hours []int
	for h := 0; h < 24; h++ {
		v, ok := hm[h]
		if ok && v >= rq && ht[h] <= tq {
			hours = append(hours, h)
		}
	}
	return hours
}

// perfDenseMask 在 (x, y) 上做二维密度 (100x100 + 3x3 环状平滑),
// 返回密度前 frac 的布尔掩码 (与 python 的 dense_mask 对齐).
func perfDenseMask(x, y []float64, frac float64, nx, ny int) []bool {
	xlo := perfPct(x, 0.005) - 0.2
	xhi := perfPct(x, 0.995) + 0.2
	ylo := perfPct(y, 0.005) - 0.2
	yhi := perfPct(y, 0.995) + 0.2

	H := make([][]int, nx)
	for i := range H {
		H[i] = make([]int, ny)
	}
	ix := make([]int, len(x))
	iy := make([]int, len(y))
	xd := (xhi - xlo) / float64(nx) // numpy bin 宽度 (先除, 再除的形式)
	yd := (yhi - ylo) / float64(ny)
	for k := range x {
		i := int((x[k] - xlo) / xd)
		j := int((y[k] - ylo) / yd)
		if i < 0 {
			i = 0
		} else if i >= nx {
			i = nx - 1
		}
		if j < 0 {
			j = 0
		} else if j >= ny {
			j = ny - 1
		}
		ix[k], iy[k] = i, j
		H[i][j]++
	}

	// 3x3 环状平滑 (等价 np.roll 求和的 3x3 窗口).
	Hs := make([][]int, nx)
	for i := range Hs {
		Hs[i] = make([]int, ny)
	}
	for di := -1; di <= 1; di++ {
		for dj := -1; dj <= 1; dj++ {
			for i := 0; i < nx; i++ {
				ii := (i + di + nx) % nx
				for j := 0; j < ny; j++ {
					jj := (j + dj + ny) % ny
					Hs[i][j] += H[ii][jj]
				}
			}
		}
	}

	dens := make([]float64, len(x))
	for k := range x {
		dens[k] = float64(Hs[ix[k]][iy[k]])
	}
	sd := perfSorted(dens)
	// 与 numpy.quantile (method=linear) 对齐.
	pos := (1 - frac) * float64(len(sd)-1)
	lo := int(math.Floor(pos))
	var th float64
	if lo >= len(sd)-1 {
		th = sd[len(sd)-1]
	} else {
		f := pos - float64(lo)
		th = sd[lo]*(1-f) + sd[lo+1]*f
	}
	sel := make([]bool, len(x))
	for k := range x {
		sel[k] = dens[k] >= th
	}
	return sel
}

// perfOLS 最小二乘拟合 y = a + g*z, 返回 (g, a).
func perfOLS(z, y []float64) (g, a float64) {
	n := len(z)
	if n == 0 {
		return 0, 0
	}
	zm, ym := 0.0, 0.0
	for k := range z {
		zm += z[k]
		ym += y[k]
	}
	zm /= float64(n)
	ym /= float64(n)
	var sxy, sxx float64
	for k := range z {
		dz := z[k] - zm
		sxy += dz * (y[k] - ym)
		sxx += dz * dz
	}
	// 浮点累积误差下恒定 z 的 sxx 约 1e-27, 视为退化: g=0 (真实数据 sxx 远大于此).
	if sxx < 1e-12 {
		return 0, ym
	}
	g = sxy / sxx
	return g, ym - g*zm
}

// perfLoadRows 读 logDir 下全部 requests*.jsonl, 过滤到目标 上游/模型 的有效样本.
func perfLoadRows(logDir, up, model string) ([]perfSample, error) {
	matches, err := filepath.Glob(filepath.Join(logDir, "requests*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var rows []perfSample
	for _, p := range matches {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var r logRec
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				continue // 跳过坏行
			}
			if r.Status >= 400 || !r.Stream || r.Upstream != up || r.Model != model {
				continue
			}
			if r.Usage == nil {
				continue
			}
			tt, d, c, b := r.TtfbMs, r.DecodeMs, int64(r.Usage.Completion), r.Bytes
			if r.Ts == "" || tt <= 0 || d <= 0 || c <= 0 || b <= 0 {
				continue
			}
			hour, ok := perfHour(r.Ts)
			if !ok {
				continue
			}
			if c >= perfCMin && d >= perfDMin {
				rows = append(rows, perfSample{hour: hour, ttfb: tt, d: d, c: c, b: b})
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// recomputePerfRef 用 logDir 下的全部请求日志重算 models (缺省 perfModels) 的速度
// 参考参数, 返回新参数表与因理想样本不足被跳过的模型列表.
func recomputePerfRef(logDir string, models [][2]string) (*PerfRef, []string, error) {
	if models == nil {
		models = perfModels
	}
	byModel := make(map[string]perfParam)
	var skipped []string
	for _, m := range models {
		rows, err := perfLoadRows(logDir, m[0], m[1])
		if err != nil {
			return nil, nil, err
		}
		ih := perfIdealHours(rows)
		in := make(map[int]bool, len(ih))
		for _, h := range ih {
			in[h] = true
		}
		var ideal []perfSample
		for _, r := range rows {
			if in[r.hour] {
				ideal = append(ideal, r)
			}
		}
		if len(ideal) < perfMinIdeal {
			skipped = append(skipped, m[0]+"/"+m[1])
			continue
		}
		x := make([]float64, len(ideal))
		y := make([]float64, len(ideal))
		z := make([]float64, len(ideal))
		for k, r := range ideal {
			x[k] = math.Log(float64(r.b) / float64(r.c))        // ln(b/c)
			y[k] = math.Log(float64(r.c) / float64(r.d) * 1000) // ln(c/d), 毫秒运算最后转 /s
			z[k] = math.Log(float64(r.c) / float64(r.b))        // ln(c/b)
		}
		sel := perfDenseMask(x, y, perfFrac, perfNX, perfNY)
		zs := make([]float64, 0, len(ideal))
		ys := make([]float64, 0, len(ideal))
		for k := range sel {
			if sel[k] {
				zs = append(zs, z[k])
				ys = append(ys, y[k])
			}
		}
		g, a := perfOLS(zs, ys)
		// 缩放常数 m = (典型区 c/b 中位)^g: 把归一后的 r* 校准回典型解码速率量级
		scale := math.Exp(g * perfMedian(zs))
		byModel[m[0]+"/"+m[1]] = perfParam{G: g, A: a, M: scale}
	}
	return &PerfRef{byModel: byModel}, skipped, nil
}
