// tui_priority.go: priority 页 —— 调 tollgate_priority 的编辑器 (原 scripts/set_priority.ts
// 的 Go 版, 与状态台合成一个界面)。
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

// tuiEditor 是 priority 页的全部状态。
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
	focus    int  // 0 = 模型, 1 = provider
	provSel  int
	modelSel int
	top      int
	vis      int // 模型列表当前能显示的行数 (翻页步长, draw 里刷新)
	filter   string
	message  string
	prompt   *tuiPrompt
	err      string
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
	e.rebuild()
	return nil
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

// providerOrder 取配置里的上游顺序; 只留 store 里真有的, 并把本代理自己排除掉。
func (e *tuiEditor) providerOrder() []string {
	names := prioFallbackProviders
	if b, err := os.ReadFile(e.config); err == nil {
		var cfg struct {
			Upstreams []struct {
				ID string `json:"id"`
			} `json:"upstreams"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			var ids []string
			for _, u := range cfg.Upstreams {
				ids = append(ids, u.ID)
			}
			if len(ids) > 0 {
				names = ids
			}
		}
	}
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
	if e == nil || !e.usable() {
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

// key 处理 priority 页的一个按键; 返回 true = 请求退出 (未保存时先问一次)。
func (e *tuiEditor) key(b byte) (quit bool, handled bool) {
	if e.prompt != nil {
		return false, e.promptKey(b)
	}
	if !e.usable() {
		return false, false
	}
	switch b {
	case 'q', 0x03, 0x04:
		// 有未保存改动先问一次; 再按一次 q 就是确认放弃 (与旧脚本一致)。
		if e.dirty && !e.asked {
			e.message = "有未保存改动: 再按 q 放弃退出, 或按 s 保存"
			e.asked = true
			return false, true
		}
		return true, true
	case '\t':
		e.focus = 1 - e.focus
		return false, true
	case 'm':
		e.focus = 0 // m = 切到模型 (改模型优先级)
		return false, true
	case 'p':
		e.focus = 1 // p = 切到 provider (改 provider 优先级)
		return false, true
	case 'j':
		e.moveFocus(1)
	case 'k':
		e.moveFocus(-1)
	case 'g':
		e.jump(0)
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
	if e.focus == 1 {
		e.provSel = clampInt(e.provSel+d, 0, max(0, len(e.providers)-1))
		return
	}
	e.modelSel = clampInt(e.modelSel+d, 0, max(0, len(e.filtered())-1))
}

func (e *tuiEditor) jump(to int) {
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

// drawPriority 画 priority 页: 左 provider 面板 + 模型列表, 右候选顺序预览, 底部消息行。
func (m *tuiModel) drawPriority(c *tuiCanvas, body int) {
	e := m.prio
	if e == nil {
		c.line(tuiIndent + tuiYell + "priority 页未装载" + tuiReset)
		return
	}
	leftW := m.cols / 2
	if leftW < 38 {
		leftW = 38
	}
	if leftW > m.cols-2 {
		leftW = m.cols - 2
	}
	rightW := m.cols - leftW - 1
	if rightW < 8 {
		rightW = 8
	}
	rows := e.filtered()
	visible := max(1, body-4)
	e.vis = visible
	if e.focus == 0 {
		e.modelSel = clampInt(e.modelSel, 0, max(0, len(rows)-1))
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
	left = append(left, tuiBold+tuiPad(e.provTitle(), leftW)+tuiReset)
	provH := clampInt(len(e.providers)+1, 2, max(2, body/3))
	for i, name := range e.providers {
		if i+1 >= provH {
			break
		}
		attr := ""
		if e.focus == 1 && i == e.provSel {
			attr = tuiRev
		}
		left = append(left, attr+tuiPad(tuiIndent+name+"  priority="+strconv.Itoa(prioOf(e.provEntry(name))), leftW)+tuiReset)
	}
	for len(left) < provH {
		left = append(left, strings.Repeat(" ", leftW))
	}
	hidden := len(e.droplist())
	left = append(left, tuiBold+tuiPad(e.listTitle(len(rows), hidden), leftW)+tuiReset)
	for i := e.top; i < min(len(rows), e.top+visible); i++ {
		row := rows[i]
		attr := ""
		if e.focus == 0 && i == e.modelSel {
			attr = tuiRev
		}
		text := tuiIndent + "M" + strconv.Itoa(prioOf(row.model)) + " P" + strconv.Itoa(prioOf(row.entry)) +
			" " + row.provider + "/" + modelID(row.model)
		left = append(left, attr+tuiPad(text, leftW)+tuiReset)
	}
	for len(left) < body-1 {
		left = append(left, strings.Repeat(" ", leftW))
	}

	right := make([]string, 0, body)
	right = append(right, tuiBold+tuiPad(tuiIndent+"候选顺序预览", rightW)+tuiReset)
	if row := e.selected(); row != nil {
		cands := e.candsFor(row.alias)
		right = append(right, tuiBold+tuiPad(tuiIndent+row.alias+" ("+strconv.Itoa(len(cands))+" 候选)", rightW)+tuiReset)
		for i, cand := range cands {
			attr := ""
			if cand.provider == row.provider && cand.modelID == modelID(row.model) {
				attr = tuiRev
			}
			text := tuiIndent + strconv.Itoa(i+1) + ". " + cand.provider + "/" + cand.modelID +
				"  M" + strconv.Itoa(cand.modelPrio) + " P" + strconv.Itoa(cand.provPrio)
			right = append(right, attr+tuiPad(text, rightW)+tuiReset)
		}
	}
	for len(right) < body-1 {
		right = append(right, strings.Repeat(" ", rightW))
	}

	for i := 0; i < body-1; i++ {
		c.line(tuiPad(left[i], leftW) + tuiPad(right[i], rightW))
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
		text += " [未保存]"
	}
	if e.err != "" {
		text = "store 读不到: " + e.err
	}
	return tuiRev + tuiPad(tuiIndent+text+" ", m.cols-1) + tuiReset
}
