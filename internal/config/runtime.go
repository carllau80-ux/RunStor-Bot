package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"

	"backup-restore-agent/internal/fileio"
)

// 本文件实现"网页运行态配置": 用户在界面上选厂商、填 API Key 即可启用模型,
// 不需要 ssh 上机改文件。
//
// 为什么不写进 config.toml
// ------------------------
//  1. config.toml 是 0640 root:bra, 服务用户本来就无法写它。为了让服务能写而放松
//     权限是安全倒退。
//  2. config.toml 应保持"管理员声明的基线", 部署脚本会在升级时保留它, 不适合被
//     网页反复改写。
//  3. 分开之后, 网页创建的 profile 可以明确标记来源、单独删除, 与基线互不污染。
//
// 因此密钥落在服务自己的运行态文件里 (默认 /var/lib/backup-restore-agent/runtime.json,
// 由 systemd 的 StateDirectory 保证目录存在且属服务用户), 权限 0600。
//
// 注意这里仍是 JSON 而非 TOML: 它是**运行态数据** (服务自己读写), 不是给人编辑的
// 配置文件; 用 JSON 是为了读写两端同一套结构体, 避免多一个格式层。

// RuntimeProfile 是网页创建/修改的 profile 定义。
type RuntimeProfile struct {
	Preset     string `json:"preset,omitempty"`
	Provider   string `json:"provider,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	Model      string `json:"model,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	Note       string `json:"note,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

// LogValue 让 slog 记录 RuntimeProfile 时脱敏。
//
// 值接收者: 它作为 map 的值出现 (不可寻址), 指针接收者的方法在那时不会被调用。
func (p RuntimeProfile) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("preset", p.Preset),
		slog.String("provider", p.Provider),
		slog.String("base_url", p.BaseURL),
		slog.String("model", p.Model),
		slog.String("api_key", Mask(p.APIKey)),
	)
}

// String 让 fmt 的 %v / %+v 也不泄露密钥。
//
// 关键: RuntimeStore 的 data 是未导出字段, %+v 打印 RuntimeStore 时会连它一起打;
// 值接收者的 String 才能在这个场景下被 fmt 调用到 (map 的值不可寻址)。
func (p RuntimeProfile) String() string {
	return fmt.Sprintf("RuntimeProfile{provider=%s model=%s base_url=%s api_key=%s}",
		p.Provider, p.Model, p.BaseURL, Mask(p.APIKey))
}

type runtimeData struct {
	ActiveProfile string                    `json:"active_profile,omitempty"`
	Profiles      map[string]RuntimeProfile `json:"profiles"`
}

// RuntimeStore 是运行态配置的读写入口。
type RuntimeStore struct {
	path string

	mu   sync.Mutex
	data runtimeData
}

// profileNamePattern 限制 profile 名: 它会出现在 URL 路径与 JSON 键里。
var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// OpenRuntime 打开 (或初始化) 运行态配置。文件不存在不算错误。
//
// path 为空表示禁用网页改配置 —— 此时所有写操作都会明确报错, 而不是静默丢弃。
func OpenRuntime(path string) (*RuntimeStore, error) {
	store := &RuntimeStore{
		path: path,
		data: runtimeData{Profiles: map[string]RuntimeProfile{}},
	}
	if path == "" {
		return store, nil
	}

	// 有界读取: 这个文件是服务自己写的 (只有几行 profile), 给 4 MiB 已经很宽松。
	// 一个被塞大的文件不该在读的时候把内存吃光; 超限时报错而不是静默当作空配置 ——
	// 静默的话用户会发现"我配的模型全没了", 更难查。
	raw, err := fileio.ReadBounded(path, 4<<20)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("读取运行态配置失败 (%s): %w", path, err)
	}
	if err := json.Unmarshal(raw, &store.data); err != nil {
		return nil, fmt.Errorf("运行态配置格式错误 (%s): %w", path, err)
	}
	if store.data.Profiles == nil {
		store.data.Profiles = map[string]RuntimeProfile{}
	}
	return store, nil
}

// Enabled 报告网页改配置是否可用。
func (s *RuntimeStore) Enabled() bool { return s != nil && s.path != "" }

// Path 返回运行态文件路径 (空表示禁用)。
func (s *RuntimeStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Active 返回网页选定的 profile 名 (可能为空)。
func (s *RuntimeStore) Active() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ActiveProfile
}

// SetActive 记住网页选定的 profile。空名表示清除该选择。
func (s *RuntimeStore) SetActive(name string) error {
	if !s.Enabled() {
		return errRuntimeDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.ActiveProfile = name
	return s.persistLocked()
}

// Names 返回全部运行态 profile 名 (已排序)。
func (s *RuntimeStore) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.data.Profiles))
	for name := range s.data.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has 判断是否存在某个运行态 profile。
func (s *RuntimeStore) Has(name string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data.Profiles[name]
	return ok
}

// Raw 返回运行态里的原始定义 (含密钥, 仅供服务内部使用)。
func (s *RuntimeStore) Raw(name string) (RuntimeProfile, bool) {
	if s == nil {
		return RuntimeProfile{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.data.Profiles[name]
	return entry, ok
}

// Profile 把运行态定义解析成统一的 Profile。
func (s *RuntimeStore) Profile(name string) (*Profile, bool) {
	entry, ok := s.Raw(name)
	if !ok {
		return nil, false
	}
	profile, err := fillProfile(name, profileSpec{
		Preset:     entry.Preset,
		Provider:   entry.Provider,
		BaseURL:    entry.BaseURL,
		Model:      entry.Model,
		APIVersion: entry.APIVersion,
		Note:       entry.Note,
		APIKey:     entry.APIKey,
		Source:     SourceRuntime,
	})
	if err != nil {
		return nil, false
	}
	return profile, true
}

// All 返回全部运行态 profile (已按名字排序)。
func (s *RuntimeStore) All() []*Profile {
	out := []*Profile{}
	for _, name := range s.Names() {
		if profile, ok := s.Profile(name); ok {
			out = append(out, profile)
		}
	}
	return out
}

var errRuntimeDisabled = fmt.Errorf(
	"网页改配置已禁用 (未配置运行态文件路径); 请改用 config.toml")

// String 让 fmt 打印 RuntimeStore 时不带出密钥。
//
// 为什么 RuntimeProfile.String 不够: 它只能保护"直接打印一个 profile"; 而
// %+v 打印 RuntimeStore 时, fmt 会递归进未导出字段 (data → Profiles map), 那一步
// **不会**再调用 map 元素上的 String —— 于是密钥原样出现在日志里 (这是实测出来的,
// 不是推测)。
//
// 这里会加锁: 不加锁读 map 与并发写撞上就是 "concurrent map read and map write"。
// 代价是它不能在已持锁的临界区里被调用 (本包内没有这种用法)。
func (s *RuntimeStore) String() string {
	if s == nil {
		return "(nil)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	names := make([]string, 0, len(s.data.Profiles))
	withKey := 0
	for name, entry := range s.data.Profiles {
		names = append(names, name)
		if entry.APIKey != "" {
			withKey++
		}
	}
	sort.Strings(names)
	return fmt.Sprintf("RuntimeStore{path=%s profiles=[%s] with_key=%d active=%s}",
		s.path, strings.Join(names, " "), withKey, s.data.ActiveProfile)
}

// LogValue 让 slog 记录 RuntimeStore 时同样只留安全摘要。
func (s *RuntimeStore) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// Put 新建或更新一个运行态 profile。
//
// 语义说明: APIKey 留空表示"保留原有密钥" —— 界面上不回显已保存的密钥, 编辑其它字段时
// 不该被迫重新粘贴一遍。
//
// **但保留有前提: 接入点没变。** 密钥是按接入点签发的, base_url 一换还沿用旧密钥, 就等于
// 把密钥发给另一个服务器。曾经只看"本次没填 key"就沿用 (不比较端点), 于是形成一条完整的
// 凭据外发路径: 把同名的 base_url 改到自己的机器、再点一次"测试连接", 就能收到那把密钥。
//
// 返回值 keyCleared 表示"接入点变了, 原有密钥没有沿用", 由界面如实告知用户 (要重填)。
func (s *RuntimeStore) Put(name string, entry RuntimeProfile) (keyCleared bool, err error) {
	if !s.Enabled() {
		return false, errRuntimeDisabled
	}
	name = strings.TrimSpace(name)
	if !profileNamePattern.MatchString(name) {
		return false, fmt.Errorf(
			"profile 名不合法: %q (只允许字母数字与 _ . -, 且以字母数字开头, 最长 64)", name)
	}

	entry.Preset = strings.TrimSpace(entry.Preset)
	entry.Provider = strings.TrimSpace(entry.Provider)
	entry.BaseURL = strings.TrimSpace(entry.BaseURL)
	entry.Model = strings.TrimSpace(entry.Model)
	entry.APIVersion = strings.TrimSpace(entry.APIVersion)
	entry.Note = strings.TrimSpace(entry.Note)
	entry.APIKey = strings.TrimSpace(entry.APIKey)

	if entry.Provider != "" &&
		entry.Provider != ProviderOpenAICompat && entry.Provider != ProviderAzureOpenAI {
		return false, fmt.Errorf("provider 只能是 %s 或 %s, 实际是 %q",
			ProviderOpenAICompat, ProviderAzureOpenAI, entry.Provider)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.APIKey == "" {
		if existing, ok := s.data.Profiles[name]; ok {
			switch {
			case sameEndpoint(existing, entry):
				entry.APIKey = existing.APIKey
			case existing.APIKey != "":
				// 接入点变了: 不沿用旧密钥, 并告诉界面"要重填"
				keyCleared = true
			}
		}
	}

	// 先按同一套规则解析一次, 尽早暴露"preset 不存在"这类错误
	if _, err := fillProfile(name, profileSpec{
		Preset: entry.Preset, Provider: entry.Provider, BaseURL: entry.BaseURL,
		Model: entry.Model, APIVersion: entry.APIVersion, Note: entry.Note,
		APIKey: entry.APIKey, Source: SourceRuntime,
	}); err != nil {
		return false, err
	}

	s.data.Profiles[name] = entry
	return keyCleared, s.persistLocked()
}

// sameEndpoint 判断两份配置指向同一个接入点。
//
// 密钥是按接入点签发的: 同一个服务器换模型没问题, 换服务器必须重填密钥。
// 解析不出来 (preset 拼错等) 时一律当作"不同的接入点"—— 宁可让用户重填, 也不要
// 把密钥发给一个说不清的地址。
func sameEndpoint(existing, next RuntimeProfile) bool {
	before, okBefore := endpointOf(existing)
	after, okAfter := endpointOf(next)
	return okBefore && okAfter && before == after
}

// endpointOf 给出接入点标识: provider + 生效的 base_url (base_url 留空时按 preset 补)。
func endpointOf(entry RuntimeProfile) (string, bool) {
	profile, err := fillProfile("endpoint-probe", profileSpec{
		Preset: entry.Preset, Provider: entry.Provider, BaseURL: entry.BaseURL,
		Model: entry.Model, APIVersion: entry.APIVersion,
		APIKey: "probe", Source: SourceRuntime, // 只为解析端点, 密钥值无关
	})
	if err != nil {
		return "", false
	}
	return profile.Provider + "|" + profile.BaseURL, true
}

// Delete 删除一个运行态 profile。若它正被选为当前模型, 一并清除选择。
func (s *RuntimeStore) Delete(name string) error {
	if !s.Enabled() {
		return errRuntimeDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.data.Profiles[name]; !ok {
		return fmt.Errorf("运行态里没有 profile %q", name)
	}
	delete(s.data.Profiles, name)
	if s.data.ActiveProfile == name {
		s.data.ActiveProfile = ""
	}
	return s.persistLocked()
}

// persistLocked 原子落盘, 权限 0600。调用方需持有锁。
func (s *RuntimeStore) persistLocked() error {
	if s.path == "" {
		return errRuntimeDisabled
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化运行态配置失败: %w", err)
	}
	body = append(body, '\n')

	if err := fileio.WriteAtomic(s.path, body, 0o600); err != nil {
		return fmt.Errorf("保存运行态配置失败: %w", err)
	}
	return nil
}
