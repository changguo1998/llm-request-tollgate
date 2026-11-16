// version.go: 构建版本号 —— 编译期注入优先, 否则回退嵌入的 git 提交 (vcs.revision)。

package main

import (
	"runtime/debug"
)

// version 是构建版本号 (语义化版本, 规则见根目录 AGENTS.md): 提交时同步递增。
// 发布构建可用 -ldflags "-X main.version=…" 覆盖;
// 没注入时 (默认编译 / go test) 直接用这里, 兜底才读二进制嵌入的 git 提交。
var version = "0.2.0"

// buildVersion 组装对外展示的版本串:
//   - 编译期注入了非 dev 版本: 直接用注入值;
//   - 否则用编译进去的版本常量 (var version, 常规构建即 "0.1.0");
//   - 以上都没有 (version 留空被清掉等极端情况): 回退嵌入的 git 提交,
//     构建时工作树有未提交改动 (vcs.modified=true) 时追加 "-dirty";
//   - 都拿不到 (非 git 构建): 返回 dev。
func buildVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return rev
}
