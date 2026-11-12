// tui_gate.go: settings 页的调度参数 (gate) 统一编辑。

package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// gateLabels 是设置页里 gate 参数的顺序与名字, 与 GateCfg 的字段一一对应;
// 同类配置归到一起: 调度等待 -> 各锁时长 -> 低性能判定 -> 显示窗口 -> 粘性失效窗口 -> 恢复探测。
// 时间字段与配置文件的顶层键一致用秒 (`*_sec`, 可小数), 单位见各 gateText;
// 退避序列只能用 = 按逗号输入 (秒); probe_enabled 是开关 (0/1 = 关/开)。
var gateLabels = []string{
	"interval_sec", "sem_wait_sec", "queue_wait_sec", "retry_backoff_sec",
	"concurrency_lock_sec", "quota_lock_sec", "breaker_lock_sec", "lowperf_lock_sec",
	"lowperf_window_sec", "lowperf_peak_max", "lowperf_avg_max", "lowperf_rel_max", "lowperf_ttfb_max_sec",
	"rate_window_sec", "block_window_sec", "sticky_ttl_sec", "probe_enabled",
}

// gateCN 与 gateLabels 一一对应的中文显示名 (设置页用)。
var gateCN = []string{
	"出队间隔", "等并发槽位", "排队上限", "退避序列",
	"并发锁时长", "额度锁时长", "熔断锁时长", "低性能锁时长",
	"低性能窗口", "低吞吐峰值", "低吞吐平均", "低吞吐历史比", "高延迟阈值",
	"速度窗口", "采样尺度", "粘性失效窗口", "恢复探测",
}

// 特殊字段的下标 (code 里按下标分支, 集中在这些常量上, 重排顺序只改 gateLabels/gateCN)。
const (
	gateIdxRetry = 3  // 退避序列: 逗号秒串, 只能 = 输入
	gateIdxPeak  = 9  // 低吞吐峰值门槛 (t/s)
	gateIdxAvg   = 10 // 低吞吐平均门槛 (t/s)
	gateIdxRel   = 11 // 低吞吐历史比门槛 (百分比)
	gateIdxTtfb  = 12 // 高延迟阈值 (0 = 关闭该项)
	gateIdxProbe = 16 // 恢复探测开关
)

// gateStep 是 +/- 一次调整的步长 (退避序列返回 0 = 不用加减键)。
func gateStep(i int) int {
	switch i {
	case 0:
		return 100
	case 1, 2:
		return 500
	case 3:
		return 0 // retry: 用 = 按逗号输入
	case 4, 5, 6, 7, 8:
		return 60000 // 四把锁 + 低性能判定窗口
	case 9, 10, 11:
		return 1 // 低吞吐门槛 t/s / 历史比 1%
	case 12:
		return 1000 // 高延迟阈值
	case 13:
		return 600000 // rate_window: 每次 10 分钟
	case 14:
		return 5000 // block_window: 每次 5 秒
	case 15:
		return 60000 // sticky_ttl: 每次 1 分钟
	case 16:
		return 1 // probe_enabled: 每次翻转 0/1
	}
	return 0
}

// gateField 取一个 gate 参数 (i 是 gateLabels 的下标);
// 13/14 是扁平顶层窗口参数 (rate/block), 不走 GateCfg。
func (e *tuiEditor) gateField(i int) int {
	g := e.gate
	switch i {
	case 0:
		return g.IntervalMs
	case 1:
		return g.SemWaitMs
	case 2:
		return g.QueueWaitMs
	case 4:
		return g.ConcurrencyLockMs
	case 5:
		return g.QuotaLockMs
	case 6:
		return g.BreakerLockMs
	case 7:
		return g.LowPerfLockMs
	case 8:
		return g.LowPerfWindowMs
	case 9:
		return g.LowPerfPeakMax
	case 10:
		return g.LowPerfAvgMax
	case 11:
		return int(math.Round(g.LowPerfRelMax * 100)) // 百分比显示
	case 12:
		return g.LowPerfTtfbMaxMs
	case 13:
		return e.rateWinMs
	case 14:
		return e.blockWinMs
	case 15:
		return g.StickyTTLMs
	case 16:
		if e.probeOn {
			return 1
		}
		return 0
	}
	return 0
}

// gateText 是设置页里那一行的值+单位: 时间字段与配置一致按秒 (可小数, 单位 s),
// 退避序列写成逗号秒串; 两个门槛单位 t/s, 历史比按百分比, 开关写 开/关。
func (e *tuiEditor) gateText(i int) string {
	if i == gateIdxRetry {
		parts := make([]string, 0, len(e.gate.RetryBackoffMs))
		for _, v := range e.gate.RetryBackoffMs {
			parts = append(parts, fsec(v))
		}
		return strings.Join(parts, ",") + " s"
	}
	if i == gateIdxPeak || i == gateIdxAvg {
		return strconv.Itoa(e.gateField(i)) + " t/s"
	}
	if i == gateIdxRel {
		return strconv.Itoa(e.gateField(i)) + "%"
	}
	if i == gateIdxProbe {
		if e.probeOn {
			return "开"
		}
		return "关"
	}
	return fsec(e.gateField(i)) + " s" // 秒, 可小数
}

// fsec 把内部毫秒按配置口径写成秒的显示串 (去尾零, 保留小数)。
func fsec(ms int) string {
	return strconv.FormatFloat(msToSec(ms), 'f', -1, 64)
}

// gateNumUnit 把一行的值拆成「数字」与「单位」: 数字列右对齐 (小数点/最后一位对齐),
// 单位跟在后面成列。开关行没有数字, 开/关本身当单位摆进单位列。
func (e *tuiEditor) gateNumUnit(i int) (num, unit string) {
	if i == gateIdxRetry {
		parts := make([]string, 0, len(e.gate.RetryBackoffMs))
		for _, v := range e.gate.RetryBackoffMs {
			parts = append(parts, fsec(v))
		}
		return strings.Join(parts, ","), "s"
	}
	if i == gateIdxPeak || i == gateIdxAvg {
		return strconv.Itoa(e.gateField(i)), "t/s"
	}
	if i == gateIdxRel {
		return strconv.Itoa(e.gateField(i)), "%"
	}
	if i == gateIdxProbe {
		if e.probeOn {
			return "", "开"
		}
		return "", "关"
	}
	return fsec(e.gateField(i)), "s"
}

// isSecField 是否时间类字段 (显示/输入都用秒)。
func isSecField(i int) bool {
	switch i {
	case 0, 1, 2, 4, 5, 6, 7, 8, 12, 13, 14, 15:
		return true
	}
	return false
}

// parseSec 把"秒"(可小数) 解析成内部毫秒。
func parseSec(text string) (int, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || f < 0 {
		return 0, err
	}
	return secToMs(f), nil
}

// loadGate 读配置文件 (扁平顶层键, 时间为秒); 内部转为毫秒的 GateCfg 显示用。
func (e *tuiEditor) loadGate() {
	b, err := os.ReadFile(e.config)
	if err != nil {
		e.gateErr = err.Error()
		return
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		e.gateErr = err.Error()
		return
	}
	e.gate = normGate(c.gate())
	// 扁平窗口 (秒 x1000 -> 毫秒): 0=缺省 (速度窗 1h, 分块采样 35s)。
	if c.RateWindowSec > 0 {
		e.rateWinMs = secToMs(c.RateWindowSec)
	} else {
		e.rateWinMs = 3600000
	}
	if c.BlockWindowSec > 0 {
		e.blockWinMs = secToMs(c.BlockWindowSec)
	} else {
		e.blockWinMs = 35000
	}
	e.probeOn = c.probeEnabled() // 未写缺省开; 显式 false 为关
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
	lo, hi := gateBounds(i)
	v := clampInt(e.gateField(i)+delta*step, lo, hi)
	switch {
	case isSecField(i):
		e.setSecField(i, v)
	case i == gateIdxProbe:
		e.probeOn = v == 1
	case i == gateIdxPeak:
		e.gate.LowPerfPeakMax = v
	case i == gateIdxAvg:
		e.gate.LowPerfAvgMax = v
	case i == gateIdxRel:
		e.gate.LowPerfRelMax = float64(v) / 100
	}
	e.gateDirty = true
	e.message = gateCN[i] + " = " + e.gateText(i)
}

// gateBounds 是 +/- 调整时的取值范围 (毫秒 / t/s / 百分比 / 0-1 开关)。
func gateBounds(i int) (lo, hi int) {
	switch i {
	case 0:
		return 100, 60000
	case 1, 2:
		return 100, 600000
	case 4, 5, 6, 7, 8, 14, 15:
		return 1000, 86400000
	case 9, 10:
		return 1, 100000
	case 11:
		return 1, 100
	case 12:
		return 0, 86400000
	case 13:
		return 60000, 86400000
	case 16:
		return 0, 1
	}
	return 0, 0
}

// setGatePrompt 处理 = 输入: 时间字段与退避序列都用秒 (可小数, 与配置文件一致),
// 两个 t/s 门槛 / 历史比 (百分比) / 开关用 0/1 / on/off / 开/关。
func (e *tuiEditor) setGatePrompt(i int, text string) {
	switch {
	case i == gateIdxRetry:
		e.setRetrySeq(text)
	case i == gateIdxProbe:
		e.setProbeSwitch(text)
	case i == gateIdxRel:
		e.setRelInput(text)
	case isSecField(i):
		e.setSecInput(i, text)
	default:
		e.setRateLimit(i, text)
	}
}

// setRetrySeq 解析退避序列: 逗号分隔的正秒数 (可小数)。
func (e *tuiEditor) setRetrySeq(text string) {
	parts := strings.Split(text, ",")
	seq := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ms, err := parseSec(p)
		if err != nil || ms <= 0 {
			e.message = "退避序列要逗号分隔的正秒数 (可小数)"
			return
		}
		seq = append(seq, ms)
	}
	if len(seq) == 0 {
		e.message = "退避序列不能为空"
		return
	}
	e.gate.RetryBackoffMs = seq
	e.gateDirty = true
	e.message = "retry_backoff_sec = " + text
}

// setProbeSwitch 解析恢复探测开关。
func (e *tuiEditor) setProbeSwitch(text string) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "0", "false", "off", "关":
		e.probeOn = false
	case "1", "true", "on", "开":
		e.probeOn = true
	default:
		e.message = "probe_enabled 要用 0/1 / on/off / 开/关"
		return
	}
	e.gateDirty = true
	if e.probeOn {
		e.message = "probe_enabled = 开"
	} else {
		e.message = "probe_enabled = 关"
	}
}

// setSecInput 解析时间字段 (非负秒数, 可小数); 高延迟阈值允许 0 (= 关闭判定)。
func (e *tuiEditor) setSecInput(i int, text string) {
	ms, err := parseSec(text)
	if err != nil || (ms == 0 && i != gateIdxTtfb) {
		e.message = gateCN[i] + " 要非负秒数 (可小数)"
		return
	}
	e.setSecField(i, ms)
	e.gateDirty = true
	e.message = gateCN[i] + " = " + fsec(ms)
}

// setRateLimit 解析低吞吐门槛 (正整数 t/s)。
func (e *tuiEditor) setRateLimit(i int, text string) {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || n < 1 {
		e.message = gateCN[i] + " 要正整数 (t/s)"
		return
	}
	if i == gateIdxPeak {
		e.gate.LowPerfPeakMax = n
	} else if i == gateIdxAvg {
		e.gate.LowPerfAvgMax = n
	}
	e.gateDirty = true
	e.message = gateCN[i] + " = " + strconv.Itoa(n)
}

// setSecField 把一个时间字段 (毫秒) 写回, i 是 gateLabels 下标 (isSecField 内的那些)。
func (e *tuiEditor) setSecField(i, ms int) {
	switch i {
	case 0:
		e.gate.IntervalMs = ms
	case 1:
		e.gate.SemWaitMs = ms
	case 2:
		e.gate.QueueWaitMs = ms
	case 4:
		e.gate.ConcurrencyLockMs = ms
	case 5:
		e.gate.QuotaLockMs = ms
	case 6:
		e.gate.BreakerLockMs = ms
	case 7:
		e.gate.LowPerfLockMs = ms
	case 8:
		e.gate.LowPerfWindowMs = ms
	case 12:
		e.gate.LowPerfTtfbMaxMs = ms // 0 = 关闭高延迟判定
	case 13:
		e.rateWinMs = ms
	case 14:
		e.blockWinMs = ms
	case 15:
		e.gate.StickyTTLMs = ms
	}
}

// setRelInput 解析低吞吐历史比门槛 (整数百分比, 1-100)。
func (e *tuiEditor) setRelInput(text string) {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || n < 1 || n > 100 {
		e.message = gateCN[gateIdxRel] + " 要 1-100 的整数百分比"
		return
	}
	e.gate.LowPerfRelMax = float64(n) / 100
	e.gateDirty = true
	e.message = gateCN[gateIdxRel] + " = " + strconv.Itoa(n) + "%"
}

// marshalConfig 按字典序重排顶层键 (与 store 保存同一格式口径): 2 空格缩进,
// 值原样保留 (RawMessage, 密钥等不认识的字段不丢)。
func marshalConfig(top map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteString("{")
	for i, k := range keys {
		var body bytes.Buffer
		if err := json.Indent(&body, top[k], "  ", "  "); err != nil {
			return nil, err
		}
		if i > 0 {
			b.WriteString(",")
		}
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
	for k, v := range e.gateFlat(normGate(e.gate)) {
		if w, err := json.Marshal(v); err == nil {
			top[k] = w
		}
	}
	out, err := marshalConfig(top)
	if err != nil {
		return err
	}
	if err := writeConfigAtomic(target, out); err != nil {
		return err
	}
	e.gateDirty = false
	return nil
}

// gateFlat 是写回配置的调度参数扁平键 (顶层, 秒; 开关写 bool)。
func (e *tuiEditor) gateFlat(g GateCfg) map[string]any {
	return map[string]any{
		"interval_sec":         msToSec(g.IntervalMs),
		"sem_wait_sec":         msToSec(g.SemWaitMs),
		"queue_wait_sec":       msToSec(g.QueueWaitMs),
		"concurrency_lock_sec": msToSec(g.ConcurrencyLockMs),
		"quota_lock_sec":       msToSec(g.QuotaLockMs),
		"breaker_lock_sec":     msToSec(g.BreakerLockMs),
		"lowperf_window_sec":   msToSec(g.LowPerfWindowMs),
		"lowperf_peak_max":     g.LowPerfPeakMax,
		"lowperf_avg_max":      g.LowPerfAvgMax,
		"lowperf_rel_max":      g.LowPerfRelMax,
		"lowperf_ttfb_max_sec": msToSec(g.LowPerfTtfbMaxMs),
		"lowperf_lock_sec":     msToSec(g.LowPerfLockMs),
		"retry_backoff_sec":    msListToSec(g.RetryBackoffMs),
		"rate_window_sec":      msToSec(e.rateWinMs),
		"block_window_sec":     msToSec(e.blockWinMs),
		"sticky_ttl_sec":       msToSec(g.StickyTTLMs),
		"probe_enabled":        e.probeOn,
	}
}

// writeConfigAtomic 按目标现有权限原子写回 (临时文件 + rename)。
func writeConfigAtomic(target string, out []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(target); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, out, mode); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// saveSettings 一条龙保存: 调度参数写配置文件, 权重有改动时重生成配置
// (tollgate.json 的 aliases/upstreams 与权重 + 模型列表保持一致, 含停用模型剔除), 最后热加载。
// 任一环节失败即停, 不把不一致的半套推给运行中的代理。
func (m *tuiModel) saveSettings() {
	e := m.prio
	if e == nil {
		return
	}
	weightChanged := e.usable() && e.dirty
	gateChanged := e.gateDirty
	if !weightChanged && !gateChanged {
		return // 没有改动, 无事可做
	}
	if gateChanged {
		if err := e.saveGate(); err != nil {
			e.message = "调度参数保存失败: " + err.Error()
			return
		}
	}
	if weightChanged {
		if err := e.regenerate(); err != nil { // 内部先把权重写回配置再跑生成器
			return // 失败原因已写进 message
		}
	}
	m.prioHup()
}
