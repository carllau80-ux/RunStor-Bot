package llm

import (
	"fmt"
	"sort"
	"sync"

	"backup-restore-agent/internal/config"
)

// DemoName 是内置 demo 客户端的 profile 名。
const DemoName = "demo"

// Registry 管理 profile 的可用性与运行时切换。
//
// 优先级链的落点就在这里:
//
//	请求级覆盖 > 运行时切换(本结构的 active) > default_profile > 环境变量 > demo 兜底
//
// 关键行为: 任何一环不可用时都不报错, 而是退到 demo 并由调用方从 Client.Name()
// 看出实际用了什么。服务因此永远起得来, 演示永远能跑。
type Registry struct {
	cfg *config.Config

	mu      sync.Mutex
	active  string
	clients map[string]Client
}

// NewRegistry 构造注册表。
//
// 初始 active 的优先级: 网页上次选定的 profile > config.toml 的 default_profile。
// 这样用户在界面上选好的模型在重启后依然是当前模型。
func NewRegistry(cfg *config.Config) *Registry {
	registry := &Registry{
		cfg:     cfg,
		active:  cfg.DefaultProfile,
		clients: map[string]Client{},
	}
	if chosen := cfg.Runtime.Active(); chosen != "" {
		if profile := registry.resolveProfile(chosen); profile != nil && profile.Available() {
			registry.active = chosen
		}
	}
	return registry
}

// resolveProfile 在"运行态 + 配置文件"的合并视图里查找 profile。
//
// 同名时运行态优先: 用户在界面上的操作是最新意图。删掉网页创建的 profile 后,
// config.toml 里的同名定义会重新生效。
func (r *Registry) resolveProfile(name string) *config.Profile {
	if profile, ok := r.cfg.Runtime.Profile(name); ok {
		return profile
	}
	return r.cfg.FindProfile(name)
}

// RuntimeEnabled 报告网页改配置是否可用。
func (r *Registry) RuntimeEnabled() bool { return r.cfg.Runtime.Enabled() }

// RuntimePath 返回运行态文件路径 (空表示关闭)。
func (r *Registry) RuntimePath() string { return r.cfg.Runtime.Path() }

// Config 返回配置。
func (r *Registry) Config() *config.Config { return r.cfg }

// ActiveName 返回当前生效的 profile 名。
func (r *Registry) ActiveName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// ActiveProfile 返回当前 profile; demo 模式或未配置时返回 nil。
func (r *Registry) ActiveProfile() *config.Profile {
	return r.resolveProfile(r.ActiveName())
}

// Mode 报告当前生效的是真实模型还是 demo。
func (r *Registry) Mode() string {
	if profile := r.ActiveProfile(); profile != nil && profile.Available() {
		return "llm"
	}
	return DemoName
}

// Warnings 返回**当前**仍然成立的配置警告。
//
// 配置里的"没有可用 profile"是加载那一刻的结论, 而用户完全可能在那之后才在网页上
// 填好 key。照着旧快照报"将使用 demo 客户端", 而实际正在用真实模型 —— 这种自相
// 矛盾的提示会让人不再相信这块面板 (健康检查里 mode=llm 与这句提示同时出现)。
// 所以这条警告按当前状态重新判断, 其余与加载时无关的警告照原样带出。
func (r *Registry) Warnings() []string {
	warnings := make([]string, 0, len(r.cfg.Warnings)+1)
	for _, warning := range r.cfg.Warnings {
		if warning == config.NoUsableProfileWarning {
			continue
		}
		warnings = append(warnings, warning)
	}
	if r.Mode() == DemoName {
		warnings = append(warnings, config.NoUsableProfileWarning)
	}
	return warnings
}

// Client 取指定 profile 的客户端; 不可用或未指定时退到 demo。
//
// 返回值可能是 demo —— 调用方通过 Client.Name() 即可判断, 不需要额外问一次。
func (r *Registry) Client(name string) Client {
	if name == "" {
		name = r.ActiveName()
	}
	if name == DemoName {
		return NewDemo()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if cached, ok := r.clients[name]; ok {
		return cached
	}

	var client Client
	if profile := r.resolveProfile(name); profile != nil && profile.Available() {
		client = NewOpenAICompat(profile, 0)
	} else {
		client = NewDemo()
	}
	r.clients[name] = client
	return client
}

// ActiveClient 取当前生效的客户端。
func (r *Registry) ActiveClient() Client {
	return r.Client(r.ActiveName())
}

// SetActive 运行时切换 profile, 并把选择记到运行态文件 (重启后仍生效)。
//
// 返回的 warning 非空表示"已生效但没能持久化" —— 这种情况不该当成失败:
// 当前进程确实换了模型, 只是重启后会回到原来的选择。
func (r *Registry) SetActive(name string) (string, error) {
	if name == DemoName {
		return r.commitActive(DemoName), nil
	}

	profile := r.resolveProfile(name)
	if profile == nil {
		known := append(r.allProfileNames(), DemoName)
		sort.Strings(known)
		return "", fmt.Errorf("未知 profile: %q; 可用: %v", name, known)
	}
	if !profile.Available() {
		return "", fmt.Errorf("profile %q 当前不可用: %s", name, profile.UnavailableReason())
	}
	return r.commitActive(name), nil
}

// commitActive 切换并持久化, 返回持久化失败的提示。
func (r *Registry) commitActive(name string) string {
	r.mu.Lock()
	r.active = name
	// 切换即重建客户端, 避免复用陈旧连接
	delete(r.clients, name)
	r.mu.Unlock()

	if err := r.cfg.Runtime.SetActive(name); err != nil {
		return fmt.Sprintf("已切换为 %s, 但未能持久化 (%v); 重启后会回到原选择", name, err)
	}
	return ""
}

// AddProfile 新建或更新一个网页托管的 profile。
//
// keyCleared 表示"接入点变了, 原有密钥没有沿用"(见 config.RuntimeStore.Put),
// 由界面如实告知用户需要重填。
func (r *Registry) AddProfile(name string, entry config.RuntimeProfile) (keyCleared bool, err error) {
	keyCleared, err = r.cfg.Runtime.Put(name, entry)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	delete(r.clients, name) // 定义变了, 丢掉缓存的客户端
	r.mu.Unlock()
	return keyCleared, nil
}

// DeleteProfile 删除一个网页托管的 profile。
//
// 只能删运行态里的; config.toml 声明的 profile 由管理员维护, 网页无权删除。
func (r *Registry) DeleteProfile(name string) error {
	if !r.cfg.Runtime.Has(name) {
		if r.cfg.FindProfile(name) != nil {
			return fmt.Errorf("profile %q 来自配置文件, 不能在网页上删除", name)
		}
		return fmt.Errorf("没有名为 %q 的网页配置", name)
	}
	if err := r.cfg.Runtime.Delete(name); err != nil {
		return err
	}

	r.mu.Lock()
	delete(r.clients, name)
	// 删掉的正是当前模型时, 回退到配置文件里的默认 profile
	if r.active == name {
		r.active = r.cfg.DefaultProfile
	}
	r.mu.Unlock()
	return nil
}

// allProfileNames 返回合并视图里的全部 profile 名。
func (r *Registry) allProfileNames() []string {
	seen := map[string]bool{}
	names := []string{}
	for _, name := range r.cfg.ProfileNames() {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range r.cfg.Runtime.Names() {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ListProfiles 返回脱敏后的 profile 列表 (合并运行态与配置文件)。
func (r *Registry) ListProfiles() []map[string]any {
	active := r.ActiveName()
	if r.Mode() == DemoName {
		active = DemoName
	}

	items := make([]map[string]any, 0, len(r.cfg.Profiles)+2)
	for _, name := range r.allProfileNames() {
		profile := r.resolveProfile(name)
		if profile == nil {
			continue
		}
		view := profile.SafeView(active)
		// 同名时运行态覆盖配置文件, 这点要在界面上说清楚
		if profile.Source == config.SourceRuntime && r.cfg.FindProfile(name) != nil {
			view["overrides_config"] = true
		}
		items = append(items, view)
	}

	items = append(items, map[string]any{
		"name":               DemoName,
		"provider":           "builtin",
		"base_url":           "",
		"model":              "关键词规则",
		"has_key":            false,
		"key_source":         "none",
		"requires_key":       false,
		"available":          true,
		"unavailable_reason": nil,
		"is_active":          active == DemoName,
		"note":               "内置确定性客户端, 无需 Key, 用于离线演示与联调",
		"source":             "builtin",
		"editable":           false,
	})
	return items
}

// Ping 测试连通性, 只验证 key / 端点 / 模型名三件事。
//
// 不做"抽取冒烟"是有意的分层: 冒烟需要抽取逻辑 (agent 包), 而 agent 依赖本包,
// 放在这里会形成导入环。冒烟测试见 agent.SmokeTest。
func (r *Registry) Ping(name string) map[string]any {
	if name == "" {
		name = r.ActiveName()
	}
	result := map[string]any{"profile": name}

	if name == DemoName {
		result["ok"] = true
		result["kind"] = "builtin"
		result["reply"] = "demo 客户端始终可用"
		result["elapsed_ms"] = 0
		return result
	}

	profile := r.resolveProfile(name)
	if profile == nil {
		result["ok"] = false
		result["error"] = map[string]any{
			"kind":    string(ErrModel),
			"message": fmt.Sprintf("未知 profile: %s", name),
			"hint":    fmt.Sprintf("可用: %v / %s", r.allProfileNames(), DemoName),
			"detail":  "",
		}
		return result
	}
	if !profile.Available() {
		result["ok"] = false
		result["error"] = map[string]any{
			"kind":    string(ErrAuth),
			"message": fmt.Sprintf("profile %s 不可用: %s", name, profile.UnavailableReason()),
			"hint":    "补齐 base_url / model / API Key 后重试",
			"detail":  "",
		}
		return result
	}

	client := NewOpenAICompat(profile, 0)
	started := nowMillis()
	ping, err := client.Ping()
	elapsed := nowMillis() - started

	if err != nil {
		classified := AsError(err)
		result["ok"] = false
		result["elapsed_ms"] = elapsed
		result["error"] = classified.ToMap()
		return result
	}

	result["ok"] = true
	result["kind"] = "llm"
	result["elapsed_ms"] = elapsed
	for key, value := range ping {
		result[key] = value
	}
	return result
}
