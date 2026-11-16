// version_test.go: 版本号组装 (编译期注入优先, 兜底 buildinfo 的 git 提交)。
package main

import (
	"testing"
)

func TestBuildVersionInjected(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	// 编译期注入 (非 dev) 直接返回, 不再读 buildinfo。
	version = "v1.2.3"
	if got := buildVersion(); got != "v1.2.3" {
		t.Errorf("注入的版本应原样返回, 得到 %q", got)
	}
}

func TestBuildVersionFallback(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	// 没注入 (默认 dev): 兜底到嵌入的 git 提交号; 非 git 构建回退 dev。两者都非空。
	version = "dev"
	if got := buildVersion(); got == "" {
		t.Errorf("兜底版本不应为空")
	}
}
