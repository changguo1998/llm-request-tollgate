// config_env_test.go: key_env 读取口径的回归 (拆分 loadConfigOpt 前的特征测试)。

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// key_env 只存环境变量名; 严格模式缺变量报错, lenient 模式放行 (settings 页离线编辑用),
// 变量存在时写进内存里的 Key (不落盘)。
func TestLoadConfigKeyEnv(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tollgate.json")
	body := `{"upstreams":[{"id":"u1","base_url":"http://x","key_env":"TOLLGATE_TEST_KEY",
	  "models":[{"id":"m1"}]}]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOLLGATE_TEST_KEY", "")
	os.Unsetenv("TOLLGATE_TEST_KEY")
	if _, err := loadConfig(cfg); err == nil || !strings.Contains(err.Error(), "TOLLGATE_TEST_KEY") {
		t.Fatalf("严格模式缺 key_env 应报错: %v", err)
	}
	c, err := loadConfigOpt(cfg, true)
	if err != nil {
		t.Fatalf("lenient 模式应放行: %v", err)
	}
	if got := c.Upstreams[0].Key; got != "" {
		t.Errorf("变量为空时 Key 应留空, 得到 %q", got)
	}

	t.Setenv("TOLLGATE_TEST_KEY", "sk-test")
	c, err = loadConfig(cfg)
	if err != nil {
		t.Fatalf("有变量时应加载成功: %v", err)
	}
	if got := c.Upstreams[0].Key; got != "sk-test" {
		t.Errorf("Key 应取自环境变量, 得到 %q", got)
	}
}
