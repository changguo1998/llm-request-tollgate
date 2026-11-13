// tui_priokey.go: 优先级编辑器按键、存盘与重生成。

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (e *tuiEditor) saveWeights() {
	if !e.usable() {
		return
	}
	if err := e.save(); err != nil {
		e.message = "保存失败: " + err.Error()
		return
	}
	e.dirty = false
	e.message = "已保存 " + e.config
}

// regenerate 先把权重写回配置, 再跑生成器 (生成器读 store 的模型列表 + 配置里的权重, 不先存就白跑); 失败返回 error, 原因也写进 message。
func (e *tuiEditor) regenerate() error {
	if !e.usable() {
		e.message = "store 或配置读不到, 无法生成配置"
		return fmt.Errorf("editor unavailable")
	}
	e.saveWeights()
	if e.dirty {
		return fmt.Errorf("权重存盘失败, 配置未重新生成")
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
		return err
	}
	e.message = "已重新生成配置 " + e.config
	return nil
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
		return false, e.gateKey(b)
	}
	return false, e.panelKey(b)
}

// gateKey 是调度参数面板的导航与编辑键 (= 打开行内输入)。
func (e *tuiEditor) gateKey(b byte) bool {
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
		e.prompt = &tuiPrompt{label: "新的 " + gateCN[i] + ":", commit: func(v string) {
			e.setGatePrompt(i, v)
		}}
	default:
		return false
	}
	return true
}

// panelKey 是模型 / provider 面板的导航与编辑键 (/ 过滤, = 改优先级)。
func (e *tuiEditor) panelKey(b byte) bool {
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
			e.modelSel, e.scrollTop, e.prevKey = 0, 0, ""
			e.rebuild()
		}}
	default:
		return false
	}
	return true
}

// promptKey 行内输入: 可打印字符进缓冲, 退格删, 回车提交, Esc / Ctrl-C 取消。
func (e *tuiEditor) promptKey(b byte) bool {
	if feedPrompt(e.prompt, b) {
		e.prompt = nil
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
	e.modelSel = clampInt(e.modelSel+d, 0, max(0, len(e.cands())-1))
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
	e.modelSel = clampInt(to, 0, max(0, len(e.cands())-1))
}

func (e *tuiEditor) visibleRows() int {
	if e.vis > 0 {
		return e.vis
	}
	return 10
}

// sideBase 是旁路写接口的地址: 远程看 -tui-state 给的 base, 同进程用自己的监听地址。
func (m *tuiModel) sideBase() string {
	if m.stateURL != "" {
		return m.stateURL
	}
	if m.listen == "" {
		return ""
	}
	return "http://" + m.listen
}

// postSide 发一个旁路写请求 (POST /unlock 或 /sticky, 参数走 query), 返回响应体;
// 非 200 时带上响应片段。同进程与 -tui-state 两种模式走同一条路。
func (m *tuiModel) postSide(path string, q url.Values) ([]byte, error) {
	base := m.sideBase()
	if base == "" {
		return nil, fmt.Errorf("没有服务地址 (配置缺 listen)")
	}
	req, err := http.NewRequest(http.MethodPost, base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return body, fmt.Errorf("状态码 %d: %s", resp.StatusCode, msg)
	}
	return body, nil
}
