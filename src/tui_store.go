// tui_store.go: 模型列表 store 与权重 (config) 读取。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

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

// prioOf 读优先级; 缺省 0 (最小); 负值 = 停用: 模型级负值停用该模型 (生成配置时跳过,
// settings 页该行仍显示, 可调回非负), provider 级负值停用整个 provider (同样跳生成,
// provider 面板仍显示, 可调回非负)。
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

// setPrio 写优先级; 下限 -1 (停用): 比 -1 更小的值一律钳到 -1, 不再往下减。
func setPrio(entry map[string]any, v int) {
	if v < -1 {
		v = -1
	}
	entry[prioField] = int64(v)
}

func modelID(model map[string]any) string {
	id, _ := model[idField].(string)
	return id
}

// aliasOf: 有条目的 group 就用它, 否则用模型 id (与生成器同口径)。
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
	var names []string
	if e.cfg != nil {
		if list, ok := e.cfg["upstreams"].([]any); ok {
			for _, raw := range list {
				if up, ok := raw.(map[string]any); ok {
					if id, ok := up["id"].(string); ok && id != "" {
						names = append(names, id)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		names = prioFallbackProviders
	}
	storeProvs, _ := e.store["providers"].(map[string]any)
	var out []string
	for _, name := range names {
		entry, _ := storeProvs[name].(map[string]any)
		if entry == nil || isProxyEntry(name, entry) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// provPrios 只读配置里各上游的优先级, 给第 1 页上游表排序用。别为了排序去装载整个
// 编辑器: 那会把 settings 页的 gate 快照提前钉在启动时刻。读不到返回 nil。
// 权重 (priority) 在 config.local.json 的 upstreams 上, 不再读 store。
func provPrios(cfgPath string) map[string]int {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top map[string]any
	if dec.Decode(&top) != nil {
		return nil
	}
	list, _ := top["upstreams"].([]any)
	out := make(map[string]int, len(list))
	for _, raw := range list {
		if up, ok := raw.(map[string]any); ok {
			if id, ok := up["id"].(string); ok && id != "" {
				out[id] = prioOf(up)
			}
		}
	}
	return out
}

// provRank 是第 1 页上游表的排序键: [优先级, 配置顺序], 优先级降序, 同优先级保持配置顺序,
// 与 settings 页的 provider 面板同一口径。配置读不到时返回 nil (调用方保持观测面原序)。
func provRank(cfgPath string) map[string][2]int {
	prios := provPrios(cfgPath)
	if prios == nil {
		return nil
	}
	rank := make(map[string][2]int, len(prios))
	for i, name := range configOrder(cfgPath) {
		if _, ok := prios[name]; ok {
			rank[name] = [2]int{prios[name], i}
		}
	}
	// 配置里有优先级、但顺序表里没有 (理论不该有, 兜底): 顺序排在同优先级最后。
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
