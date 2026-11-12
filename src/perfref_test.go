package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePerfRef 写一份最小速度参考 JSON 到临时目录, 返回路径.
func writePerfRef(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "speedref_params.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const perfRefSample = `{
  "_meta": {"version": "2026-11", "generated_at": "2026-11-12T21:40:10"},
  "ustc/deepseek-flash": {"g": 0.7937, "a": 7.9505},
  "group/gpt-6-sol": {"g": -0.2, "a": 2.0},
  "group/kimi-k3.1-pro": {"g": 0, "a": 1.0}
}`

func TestPerfRefLoadAndIndex(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatalf("loadPerfRef: %v", err)
	}

	// 已知参数: c=100, d=5000ms, b=6000, g=0.7937
	c, d, b := int64(100), int64(5000), int64(6000)
	got := p.index("ustc", "deepseek-flash", c, d, b)
	want := 20.0 * math.Pow(float64(c)/float64(b), 0.7937)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("index = %v, want %v", got, want)
	}
}

// g=0 且参数在表中: (c/b)^0=1, perf 等于原始速率 c/d.
func TestPerfRefGZeroInTable(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatal(err)
	}
	got := p.index("group", "kimi-k3.1-pro", 100, 5000, 6000)
	if want := 20.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("g=0 应返回原始速率 %v, 得到 %v", want, got)
	}
}

// 参数表里没有的模型同样退回原始速率 (不因缺参数而报错).
func TestPerfRefMissingModelFallsBack(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatal(err)
	}
	got := p.index("nowhere", "unknown-model", 100, 5000, 6000)
	if want := 100.0 / (5000.0 / 1000); math.Abs(got-want) > 1e-9 {
		t.Errorf("缺模型应返回原始速率 %v, 得到 %v", want, got)
	}
}

// 负 g 同样按公式计算 (如 arkcp / v4.1flash-off).
func TestPerfRefNegativeG(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatal(err)
	}
	got := p.index("group", "gpt-6-sol", 100, 5000, 6000)
	want := 20.0 * math.Pow(100.0/6000.0, -0.2)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("index = %v, want %v", got, want)
	}
}

// 非法输入返回 0.
func TestPerfRefInvalidInput(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		c, d, b int64
	}{
		{0, 5000, 6000}, {100, 0, 6000}, {100, 5000, 0}, {-1, 5000, 6000},
	} {
		if got := p.index("ustc", "deepseek-flash", r.c, r.d, r.b); got != 0 {
			t.Errorf("输入 (%d,%d,%d) 应返回 0, 得到 %v", r.c, r.d, r.b, got)
		}
	}
}

// nil 接收者按无参数 (g=0) 处理, 不应崩溃.
func TestPerfRefNilReceiver(t *testing.T) {
	var p *PerfRef
	if got := p.index("ustc", "deepseek-flash", 100, 5000, 6000); got != 20.0 {
		t.Errorf("nil 接收者应返回原始速率 20, 得到 %v", got)
	}
}

// 文件缺失 / JSON 损坏返回错误.
func TestPerfRefLoadErrors(t *testing.T) {
	if _, err := loadPerfRef(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("缺失文件应报错")
	}
	if _, err := loadPerfRef(writePerfRef(t, `{"x": [`)); err == nil {
		t.Error("损坏 JSON 应报错")
	}
	if p, err := loadPerfRef(writePerfRef(t, `{"_meta": {"version": "x"}}`)); err != nil {
		t.Error("仅含元数据应可加载")
	} else if got := p.index("a", "b", 100, 5000, 6000); got != 20.0 {
		t.Errorf("空表应退回原始速率 20, 得到 %v", got)
	}
}

// 旧参数 JSON 无 a 字段也可正常计算 (a 仅参考, 不参与).
func TestPerfRefMissingANoNormalization(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, `{"group/kimi-k3.1-pro": {"g": 0.2223}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := p.index("group", "kimi-k3.1-pro", 100, 5000, 6000)
	want := 20.0 * math.Pow(100.0/6000.0, 0.2223)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("缺 a 应不归一, got %v want %v", got, want)
	}
}

// perfRefTestLog 写一份 n 条同模型样本的日志 (确定性: 每样本同速率同密度).
func perfRefTestLog(t *testing.T, dir string, n int) string {
	t.Helper()
	path := filepath.Join(dir, "requests-test.jsonl")
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(`{"ts":"2026-11-05T10:00:00","upstream":"group","model":"gpt-6-sol",` +
			`"stream":true,"status":200,"ttfb_ms":1000,"decode_ms":1000,` +
			`"usage":{"completion":50},"bytes":200}` + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// 恒定速率+恒定密度: 样本落同一点, 密度全保留, OLS 退化 g=0, a=ln(50).
func TestPerfRecomputeFromLogs(t *testing.T) {
	dir := t.TempDir()
	perfRefTestLog(t, dir, 150)
	p, skipped, err := recomputePerfRef(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != len(perfModels)-1 {
		t.Errorf("应跳过 %d 个模型, 得到 %v", len(perfModels)-1, skipped)
	}
	for _, s := range skipped {
		if s == "group/gpt-6-sol" {
			t.Errorf("gpt-6-sol 不应被跳过")
		}
	}
	pr, ok := p.param("group", "gpt-6-sol")
	if !ok {
		t.Fatal("参数表应含 group/gpt-6-sol")
	}
	if math.Abs(pr.G) > 1e-9 {
		t.Errorf("恒定样本 g 应为 0, 得到 %v", pr.G)
	}
	if want := math.Log(50.0); math.Abs(pr.A-want) > 1e-9 {
		t.Errorf("a 应为 ln(50)=%v, 得到 %v", want, pr.A)
	}
}

// 空日志目录: 全部模型被跳过, 表为空, 不报错.
func TestPerfRecomputeEmptyDir(t *testing.T) {
	p, skipped, err := recomputePerfRef(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != len(perfModels) {
		t.Errorf("应跳过全部 %d 个模型, 得到 %v", len(perfModels), skipped)
	}
	if len(p.byModel) != 0 {
		t.Errorf("空日志应产生空表, 得到 %d 项", len(p.byModel))
	}
}

// 最小二乘数值: y = 1 + 2z 应得 g=2, a=1.
func TestPerfOLS(t *testing.T) {
	z := []float64{1, 2, 3, 4}
	y := []float64{3, 5, 7, 9}
	g, a := perfOLS(z, y)
	if math.Abs(g-2) > 1e-9 || math.Abs(a-1) > 1e-9 {
		t.Errorf("OLS 应得 g=2,a=1, 得到 g=%v,a=%v", g, a)
	}
}

// 恒定 z 时 OLS 应退化 (浮点误差下 sxx 非精确 0).
func TestPerfOLSConstantZ(t *testing.T) {
	z := make([]float64, 200)
	y := make([]float64, 200)
	for i := range z {
		z[i] = -1.3862943611198906
		y[i] = 3.912023005428146
	}
	g, a := perfOLS(z, y)
	if math.Abs(g) > 1e-12 {
		t.Errorf("恒定 z 应得 g=0, 得到 %v", g)
	}
	if math.Abs(a-3.912023005428146) > 1e-9 {
		t.Errorf("a 应等于均值 y, 得到 %v", a)
	}
}

// 周期重算入口: 空目录应置空表, 有日志时应填参数表; perfIndex 线程安全读取.
func TestPerfRuntimeRecompute(t *testing.T) {
	dir := t.TempDir()
	a := &App{cfg: &Config{LogPath: filepath.Join(dir, "requests.jsonl")}}
	a.recomputePerf()
	a.perfRefMu.RLock()
	empty := a.perfRef != nil && len(a.perfRef.byModel) == 0
	a.perfRefMu.RUnlock()
	if !empty {
		t.Fatalf("空目录重算应得空表")
	}

	perfRefTestLog(t, dir, 150)
	a.recomputePerf()
	a.perfRefMu.RLock()
	pr, ok := a.perfRef.byModel["group/gpt-6-sol"]
	a.perfRefMu.RUnlock()
	if !ok {
		t.Fatalf("有样本后应含 group/gpt-6-sol")
	}
	if math.Abs(pr.G) > 1e-9 {
		t.Errorf("恒定样本 g 应为 0, 得到 %v", pr.G)
	}
	if got := a.perfIndex("group", "gpt-6-sol", 100, 5000, 200); got != 20.0 {
		t.Errorf("perfIndex 应返回原始速率 20, 得到 %v", got)
	}
}
