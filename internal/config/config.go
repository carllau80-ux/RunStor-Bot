// Package config 负责配置加载: config.toml + 环境变量覆盖 + 可用性判定。
//
// 三条设计底线:
//  1. 任何一环缺失都不报错 —— 最差退到 demo 客户端, 服务照常启动。
//  2. API Key 只接受三种来源: 环境变量 / 0600 密钥文件 / 本地端点内联。
//     真实密钥绝不写进 config.toml, 也不出现在日志与接口响应里。
//  3. profile 是否可用由内容决定, 不靠"配了就假设能用"。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"backup-restore-agent/internal/fileio"

	"backup-restore-agent/internal/i18n"
)

// Provider 常量。
const (
	ProviderOpenAICompat = "openai_compat"
	ProviderAzureOpenAI  = "azure_openai"
)

// profile 来源。
const (
	SourceConfig  = "config"  // 来自 config.toml, 由管理员维护
	SourceRuntime = "runtime" // 由网页创建, 存于 0600 的运行态文件
)

// 默认配置文件位置 (按顺序查找)。
//
// 配置是 TOML 唯一格式(基线第 4 条)。旧的 config.json 不再被读取 —— 若沿用,
// 会出现"文件在那儿但配置没生效"的静默失败; 迁移做法见 README 的部署一节。
var defaultPaths = []string{
	"config.toml",
	"/etc/backup-restore-agent/config.toml",
}

// 环境变量兜底 profile 的名字。
const envProfileName = "env"

// defaultRuntimePath 是网页创建配置的默认落盘位置。
// 目录由 systemd 的 StateDirectory 保证存在且属服务用户。
const defaultRuntimePath = "/var/lib/backup-restore-agent/runtime.json"

// defaultSessionsPath 是对话历史的默认落盘位置。
const defaultSessionsPath = "/var/lib/backup-restore-agent/sessions.json"

const (
	defaultBaseURL     = "https://api.deepseek.com"
	defaultModel       = "deepseek-chat"
	defaultAPIVersion  = "2024-06-01"
	defaultPort        = 8081
	defaultListenHost  = "0.0.0.0"
	defaultPromptLimit = 20000
)

// CatalogConfig 是"能备份哪些应用 / 能备份到哪些介质服务器"这份清单的查询配置。
//
// 清单随环境变化 (新上线的库、新扩容的存储池), 所以它的实际来源是外部接口;
// 未配置或接口不可用时退回内置清单, 服务照常可用。
type CatalogConfig struct {
	// Enabled 为 false 时完全不查接口, 直接用内置清单。
	Enabled *bool `toml:"enabled"`
	// URL 同时提供应用与介质服务器; 也可分别用下面两个地址。
	URL             string            `toml:"url"`
	ApplicationsURL string            `toml:"applications_url"`
	MediaServersURL string            `toml:"media_servers_url"`
	APIKeyEnv       string            `toml:"api_key_env"`
	AuthHeader      string            `toml:"auth_header"` // 默认 Authorization (Bearer)
	Headers         map[string]string `toml:"headers"`
	TimeoutSeconds  int               `toml:"timeout_seconds"`
	TTLSeconds      int               `toml:"ttl_seconds"`
}

// InspectConfig 是服务器巡检的配置。
//
// 巡检会真的连服务器执行命令, 因此这里的每一项都与安全或可用性相关:
// 主机指纹策略决定信任边界, 超时决定"一台挂住的机器"会不会拖垮整批巡检。
type InspectConfig struct {
	// Enabled 为 false 时关闭巡检能力 (接口直接拒绝)。
	Enabled *bool `toml:"enabled"`
	// Concurrency 是同时巡检的主机数上限。
	Concurrency int `toml:"concurrency"`
	// ConnectTimeoutSeconds 是 SSH 连接超时。
	ConnectTimeoutSeconds int `toml:"connect_timeout_seconds"`
	// CommandTimeoutSeconds 是远端单项检查的超时 (会写进采集脚本)。
	CommandTimeoutSeconds int `toml:"command_timeout_seconds"`
	// AnalyzeLogs 控制是否用模型分析日志 (影响与处置建议)。nil 表示默认开启。
	//
	// 规则库的结论总会给出; 这一项只决定要不要额外让模型补长尾。
	AnalyzeLogs *bool `toml:"analyze_logs"`
	// PDFFont 指定导出 PDF 用的拉丁字体 (TrueType), 空表示用内置字体。
	// 中文部分始终用内置的中日韩字体 —— 目标机上通常没有可嵌入的 TrueType 中文字体。
	PDFFont string `toml:"pdf_font"`
	// HostKeyPolicy: tofu (默认, 未知主机接受但报告指纹) / strict / insecure
	HostKeyPolicy string `toml:"host_key_policy"`
	// KnownHosts 是 known_hosts 路径; 留空用 ssh 默认 (~/.ssh/known_hosts)。
	KnownHosts string `toml:"known_hosts"`
	// ReportPath 是巡检历史的落盘位置; 留空用内置默认值。
	ReportPath string `toml:"report_path"`
	// MaxReports 限制保留的历史条数。
	MaxReports int `toml:"max_reports"`
}

// Server 是 HTTP 服务配置。
type Server struct {
	Host          string `toml:"host"`
	Port          int    `toml:"port"`
	PromptLimit   int    `toml:"prompt_limit"`    // 需求文本最大长度
	MaxTurns      int    `toml:"max_turns"`       // 追问熔断轮次
	SessionTTLMin int    `toml:"session_ttl_min"` // 会话闲置回收时间(分钟)

	// AllowUIConfig 是否允许在网页上创建/修改模型配置。
	// 置 false 则关闭该能力, 模型配置只能改 config.toml。
	AllowUIConfig *bool `toml:"allow_ui_config"`
	// RuntimePath 是网页创建配置的落盘位置; 留空用内置默认值。
	// 网页写入的密钥只存在这个文件里, 不会污染 config.toml。
	RuntimePath string `toml:"runtime_path"`
	// SessionPath 是对话历史的落盘位置; 留空用内置默认值。
	// 界面有"历史对话"侧栏, 历史需要在重启后仍在, 否则侧栏等于没用。
	SessionPath string `toml:"session_path"`
	// MaxSessions 限制保留的历史条数, 让落盘文件大小有界。
	MaxSessions int `toml:"max_sessions"`
	// Language 是默认界面语言 (zh-CN / en-US)。
	//
	// 只是"没有请求参数时的默认值": 真正的选择存在每台浏览器的 localStorage 里,
	// 每个 API 请求带 ?lang= 过来 —— 一个人切英文不该影响同事。
	Language string `toml:"language"`

	// Catalog 是外部清单接口的配置。
	Catalog CatalogConfig `toml:"catalog"`
	// Inspect 是服务器巡检的配置。
	Inspect InspectConfig `toml:"inspect"`
	// KB 是知识库 (导入 Markdown, 按问题检索并引用) 的配置。
	KB KBConfig `toml:"kb"`
}

// Profile 是一个已解析密钥与可用性的模型接入点。
type Profile struct {
	Name        string
	Provider    string
	BaseURL     string
	Model       string
	APIKey      string
	KeySource   string // env:BRA_X / file:/path / inline / none
	RequiresKey bool
	Note        string
	APIVersion  string
	// Source 标明该 profile 来自哪里: "config" (配置文件) 或 "runtime" (网页创建)。
	// 界面据此区分展示, 删除接口据此拒绝删除管理员声明的 profile。
	Source string
}

// Available 判断该 profile 现在能否真正发起请求。
func (p *Profile) Available() bool {
	if p.BaseURL == "" || p.Model == "" {
		return false
	}
	if p.RequiresKey && p.APIKey == "" {
		return false
	}
	return true
}

// UnavailableReason 说明不可用的原因; 可用时返回空串。
func (p *Profile) UnavailableReason() string {
	switch {
	case p.BaseURL == "":
		return "缺少 base_url"
	case p.Model == "":
		return "缺少 model"
	case p.RequiresKey && p.APIKey == "":
		return "缺少 API Key (期望环境变量或 0600 密钥文件)"
	default:
		return ""
	}
}

// LogValue 让 slog 记录 Profile 时自动脱敏。
//
// 基线红线: 日志、错误、panic 里都不能出现密钥, 包括 %+v 打印整个配置结构体。
// 实现了 LogValue 的类型在 slog 里等于"显式声明怎么记", 因此
// slog.Info("配置", "profile", p) 不会把 APIKey 带出去。
//
// 用指针接收者: nil 也要能安全调用 (nil 的 LogValuer 会 panic)。
func (p *Profile) LogValue() slog.Value {
	if p == nil {
		return slog.StringValue("(nil)")
	}
	return slog.GroupValue(
		slog.String("name", p.Name),
		slog.String("provider", p.Provider),
		slog.String("base_url", p.BaseURL),
		slog.String("model", p.Model),
		slog.String("api_key", Mask(p.APIKey)),
		slog.String("key_source", p.KeySource),
		slog.String("source", p.Source),
	)
}

// String 让 fmt 的 %v / %+v / %s 也不泄露密钥。
//
// LogValue 只管 slog; 而排查问题时顺手 %+v 打印配置结构体, 恰恰是泄露密钥最常见的
// 路径。两条都堵上, 缺一条就留一个口子。
//
// 值接收者: Profile 会以值的形式出现在 map/slice 里 (不可寻址), 那时指针接收者的
// String 不会被 fmt 调用。
func (p Profile) String() string {
	return fmt.Sprintf("Profile{name=%s provider=%s model=%s base_url=%s api_key=%s}",
		p.Name, p.Provider, p.Model, p.BaseURL, Mask(p.APIKey))
}

// SafeView 返回脱敏视图 —— 绝不包含密钥本体。
func (p *Profile) SafeView(active string) map[string]any {
	return map[string]any{
		"name":               p.Name,
		"provider":           p.Provider,
		"base_url":           p.BaseURL,
		"model":              p.Model,
		"has_key":            p.APIKey != "",
		"key_source":         p.KeySource,
		"requires_key":       p.RequiresKey,
		"available":          p.Available(),
		"unavailable_reason": p.UnavailableReason(),
		"is_active":          p.Name == active,
		"note":               p.Note,
		"source":             p.Source,
		"editable":           p.Source == SourceRuntime,
	}
}

// Config 是全部配置。
type Config struct {
	Server         Server
	Profiles       []*Profile
	DefaultProfile string
	Path           string // 实际加载的配置文件; 空表示没找到
	Warnings       []string
	// Runtime 是网页创建/修改的模型配置 (含密钥, 0600)。
	// 它覆盖同名的 config.toml profile —— 用户在界面上的操作是最新意图。
	Runtime *RuntimeStore
	// SessionPath 是已解析的对话历史落盘位置 (空表示仅内存)。
	SessionPath string
}

// FindProfile 按名字查找。
func (c *Config) FindProfile(name string) *Profile {
	for _, profile := range c.Profiles {
		if profile.Name == name {
			return profile
		}
	}
	return nil
}

// NoUsableProfileWarning 是"没有可用模型, 将退回关键词规则"的提示。
//
// 单独提出来是因为它会**过期**: 配置加载时可能确实没有可用 profile, 但用户随后在
// 网页里填好 key 就有了 —— 那时还照着加载时的快照显示这句话, 面板就不可信了。
// 所以它由 Registry 按当前状态重新判断, 而不是照抄加载时的结论。
const NoUsableProfileWarning = "没有任何可用 profile, 将使用 demo 客户端 (关键词规则)"

// HasUsableProfile 判断是否存在可用 profile。
//
// 同时看配置文件和运行态 (网页里创建的): 后者存在时, 按 Registry 的同名优先规则,
// 它才是真正生效的那一个。只看配置文件会漏判 —— 表现为"模型明明配好了, 却还在
// 提示会退回 demo"。
func (c *Config) HasUsableProfile() bool {
	for _, profile := range c.Profiles {
		if profile.Available() {
			return true
		}
	}
	for _, profile := range c.Runtime.All() {
		if profile.Available() {
			return true
		}
	}
	return false
}

// ProfileNames 返回全部 profile 名 (已排序)。
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for _, profile := range c.Profiles {
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	return names
}

// String 让 fmt 打印整个 Config 时不带出密钥与密钥来源路径。
//
// 存在的理由和 Profile.String 一样: %+v 打配置是排障时的常规动作, 而 Config 里
// 嵌着 Profiles 与 Runtime。这里只输出"排查需要的那几个字段"。
func (c Config) String() string {
	return fmt.Sprintf("Config{path=%s profiles=[%s] default=%s host=%s port=%d}",
		c.Path, strings.Join(c.ProfileNames(), " "), c.DefaultProfile,
		c.Server.Host, c.Server.Port)
}

// ---------------------------------------------------------------------------
// 原始 (JSON) 结构
// ---------------------------------------------------------------------------

// rawServer 是配置文件里 server 表的结构。
//
// 指针字段表示"没写就保持缺省"; 值字段由 applyServer 在零值时补缺省。
//
// 这个结构体必须与 Server 一一对应 —— 漏一个键, 那份配置就直接加载失败(未知键报错),
// 而且往往直到有人用那份配置部署时才暴露。曾经就漏了 language 与 kb: 自带的
// config.example.json 根本加载不了, server.kb.* 只能靠环境变量设。
// TestExampleConfigLoads 守这条。
type rawServer struct {
	Host          *string       `toml:"host"`
	Port          *int          `toml:"port"`
	PromptLimit   *int          `toml:"prompt_limit"`
	MaxTurns      *int          `toml:"max_turns"`
	SessionTTLMin *int          `toml:"session_ttl_min"`
	MaxSessions   *int          `toml:"max_sessions"`
	AllowUIConfig *bool         `toml:"allow_ui_config"`
	RuntimePath   *string       `toml:"runtime_path"`
	SessionPath   *string       `toml:"session_path"`
	Language      *string       `toml:"language"`
	Catalog       CatalogConfig `toml:"catalog"`
	Inspect       InspectConfig `toml:"inspect"`
	KB            KBConfig      `toml:"kb"`
}

type rawProfile struct {
	Preset      string `toml:"preset"`
	Provider    string `toml:"provider"`
	BaseURL     string `toml:"base_url"`
	Model       string `toml:"model"`
	APIKeyEnv   string `toml:"api_key_env"`
	APIKeyFile  string `toml:"api_key_file"`
	APIKey      string `toml:"api_key"`
	APIVersion  string `toml:"api_version"`
	RequiresKey *bool  `toml:"requires_key"`
	Note        string `toml:"note"`
}

type rawLLM struct {
	DefaultProfile string                `toml:"default_profile"`
	Profiles       map[string]rawProfile `toml:"profiles"`
}

type rawConfig struct {
	Server rawServer `toml:"server"`
	LLM    rawLLM    `toml:"llm"`
}

// ---------------------------------------------------------------------------
// 加载
// ---------------------------------------------------------------------------

// Load 加载配置。找不到配置文件不是错误 —— 会退化为 demo 模式。
func Load(explicitPath string) (*Config, error) {
	cfg := &Config{
		Server: Server{
			Host:          defaultListenHost,
			Port:          defaultPort,
			PromptLimit:   defaultPromptLimit,
			MaxTurns:      10,
			SessionTTLMin: 120,
			MaxSessions:   30,
		},
		Profiles: []*Profile{},
	}

	path, err := findConfigPath(explicitPath)
	if err != nil {
		return nil, err
	}
	cfg.Path = path

	var raw rawConfig
	if path != "" {
		// 有界读取: 配置文件就是几十行, 给 1 MiB 已经很宽松。上限同时挡住了
		// "config.toml 被软链到 /dev/zero"这类会把内存读光的极端情况。
		data, readErr := fileio.ReadBounded(path, 1<<20)
		if readErr != nil {
			return nil, fmt.Errorf("读取配置文件失败 (%s): %w", path, readErr)
		}
		meta, decodeErr := toml.Decode(string(data), &raw)
		if decodeErr != nil {
			return nil, fmt.Errorf("配置文件格式错误 (%s): %w", path, decodeErr)
		}
		// 未知键必须报错 (基线第 4 条)。打错的键被静默忽略, 症状是"配置看着生效
		// 其实没生效", 这类故障最难查 —— 尤其是把密钥写到不存在的键上时。
		if undecoded := meta.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, key := range undecoded {
				keys = append(keys, key.String())
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("配置文件有未知的键 (%s): %s",
				path, strings.Join(keys, ", "))
		}
	} else {
		cfg.Warnings = append(cfg.Warnings,
			"未找到配置文件, 仅使用环境变量; 无可用 Key 时进入 demo 模式")
	}

	if err := applyServer(cfg, raw.Server); err != nil {
		return nil, err
	}
	runtimePath := runtimePathOf(cfg.Server)
	openRuntimeStore(cfg, runtimePath)
	cfg.SessionPath = resolveSessionPath(cfg.Server, runtimePath)
	if err := applyProfiles(cfg, raw.LLM.Profiles); err != nil {
		return nil, err
	}
	applyEnvProfile(cfg)
	applyDefaultProfile(cfg, raw.LLM.DefaultProfile)

	if !cfg.HasUsableProfile() {
		cfg.Warnings = append(cfg.Warnings, NoUsableProfileWarning)
	}
	return cfg, nil
}

func applyServer(cfg *Config, raw rawServer) error {
	if raw.Host != nil {
		cfg.Server.Host = *raw.Host
	}
	if raw.Port != nil {
		if *raw.Port < 1 || *raw.Port > 65535 {
			return fmt.Errorf("server.port 超出范围: %d", *raw.Port)
		}
		cfg.Server.Port = *raw.Port
	}
	if raw.PromptLimit != nil && *raw.PromptLimit > 0 {
		cfg.Server.PromptLimit = *raw.PromptLimit
	}
	if raw.MaxTurns != nil && *raw.MaxTurns > 0 {
		cfg.Server.MaxTurns = *raw.MaxTurns
	}
	if raw.SessionTTLMin != nil && *raw.SessionTTLMin > 0 {
		cfg.Server.SessionTTLMin = *raw.SessionTTLMin
	}
	if raw.AllowUIConfig != nil {
		cfg.Server.AllowUIConfig = raw.AllowUIConfig
	}
	if raw.RuntimePath != nil {
		cfg.Server.RuntimePath = strings.TrimSpace(*raw.RuntimePath)
	}
	if raw.SessionPath != nil {
		cfg.Server.SessionPath = strings.TrimSpace(*raw.SessionPath)
	}
	if raw.Language != nil {
		value := strings.TrimSpace(*raw.Language)
		lang, ok := i18n.Parse(value)
		if !ok {
			return fmt.Errorf("server.language 只能是 zh-CN 或 en-US, 实际是 %q", value)
		}
		cfg.Server.Language = string(lang)
	}
	cfg.Server.Catalog = raw.Catalog
	// 这一行曾经漏了: [server.kb] 整张表都不会生效 (连 path/import_dir/seed_dir 都读不到),
	// 表现为"配置里写得明明白白, 实际用的是默认值"。零值由 kb 层的 withDefaults 补,
	// 所以这里原样赋值即可。
	cfg.Server.KB = raw.KB
	if raw.Catalog.Enabled == nil {
		enabled := true
		cfg.Server.Catalog.Enabled = &enabled
	}
	if raw.MaxSessions != nil && *raw.MaxSessions > 0 {
		cfg.Server.MaxSessions = *raw.MaxSessions
	}
	if cfg.Server.Catalog.TimeoutSeconds <= 0 {
		cfg.Server.Catalog.TimeoutSeconds = 5
	}
	if cfg.Server.Catalog.TTLSeconds <= 0 {
		cfg.Server.Catalog.TTLSeconds = 60
	}
	if cfg.Server.Catalog.Enabled == nil {
		enabled := true
		cfg.Server.Catalog.Enabled = &enabled
	}
	cfg.Server.Inspect = raw.Inspect
	if cfg.Server.Inspect.Enabled == nil {
		enabled := true
		cfg.Server.Inspect.Enabled = &enabled
	}
	if cfg.Server.Inspect.Concurrency <= 0 {
		cfg.Server.Inspect.Concurrency = 4
	}
	if cfg.Server.Inspect.ConnectTimeoutSeconds <= 0 {
		cfg.Server.Inspect.ConnectTimeoutSeconds = 8
	}
	if cfg.Server.Inspect.CommandTimeoutSeconds <= 0 {
		cfg.Server.Inspect.CommandTimeoutSeconds = 15
	}
	if cfg.Server.Inspect.MaxReports <= 0 {
		cfg.Server.Inspect.MaxReports = 30
	}
	if cfg.Server.Inspect.HostKeyPolicy == "" {
		cfg.Server.Inspect.HostKeyPolicy = "tofu"
	}
	switch cfg.Server.Inspect.HostKeyPolicy {
	case "tofu", "strict", "insecure":
	default:
		return fmt.Errorf(
			"inspect.host_key_policy 只能是 tofu / strict / insecure, 实际是 %q",
			cfg.Server.Inspect.HostKeyPolicy)
	}

	// 环境变量覆盖端口, 便于部署脚本与测试在不改文件的前提下换端口
	if value := strings.TrimSpace(os.Getenv("BRA_PORT")); value != "" {
		var port int
		if _, err := fmt.Sscanf(value, "%d", &port); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("环境变量 BRA_PORT 不是合法端口: %q", value)
		}
		cfg.Server.Port = port
	}
	if value := strings.TrimSpace(os.Getenv("BRA_HOST")); value != "" {
		cfg.Server.Host = value
	}
	return nil
}

// runtimePathOf 决定运行态配置文件的位置。
//
// 优先级: BRA_RUNTIME 环境变量 > config.toml 的 server.runtime_path > 内置默认值。
// allow_ui_config=false 时返回空串, 表示关闭网页改配置的能力。
func runtimePathOf(server Server) string {
	if server.AllowUIConfig != nil && !*server.AllowUIConfig {
		return ""
	}
	if value := strings.TrimSpace(os.Getenv("BRA_RUNTIME")); value != "" {
		return value
	}
	if value := strings.TrimSpace(server.RuntimePath); value != "" {
		return value
	}
	return defaultRuntimePath
}

// openRuntimeStore 打开运行态配置, 并把"网页改配置不可用"的情况记成提示。
//
// 打开失败或目录不可写都不该让服务起不来 —— 最差是退回"只能改 config.toml",
// 而那仍然是一个可用的服务 (至少 demo 模式能跑)。
func openRuntimeStore(cfg *Config, path string) {
	if path == "" {
		cfg.Warnings = append(cfg.Warnings,
			"网页改配置已关闭 (allow_ui_config=false); 模型配置请改 config.toml")
		return
	}

	store, err := OpenRuntime(path)
	if err != nil {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("运行态配置不可用, 网页将无法保存模型设置: %v", err))
		return
	}
	cfg.Runtime = store

	// 文件已存在就不必动目录; 部署环境由 systemd 的 StateDirectory 提前建好目录
	if !fileExists(path) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o750); mkErr != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"运行态配置目录不可写 (%s), 网页保存模型设置会失败; "+
					"可用 BRA_RUNTIME 指向可写位置", filepath.Dir(path)))
		}
	}
}

func applyProfiles(cfg *Config, rawProfiles map[string]rawProfile) error {
	names := make([]string, 0, len(rawProfiles))
	for name := range rawProfiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		profile, err := buildProfile(name, rawProfiles[name])
		if err != nil {
			return err
		}
		cfg.Profiles = append(cfg.Profiles, profile)
	}
	return nil
}

// profileSpec 是配置文件与网页运行态共用的 profile 定义。
//
// 抽出来是为了让两条来源走同一套"预设填充 + 密钥解析 + 可用性判定"逻辑 ——
// 否则 config.toml 与网页创建出来的 profile 行为会慢慢分叉。
type profileSpec struct {
	Preset      string
	Provider    string
	BaseURL     string
	Model       string
	APIVersion  string
	RequiresKey *bool
	Note        string

	// 密钥来源, 三选一
	APIKeyEnv  string
	APIKeyFile string
	APIKey     string

	Source string
}

func buildProfile(name string, raw rawProfile) (*Profile, error) {
	return fillProfile(name, profileSpec{
		Preset:      raw.Preset,
		Provider:    raw.Provider,
		BaseURL:     raw.BaseURL,
		Model:       raw.Model,
		APIVersion:  raw.APIVersion,
		RequiresKey: raw.RequiresKey,
		Note:        raw.Note,
		APIKeyEnv:   raw.APIKeyEnv,
		APIKeyFile:  raw.APIKeyFile,
		APIKey:      raw.APIKey,
		Source:      SourceConfig,
	})
}

// fillProfile 用预设补全缺省项, 解析密钥, 产出可用性判定所需的完整信息。
func fillProfile(name string, spec profileSpec) (*Profile, error) {
	var preset Preset
	hasPreset := false
	if spec.Preset != "" {
		found, ok := FindPreset(spec.Preset)
		if !ok {
			return nil, fmt.Errorf("profile %q: 未知的 preset %q; 可用: %s",
				name, spec.Preset, strings.Join(PresetNames(), ", "))
		}
		preset, hasPreset = found, true
	}

	profile := &Profile{Name: name, Source: spec.Source}

	profile.Provider = spec.Provider
	if profile.Provider == "" && hasPreset {
		profile.Provider = preset.Provider
	}
	if profile.Provider == "" {
		profile.Provider = ProviderOpenAICompat
	}

	profile.BaseURL = strings.TrimRight(spec.BaseURL, "/")
	if profile.BaseURL == "" && hasPreset {
		profile.BaseURL = strings.TrimRight(preset.BaseURL, "/")
	}

	profile.Model = spec.Model
	if profile.Model == "" && hasPreset {
		profile.Model = preset.Model
	}

	profile.RequiresKey = true
	if hasPreset {
		profile.RequiresKey = preset.RequiresKey
	}
	if spec.RequiresKey != nil {
		profile.RequiresKey = *spec.RequiresKey
	}

	profile.Note = spec.Note
	if profile.Note == "" && hasPreset {
		profile.Note = preset.Note
	}

	profile.APIVersion = spec.APIVersion
	if profile.APIVersion == "" {
		profile.APIVersion = defaultAPIVersion
	}

	key, source, err := resolveKey(name, spec)
	if err != nil {
		return nil, err
	}
	profile.APIKey = key
	profile.KeySource = source

	return profile, nil
}

// resolveKey 按 环境变量 > 密钥文件 > 内联 的顺序解析密钥。
func resolveKey(name string, spec profileSpec) (string, string, error) {
	if spec.APIKeyEnv != "" {
		value := strings.TrimSpace(os.Getenv(spec.APIKeyEnv))
		if value == "" {
			// 配了环境变量但没设 —— 不算错误, 交给可用性判定去提示
			return "", "env:" + spec.APIKeyEnv + "(未设置)", nil
		}
		return value, "env:" + spec.APIKeyEnv, nil
	}

	if spec.APIKeyFile != "" {
		value, err := readKeyFile(spec.APIKeyFile)
		switch {
		case err == nil:
			return value, "file:" + spec.APIKeyFile, nil
		case errors.Is(err, errKeyFileMissing):
			// 文件还没准备好 (尚未部署、密钥还没放上去) —— 与"环境变量没设"同等对待:
			// 记成不可用, 但不拦住启动。落到 demo 模式比服务起不来更可用, 而且
			// 界面上会照实显示"不可用"的原因。
			return "", "file:" + spec.APIKeyFile + "(不存在)", nil
		default:
			// 其他情况 (权限过宽、读不了) 是必须让人知道的配置问题: 直接失败。
			return "", "", fmt.Errorf("profile %s: %w", name, err)
		}
	}

	if spec.APIKey != "" {
		// 只有来自配置文件的明文密钥才限制端点。原因见 requireLocalEndpoint。
		if spec.Source == SourceConfig {
			if err := requireLocalEndpoint(name, spec.BaseURL, spec.Preset); err != nil {
				return "", "", err
			}
		}
		return strings.TrimSpace(spec.APIKey), "runtime", nil
	}

	return "", "none", nil
}

// requireLocalEndpoint 拒绝把明文密钥配给远程端点。
//
// 明文写在配置文件里, 一旦文件被复制/备份/提交, 密钥就扩散了。本地端点
// (Ollama 之类) 通常不校验密钥, 允许内联只是为了少一层手续。
//
// 该限制只作用于 config.toml。网页创建并存在运行态文件里的密钥是允许内联的 ——
// 那个文件由服务自己写、权限 0600、从不经人手编辑或提交, 扩散路径不存在;
// 若也一刀切禁止, 用户就无法在界面上直接粘贴密钥, 而这正是他需要的用法。
func requireLocalEndpoint(name, baseURL, presetName string) error {
	effective := strings.TrimRight(baseURL, "/")
	if effective == "" && presetName != "" {
		if preset, ok := FindPreset(presetName); ok {
			effective = strings.TrimRight(preset.BaseURL, "/")
		}
	}
	host := hostOf(effective)
	if host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "" {
		return nil
	}
	return fmt.Errorf(
		"llm.profiles.%s: 用了内联明文 api_key, 但 base_url 指向远程主机 (%s)。"+
			"请改用 api_key_env 或 api_key_file(0600), 避免密钥随配置文件扩散",
		name, host)
}

func hostOf(url string) string {
	trimmed := url
	if index := strings.Index(trimmed, "//"); index != -1 {
		trimmed = trimmed[index+2:]
	}
	if index := strings.IndexAny(trimmed, "/?#"); index != -1 {
		trimmed = trimmed[:index]
	}
	if index := strings.LastIndex(trimmed, ":"); index != -1 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// errKeyFileMissing 表示密钥文件还不存在。
//
// 与"权限过宽"区别对待: 没准备好是部署时序问题 (先起服务、后放密钥), 权限过宽是
// 安全问题。前者降级 (profile 不可用, 服务照常起), 后者 fail-fast。
var errKeyFileMissing = errors.New("密钥文件不存在")

// readKeyFile 读取密钥文件, 并要求权限为 0600。
func readKeyFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w (%s)", errKeyFileMissing, path)
		}
		return "", fmt.Errorf("api_key_file 不可用 (%s): %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf(
			"api_key_file 权限过宽: %s 是 %04o, 要求 0600。请执行: chmod 600 %s",
			path, mode, path)
	}
	// 有界读取 (密钥文件只有一行)
	data, err := fileio.ReadBounded(path, 64<<10)
	if err != nil {
		return "", fmt.Errorf("读取 api_key_file 失败 (%s): %w", path, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("api_key_file 内容为空: %s", path)
	}
	return value, nil
}

// applyEnvProfile 用 BRA_* 环境变量兜底出一个 profile。
func applyEnvProfile(cfg *Config) {
	baseURL := strings.TrimSpace(os.Getenv("BRA_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("BRA_MODEL"))
	apiKey := strings.TrimSpace(os.Getenv("BRA_API_KEY"))
	if baseURL == "" && model == "" && apiKey == "" {
		return
	}

	name := strings.TrimSpace(os.Getenv("BRA_PROFILE"))
	if name == "" {
		name = envProfileName
	}
	if cfg.FindProfile(name) != nil {
		return // 显式配置优先
	}

	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	source := "none"
	if apiKey != "" {
		source = "env:BRA_API_KEY"
	}

	cfg.Profiles = append(cfg.Profiles, &Profile{
		Name:        name,
		Provider:    ProviderOpenAICompat,
		BaseURL:     strings.TrimRight(baseURL, "/"),
		Model:       model,
		APIKey:      apiKey,
		KeySource:   source,
		RequiresKey: true,
		Note:        "来自环境变量 BRA_API_KEY / BRA_BASE_URL / BRA_MODEL",
		APIVersion:  defaultAPIVersion,
	})
}

func applyDefaultProfile(cfg *Config, requested string) {
	names := cfg.ProfileNames()

	requested = strings.TrimSpace(requested)
	if requested != "" && cfg.FindProfile(requested) != nil {
		cfg.DefaultProfile = requested
		return
	}
	if requested != "" {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("default_profile=%q 不在 profiles 中, 已回退", requested))
	}

	// 没指定就挑第一个可用的, 尽量让开箱体验是"能跑"而不是"要配"
	for _, name := range names {
		if profile := cfg.FindProfile(name); profile != nil && profile.Available() {
			cfg.DefaultProfile = name
			return
		}
	}
	if len(names) > 0 {
		cfg.DefaultProfile = names[0]
	}
}

func findConfigPath(explicit string) (string, error) {
	if explicit != "" {
		path, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("配置文件路径无效 (%s): %w", explicit, err)
		}
		if !fileExists(path) {
			return "", fmt.Errorf("配置文件不存在: %s", path)
		}
		return path, nil
	}

	if envPath := strings.TrimSpace(os.Getenv("BRA_CONFIG")); envPath != "" {
		path, err := filepath.Abs(envPath)
		if err != nil {
			return "", fmt.Errorf("BRA_CONFIG 路径无效 (%s): %w", envPath, err)
		}
		if !fileExists(path) {
			return "", fmt.Errorf("BRA_CONFIG 指向的配置文件不存在: %s", path)
		}
		return path, nil
	}

	for _, candidate := range defaultPaths {
		if fileExists(candidate) {
			path, err := filepath.Abs(candidate)
			if err == nil {
				return path, nil
			}
		}
	}
	return "", nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Mask 脱敏工具: 给日志用。
//
// 一律返回定长的 ***, **不回显任何真实字符**。曾经用"首 4 位...末 4 位"的写法,
// 那等于把 8 个字符写进日志 —— 对短密钥就是泄露大半, 而日志会进工单、进备份、
// 进别人的终端。调用方需要的只是"有没有配"。
func Mask(value string) string {
	if value == "" {
		return "(未设置)"
	}
	return "***"
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// resolveSessionPath 决定对话历史的落盘位置。
//
// 优先级: BRA_SESSIONS 环境变量 > config.toml 的 server.session_path >
// 与运行态配置同目录 > 内置默认值。
//
// 与运行态配置同目录这一层是为本地开发准备的: `BRA_RUNTIME=./runtime.json make run`
// 会让会话历史也落在当前目录, 不需要额外设一个变量。
func resolveSessionPath(server Server, runtimePath string) string {
	if value := strings.TrimSpace(os.Getenv("BRA_SESSIONS")); value != "" {
		return value
	}
	if value := strings.TrimSpace(server.SessionPath); value != "" {
		return value
	}
	if runtimePath != "" {
		return filepath.Join(filepath.Dir(runtimePath), "sessions.json")
	}
	return defaultSessionsPath
}

// InspectReportPath 决定巡检历史的落盘位置。
//
// 与运行态配置同目录: 部署时都在 StateDirectory 下, 本地开发时跟着 BRA_RUNTIME 走。
func InspectReportPath(cfg *Config, runtimePath string) string {
	if value := strings.TrimSpace(os.Getenv("BRA_INSPECT_REPORTS")); value != "" {
		return value
	}
	if value := strings.TrimSpace(cfg.Server.Inspect.ReportPath); value != "" {
		return value
	}
	if runtimePath != "" {
		return filepath.Join(filepath.Dir(runtimePath), "inspections.json")
	}
	return "/var/lib/backup-restore-agent/inspections.json"
}

// InspectKnownHostsPath 决定巡检用哪个 known_hosts。
//
// 为什么不能直接用 ssh 的默认值 (~/.ssh/known_hosts): 服务以专用系统用户运行且
// 没有家目录 (useradd --system --no-create-home), systemd 还开了 ProtectHome。
// 而 TOFU 策略需要往这个文件里**写入**首次见到的主机指纹 —— 默认路径在服务上下文
// 里根本不可写, 表现为"首次连接就失败"。因此默认落到可写的状态目录。
func InspectKnownHostsPath(cfg *Config, runtimePath string) string {
	if value := strings.TrimSpace(cfg.Server.Inspect.KnownHosts); value != "" {
		return value
	}
	if runtimePath != "" {
		return filepath.Join(filepath.Dir(runtimePath), "known_hosts")
	}
	return "/var/lib/backup-restore-agent/known_hosts"
}

// KBConfig 是知识库的配置。
type KBConfig struct {
	// Enabled 是否启用知识库; nil 表示默认启用。
	Enabled *bool `toml:"enabled"`
	// Path 是知识库落盘位置; 留空用内置默认值。
	Path string `toml:"path"`
	// MaxDocuments 限制文档数。
	MaxDocuments int `toml:"max_documents"`
	// MaxDocumentBytes 限制单份文档大小 (字节)。
	MaxDocumentBytes int `toml:"max_document_bytes"`
	// MaxTotalBytes 限制全部文档的总大小 (字节)。
	MaxTotalBytes int `toml:"max_total_bytes"`
	// TopK 是问答时默认取用的片段数。
	TopK int `toml:"top_k"`
	// Expand 控制是否默认用模型扩展检索词 (提升召回); nil 表示默认开启。
	Expand *bool `toml:"expand"`
	// ImportDir 是"从服务器目录导入"的受限目录; 留空用内置默认值, 设为 "-" 关闭该功能。
	//
	// 只读这个目录里的 .md/.markdown/.txt, 且拒绝任何逃逸出该目录的路径 ——
	// 这是"按路径读文件"能力的边界, 写松了就是任意文件读取。
	ImportDir string `toml:"import_dir"`
	// MaxRecords 限制保留的导入记录条数。
	MaxRecords int `toml:"max_records"`
	// SeedDir 是"随部署预置的文档"目录; 留空用内置默认值, "-" 表示不做预置同步。
	//
	// 服务启动时把该目录里的 .md 同步进知识库 (缺什么补什么, 不覆盖操作者自己的文档),
	// 这样一套装完就能直接用 —— 尤其适合"还没人写过手册"的初次部署。
	SeedDir string `toml:"seed_dir"`
}

// KBEnabled 报告知识库是否启用 (默认启用)。
func (c KBConfig) KBEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ExpandEnabled 报告是否用模型扩展检索词 (默认启用; 没有可用模型时会自动跳过)。
func (c KBConfig) ExpandEnabled() bool {
	if c.Expand == nil {
		return true
	}
	return *c.Expand
}

// KBImportDir 返回"从服务器目录导入"的目录。
//
// 默认放在服务的数据目录下的 kb-inbox: 它必须**不是** kb.json 所在目录本身,
// 否则知识库文件自己也在这个目录里 (虽然有扩展名校验兜底, 但没有理由留这种口子)。
// 返回空字符串表示该功能关闭。
func KBImportDir(cfg *Config, runtimePath string) string {
	if value := strings.TrimSpace(os.Getenv("BRA_KB_INBOX")); value != "" {
		return value
	}
	value := strings.TrimSpace(cfg.Server.KB.ImportDir)
	if value == "-" {
		return ""
	}
	if value != "" {
		return value
	}
	if runtimePath != "" {
		return filepath.Join(filepath.Dir(runtimePath), "kb-inbox")
	}
	return "/var/lib/backup-restore-agent/kb-inbox"
}

// KBSeedDir 返回预置文档目录。
//
// 默认值在 /usr/share 下: 那是 root 装的只读数据, 服务只读它 —— 预置文档属于"产品带的
// 内容", 不跟运行态数据混在一起。返回空字符串表示不做预置同步。
func KBSeedDir(cfg *Config) string {
	if value := strings.TrimSpace(os.Getenv("BRA_KB_SEED")); value != "" {
		return value
	}
	value := strings.TrimSpace(cfg.Server.KB.SeedDir)
	if value == "-" {
		return ""
	}
	if value != "" {
		return value
	}
	return "/usr/share/backup-restore-agent/knowledge"
}

// KBPath 返回知识库的落盘位置。
func KBPath(cfg *Config, runtimePath string) string {
	if value := strings.TrimSpace(os.Getenv("BRA_KB")); value != "" {
		return value
	}
	if value := strings.TrimSpace(cfg.Server.KB.Path); value != "" {
		return value
	}
	if runtimePath != "" {
		return filepath.Join(filepath.Dir(runtimePath), "kb.json")
	}
	return "/var/lib/backup-restore-agent/kb.json"
}

// LogAnalysisEnabled 报告是否启用模型日志分析 (默认启用; 没有可用模型时会自动跳过)。
func (c InspectConfig) LogAnalysisEnabled() bool {
	if c.AnalyzeLogs == nil {
		return true
	}
	return *c.AnalyzeLogs
}
