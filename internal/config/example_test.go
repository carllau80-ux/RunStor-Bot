package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExampleConfigLoads 守住"随包发布的示例配置一定加载得起来"。
//
// 这条真的坏过: rawServer 漏了 language 与 kb 两个子表, 而未知键是硬错误 ——
// 于是 config.example.json 一用就启动失败, server.kb.* 只能靠环境变量设。
// 示例配置是运维最先接触的东西, 它坏了等于产品坏了, 所以用测试盯住。
func TestExampleConfigLoads(t *testing.T) {
	// 先取绝对路径: 下面要切工作目录 (避免示例里的相对默认值落到仓库里)
	path, err := filepath.Abs(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatalf("解析示例配置路径失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("读不到示例配置: %v", err)
	}
	clearEnv(t)
	t.Chdir(t.TempDir())

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("示例配置加载失败: %v", err)
	}

	// 关键: 这张表里的每一项都必须真的被解析进来。
	// 只断言"没报错"是不够的 —— 漏了字段的结构体会让解析结果静默为空。
	if cfg.Server.Port != 8081 {
		t.Errorf("server.port 未生效: %d", cfg.Server.Port)
	}
	if cfg.Server.Language != "zh" {
		t.Errorf("server.language 未生效: %q", cfg.Server.Language)
	}
	if !cfg.Server.KB.KBEnabled() {
		t.Error("server.kb.enabled 未生效")
	}
	if cfg.Server.KB.TopK != 5 {
		t.Errorf("server.kb.top_k 未生效: %d", cfg.Server.KB.TopK)
	}
	if cfg.Server.KB.MaxDocumentBytes != 2097152 {
		t.Errorf("server.kb.max_document_bytes 未生效: %d", cfg.Server.KB.MaxDocumentBytes)
	}
	if cfg.Server.Inspect.HostKeyPolicy != "tofu" {
		t.Errorf("server.inspect.host_key_policy 未生效: %q", cfg.Server.Inspect.HostKeyPolicy)
	}
	if cfg.Server.Inspect.Concurrency != 4 {
		t.Errorf("server.inspect.concurrency 未生效: %d", cfg.Server.Inspect.Concurrency)
	}
	if cfg.Server.Catalog.APIKeyEnv != "BRA_CATALOG_KEY" {
		t.Errorf("server.catalog.api_key_env 未生效: %q", cfg.Server.Catalog.APIKeyEnv)
	}
	if cfg.DefaultProfile != "deepseek" {
		t.Errorf("llm.default_profile 未生效: %q", cfg.DefaultProfile)
	}
	for _, name := range []string{"azure", "deepseek", "gateway", "kimi", "local", "qwen"} {
		if cfg.FindProfile(name) == nil {
			t.Errorf("示例里的 profile %q 没被解析出来", name)
		}
	}
	// 示例必须自洽: 至少有一个"按配置就算可用"的 profile (示例里的 local/ollama
	// 不需要密钥), 这样界面上的模型面板不会是一张全红表。
	// 注意这不等于"开箱就是 llm 模式": 显式指定的 default_profile=deepseek 没配 key,
	// 服务会照常起在 demo 模式 —— 那是预期行为, 不是配置错误。
	if !cfg.HasUsableProfile() {
		t.Errorf("示例配置应至少有一个按配置可用的 profile (本地预设无需密钥): %v", cfg.Warnings)
	}
}

// 示例配置里不能出现明文密钥 —— 它是要入库、要被人复制传播的文件。
func TestExampleConfigHasNoInlineSecrets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatalf("读不到示例配置: %v", err)
	}
	// 只看被赋值的 api_key (api_key_env / api_key_file 是引用, 允许)
	inline := regexp.MustCompile(`(?m)^\s*api_key\s*=\s*"([^"]*)"`)
	for _, match := range inline.FindAllStringSubmatch(string(data), -1) {
		if match[1] != "" {
			t.Errorf("示例配置里出现了内联明文密钥: %s", match[0])
		}
	}
	// 顺带守住: 示例里的凭据项都必须走 *_env / *_file 引用
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "api_key_") && !strings.Contains(trimmed, "=") {
			t.Errorf("凭据项写法可疑: %s", trimmed)
		}
	}
}

// 未知键的报错必须**点名是哪个键** —— 只说"配置有错"等于让人自己找。
func TestUnknownKeyErrorNamesTheKey(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[server]
port = 8081
portt = 8082
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("未知键应报错")
	}
	if !strings.Contains(err.Error(), "portt") {
		t.Errorf("报错应点名未知的键: %v", err)
	}
}

// 语言写错要当场报错, 不能静默退回中文 —— 否则"我明明配了英文"没人查得出来。
func TestInvalidLanguageRejected(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "[server]\nlanguage = \"jp\"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("不支持的语言应报错")
	}

	for _, value := range []string{"zh-CN", "zh", "en-US", "en"} {
		valid := writeConfig(t, "[server]\nlanguage = \""+value+"\"\n")
		cfg, err := Load(valid)
		if err != nil {
			t.Errorf("%s 应被接受: %v", value, err)
			continue
		}
		if cfg.Server.Language != "zh" && cfg.Server.Language != "en" {
			t.Errorf("%s 未归一成 zh/en: %q", value, cfg.Server.Language)
		}
	}
}

// 密钥文件不存在 = "还没准备好", 应当降级 (profile 不可用、服务照常起),
// 而不是让整个服务起不来 —— 部署时序上"先起服务、后放密钥"很常见。
func TestMissingKeyFileDegradesInsteadOfFailing(t *testing.T) {
	clearEnv(t)
	missing := filepath.Join(t.TempDir(), "not-there.key")
	path := writeConfig(t, `
[llm.profiles.gw]
preset = "deepseek"
api_key_file = "`+missing+`"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("密钥文件还不存在不该拦住启动: %v", err)
	}
	profile := cfg.FindProfile("gw")
	if profile == nil {
		t.Fatal("未找到 profile gw")
	}
	if profile.Available() {
		t.Error("密钥文件缺失时不该判为可用")
	}
	if !strings.Contains(profile.KeySource, "不存在") {
		t.Errorf("密钥来源应标明文件不存在, 便于排查: %q", profile.KeySource)
	}
}
