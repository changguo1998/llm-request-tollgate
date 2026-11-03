// tui_settings.go: settings 页的编辑器模型与存盘。
//
// 左栏是 provider 面板 + 模型列表 (别名 -> 候选), 右栏是所选别名的候选顺序实时预览:
// 候选顺序 = 模型优先级降序 -> provider 优先级降序 -> 上游顺序 -> 模型 id, 与配置生成器
// (scripts/gen_tollgate_config.py 的 build_aliases) 同一口径, 所以调完直接 r 重生成
// 配置, 预览里看到的就是落进配置的顺序。
//
// 改的是模型列表 store (llm-models.json): 4 空格缩进、键按字典序、写 .tmp 再 rename,
// 数值走 json.Number 原样过一遍, 没动过的字段一个都不碰。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strconv"
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

	// 全局区 (gate 列表) 里的扁平顶层窗口参数: 速度窗 / 分块采样时间尺度。
	rateWinMs  int
	blockWinMs int
}

// storePathFor 定位模型列表: 先看配置实文件所在目录 (config.local.json 软链到
// ~/fff/config/tollgate.json, store 就在旁边), 再退回 FFF_HOME / ~/fff。
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

// save 原子写回 store (先写 .tmp 再 rename)。
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
