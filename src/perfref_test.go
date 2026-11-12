package main

import (
	"math"
	"os"
	"path/filepath"
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
  "ustc/deepseek-flash": {"g": 0.7937},
  "group/gpt-6-sol": {"g": -0.2},
  "group/kimi-k3.1-pro": {"g": 0}
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

// g=0 的模型应退化为原始解码速率 c/d.
func TestPerfRefGZeroFallsBackToRawRate(t *testing.T) {
	p, err := loadPerfRef(writePerfRef(t, perfRefSample))
	if err != nil {
		t.Fatal(err)
	}
	got := p.index("group", "kimi-k3.1-pro", 100, 5000, 6000)
	if want := 100.0 / (5000.0 / 1000); math.Abs(got-want) > 1e-9 {
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
}
