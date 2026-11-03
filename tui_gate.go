// tui_gate.go: settings 页的调度参数 (gate) 统一编辑。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// gateLabels 是设置页里 gate 参数的顺序与名字, 与 GateCfg 的字段一一对应;
// 最后一项是退避序列, 只能用 = 按逗号输入。
var gateLabels = []string{
	"interval_ms", "sem_wait_ms", "queue_wait_ms",
	"concurrency_lock_ms", "quota_lock_ms", "breaker_lock_ms",
	"lowtps_window_ms", "lowtps_peak_max", "lowtps_avg_max", "lowtps_lock_ms",
	"retry_backoff_ms",
}

// gateStep 是 +/- 一次调整的步长 (退避序列返回 0 = 不用加减键)。
func gateStep(i int) int {
	switch i {
	case 0:
		return 100
	case 1, 2:
		return 500
	case 3, 4, 5, 6, 9:
		return 60000
	case 7, 8:
		return 1
	}
	return 0
}

// gateField 取/设一个 gate 参数 (i 是 gateLabels 的下标)。
func (e *tuiEditor) gateField(i int) int {
	g := e.gate
	switch i {
	case 0:
		return g.IntervalMs
	case 1:
		return g.SemWaitMs
	case 2:
		return g.QueueWaitMs
	case 3:
		return g.ConcurrencyLockMs
	case 4:
		return g.QuotaLockMs
	case 5:
		return g.BreakerLockMs
	case 6:
		return g.LowTpsWindowMs
	case 7:
		return g.LowTpsPeakMax
	case 8:
		return g.LowTpsAvgMax
	case 9:
		return g.LowTpsLockMs
	}
	return 0
}

// gateText 是设置页里那一行的值 (退避序列写成逗号串, 与输入格式一致;
// 两个门槛的单位是 tokens/s, 其余是毫秒)。
func (e *tuiEditor) gateText(i int) string {
	if i == len(gateLabels)-1 {
		parts := make([]string, 0, len(e.gate.RetryBackoffMs))
		for _, v := range e.gate.RetryBackoffMs {
			parts = append(parts, strconv.Itoa(v))
		}
		return strings.Join(parts, ",") + " ms"
	}
	if i == 7 || i == 8 {
		return strconv.Itoa(e.gateField(i)) + " t/s"
	}
	return strconv.Itoa(e.gateField(i)) + " ms"
}

// loadGate 读配置文件里的 gate 段 (只读这一段, 不碰密钥; 缺省值用 normGate 补齐)。
func (e *tuiEditor) loadGate() {
	b, err := os.ReadFile(e.config)
	if err != nil {
		e.gateErr = err.Error()
		return
	}
	var raw struct {
		Gate GateCfg `json:"gate"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		e.gateErr = err.Error()
		return
	}
	e.gate = normGate(raw.Gate)
	e.gateErr = ""
}

// adjustGate 用 +/- 改一个参数; 退避序列只走 = 输入。
func (e *tuiEditor) adjustGate(delta int) {
	i := e.gateSel
	step := gateStep(i)
	if step == 0 {
		e.message = "退避序列用 = 按逗号输入"
		return
	}
	v := e.gateField(i) + delta*step
	switch i {
	case 0:
		v = clampInt(v, 100, 60000)
		e.gate.IntervalMs = v
	case 1:
		v = clampInt(v, 100, 600000)
		e.gate.SemWaitMs = v
	case 2:
		v = clampInt(v, 100, 600000)
		e.gate.QueueWaitMs = v
	case 3:
		v = clampInt(v, 1000, 86400000)
		e.gate.ConcurrencyLockMs = v
	case 4:
		v = clampInt(v, 1000, 86400000)
		e.gate.QuotaLockMs = v
	case 5:
		v = clampInt(v, 1000, 86400000)
		e.gate.BreakerLockMs = v
	case 6:
		v = clampInt(v, 1000, 86400000)
		e.gate.LowTpsWindowMs = v
	case 7:
		v = clampInt(v, 1, 100000)
		e.gate.LowTpsPeakMax = v
	case 8:
		v = clampInt(v, 1, 100000)
		e.gate.LowTpsAvgMax = v
	case 9:
		v = clampInt(v, 1000, 86400000)
		e.gate.LowTpsLockMs = v
	}
	e.gateDirty = true
	e.message = gateLabels[i] + " = " + strconv.Itoa(v)
}

// setGatePrompt 处理 = 输入: 普通参数一个整数, 退避序列是逗号分隔的毫秒数。
func (e *tuiEditor) setGatePrompt(i int, text string) {
	if i == len(gateLabels)-1 {
		parts := strings.Split(text, ",")
		seq := make([]int, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 {
				e.message = "退避序列要逗号分隔的正整数"
				return
			}
			seq = append(seq, n)
		}
		if len(seq) == 0 {
			e.message = "退避序列不能为空"
			return
		}
		e.gate.RetryBackoffMs = seq
		e.gateDirty = true
		e.message = "retry_backoff_ms = " + text
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || n <= 0 {
		e.message = gateLabels[i] + " 要正整数"
		return
	}
	switch i {
	case 0:
		e.gate.IntervalMs = n
	case 1:
		e.gate.SemWaitMs = n
	case 2:
		e.gate.QueueWaitMs = n
	case 3:
		e.gate.ConcurrencyLockMs = n
	case 4:
		e.gate.QuotaLockMs = n
	case 5:
		e.gate.BreakerLockMs = n
	case 6:
		e.gate.LowTpsWindowMs = n
	case 7:
		e.gate.LowTpsPeakMax = n
	case 8:
		e.gate.LowTpsAvgMax = n
	case 9:
		e.gate.LowTpsLockMs = n
	}
	e.gateDirty = true
	e.message = gateLabels[i] + " = " + strconv.Itoa(n)
}

// marshalConfig 按固定的顶层顺序写回配置文件: 手写 JSON 是因为 map 会把键排序,
// 让整份配置产生无关的 diff。raw 里没认识的键按字典序补在最后, 不丢数据。
func marshalConfig(top map[string]json.RawMessage) ([]byte, error) {
	known := []string{"listen", "log_path", "timeouts", "gate", "upstreams", "aliases"}
	rest := make([]string, 0)
	for k := range top {
		seen := false
		for _, k2 := range known {
			if k == k2 {
				seen = true
				break
			}
		}
		if !seen {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys := append(append([]string{}, known...), rest...)

	var b bytes.Buffer
	b.WriteString("{")
	first := true
	for _, k := range keys {
		raw, ok := top[k]
		if !ok {
			continue
		}
		var body bytes.Buffer
		if err := json.Indent(&body, raw, "  ", "  "); err != nil {
			return nil, err
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("\n  ")
		b.WriteString(strconv.Quote(k))
		b.WriteString(": ")
		b.Write(body.Bytes())
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}

// saveGate 只改配置文件里的 gate 段, 其余内容原样保留 (密钥不会写回)。
// config.local.json 通常是软链: 写到软链的真实目标, 别把软链本身换成普通文件。
func (e *tuiEditor) saveGate() error {
	target := e.config
	if real, err := filepath.EvalSymlinks(e.config); err == nil {
		target = real
	}
	b, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	raw, err := json.Marshal(normGate(e.gate))
	if err != nil {
		return err
	}
	top["gate"] = raw
	out, err := marshalConfig(top)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	e.gateDirty = false
	return nil
}

// saveSettings 先存优先级 store, 再存 gate 到配置文件, 最后热加载一次。
// 任一保存失败就停下, 不把半套配置推给运行中的代理。
func (m *tuiModel) saveSettings() {
	e := m.prio
	if e == nil {
		return
	}
	if e.usable() && e.dirty {
		e.saveStore()
		if e.dirty {
			return // store 保存失败, 别再热加载
		}
	}
	if !e.gateDirty {
		return
	}
	if err := e.saveGate(); err != nil {
		e.message = "调度参数保存失败: " + err.Error()
		return
	}
	m.prioHup()
}
