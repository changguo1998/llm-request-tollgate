// tui_priority.go: settings 页 —— 调度参数 (gate) 与 tollgate_priority 的编辑器
// (原 scripts/set_priority.ts 的 Go 版, 与状态台合成一个界面)。
//
// 左栏是 provider 面板 + 模型列表 (别名 -> 候选), 右栏是所选别名的候选顺序实时预览:
// 候选顺序 = 模型优先级降序 -> provider 优先级降序 -> 上游顺序 -> 模型 id, 与生成器
// (scripts/gen_tollgate_config.py 的 build_aliases) 同一口径, 所以调完直接 r 重生成
// 配置, 预览里看到的就是落进配置的顺序。
//
// 改的是 fff 的模型列表 store (llm-models.json)。存盘口径: 4 空格缩进、键按字典序
// (Go 的 map 编码行为)、写 .tmp 再 rename, 数值走 json.Number 原样过一遍, 没动过的
// 字段一个都不碰。
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	prioField  = "tollgate_priority"
	groupField = "tollgate_group"
	idField    = "id"
	proxyKind  = "proxy"
	proxyName  = "tollgate" // store 里代表本代理自己的 provider
)

// prioFallbackProviders 是配置里读不到上游顺序时的兜底 (与生成器的缺省 --providers 一致)。
var prioFallbackProviders = []string{"ustc", "arkcp", "arkap", "group"}

// tuiPrioRow 是模型列表的一行; model / entry 直接指向 store 里的对象, 改它就是改 store。
type tuiPrioRow struct {
	alias    string
	provider string
	model    map[string]any
	entry    map[string]any
}

// tuiPrioCand 是候选顺序预览里的一项。
type tuiPrioCand struct {
	provider  string
	modelID   string
	modelPrio int
	provPrio  int
}

type tuiAlias struct {
	name  string
	cands []tuiPrioCand
}

// tuiPrompt 是 "=" / "/" 的行内输入态。
type tuiPrompt struct {
	label  string
	buffer string
	commit func(string)
}

// tuiEditor 是 settings 页的全部状态: 优先级 (store) + 调度参数 (gate, 写配置文件)。
type tuiEditor struct {
	path   string // store: llm-models.json
	config string // 代理配置 (取上游顺序, 也是重生成的目标)
	root   string // 仓库根 (生成器与 deploy/tollgate.sh 所在)

	store     map[string]any
	providers []string // 上游顺序 (配置的 upstreams 顺序, 兜底见 prioFallbackProviders)
	order     []tuiPrioRow
	prevKey   string // 重建列表前选中的行 (provider + "/" + 模型 id), 用来把光标钉在同一行

	dirty    bool
	asked    bool // q 已经提示过一次未保存
	focus    int  // 0 = 模型, 1 = provider, 2 = 调度参数 (gate)
	provSel  int
	modelSel int
	top      int
	vis      int // 模型列表当前能显示的行数 (翻页步长, draw 里刷新)
	filter   string
	message  string
	prompt   *tuiPrompt
	err      string

	gate      GateCfg // 配置文件里的调度参数 (显示用 normGate 补齐后的值)
	gateSel   int
	gateDirty bool
	gateErr   string
}

// ---------- 载入与保存 ----------

// storePathFor 定位模型列表: 先看配置实文件所在目录 (config.local.json 软链到
// ~/fff/config/tollgate.json, store 就在旁边), 再退回 FFF_HOME / ~/fff。
func storePathFor(cfgPath string) string {
	if real, err := filepath.EvalSymlinks(cfgPath); err == nil {
		p := filepath.Join(filepath.Dir(real), "llm-models.json")
		if fileExists(p) {
			return p
		}
	}
	home := os.Getenv("FFF_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, "fff")
		}
	}
	return filepath.Join(home, "config", "llm-models.json")
}

// repoRootFor 找仓库根: 生成器与 deploy/tollgate.sh 都在那里。二进制在仓库根时用
// 二进制的目录; 测试里退到配置目录与当前目录, 谁有 scripts/gen_tollgate_config.py 用谁。
func repoRootFor(cfgPath string) string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	dirs = append(dirs, filepath.Dir(cfgPath))
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		if fileExists(filepath.Join(d, "scripts", "gen_tollgate_config.py")) {
			return d
		}
	}
	if len(dirs) > 0 {
		return dirs[0]
	}
	return "."
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// newTuiEditor 装载 store; 失败时把原因放进 message, 画面照常出 (还能看, 只是不能改)。
func newTuiEditor(cfgPath string) *tuiEditor {
	e := &tuiEditor{
		path:   storePathFor(cfgPath),
		config: cfgPath,
		root:   repoRootFor(cfgPath),
	}
	e.loadGate()
	if err := e.load(); err != nil {
		e.err = err.Error()
		e.message = "store 读不到: " + err.Error()
		return e
	}
	e.message = e.path + " | " + strconv.Itoa(len(e.providers)) + " 个 provider"
	return e
}

func (e *tuiEditor) usable() bool { return e.store != nil }

func (e *tuiEditor) load() error {
	b, err := os.ReadFile(e.path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // 数值原样过一遍, 免得 int 变 float 写回去多个 .0
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return err
	}
	e.store = top
	e.providers = e.providerOrder()
	e.sortProviders()
	e.rebuild()
	return nil
}

// sortProviders 按 provider 优先级降序排 (同优先级保持配置顺序), 选中项跟着名字走。
func (e *tuiEditor) sortProviders() {
	keep := ""
	if e.provSel >= 0 && e.provSel < len(e.providers) {
		keep = e.providers[e.provSel]
	}
	sort.SliceStable(e.providers, func(i, j int) bool {
		return prioOf(e.provEntry(e.providers[i])) > prioOf(e.provEntry(e.providers[j]))
	})
	for i, name := range e.providers {
		if keep != "" && name == keep {
			e.provSel = i
			return
		}
	}
}

// save 原子写回 store (先写 .tmp 再 rename, 与旧脚本一致)。
func (e *tuiEditor) save() error {
	b, err := json.MarshalIndent(e.store, "", "    ")
	if err != nil {
		return err
	}
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.path)
}

// ---------- 调度参数 (gate) ----------

// gateLabels 是设置页里 gate 参数的顺序与名字, 与 GateCfg 的字段一一对应;
// 最后一项是退避序列, 只能用 = 按逗号输入。
var gateLabels = []string{
	"interval_ms", "sem_wait_ms", "queue_wait_ms",
	"concurrency_lock_ms", "quota_lock_ms", "breaker_lock_ms", "retry_backoff_ms",
}

// gateStep 是 +/- 一次调整的步长 (退避序列返回 0 = 不用加减键)。
func gateStep(i int) int {
	switch i {
	case 0:
		return 100
	case 1, 2:
		return 500
	case 3, 4, 5:
		return 60000
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
	}
	return 0
}

// gateText 是设置页里那一行的值 (退避序列写成逗号串, 与输入格式一致)。
func (e *tuiEditor) gateText(i int) string {
	if i == len(gateLabels)-1 {
		parts := make([]string, 0, len(e.gate.RetryBackoffMs))
		for _, v := range e.gate.RetryBackoffMs {
			parts = append(parts, strconv.Itoa(v))
		}
		return strings.Join(parts, ",") + " ms"
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
	}
	e.gateDirty = true
	e.message = gateLabels[i] + " = " + strconv.Itoa(n)
}

// marshalConfig 按固定的顶层顺序写回配置文件: 手写 JSON 是因为 map 会把键排序,
// 让整份配置产生无关的 diff。raw 里没认识的键按字典序补在最后, 不丢数据。
func marshalConfig(top map[string]json.RawMessage) ([]byte, error) {
	known := []string{"listen", "log_path", "timeouts", "gate", "upstreams", "aliases", "probe"}
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

// ---------- store 视图 ----------

func (e *tuiEditor) provEntries() map[string]any {
	if p, ok := e.store["providers"].(map[string]any); ok {
		return p
	}
	p := map[string]any{}
	e.store["providers"] = p
	return p
}

func (e *tuiEditor) provEntry(name string) map[string]any {
	entry, _ := e.provEntries()[name].(map[string]any)
	return entry
}

func modelsOf(entry map[string]any) []map[string]any {
	list, _ := entry["models"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		if obj, ok := m.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

// prioOf 读优先级; 缺省 0 (最小), 负数按 0 算 (与旧脚本一致)。
// int64 是自己刚写进去的 (setPrio), json.Number 是从文件读进来的, 两种都要认。
func prioOf(entry map[string]any) int {
	switch v := entry[prioField].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case int64:
		return int(v)
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func setPrio(entry map[string]any, v int) {
	if v < 0 {
		v = 0
	}
	entry[prioField] = int64(v)
}

func modelID(model map[string]any) string {
	id, _ := model[idField].(string)
	return id
}

// aliasOf: 有条目的 tollgate_group 就用它, 否则用模型 id (与生成器同口径)。
func aliasOf(model map[string]any) string {
	if g, ok := model[groupField].(string); ok && strings.TrimSpace(g) != "" {
		return strings.TrimSpace(g)
	}
	return modelID(model)
}

func isProxyEntry(name string, entry map[string]any) bool {
	if name == proxyName {
		return true
	}
	kind, _ := entry["kind"].(string)
	return kind == proxyKind
}

// configOrder 取配置里的上游顺序 (即生成器的 --providers 次序); 读不到时用兜底列表。
func configOrder(cfgPath string) []string {
	var cfg struct {
		Upstreams []struct {
			ID string `json:"id"`
		} `json:"upstreams"`
	}
	if b, err := os.ReadFile(cfgPath); err == nil && json.Unmarshal(b, &cfg) == nil {
		ids := make([]string, 0, len(cfg.Upstreams))
		for _, u := range cfg.Upstreams {
			ids = append(ids, u.ID)
		}
		if len(ids) > 0 {
			return ids
		}
	}
	return prioFallbackProviders
}

// providerOrder 取配置里的上游顺序; 只留 store 里真有的, 并把本代理自己排除掉。
func (e *tuiEditor) providerOrder() []string {
	names := configOrder(e.config)
	entries := e.provEntries()
	var out []string
	for _, name := range names {
		entry, _ := entries[name].(map[string]any)
		if entry == nil || isProxyEntry(name, entry) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// provPrios 只读 store 里各 provider 的优先级, 给第 1 页上游表排序用。别为了排序去装载
// 整个编辑器: 那会把 settings 页的 gate 快照提前钉在启动时刻。读不到返回 nil。
func provPrios(cfgPath string) map[string]int {
	b, err := os.ReadFile(storePathFor(cfgPath))
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top map[string]any
	if dec.Decode(&top) != nil {
		return nil
	}
	entries, _ := top["providers"].(map[string]any)
	out := make(map[string]int, len(entries))
	for name, raw := range entries {
		if entry, ok := raw.(map[string]any); ok {
			out[name] = prioOf(entry)
		}
	}
	return out
}

// provRank 是第 1 页上游表的排序键: [优先级, 配置顺序], 优先级降序, 同优先级保持配置顺序,
// 与 settings 页的 provider 面板同一口径。store 读不到时返回 nil (调用方保持观测面原序)。
func provRank(cfgPath string) map[string][2]int {
	prios := provPrios(cfgPath)
	if prios == nil {
		return nil
	}
	rank := make(map[string][2]int, len(prios))
	for i, name := range configOrder(cfgPath) {
		rank[name] = [2]int{prios[name], i}
	}
	// store 里有、配置里没有的上游: 优先级照算, 顺序排在同优先级最后。
	last := len(rank)
	for name, p := range prios {
		if _, ok := rank[name]; !ok {
			rank[name] = [2]int{p, last}
		}
	}
	return rank
}

// droplist 读黑名单 (精确匹配模型 id); store 目录优先, 其次配置目录。
func (e *tuiEditor) droplist() map[string]bool {
	for _, base := range []string{filepath.Dir(e.path), filepath.Dir(e.config)} {
		b, err := os.ReadFile(filepath.Join(base, "llm-models-droplist.json"))
		if err != nil {
			continue
		}
		var list []string
		if json.Unmarshal(b, &list) != nil {
			continue
		}
		out := make(map[string]bool, len(list))
		for _, id := range list {
			out[id] = true
		}
		return out
	}
	return map[string]bool{}
}

// buildAliases 复刻生成器的归并与排序, 右栏预览与落进配置的候选顺序完全一致。
func (e *tuiEditor) buildAliases() []tuiAlias {
	dropped := e.droplist()
	type item struct {
		rank int
		cand tuiPrioCand
	}
	byAlias := map[string][]item{}
	for rank, name := range e.providers {
		entry := e.provEntry(name)
		if entry == nil {
			continue
		}
		provPrio := prioOf(entry)
		for _, model := range modelsOf(entry) {
			id := modelID(model)
			if id == "" || dropped[id] {
				continue
			}
			alias := aliasOf(model)
			byAlias[alias] = append(byAlias[alias], item{rank, tuiPrioCand{name, id, prioOf(model), provPrio}})
		}
	}
	names := make([]string, 0, len(byAlias))
	for name := range byAlias {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]tuiAlias, 0, len(names))
	for _, name := range names {
		items := byAlias[name]
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.cand.modelPrio != b.cand.modelPrio {
				return a.cand.modelPrio > b.cand.modelPrio
			}
			if a.cand.provPrio != b.cand.provPrio {
				return a.cand.provPrio > b.cand.provPrio
			}
			if a.rank != b.rank {
				return a.rank < b.rank
			}
			return a.cand.modelID < b.cand.modelID
		})
		alias := tuiAlias{name: name}
		for _, it := range items {
			alias.cands = append(alias.cands, it.cand)
		}
		out = append(out, alias)
	}
	return out
}

// rebuild 重算列表 (别名升序, 别名内按候选顺序), 并把光标钉回原来那一行 —— 调优先级
// 会让行序实时变化, 不钉住的话光标会跳到别人身上。
func (e *tuiEditor) rebuild() {
	if e.prevKey == "" {
		if row := e.selected(); row != nil {
			e.prevKey = row.provider + "/" + modelID(row.model)
		}
	}
	e.order = e.order[:0]
	for _, alias := range e.buildAliases() {
		for _, c := range alias.cands {
			entry := e.provEntry(c.provider)
			for _, model := range modelsOf(entry) {
				if modelID(model) == c.modelID {
					e.order = append(e.order, tuiPrioRow{alias.name, c.provider, model, entry})
					break
				}
			}
		}
	}
	if e.prevKey != "" {
		for i, row := range e.order {
			if row.provider+"/"+modelID(row.model) == e.prevKey {
				e.modelSel = i
				break
			}
		}
	}
	rows := e.filtered()
	if len(rows) == 0 {
		e.modelSel = 0
	} else if e.modelSel >= len(rows) {
		e.modelSel = len(rows) - 1
	}
	if e.modelSel < 0 {
		e.modelSel = 0
	}
	if e.provSel >= len(e.providers) {
		e.provSel = max(0, len(e.providers)-1)
	}
}

func (e *tuiEditor) filtered() []tuiPrioRow {
	if e.filter == "" {
		return e.order
	}
	var out []tuiPrioRow
	for _, row := range e.order {
		if strings.Contains(row.alias, e.filter) ||
			strings.Contains(row.provider, e.filter) ||
			strings.Contains(modelID(row.model), e.filter) {
			out = append(out, row)
		}
	}
	return out
}

func (e *tuiEditor) selected() *tuiPrioRow {
	rows := e.filtered()
	if len(rows) == 0 {
		return nil
	}
	i := clampInt(e.modelSel, 0, len(rows)-1)
	return &rows[i]
}

// candsFor 取某别名的候选顺序 (右栏预览)。
func (e *tuiEditor) candsFor(alias string) []tuiPrioCand {
	for _, a := range e.buildAliases() {
		if a.name == alias {
			return a.cands
		}
	}
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- 动作 ----------

func (e *tuiEditor) markDirty() {
	e.dirty = true
	e.asked = false
	e.rebuild()
}

func (e *tuiEditor) adjust(delta int) {
	if e.focus == 2 {
		e.adjustGate(delta)
		return
	}
	if !e.usable() {
		return
	}
	if e.focus == 1 {
		if len(e.providers) == 0 {
			return
		}
		name := e.providers[clampInt(e.provSel, 0, len(e.providers)-1)]
		entry := e.provEntry(name)
		setPrio(entry, prioOf(entry)+delta)
		e.message = name + ": provider 优先级 = " + strconv.Itoa(prioOf(entry))
		e.sortProviders()
	} else {
		row := e.selected()
		if row == nil {
			return
		}
		e.prevKey = row.provider + "/" + modelID(row.model)
		setPrio(row.model, prioOf(row.model)+delta)
		e.message = row.provider + "/" + modelID(row.model) + ": 模型优先级 = " + strconv.Itoa(prioOf(row.model))
	}
	e.markDirty()
}

func (e *tuiEditor) setValue(v int) {
	if !e.usable() {
		return
	}
	if e.focus == 1 {
		if len(e.providers) == 0 {
			return
		}
		entry := e.provEntry(e.providers[clampInt(e.provSel, 0, len(e.providers)-1)])
		setPrio(entry, v)
		e.message = "provider 优先级 = " + strconv.Itoa(prioOf(entry))
		e.sortProviders()
	} else {
		row := e.selected()
		if row == nil {
			return
		}
		e.prevKey = row.provider + "/" + modelID(row.model)
		setPrio(row.model, v)
		e.message = row.provider + "/" + modelID(row.model) + ": 模型优先级 = " + strconv.Itoa(prioOf(row.model))
	}
	e.markDirty()
}

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
// 走 deploy/tollgate.sh hup 发 SIGHUP —— 与旧 set_priority.ts 的语义一致。
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

// ---------- 键位 ----------

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
		// 有未保存改动先问一次; 再按一次 q 就是确认放弃 (与旧脚本一致)。
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

// ---------- 渲染 ----------

// drawSettings 画设置页: 左栏调度参数 (gate) + provider 面板 + 模型列表, 右栏候选顺序
// 预览, 底部消息行。列宽按内容算, 右栏一直顶到终端右边界; 不补装饰空行, 短的一栏就
// 让右侧继续显示。
func (m *tuiModel) drawSettings(c *tuiCanvas, body int) {
	e := m.prio
	if e == nil {
		c.line(tuiIndent + tuiYell + "设置页未装载" + tuiReset)
		return
	}
	gateLines := 1 + len(gateLabels)
	provH := 0
	if e.usable() {
		// 上游列表优先显示全 (通常就 4-6 个), 剩下的行给模型列表。
		provH = min(len(e.providers), max(1, body-gateLines-7))
	}
	// 段与段之间留一个空行: gate / Providers / 模型。
	visible := max(1, body-gateLines-provH-5)
	rows := e.filtered()
	if len(rows) == 0 {
		visible = 0
	} else {
		visible = min(visible, len(rows))
	}
	e.vis = max(1, visible)
	if e.focus == 0 && len(rows) > 0 {
		e.modelSel = clampInt(e.modelSel, 0, len(rows)-1)
		if e.modelSel < e.top {
			e.top = e.modelSel
		}
		if e.modelSel >= e.top+visible {
			e.top = e.modelSel - visible + 1
		}
	}
	if e.top > max(0, len(rows)-1) {
		e.top = max(0, len(rows)-1)
	}

	left := make([]string, 0, body)
	focusGate := ""
	if e.focus == 2 {
		focusGate = " (焦点)"
	}
	left = append(left, tuiBold+"调度参数 (gate)"+focusGate+tuiReset)
	for i := range gateLabels {
		text := tuiIndent + gateLabels[i] + "  " + e.gateText(i)
		if e.focus == 2 && i == e.gateSel {
			text = tuiRev + text + tuiReset
		}
		left = append(left, text)
	}
	left = append(left, "")
	if !e.usable() {
		left = append(left, tuiYell+"store 读不到: "+e.err+tuiReset)
	} else {
		left = append(left, tuiBold+e.provTitle()+tuiReset)
		for i, name := range e.providers {
			if i >= provH {
				break
			}
			attr := ""
			if e.focus == 1 && i == e.provSel {
				attr = tuiRev
			}
			left = append(left, attr+tuiIndent+name+"  priority="+strconv.Itoa(prioOf(e.provEntry(name)))+tuiReset)
		}
		left = append(left, "")
		hidden := len(e.droplist())
		left = append(left, tuiBold+e.listTitle(len(rows), hidden)+tuiReset)
		for i := e.top; i < min(len(rows), e.top+visible); i++ {
			row := rows[i]
			attr := ""
			if e.focus == 0 && i == e.modelSel {
				attr = tuiRev
			}
			text := tuiIndent + "M" + strconv.Itoa(prioOf(row.model)) + " P" + strconv.Itoa(prioOf(row.entry)) +
				" " + row.provider + "/" + modelID(row.model)
			left = append(left, attr+text+tuiReset)
		}
	}

	right := make([]string, 0, body)
	right = append(right, tuiBold+tuiIndent+"候选顺序预览"+tuiReset)
	if row := e.selected(); row != nil {
		cands := e.candsFor(row.alias)
		right = append(right, tuiBold+tuiIndent+row.alias+" ("+strconv.Itoa(len(cands))+" 候选)"+tuiReset)
		for i, cand := range cands {
			attr := ""
			if cand.provider == row.provider && cand.modelID == modelID(row.model) {
				attr = tuiRev
			}
			text := tuiIndent + strconv.Itoa(i+1) + ". " + cand.provider + "/" + cand.modelID +
				"  M" + strconv.Itoa(cand.modelPrio) + " P" + strconv.Itoa(cand.provPrio)
			right = append(right, attr+text+tuiReset)
		}
	}

	// 左右宽度都按内容算; 右栏吃掉剩余宽度, 一直顶到终端右边界。
	leftW := 32
	for _, s := range left {
		leftW = max(leftW, tuiWidth(s)+1)
	}
	leftW = min(leftW, max(24, m.cols-16))
	rightW := max(8, m.cols-leftW-1)

	n := max(len(left), len(right))
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		c.line(tuiPad(l, leftW) + tuiPad(r, rightW))
	}
	c.line(m.prioMessage(e))
}

func (e *tuiEditor) provTitle() string {
	focus := ""
	if e.focus == 1 {
		focus = " (焦点)"
	}
	return tuiIndent + "Providers" + focus
}

func (e *tuiEditor) listTitle(shown, hidden int) string {
	focus := ""
	if e.focus == 0 {
		focus = " (焦点)"
	}
	filter := e.filter
	if filter == "" {
		filter = "-"
	}
	return tuiIndent + "模型 (过滤: " + filter + ", " + strconv.Itoa(shown) + " 个, 黑名单剔除 " +
		strconv.Itoa(hidden) + ")" + focus
}

// prioMessage 拼底部消息行 (输入态、未保存提醒、错误都在这一行)。
func (m *tuiModel) prioMessage(e *tuiEditor) string {
	text := e.message
	if e.prompt != nil {
		text = e.prompt.label + " " + e.prompt.buffer
	}
	if e.dirty {
		text += " [优先级未保存]"
	}
	if e.gateDirty {
		text += " [调度参数未保存]"
	}
	if e.gateErr != "" {
		text += " 配置读不到: " + e.gateErr
	}
	if e.err != "" {
		text += " store 读不到: " + e.err
	}
	return tuiRev + tuiPad(tuiIndent+text+" ", m.cols-1) + tuiReset
}
