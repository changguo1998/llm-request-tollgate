// tui_priokey.go: 优先级编辑器按键、存盘与重生成。

package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func (e *tuiEditor) saveStore() {
	if !e.usable() {
		return
	}
	if err := e.save(); err != nil {
		e.message = "保存失败: " + err.Error()
		return
	}
	e.dirty = false
	e.message = "已保存 " + e.path
}

// regenerate 先存盘再跑生成器 (生成器读 store, 不先存就白跑)。
func (e *tuiEditor) regenerate() {
	if !e.usable() {
		return
	}
	e.saveStore()
	if e.dirty {
		return // 存盘失败, 别再往下跑
	}
	args := []string{
		filepath.Join(e.root, "scripts", "gen_tollgate_config.py"),
		"--fff-dir", filepath.Dir(e.path),
		"--out", e.config,
		"--providers", strings.Join(e.providers, ","),
	}
	cmd := exec.Command("python3", args...)
	cmd.Dir = e.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.message = "生成失败: " + lastLine(string(out), err.Error())
		return
	}
	e.message = "已重新生成配置 " + e.config
}

// lastLine 取输出的最后一行 (报错时最有信息量的那行), 空则用兜底文案。
func lastLine(out, fallback string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return fallback
}

// prioHup 让代理吃上新配置: 同进程模式直接重载 (不依赖 systemd), 观察常驻服务时
// 走 deploy/tollgate.sh hup 发 SIGHUP。
func (m *tuiModel) prioHup() {
	e := m.prio
	if e == nil {
		return
	}
	if m.reloadFn != nil {
		if err := m.reloadFn(); err != nil {
			e.message = "热加载失败: " + err.Error()
			return
		}
		e.message = "已热加载 " + e.config
		return
	}
	cmd := exec.Command(filepath.Join(e.root, "deploy", "tollgate.sh"), "hup")
	cmd.Dir = e.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.message = "hup 失败: " + lastLine(string(out), err.Error())
		return
	}
	e.message = "已 hup, 代理生效"
}

// key 处理 settings 页的一个按键; 返回 true = 请求退出 (未保存时先问一次)。
func (e *tuiEditor) key(b byte) (quit bool, handled bool) {
	if e.prompt != nil {
		return false, e.promptKey(b)
	}
	if !e.usable() && e.focus != 2 {
		return false, false
	}
	switch b {
	case 'q', 0x03, 0x04:
		// 有未保存改动先问一次; 再按一次 q 就是确认放弃。
		if (e.dirty || e.gateDirty) && !e.asked {
			e.message = "有未保存改动: 再按 q 放弃退出, 或按 s 保存"
			e.asked = true
			return false, true
		}
		return true, true
	case 'm':
		e.focus = 0 // m = 切到模型 (改模型优先级)
		return false, true
	case 'p':
		e.focus = 1 // p = 切到 provider (改 provider 优先级)
		return false, true
	case 'g':
		e.focus = 2 // g = 切到调度参数 (gate)
		return false, true
	}
	if e.focus == 2 {
		switch b {
		case 'j':
			e.moveFocus(1)
		case 'k':
			e.moveFocus(-1)
		case 'G':
			e.jump(1 << 30)
		case ' ':
			e.moveFocus(1 << 20)
		case '+':
			e.adjust(1)
		case '-':
			e.adjust(-1)
		case '=':
			i := e.gateSel
			e.prompt = &tuiPrompt{label: "新的 " + gateLabels[i] + ":", commit: func(v string) {
				e.setGatePrompt(i, v)
			}}
		default:
			return false, false
		}
		return false, true
	}
	switch b {
	case 'j':
		e.moveFocus(1)
	case 'k':
		e.moveFocus(-1)
	case 'G':
		e.jump(1 << 30)
	case ' ':
		e.moveFocus(e.visibleRows())
	case '+':
		e.adjust(1)
	case '-':
		e.adjust(-1)
	case '=':
		e.prompt = &tuiPrompt{label: "新的优先级数值:", commit: func(v string) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				e.setValue(n)
			}
		}}
	case '/':
		e.prompt = &tuiPrompt{label: "过滤 (空 = 清除):", commit: func(v string) {
			e.filter = v
			e.modelSel, e.top, e.prevKey = 0, 0, ""
			e.rebuild()
		}}
	default:
		return false, false
	}
	return false, true
}

// promptKey 行内输入: 可打印字符进缓冲, 退格删, 回车提交, Esc / Ctrl-C 取消。
func (e *tuiEditor) promptKey(b byte) bool {
	p := e.prompt
	switch {
	case b == '\r' || b == '\n':
		e.prompt = nil
		p.commit(p.buffer)
	case b == 0x1b || b == 0x03:
		e.prompt = nil
	case b == 0x7f || b == 0x08:
		if len(p.buffer) > 0 {
			p.buffer = p.buffer[:len(p.buffer)-1]
		}
	case b >= 0x20:
		p.buffer += string(b)
	}
	return false
}

func (e *tuiEditor) moveFocus(d int) {
	if e.focus == 2 {
		e.gateSel = clampInt(e.gateSel+d, 0, len(gateLabels)-1)
		return
	}
	if e.focus == 1 {
		e.provSel = clampInt(e.provSel+d, 0, max(0, len(e.providers)-1))
		return
	}
	e.modelSel = clampInt(e.modelSel+d, 0, max(0, len(e.filtered())-1))
}

func (e *tuiEditor) jump(to int) {
	if e.focus == 2 {
		e.gateSel = clampInt(to, 0, len(gateLabels)-1)
		return
	}
	if e.focus == 1 {
		e.provSel = clampInt(to, 0, max(0, len(e.providers)-1))
		return
	}
	e.modelSel = clampInt(to, 0, max(0, len(e.filtered())-1))
}

// visibleRows 是模型列表当前能显示的行数 (翻页步长), draw 每帧刷新。
func (e *tuiEditor) visibleRows() int {
	if e.vis > 0 {
		return e.vis
	}
	return 10
}
