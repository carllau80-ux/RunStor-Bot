package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runtimePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "runtime.json")
}

func TestRuntimeMissingFileIsNotAnError(t *testing.T) {
	store, err := OpenRuntime(runtimePath(t))
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if !store.Enabled() {
		t.Error("应处于启用状态")
	}
	if len(store.Names()) != 0 {
		t.Errorf("初始应为空: %v", store.Names())
	}
	if store.Active() != "" {
		t.Errorf("初始不该有激活项: %q", store.Active())
	}
}

func TestRuntimeDisabledRejectsWrites(t *testing.T) {
	store, err := OpenRuntime("")
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if store.Enabled() {
		t.Error("空路径应表示禁用")
	}
	if _, err := store.Put("x", RuntimeProfile{Preset: "deepseek"}); err == nil {
		t.Error("禁用时写入应报错, 而不是静默丢弃")
	}
	if err := store.SetActive("x"); err == nil {
		t.Error("禁用时切换应报错")
	}
	// 读操作不该 panic (nil 与禁用都要能安全处理)
	if len(store.Names()) != 0 || store.Active() != "" {
		t.Error("禁用时应返回空值")
	}
}

func TestRuntimePutAndProfile(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))

	_, err := store.Put("my-deepseek", RuntimeProfile{
		Preset: "deepseek",
		APIKey: "sk-secret-value",
		Note:   "网页创建",
	})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	profile, ok := store.Profile("my-deepseek")
	if !ok {
		t.Fatal("应能取到 profile")
	}
	if profile.BaseURL != "https://api.deepseek.com" {
		t.Errorf("预设未生效: %q", profile.BaseURL)
	}
	if profile.Model != "deepseek-chat" {
		t.Errorf("模型未从预设带出: %q", profile.Model)
	}
	if profile.APIKey != "sk-secret-value" {
		t.Errorf("密钥不符: %q", profile.APIKey)
	}
	if profile.Source != SourceRuntime {
		t.Errorf("来源应标为 runtime: %q", profile.Source)
	}
	if !profile.Available() {
		t.Errorf("应可用: %s", profile.UnavailableReason())
	}

	// 脱敏视图不得泄露密钥
	view := profile.SafeView("my-deepseek")
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "sk-secret-value") {
		t.Fatal("脱敏视图泄露了密钥")
	}
	if view["editable"] != true {
		t.Error("运行态 profile 应标记为可编辑")
	}
}

// 落盘的运行态文件必须是 0600 —— 里面有密钥。
func TestRuntimeFileIsPersistedAt0600(t *testing.T) {
	path := runtimePath(t)
	store, _ := OpenRuntime(path)

	if _, err := store.Put("p", RuntimeProfile{Preset: "ollama", APIKey: "secret"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("运行态文件未落盘: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("权限应为 0600, 实际 %04o", mode)
	}

	// 内容必须是合法 JSON, 且能重新读回
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var decoded runtimeData
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("内容不是合法 JSON: %v", err)
	}
	if decoded.Profiles["p"].APIKey != "secret" {
		t.Error("密钥未正确落盘")
	}
}

func TestRuntimeSurvivesReopen(t *testing.T) {
	path := runtimePath(t)
	store, _ := OpenRuntime(path)
	if _, err := store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "k1"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := store.SetActive("p"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}

	// 模拟重启
	reopened, err := OpenRuntime(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if reopened.Active() != "p" {
		t.Errorf("激活选择应持久化, 实际 %q", reopened.Active())
	}
	profile, ok := reopened.Profile("p")
	if !ok || profile.APIKey != "k1" {
		t.Error("profile 应持久化")
	}
}

// 界面不回显密钥, 所以编辑其它字段时不该被迫重填密钥。
func TestRuntimePutKeepsKeyWhenBlank(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "original-key"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	if _, err := store.Put("p", RuntimeProfile{Preset: "deepseek", Model: "deepseek-reasoner"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	profile, _ := store.Profile("p")
	if profile.APIKey != "original-key" {
		t.Errorf("留空时应保留原密钥, 实际 %q", profile.APIKey)
	}
	if profile.Model != "deepseek-reasoner" {
		t.Errorf("模型名应被更新: %q", profile.Model)
	}
}

// 换掉接入点时**绝不能**沿用旧密钥。
//
// 这条守的是一条完整的凭据外发路径: 只要"本次没填 key 就沿用旧的"而不比较端点,
// 那么把同名的 base_url 改成自己的机器、再点一次"测试连接", 就能收到那把密钥 ——
// 不需要任何权限, 也不需要知道密钥内容。密钥是按接入点签发的, 换端点就该重填。
func TestRuntimePutDropsKeyWhenEndpointChanges(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{
		Preset: "deepseek", BaseURL: "https://api.deepseek.com", APIKey: "sk-real-key",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	keyCleared, err := store.Put("p", RuntimeProfile{
		BaseURL: "https://attacker.example.com/v1", Model: "deepseek-chat",
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if !keyCleared {
		t.Error("接入点变了却没报告'密钥已不再沿用' —— 界面会以为 key 还在")
	}
	profile, _ := store.Profile("p")
	if profile.APIKey != "" {
		t.Fatalf("换了 base_url 还沿用了旧密钥, 这把密钥会被发往 %s", profile.BaseURL)
	}
	if profile.Available() {
		t.Error("没有密钥的远程端点不该判为可用")
	}
}

// 换厂商 (preset) 也是换接入点: 密钥同样不该被带走。
func TestRuntimePutDropsKeyWhenPresetChanges(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "sk-real-key"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	keyCleared, err := store.Put("p", RuntimeProfile{Preset: "qwen"})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if !keyCleared {
		t.Error("换 preset 应报告密钥未沿用")
	}
	if profile, _ := store.Profile("p"); profile.APIKey != "" {
		t.Errorf("换 preset 后不该沿用旧密钥: %q", profile.APIKey)
	}
}

// 本地端点 (不需要密钥) 之间切换时, 没有密钥可言, 不该报"密钥未沿用"吓唬人。
func TestRuntimePutDoesNotClaimKeyClearedWithoutKey(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{Preset: "ollama"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	keyCleared, err := store.Put("p", RuntimeProfile{
		BaseURL: "http://127.0.0.1:8000/v1", Model: "local-model",
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if keyCleared {
		t.Error("本来就没有密钥时不该报告'密钥已不再沿用'")
	}
}

// 同一次保存里显式给了新密钥: 以新密钥为准, 也不该报"未沿用"。
func TestRuntimePutAcceptsNewKeyWithNewEndpoint(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "sk-old"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	keyCleared, err := store.Put("p", RuntimeProfile{
		BaseURL: "https://other.example.com/v1", Model: "m", APIKey: "sk-new",
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if keyCleared {
		t.Error("本次已提供新密钥, 不该报告未沿用")
	}
	if profile, _ := store.Profile("p"); profile.APIKey != "sk-new" {
		t.Errorf("应使用新密钥: %q", profile.APIKey)
	}
}

func TestRuntimePutReplacesKeyWhenProvided(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	_, _ = store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "old"}) //nolint:errcheck
	_, _ = store.Put("p", RuntimeProfile{Preset: "deepseek", APIKey: "new"}) //nolint:errcheck

	profile, _ := store.Profile("p")
	if profile.APIKey != "new" {
		t.Errorf("提供了新密钥应替换, 实际 %q", profile.APIKey)
	}
}

// 这是与 config.toml 的关键差别: 网页粘贴的密钥允许配远程端点。
//
// 理由: 运行态文件由服务自己写、0600、从不经人手编辑或提交, 扩散路径不存在;
// 若也禁止, 用户就无法在界面上直接粘贴密钥, 而这正是他要的用法。
func TestRuntimeAllowsInlineKeyForRemoteEndpoint(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	_, err := store.Put("deepseek-direct", RuntimeProfile{
		Preset: "deepseek", APIKey: "sk-remote-key",
	})
	if err != nil {
		t.Fatalf("网页配置远程端点内联密钥应被允许: %v", err)
	}
	profile, _ := store.Profile("deepseek-direct")
	if profile.BaseURL != "https://api.deepseek.com" || profile.APIKey != "sk-remote-key" {
		t.Fatalf("配置不符: %+v", profile)
	}
	if profile.KeySource != "runtime" {
		t.Errorf("密钥来源应标为 runtime: %q", profile.KeySource)
	}
}

// 而 config.toml 的内联远程密钥仍然禁止 —— 那个文件会被复制、备份、提交。
func TestConfigStillRejectsInlineKeyForRemoteEndpoint(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, `
[llm.profiles.bad]
preset = "deepseek"
api_key = "sk-plain"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("config.toml 里的远程内联密钥仍应被拒绝")
	}
}

func TestRuntimePutValidatesName(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	for _, name := range []string{"", " ", "有中文", "has space", "-leading", "a/b"} {
		if _, err := store.Put(name, RuntimeProfile{Preset: "ollama"}); err == nil {
			t.Errorf("非法配置名 %q 应被拒绝", name)
		}
	}
}

func TestRuntimePutValidatesPreset(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	_, err := store.Put("p", RuntimeProfile{Preset: "no-such-vendor"})
	if err == nil {
		t.Fatal("未知 preset 应被拒绝")
	}
	if !strings.Contains(err.Error(), "no-such-vendor") {
		t.Errorf("报错应指明 preset 名: %v", err)
	}
}

func TestRuntimePutValidatesProvider(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, err := store.Put("p", RuntimeProfile{Provider: "anthropic-native"}); err == nil {
		t.Fatal("未知 provider 应被拒绝")
	}
	if _, err := store.Put("p", RuntimeProfile{
		Provider: ProviderOpenAICompat, BaseURL: "http://127.0.0.1:11434/v1", Model: "m",
	}); err != nil {
		t.Fatalf("合法 provider 应被接受: %v", err)
	}
}

func TestRuntimeDelete(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	_, _ = store.Put("p", RuntimeProfile{Preset: "ollama"}) //nolint:errcheck
	store.SetActive("p")                                    //nolint:errcheck

	if err := store.Delete("p"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if store.Has("p") {
		t.Error("应已删除")
	}
	if store.Active() != "" {
		t.Errorf("删除当前项后应清除激活选择, 实际 %q", store.Active())
	}
	if err := store.Delete("p"); err == nil {
		t.Error("重复删除应报错")
	}
}

func TestRuntimeProfileMissing(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	if _, ok := store.Profile("nope"); ok {
		t.Error("不存在的 profile 不该返回")
	}
	if _, ok := store.Raw("nope"); ok {
		t.Error("不存在的原始定义不该返回")
	}
}

func TestRuntimeAllSorted(t *testing.T) {
	store, _ := OpenRuntime(runtimePath(t))
	for _, name := range []string{"zeta", "alpha", "mid"} {
		_, _ = store.Put(name, RuntimeProfile{Preset: "ollama"}) //nolint:errcheck
	}
	names := store.Names()
	want := []string{"alpha", "mid", "zeta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("应排序: %v", names)
	}
	if len(store.All()) != 3 {
		t.Fatalf("All() 数量不符: %d", len(store.All()))
	}
}

func TestRuntimeRejectsCorruptFile(t *testing.T) {
	path := runtimePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := OpenRuntime(path); err == nil {
		t.Fatal("损坏的运行态文件应报错, 而不是静默忽略")
	}
}

// 运行态不可写时, 配置加载仍应成功, 只是记一条提示 —— 服务必须起得来。
func TestLoadWarnsWhenRuntimeUnwritable(t *testing.T) {
	clearEnv(t)
	t.Setenv("BRA_RUNTIME", "/proc/cannot/write/here/runtime.json")

	// 切到临时目录避免读到仓库里的 config.toml
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("运行态目录不可写不该让加载失败: %v", err)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "运行态") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示运行态不可用: %v", cfg.Warnings)
	}
}

func TestRuntimePathPrecedence(t *testing.T) {
	clearEnv(t)

	// allow_ui_config=false 优先于一切
	disabled := false
	if got := runtimePathOf(Server{AllowUIConfig: &disabled, RuntimePath: "/tmp/x.json"}); got != "" {
		t.Errorf("关闭时路径应为空, 实际 %q", got)
	}

	// 环境变量优先于配置文件
	t.Setenv("BRA_RUNTIME", "/tmp/from-env.json")
	if got := runtimePathOf(Server{RuntimePath: "/tmp/from-config.json"}); got != "/tmp/from-env.json" {
		t.Errorf("环境变量应优先, 实际 %q", got)
	}

	// 无环境变量时用配置文件里的值
	_ = os.Unsetenv("BRA_RUNTIME")
	if got := runtimePathOf(Server{RuntimePath: "/tmp/from-config.json"}); got != "/tmp/from-config.json" {
		t.Errorf("应使用配置里的路径, 实际 %q", got)
	}

	// 都没设则用内置默认值
	if got := runtimePathOf(Server{}); got != defaultRuntimePath {
		t.Errorf("应使用默认路径, 实际 %q", got)
	}

	// 显式开启时也不受影响
	enabled := true
	if got := runtimePathOf(Server{AllowUIConfig: &enabled}); got != defaultRuntimePath {
		t.Errorf("开启时应用默认路径, 实际 %q", got)
	}
}

func TestLoadOpensRuntimeStore(t *testing.T) {
	clearEnv(t)
	path := runtimePath(t)
	t.Setenv("BRA_RUNTIME", path)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !cfg.Runtime.Enabled() {
		t.Fatal("应打开运行态存储")
	}
	if cfg.Runtime.Path() != path {
		t.Errorf("路径不符: %q", cfg.Runtime.Path())
	}
}

func TestLoadCanDisableUIConfig(t *testing.T) {
	clearEnv(t)
	configPath := writeConfig(t, "[server]\nallow_ui_config = false")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Runtime.Enabled() {
		t.Fatal("allow_ui_config=false 时应关闭运行态存储")
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "网页改配置已关闭") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示已关闭: %v", cfg.Warnings)
	}
}
