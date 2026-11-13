// tui_settings.go: settings 页的编辑器模型与存盘。
//
// 左栏是 provider 面板 + 模型列表 (别名 -> 候选), 右栏是所选别名的候选顺序实时预览:
// 候选顺序 = 模型优先级降序 -> provider 优先级降序 -> 上游顺序 -> 模型 id, 与配置生成器
// (scripts/gen_tollgate_config.py 的 build_aliases) 同一口径, 所以 s 保存时自动重生成
// 配置, 预览里看到的就是落进配置的顺序。
//
// 权重 (priority / alias / billing) 在 config.local.json 的 upstreams /
// upstreams[].models 上; store (llm-models.json) 只提供模型列表与模型参数。编辑器装载
// 时把两者合并: 模型以 store 为准, 权重以配置为准 (缺省 0 / 模型 id / 无)。保存把
// 编辑态的优先级写回配置的 upstreams, 其余字段原样保留 (2 空格缩进、键按字典序、写
// .tmp 再 rename, 数值走 json.Number 原样过一遍)。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	prioField     = "priority"
	groupField    = "alias" // 模型条的逻辑名 (旧称 group, 随配置迁移改为 alias)
	billingField  = "billing"
	idField       = "id"
	proxyKind     = "proxy"
	proxyName     = "tg" // store 里代表本代理自己的 provider
	legacyGroup   = "group"
	legacyBilling = "billing_group"
)

// prioFallbackProviders 是配置里读不到上游顺序时的兜底 (与生成器的缺省 --providers 一致)。
var prioFallbackProviders = []string{"ustc", "arkcp", "arkap", "group"}

// tuiPrioRow 是模型列表的一行; model / entry 是编辑器合并后的上游条目 (含权重),
// 改它就是改合并视图, 存盘时同步回配置。
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

// feedPrompt 把一个字节喂给行内输入框; 返回 true = 输入结束 (回车已提交 / Esc 取消)。
// 调用方负责清掉自己持有的 prompt (提交回调里可能还要读 buffer)。settings 页与第 2 页
// 的筛选共用这一套输入规则。
func feedPrompt(p *tuiPrompt, b byte) bool {
	switch {
	case b == '\r' || b == '\n':
		p.commit(p.buffer)
		return true
	case b == 0x1b || b == 0x03:
		return true
	case b == 0x7f || b == 0x08:
		if len(p.buffer) > 0 {
			p.buffer = p.buffer[:len(p.buffer)-1]
		}
	case b >= 0x20:
		p.buffer += string(b)
	}
	return false
}

// tuiEditor 是 settings 页的全部状态: 权重 (priority, 写配置文件) + 调度参数 (gate, 写配置文件)。
type tuiEditor struct {
	path   string // store: llm-models.json (模型列表 + 模型参数来源)
	config string // 代理配置: 权重的存处, 也是重生成的目标
	root   string // 仓库根 (生成器与 deploy/tollgate.sh 所在)

	store     map[string]any            // store 原样 (读模型列表)
	cfg       map[string]any            // 配置原样 (写回时保留其余字段)
	merged    map[string]map[string]any // 编辑态: provider -> 合并后的上游条目 (含权重)
	scrollTop int                       // 模型列表的滚动位置 (显示行)

	providers []string // 上游顺序 (配置的 upstreams 顺序, 兜底见 prioFallbackProviders)
	order     []tuiPrioRow
	prevKey   string // 重建列表前选中的行 (provider + "/" + 模型 id), 用来把光标钉在同一行

	dirty    bool
	asked    bool // q 已经提示过一次未保存
	focus    int  // 0 = 模型, 1 = provider, 2 = 调度参数 (gate)
	provSel  int
	modelSel int
	vis      int // 模型列表当前能显示的行数 (翻页步长, draw 里刷新)
	// 组候选行缓存 (只展示组成员, 不参与权重编辑): 按配置 mtime 失效。
	groupCands    []tuiCand
	groupCandsMod time.Time
	filter        string
	message       string
	prompt        *tuiPrompt
	err           string

	gate      GateCfg // 配置文件里的调度参数 (显示用 normGate 补齐后的值)
	gateSel   int
	gateDirty bool
	gateErr   string

	// 锁恢复探测开关 (配置扁平顶层键, 本项目也用 gate 列表编辑)。
	probeOn bool
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
		e.err = "store 读不到: " + err.Error()
		e.message = e.err
		return e
	}
	if e.cfg == nil {
		e.message = e.err
		return e
	}
	e.message = e.path + " | " + strconv.Itoa(len(e.providers)) + " 个 provider"
	return e
}

func (e *tuiEditor) usable() bool { return e.store != nil && e.cfg != nil }

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
	cb, err := os.ReadFile(e.config)
	if err != nil {
		e.err = "配置读不到: " + err.Error()
		return nil // 列表照看, 但不可编辑/保存
	}
	cdec := json.NewDecoder(bytes.NewReader(cb))
	cdec.UseNumber()
	var ctop map[string]any
	if err := cdec.Decode(&ctop); err != nil {
		e.err = "配置解析失败: " + err.Error()
		return nil
	}
	e.cfg = ctop
	e.providers = e.providerOrder()
	e.buildMerged()
	e.sortProviders()
	e.rebuild()
	return nil
}

// cfgUpByID 是配置 upstreams 按 id 的索引, 给合并与同步用。
func (e *tuiEditor) cfgUpByID() map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := e.cfg["upstreams"].([]any)
	for _, raw := range list {
		if up, ok := raw.(map[string]any); ok {
			if id, ok := up["id"].(string); ok && id != "" {
				out[id] = up
			}
		}
	}
	return out
}

// groupInfo 是配置里一个组 (别名的别名) 的只读展示条目: 组名 + 成员逻辑名 (按配置顺序)。
type groupInfo struct {
	name    string
	members []string
}

// groups 读配置的 groups 键 (组名 -> 成员逻辑名列表), 按组名字典序返回;
// 键缺失或类型不对返回 ok=false。组成员在 loadConfigOpt 已校验为已注册逻辑名,
// TUI 只展示, 不重新校验。
func (e *tuiEditor) groups() ([]groupInfo, bool) {
	raw, ok := e.cfg["groups"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]groupInfo, 0, len(names))
	for _, name := range names {
		members := []string{}
		if list, ok := raw[name].([]any); ok {
			for _, m := range list {
				if s, ok := m.(string); ok && s != "" {
					members = append(members, s)
				}
			}
		}
		out = append(out, groupInfo{name: name, members: members})
	}
	return out, true
}

// buildMerged 合并 store 模型列表与配置权重: 每个上游一条合并条目, 模型以 store 为准,
// priority / alias / billing 从配置同名模型继承, 缺省 0 / 模型 id / 无。
func (e *tuiEditor) buildMerged() {
	e.merged = map[string]map[string]any{}
	cfg := e.cfgUpByID()
	storeProvs, _ := e.store["providers"].(map[string]any)
	for _, name := range e.providers {
		entry := map[string]any{}
		if up := cfg[name]; up != nil {
			for k, v := range up {
				entry[k] = v
			}
		}
		entry["id"] = name
		storeEntry, _ := storeProvs[name].(map[string]any)
		models := make([]any, 0, len(modelsOf(storeEntry)))
		for _, m := range modelsOf(storeEntry) {
			id := modelID(m)
			if id == "" {
				continue
			}
			models = append(models, mergedModel(cfg[name], id))
		}
		entry["models"] = models
		e.merged[name] = entry
	}
}

// mergedModel 是某上游单模型的编辑条目: 继承配置同名模型的权重, 缺省 0 / id / 无。
// 旧字段 group / billing_group 在新字段缺省时回落采用, 随后清掉旧键, 存盘只写
// alias / billing (与生成器同一套规范字段)。
func mergedModel(up map[string]any, id string) map[string]any {
	entry := map[string]any{"id": id}
	if up != nil {
		if list, ok := up["models"].([]any); ok {
			for _, raw := range list {
				if m, ok := raw.(map[string]any); ok && modelID(m) == id {
					for k, v := range m {
						entry[k] = v
					}
					break
				}
			}
		}
	}
	if g, ok := entry[groupField].(string); !ok || strings.TrimSpace(g) == "" {
		if legacy, ok := entry[legacyGroup].(string); ok && strings.TrimSpace(legacy) != "" {
			entry[groupField] = strings.TrimSpace(legacy)
		} else {
			entry[groupField] = id
		}
	}
	if b, ok := entry[billingField].(string); !ok || strings.TrimSpace(b) == "" {
		if legacy, ok := entry[legacyBilling].(string); ok && strings.TrimSpace(legacy) != "" {
			entry[billingField] = strings.TrimSpace(legacy)
		}
	}
	delete(entry, legacyGroup)
	delete(entry, legacyBilling)
	if _, ok := entry[prioField]; !ok {
		entry[prioField] = int64(0)
	}
	return entry
}

// provEntry 取某 provider 的合并编辑条目 (含权重); 不在合并视图里返回 nil。
func (e *tuiEditor) provEntry(name string) map[string]any {
	return e.merged[name]
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

// syncWeights 把编辑态 (e.merged) 的 upstreams 写回 e.cfg: 每个合并条目原样进数组,
// 顺序 = 当前 provider 顺序 (按优先级排序, 与生成器写上游的顺序一致)。
func (e *tuiEditor) syncWeights() {
	ups := make([]any, 0, len(e.providers))
	for _, name := range e.providers {
		if entry, ok := e.merged[name]; ok {
			ups = append(ups, entry)
		}
	}
	e.cfg["upstreams"] = ups
}

// save 原子写回配置 (先写 .tmp 再 rename); 2 空格缩进, 键字典序 (map 编码的默认行为)。
// 先把编辑态权重同步进 e.cfg, 再以磁盘上的最新配置为底 (避免盖掉刚由 saveGate 写的
// 调度参数), 其余字段原样保留。
func (e *tuiEditor) save() error {
	e.syncWeights()
	top := e.cfg
	if b, err := os.ReadFile(e.config); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var fresh map[string]any
		if dec.Decode(&fresh) == nil {
			fresh["upstreams"] = top["upstreams"]
			top = fresh
		}
	}
	b, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp := e.config + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.config)
}
