package llm

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"backup-restore-agent/internal/config"
)

// newRegistryWithRuntime 构造一个带运行态存储的注册表。
func newRegistryWithRuntime(t *testing.T, profiles ...*config.Profile) (*Registry, *config.RuntimeStore) {
	t.Helper()
	store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
	if err != nil {
		t.Fatalf("打开运行态存储失败: %v", err)
	}
	cfg := &config.Config{
		Profiles:       profiles,
		DefaultProfile: "demo",
		Runtime:        store,
	}
	return NewRegistry(cfg), store
}

func TestRegistryMergesRuntimeProfiles(t *testing.T) {
	registry, store := newRegistryWithRuntime(t, &config.Profile{
		Name: "from-config", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://config.example.com", Model: "m",
		APIKey: "k", RequiresKey: true, Source: config.SourceConfig,
	})

	if _, err := store.Put("from-ui", config.RuntimeProfile{
		Preset: "ollama", Model: "qwen2.5:14b",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	names := registry.allProfileNames()
	joined := strings.Join(names, ",")
	for _, want := range []string{"from-config", "from-ui"} {
		if !strings.Contains(joined, want) {
			t.Errorf("合并视图应包含 %s, 实际 %v", want, names)
		}
	}

	// 两个来源都要能解析出可用客户端
	if registry.resolveProfile("from-ui") == nil {
		t.Error("运行态 profile 应能解析")
	}
	if registry.resolveProfile("from-config") == nil {
		t.Error("配置 profile 应能解析")
	}
}

// 同名时运行态优先; 删掉之后配置文件里的定义重新生效。
func TestRegistryRuntimeOverridesConfigThenFallsBack(t *testing.T) {
	registry, store := newRegistryWithRuntime(t, &config.Profile{
		Name: "deepseek", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://from-config.example.com", Model: "config-model",
		APIKey: "config-key", RequiresKey: true, Source: config.SourceConfig,
	})

	if got := registry.resolveProfile("deepseek"); got.BaseURL != "https://from-config.example.com" {
		t.Fatalf("初始应取配置文件版本: %q", got.BaseURL)
	}

	if _, err := registry.AddProfile("deepseek", config.RuntimeProfile{
		Preset: "deepseek", APIKey: "sk-from-ui",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got := registry.resolveProfile("deepseek")
	if got.Source != config.SourceRuntime {
		t.Errorf("同名时应取运行态版本: %q", got.Source)
	}
	if got.BaseURL != "https://api.deepseek.com" {
		t.Errorf("生效的应是网页配置: %q", got.BaseURL)
	}

	if err := registry.DeleteProfile("deepseek"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if got := registry.resolveProfile("deepseek"); got.Source != config.SourceConfig {
		t.Errorf("删除后应回落到配置文件版本: %q", got.Source)
	}
	_ = store
}

// 配置文件声明的 profile 不该能在网页上删除。
func TestRegistryRefusesDeletingConfigProfile(t *testing.T) {
	registry, _ := newRegistryWithRuntime(t, &config.Profile{
		Name: "managed", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		APIKey: "k", RequiresKey: true, Source: config.SourceConfig,
	})

	err := registry.DeleteProfile("managed")
	if err == nil {
		t.Fatal("应拒绝删除配置文件项")
	}
	if !strings.Contains(err.Error(), "配置文件") {
		t.Errorf("报错应说明来源: %v", err)
	}
}

// 删掉当前模型后应回退到配置里的默认 profile, 而不是悬空。
func TestRegistryFallsBackWhenActiveDeleted(t *testing.T) {
	registry, _ := newRegistryWithRuntime(t)

	if _, err := registry.AddProfile("temp", config.RuntimeProfile{
		Preset: "ollama", Model: "qwen2.5:14b",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := registry.SetActive("temp"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	if registry.ActiveName() != "temp" {
		t.Fatalf("切换未生效: %q", registry.ActiveName())
	}

	if err := registry.DeleteProfile("temp"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if registry.ActiveName() == "temp" {
		t.Error("删掉当前模型后不该仍指向它")
	}
}

// 网页选定的模型要跨重启保留。
func TestRegistryActivePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	store, err := config.OpenRuntime(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	cfg := &config.Config{DefaultProfile: "demo", Runtime: store, Profiles: []*config.Profile{}}
	registry := NewRegistry(cfg)

	if _, err := registry.AddProfile("persisted", config.RuntimeProfile{
		Preset: "ollama", Model: "qwen2.5:14b",
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := registry.SetActive("persisted"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}

	// 模拟重启: 重新打开运行态文件并构造注册表
	reopened, err := config.OpenRuntime(path)
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	restarted := NewRegistry(&config.Config{DefaultProfile: "demo", Runtime: reopened})

	if restarted.ActiveName() != "persisted" {
		t.Errorf("重启后应沿用网页选定的模型, 实际 %q", restarted.ActiveName())
	}
}

// 运行态里的选择指向已删除的 profile 时, 启动应回退而不是悬空。
func TestRegistryIgnoresStaleActiveSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	store, _ := config.OpenRuntime(path)
	_ = store.SetActive("ghost")

	cfg := &config.Config{DefaultProfile: "demo", Runtime: store}
	registry := NewRegistry(cfg)
	if registry.ActiveName() != "demo" {
		t.Errorf("悬空选择应被忽略, 实际 %q", registry.ActiveName())
	}
}

// Ping 也必须走合并视图 —— 否则网页刚保存的配置"测不了"。
func TestRegistryPingResolvesRuntimeProfile(t *testing.T) {
	registry, _ := newRegistryWithRuntime(t)

	// 指向一个必然不可达的本地端点 (没有 ollama 在跑)
	if _, err := registry.AddProfile("local-probe", config.RuntimeProfile{
		BaseURL: "http://127.0.0.1:1", Model: "m", APIKey: "k", Provider: config.ProviderOpenAICompat,
	}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	result := registry.Ping("local-probe")
	if result["ok"] != false {
		t.Fatalf("不可达端点应报告失败: %v", result)
	}
	errorInfo, _ := result["error"].(map[string]any)
	message, _ := errorInfo["message"].(string)
	if strings.Contains(message, "未知 profile") {
		t.Fatalf("运行态 profile 不该被判为未知: %v", errorInfo)
	}
	if errorInfo["kind"] != "connection" {
		t.Errorf("应归类为 connection, 实际 %v", errorInfo["kind"])
	}
}

// 提示里列出的可用配置应包含网页创建的, 否则用户按提示找不到自己刚存的配置。
func TestRegistryUnknownProfileHintListsRuntimeNames(t *testing.T) {
	registry, _ := newRegistryWithRuntime(t)
	if _, err := registry.AddProfile("my-local", config.RuntimeProfile{Preset: "ollama"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	result := registry.Ping("nope")
	hint, _ := result["error"].(map[string]any)["hint"].(string)
	if !strings.Contains(hint, "my-local") {
		t.Errorf("可用列表应包含网页创建的配置: %q", hint)
	}
}

// 网页填的密钥出错时, 建议应指向界面而不是环境变量。
func TestAuthHintPointsToUIForRuntimeProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer server.Close()

	runtimeProfile := &config.Profile{
		Name: "from-ui", Provider: config.ProviderOpenAICompat,
		BaseURL: server.URL, Model: "m", APIKey: "bad",
		RequiresKey: true, Source: config.SourceRuntime,
	}
	client := NewOpenAICompat(runtimeProfile, 0)
	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	classified := AsError(err)
	if classified.Kind != ErrAuth {
		t.Fatalf("应归类为 auth: %v", classified.Kind)
	}
	if !strings.Contains(classified.Hint(), "模型设置") {
		t.Errorf("网页填的密钥应指引回界面: %q", classified.Hint())
	}

	// 而配置文件里的密钥仍指引去查环境变量
	configProfile := &config.Profile{
		Name: "from-config", Provider: config.ProviderOpenAICompat,
		BaseURL: server.URL, Model: "m", APIKey: "bad",
		RequiresKey: true, Source: config.SourceConfig,
	}
	_, err = NewOpenAICompat(configProfile, 0).Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if hint := AsError(err).Hint(); !strings.Contains(hint, "api_key_env") {
		t.Errorf("配置文件来源的建议应指向环境变量: %q", hint)
	}
}

func TestRegistryListProfilesReportsSourceAndEditability(t *testing.T) {
	registry, _ := newRegistryWithRuntime(t, &config.Profile{
		Name: "from-config", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		APIKey: "k", RequiresKey: true, Source: config.SourceConfig,
	})
	if _, err := registry.AddProfile("from-ui", config.RuntimeProfile{Preset: "ollama"}); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	items := registry.ListProfiles()
	seen := map[string]map[string]any{}
	for _, item := range items {
		name, _ := item["name"].(string)
		seen[name] = item
	}

	if item := seen["from-config"]; item == nil {
		t.Error("缺少配置文件项")
	} else if item["editable"] != false || item["source"] != "config" {
		t.Errorf("配置文件项不该可编辑: %v", item)
	}

	if item := seen["from-ui"]; item == nil {
		t.Error("缺少网页配置项")
	} else if item["editable"] != true || item["source"] != "runtime" {
		t.Errorf("网页配置项应可编辑: %v", item)
	}

	if item := seen[DemoName]; item == nil {
		t.Error("缺少内置 demo 项")
	} else if item["editable"] != false {
		t.Errorf("内置项不该可编辑: %v", item)
	}
}

func TestRegistryRuntimeDisabled(t *testing.T) {
	cfg := &config.Config{DefaultProfile: "demo"}
	registry := NewRegistry(cfg)
	if registry.RuntimeEnabled() {
		t.Error("未配置运行态时应为关闭")
	}
	if _, err := registry.AddProfile("x", config.RuntimeProfile{Preset: "ollama"}); err == nil {
		t.Error("关闭时保存应报错")
	}
}
