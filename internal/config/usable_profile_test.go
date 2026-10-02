package config

import (
	"os"
	"path/filepath"
	"testing"
)

// "将退回 demo 客户端"这条提示会过期, 必须按当前状态判断而不是照抄加载时的结论。
//
// 真实症状: 用户按界面引导在网页里填好 key 之后, /health 同时返回
// mode=llm 与 "没有任何可用 profile, 将使用 demo 客户端" —— 面板自相矛盾,
// 用户没法判断到底在用哪个模型。
func TestUsableProfileSeesRuntimeStore(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	runtimePath := filepath.Join(dir, "runtime.json")
	// 配置文件里只有一个配不全的 profile (没有 key 也没有 key 文件)
	raw := `
[server]
host = "127.0.0.1"
port = 8081
runtime_path = "` + runtimePath + `"

[llm]
default_profile = "deepseek"

[llm.profiles.deepseek]
provider = "deepseek"
api_key_env = "BRA_TEST_MISSING_KEY"
`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	t.Setenv("BRA_TEST_MISSING_KEY", "")
	if err := os.WriteFile(runtimePath, []byte(`{"profiles":{"deepseek":{
	  "provider":"deepseek","base_url":"https://api.deepseek.com/v1",
	  "model":"deepseek-chat","api_key":"sk-from-web","source":"runtime"}}}`), 0o600); err != nil {
		t.Fatalf("写运行态失败: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.HasUsableProfile() {
		t.Fatal("网页里配好的 profile 存在运行态, 应算作可用 —— 漏判会导致界面一直提示将退回 demo")
	}
	for _, warning := range cfg.Warnings {
		if warning == NoUsableProfileWarning {
			t.Errorf("有可用 profile 时不该报 %q", NoUsableProfileWarning)
		}
	}
}

// 反过来: 真的没有可用 profile 时, 这条提示必须在。
func TestNoUsableProfileStillWarns(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	raw := `
[server]
host = "127.0.0.1"
port = 8081

[llm.profiles.deepseek]
provider = "deepseek"
api_key_env = "BRA_TEST_MISSING_KEY"
`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	t.Setenv("BRA_TEST_MISSING_KEY", "")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.HasUsableProfile() {
		t.Fatal("没有任何可用 profile")
	}
	found := false
	for _, warning := range cfg.Warnings {
		if warning == NoUsableProfileWarning {
			found = true
		}
	}
	if !found {
		t.Errorf("没有可用 profile 时应提示 %q, 实际 %v", NoUsableProfileWarning, cfg.Warnings)
	}
}
