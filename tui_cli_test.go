package main

import (
	"strings"
	"testing"
)

// 只写 -tui-state 时补默认地址; 带值的三种写法原样保留; -- 之后不碰。
func TestWithTuiStateDefault(t *testing.T) {
	const def = "http://127.0.0.1:8787"
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"-tui-state"}, []string{"-tui-state=" + def}},
		{[]string{"-tui", "-tui-state"}, []string{"-tui", "-tui-state=" + def}},
		{[]string{"-tui-state", "http://x:1"}, []string{"-tui-state", "http://x:1"}},
		{[]string{"-tui-state=http://x:1"}, []string{"-tui-state=http://x:1"}},
		{[]string{"-tui-state", "-config", "c.json"}, []string{"-tui-state=" + def, "-config", "c.json"}},
		{[]string{"--", "-tui-state"}, []string{"--", "-tui-state"}},
		{[]string{"-summary", "x"}, []string{"-summary", "x"}},
	}
	for _, c := range cases {
		got := withTuiStateDefault(c.in, def)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("withTuiStateDefault(%v) = %v, 期望 %v", c.in, got, c.want)
		}
	}
}

// 两个入口互斥; -tui-state 不带地址时落到默认 URL。
func TestTuiTarget(t *testing.T) {
	const def = "http://127.0.0.1:8787"
	if on, url, err := tuiTarget(true, false, "", def); !on || url != "" || err != nil {
		t.Errorf("-tui 应走同进程: on=%v url=%q err=%v", on, url, err)
	}
	if on, url, err := tuiTarget(false, true, "", def); !on || url != def || err != nil {
		t.Errorf("-tui-state 不带地址应用默认: on=%v url=%q err=%v", on, url, err)
	}
	if on, url, err := tuiTarget(false, true, "http://x:1/", def); !on || url != "http://x:1/" || err != nil {
		t.Errorf("-tui-state 带地址应原样返回: on=%v url=%q err=%v", on, url, err)
	}
	if on, _, err := tuiTarget(true, true, "http://x:1", def); err == nil {
		t.Errorf("两个选项同时给应报错 (on=%v)", on)
	}
	if on, _, err := tuiTarget(false, false, "", def); on || err != nil {
		t.Errorf("都不给时不应启动状态台: on=%v err=%v", on, err)
	}
}
