package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv 清掉会影响用例的环境变量。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BRA_CONFIG", "BRA_API_KEY", "BRA_BASE_URL", "BRA_MODEL",
		"BRA_PROFILE", "BRA_PORT", "BRA_HOST", "BRA_TEST_KEY",
	} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
	return path
}

func TestLoadWithoutConfigFallsBackToDefaults(t *testing.T) {
	clearEnv(t)
	// 切到临时目录, 避免读到仓库里的 config.toml
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("缺少配置文件不该报错: %v", err)
	}
	if cfg.Path != "" {
		t.Errorf("不该加载到配置文件: %s", cfg.Path)
	}
	if cfg.Server.Port != 8081 {
		t.Errorf("默认端口应为 8081, 实际 %d", cfg.Server.Port)
	}
	if cfg.HasUsableProfile() {
		t.Error("无 key 时不该有可用 profile")
	}
	if len(cfg.Warnings) == 0 {
		t.Error("应提示进入了 demo 模式")
	}
}

// 预设的价值: 不用手抄 base_url 与模型名。
func TestPresetFillsBaseURLAndModel(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm]
default_profile = "ds"

[llm.profiles.ds]
preset = "deepseek"
api_key_env = "BRA_TEST_KEY"
`)
	t.Setenv("BRA_TEST_KEY", "sk-test-123")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	profile := cfg.FindProfile("ds")
	if profile == nil {
		t.Fatal("未找到 profile ds")
	}
	if profile.BaseURL != "https://api.deepseek.com" {
		t.Errorf("base_url 未从预设带出: %q", profile.BaseURL)
	}
	if profile.Model != "deepseek-chat" {
		t.Errorf("model 未从预设带出: %q", profile.Model)
	}
	if !profile.Available() {
		t.Errorf("应可用: %s", profile.UnavailableReason())
	}
	if profile.KeySource != "env:BRA_TEST_KEY" {
		t.Errorf("密钥来源标注不对: %q", profile.KeySource)
	}
}

func TestExplicitFieldsOverridePreset(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.custom]
preset = "deepseek"
model = "deepseek-reasoner"
base_url = "https://gw.internal/v1"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	profile := cfg.FindProfile("custom")
	if profile.Model != "deepseek-reasoner" {
		t.Errorf("显式 model 应覆盖预设: %q", profile.Model)
	}
	if profile.BaseURL != "https://gw.internal/v1" {
		t.Errorf("显式 base_url 应覆盖预设: %q", profile.BaseURL)
	}
}

func TestUnknownPresetIsRejected(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.x]
preset = "no-such-preset"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("未知 preset 应报错")
	} else if !strings.Contains(err.Error(), "no-such-preset") {
		t.Errorf("报错应指明 preset 名: %v", err)
	}
}

// 本地模型通常不需要 key, 不该因此被判定为不可用。
func TestLocalProfileNeedsNoKey(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.local]
preset = "ollama"
model = "qwen2.5:14b"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	profile := cfg.FindProfile("local")
	if profile.RequiresKey {
		t.Error("ollama 预设不该要求 key")
	}
	if !profile.Available() {
		t.Errorf("本地 profile 应可用: %s", profile.UnavailableReason())
	}
	if cfg.DefaultProfile != "local" {
		t.Errorf("无显式 default_profile 时应自动选中可用项, 实际 %q", cfg.DefaultProfile)
	}
}

// 明文密钥只允许本地端点 —— 否则密钥会随配置文件扩散。
func TestInlineKeyRejectedForRemoteEndpoint(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.bad]
preset = "deepseek"
api_key = "sk-plain-text"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("远程端点配明文密钥应报错")
	}
	if !strings.Contains(err.Error(), "api_key_env") {
		t.Errorf("报错应指引改用环境变量: %v", err)
	}
}

func TestInlineKeyAllowedForLocalEndpoint(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.local]
preset = "ollama"
api_key = "ollama"
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("本地端点允许内联密钥: %v", err)
	}
}

// 密钥文件权限过宽必须拒绝: 那个文件已经等于泄露了。
func TestKeyFileRequires0600(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("sk-from-file"), 0o644); err != nil {
		t.Fatalf("写密钥文件失败: %v", err)
	}
	path := writeConfig(t, `
[llm.profiles.p]
preset = "deepseek"
api_key_file = "`+keyPath+`"
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("0644 的密钥文件应被拒绝")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("报错应给出修复命令: %v", err)
	}
}

func TestKeyFileAcceptedAt0600(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("  sk-from-file\n"), 0o600); err != nil {
		t.Fatalf("写密钥文件失败: %v", err)
	}
	path := writeConfig(t, `
[llm.profiles.p]
preset = "deepseek"
api_key_file = "`+keyPath+`"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("0600 密钥文件应被接受: %v", err)
	}
	profile := cfg.FindProfile("p")
	if profile.APIKey != "sk-from-file" {
		t.Errorf("密钥应被去掉首尾空白: %q", profile.APIKey)
	}
}

func TestEmptyKeyFileIsRejected(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写密钥文件失败: %v", err)
	}
	path := writeConfig(t, `
[llm.profiles.p]
preset = "deepseek"
api_key_file = "`+keyPath+`"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("空密钥文件应被拒绝")
	}
}

// 环境变量兜底 profile: 不写配置文件也能接模型。
func TestEnvProfileFallback(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("BRA_API_KEY", "sk-from-env")
	t.Setenv("BRA_BASE_URL", "https://api.example.com/v1")
	t.Setenv("BRA_MODEL", "my-model")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	profile := cfg.FindProfile("env")
	if profile == nil {
		t.Fatal("应生成 env profile")
	}
	if profile.APIKey != "sk-from-env" || profile.Model != "my-model" {
		t.Errorf("env profile 内容不符: %+v", profile)
	}
	if !profile.Available() {
		t.Error("env profile 应可用")
	}
	if cfg.DefaultProfile != "env" {
		t.Errorf("应自动选中 env profile, 实际 %q", cfg.DefaultProfile)
	}
}

// 显式配置优先于环境变量, 不该被覆盖。
func TestExplicitProfileBeatsEnvProfile(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.env]
preset = "deepseek"
api_key_env = "BRA_TEST_KEY"
`)
	t.Setenv("BRA_TEST_KEY", "sk-explicit")
	t.Setenv("BRA_API_KEY", "sk-env-should-lose")
	t.Setenv("BRA_BASE_URL", "https://should-lose.example.com")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	profile := cfg.FindProfile("env")
	if profile.BaseURL != "https://api.deepseek.com" {
		t.Errorf("配置文件里的 profile 不该被环境变量覆盖: %q", profile.BaseURL)
	}
	if profile.APIKey != "sk-explicit" {
		t.Errorf("密钥不符: %q", profile.APIKey)
	}
}

func TestUnknownDefaultProfileFallsBack(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm]
default_profile = "ghost"

[llm.profiles.real]
preset = "ollama"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.DefaultProfile != "real" {
		t.Errorf("应回退到可用 profile, 实际 %q", cfg.DefaultProfile)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "ghost") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示回退原因: %v", cfg.Warnings)
	}
}

func TestServerPortOverrideFromEnv(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[server]
port = 8081
`)
	t.Setenv("BRA_PORT", "8095")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.Port != 8095 {
		t.Errorf("环境变量应覆盖端口, 实际 %d", cfg.Server.Port)
	}
}

func TestInvalidPortRejected(t *testing.T) {
	clearEnv(t)
	for _, body := range []string{
		"[server]\nport = 0",
		"[server]\nport = 70000",
		"[server]\nport = \"abc\"", // TOML 类型不匹配
	} {
		path := writeConfig(t, body)
		if _, err := Load(path); err == nil {
			t.Errorf("非法端口应报错: %s", body)
		}
	}
}

func TestUnknownConfigFieldRejected(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[server]
port = 8081

[llm]
defualt_profile = "typo"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("拼错的字段名应报错, 否则会被静默忽略")
	}
}

func TestSafeViewNeverLeaksKey(t *testing.T) {
	profile := &Profile{
		Name: "p", Provider: ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		APIKey: "sk-super-secret-value", KeySource: "env:X", RequiresKey: true,
	}
	view := profile.SafeView("p")

	if view["has_key"] != true {
		t.Error("应报告已配置 key")
	}
	for name, value := range view {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if strings.Contains(text, "sk-super-secret-value") {
			t.Errorf("字段 %s 泄露了密钥: %q", name, text)
		}
	}
}

func TestUnavailableReasonExplainsWhatIsMissing(t *testing.T) {
	cases := []struct {
		profile Profile
		want    string
	}{
		{Profile{Model: "m", RequiresKey: true}, "base_url"},
		{Profile{BaseURL: "http://x", RequiresKey: true}, "model"},
		{Profile{BaseURL: "http://x", Model: "m", RequiresKey: true}, "Key"},
	}
	for _, entry := range cases {
		reason := entry.profile.UnavailableReason()
		if !strings.Contains(reason, entry.want) {
			t.Errorf("不可用原因应提到 %q, 实际 %q", entry.want, reason)
		}
		if entry.profile.Available() {
			t.Errorf("缺少 %s 时不该判为可用", entry.want)
		}
	}
}

func TestPresetNamesSortedAndComplete(t *testing.T) {
	names := PresetNames()
	if len(names) != len(Presets) {
		t.Fatalf("预设数量不符: %d vs %d", len(names), len(Presets))
	}
	for index := 1; index < len(names); index++ {
		if names[index-1] > names[index] {
			t.Fatalf("预设名未排序: %v", names)
		}
	}
	// 几个关键预设必须在, 否则 README 与界面上的说明会与实现脱节
	for _, want := range []string{"deepseek", "ollama", "openai", "qwen", "azure_openai"} {
		if _, ok := FindPreset(want); !ok {
			t.Errorf("缺少预设 %q", want)
		}
	}
}

func TestMaskHidesMiddle(t *testing.T) {
	if got := Mask(""); got != "(未设置)" {
		t.Errorf("空值脱敏不符: %q", got)
	}
	if got := Mask("short"); strings.Contains(got, "short") {
		t.Errorf("短值应全部打码: %q", got)
	}
	masked := Mask("sk-1234567890abcdef")
	if strings.Contains(masked, "567890") {
		t.Errorf("中间部分应被打码: %q", masked)
	}
	// 更强的一条: 首尾各 4 位也不许出现 —— 曾经就是"首4...末4"的写法, 对短密钥
	// 等于泄露大半, 而日志会进工单、进备份、进别人的终端。
	for _, fragment := range []string{"sk-1", "cdef", "sk-1234567890abcdef"} {
		if strings.Contains(masked, fragment) {
			t.Errorf("脱敏结果不该包含 %q: %q", fragment, masked)
		}
	}
	if masked != "***" {
		t.Errorf("脱敏结果应为定长 ***, 实际 %q", masked)
	}
}

// 配置与 profile 被 fmt 打印时也不能带出密钥 —— 排查问题时顺手 %+v 打一行配置
// 是最常见的泄露路径。
func TestFormattingConfigNeverLeaksKey(t *testing.T) {
	profile := &Profile{
		Name: "p", Provider: ProviderOpenAICompat, BaseURL: "https://api.example.com",
		Model: "m", APIKey: "sk-super-secret-value", KeySource: "env:X", RequiresKey: true,
	}
	cfg := Config{
		Server:         Server{Host: "0.0.0.0", Port: 8081},
		Profiles:       []*Profile{profile},
		DefaultProfile: "p",
		Path:           "/etc/backup-restore-agent/config.toml",
		Runtime: &RuntimeStore{
			data: runtimeData{Profiles: map[string]RuntimeProfile{
				"p": {BaseURL: "https://api.example.com", Model: "m", APIKey: "sk-from-web-secret"},
			}},
		},
	}

	cases := map[string]string{
		"%v profile":  fmt.Sprintf("%v", profile),
		"%+v profile": fmt.Sprintf("%+v", profile),
		"%v config":   fmt.Sprintf("%v", cfg),
		"%+v config":  fmt.Sprintf("%+v", cfg),
		"%v runtime":  fmt.Sprintf("%v", cfg.Runtime),
		"%+v runtime": fmt.Sprintf("%+v", cfg.Runtime),
		"%+v 配置切片":    fmt.Sprintf("%+v", cfg.Profiles),
		"%+v 运行态 map": fmt.Sprintf("%+v", cfg.Runtime.data),
		"%+v 运行态条目":   fmt.Sprintf("%+v", cfg.Runtime.data.Profiles["p"]),
	}
	for name, rendered := range cases {
		if strings.Contains(rendered, "sk-super-secret-value") ||
			strings.Contains(rendered, "sk-from-web-secret") {
			t.Errorf("%s 打印时泄露了密钥: %s", name, rendered)
		}
	}
}

// 守护测试: 每个预设都必须显式声明 RequiresKey。
//
// Go 的 bool 零值是 false, 若靠"不写默认 true"的假设, 漏写就会静默变成
// "该厂商无需密钥" —— 界面会允许切换到无 Key 的云端配置, 直到调用时才报 401。
// 这条断言把这类漏写挡在提交之前。
func TestPresetRequiresKeyExplicitly(t *testing.T) {
	// 明确不需要密钥的只有本地/内网预设
	localOnly := map[string]bool{"ollama": true, "vllm": true, "lmstudio": true}

	for name, preset := range Presets {
		if localOnly[name] {
			if preset.RequiresKey {
				t.Errorf("预设 %s 是本地端点, 不该要求密钥", name)
			}
			continue
		}
		if !preset.RequiresKey {
			t.Errorf("预设 %s 未声明 RequiresKey=true; "+
				"云端厂商必须要求密钥, 否则界面会允许切换到必然失败的配置", name)
		}
	}

	// 本地预设必须指向回环地址 —— 否则"无需密钥"就是把内网服务暴露给整网
	for name := range localOnly {
		preset, ok := FindPreset(name)
		if !ok {
			t.Fatalf("缺少本地预设 %s", name)
		}
		if !strings.Contains(preset.BaseURL, "127.0.0.1") &&
			!strings.Contains(preset.BaseURL, "localhost") {
			t.Errorf("无需密钥的预设 %s 必须指向回环地址, 实际 %q", name, preset.BaseURL)
		}
	}
}

// 服务以无家目录的系统用户运行, ssh 默认的 ~/.ssh/known_hosts 不可写 ——
// 而 TOFU 需要往里写首次见到的主机指纹, 因此默认必须落到可写的状态目录。
func TestInspectKnownHostsDefaultsToStateDir(t *testing.T) {
	clearEnv(t)

	cfg := &Config{}
	if got := InspectKnownHostsPath(cfg, "/var/lib/backup-restore-agent/runtime.json"); got !=
		"/var/lib/backup-restore-agent/known_hosts" {
		t.Errorf("应跟运行态配置同目录: %q", got)
	}
	if got := InspectKnownHostsPath(cfg, ""); got !=
		"/var/lib/backup-restore-agent/known_hosts" {
		t.Errorf("无运行态配置时应用内置默认值: %q", got)
	}

	// 显式配置优先
	cfg.Server.Inspect.KnownHosts = "/etc/ssh/ssh_known_hosts"
	if got := InspectKnownHostsPath(cfg, "/var/lib/x/runtime.json"); got != "/etc/ssh/ssh_known_hosts" {
		t.Errorf("显式配置应优先: %q", got)
	}
}

// 巡检历史的默认路径同理。
func TestInspectReportPathDefaults(t *testing.T) {
	clearEnv(t)
	cfg := &Config{}
	if got := InspectReportPath(cfg, "/var/lib/backup-restore-agent/runtime.json"); got !=
		"/var/lib/backup-restore-agent/inspections.json" {
		t.Errorf("应跟运行态配置同目录: %q", got)
	}
	cfg.Server.Inspect.ReportPath = "/tmp/custom.json"
	if got := InspectReportPath(cfg, "/var/lib/x/runtime.json"); got != "/tmp/custom.json" {
		t.Errorf("显式配置应优先: %q", got)
	}
}

// host_key_policy 只接受三种取值, 写错要当场报错而不是默默按默认走。
func TestInspectHostKeyPolicyValidated(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[server.inspect]
host_key_policy = "yolo"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("非法策略应报错")
	}

	for _, policy := range []string{"tofu", "strict", "insecure"} {
		valid := writeConfig(t, "\n[server.inspect]\nhost_key_policy = \""+policy+"\"\n")
		cfg, err := Load(valid)
		if err != nil {
			t.Errorf("%s 应被接受: %v", policy, err)
			continue
		}
		if cfg.Server.Inspect.HostKeyPolicy != policy {
			t.Errorf("策略未生效: %q", cfg.Server.Inspect.HostKeyPolicy)
		}
	}
}

func TestInspectDefaultsAreSane(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Server.Inspect.Enabled == nil || !*cfg.Server.Inspect.Enabled {
		t.Error("巡检默认应开启")
	}
	if cfg.Server.Inspect.Concurrency != 4 {
		t.Errorf("默认并发应为 4: %d", cfg.Server.Inspect.Concurrency)
	}
	if cfg.Server.Inspect.HostKeyPolicy != "tofu" {
		t.Errorf("默认应为 tofu: %q", cfg.Server.Inspect.HostKeyPolicy)
	}
	if cfg.Server.Inspect.CommandTimeoutSeconds <= 0 {
		t.Error("默认应有单项超时")
	}
}
