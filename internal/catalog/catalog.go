// Package catalog 提供"能备份哪些应用"与"能备份到哪些介质服务器"的清单。
//
// 清单的实际来源是外部 API —— 它随环境变化 (新上线的库、新扩容的存储池),
// 因此不能写死在代码里。内置清单只作为兜底: API 不可用时服务仍要能用,
// 只是清单退化为通用取值, 并在结果里明确标注。
package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/config"
)

// Option 是应用的一个可配置参数 (由清单接口给出时使用)。
//
// 清单可以不提供它 —— 那时该应用的参数沿用内置驱动的定义。
type Option struct {
	Field      string            `json:"field"`
	Label      string            `json:"label"`
	Kind       string            `json:"kind"`
	Choices    []string          `json:"choices"`
	ChoicesMap map[string]string `json:"choices_map"`
	Required   *bool             `json:"required"`
	Example    string            `json:"example"`
	Suggest    []string          `json:"suggest"`
	Hint       string            `json:"hint"`
	Min        *int              `json:"min"`
	Max        *int              `json:"max"`
}

// IsRequired 报告该参数是否必填 (未声明时按必填处理)。
func (o Option) IsRequired() bool {
	return o.Required == nil || *o.Required
}

// Application 是一个可备份的应用/引擎。
type Application struct {
	Kind       string   `json:"kind"`
	Display    string   `json:"display"`
	SourceType string   `json:"source_type"`
	Aliases    []string `json:"aliases"`
	Note       string   `json:"note"`
	Options    []Option `json:"options"`
}

// MediaServer 是一台可用的目标介质服务器 (备份落地位置)。
type MediaServer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Path     string `json:"path"`
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Note     string `json:"note"`
}

// Display 返回给用户看的名称。
func (m MediaServer) Display() string {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = m.ID
	}
	// 带上落点更实用: 用户选的其实是"哪台服务器上的哪个目录"
	if location := strings.TrimSpace(m.Path); location != "" {
		return name + " " + location
	}
	if bucket := strings.TrimSpace(m.Bucket); bucket != "" {
		return name + " (桶 " + bucket + ")"
	}
	return name
}

// Catalog 是一次查询的结果。
type Catalog struct {
	Applications []Application
	MediaServers []MediaServer

	// Source 说明这份清单从哪来: "api" / "builtin" / "builtin-fallback"。
	Source string
	// Warning 非空时说明为什么退回了内置清单。
	Warning   string
	FetchedAt time.Time
}

// FromAPI 报告这份清单是否来自外部接口。
func (c Catalog) FromAPI() bool { return c.Source == "api" }

// ApplicationFor 按 kind 查找应用 (大小写不敏感)。
func (c Catalog) ApplicationFor(kind string) (Application, bool) {
	wanted := strings.ToLower(strings.TrimSpace(kind))
	for _, app := range c.Applications {
		if strings.ToLower(app.Kind) == wanted {
			return app, true
		}
		for _, alias := range app.Aliases {
			if strings.ToLower(alias) == wanted {
				return app, true
			}
		}
	}
	return Application{}, false
}

// MediaServerFor 按名称/标识查找介质服务器 (宽松匹配, 容忍用户只写了名字的一部分)。
func (c Catalog) MediaServerFor(text string) (MediaServer, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return MediaServer{}, false
	}
	lowered := strings.ToLower(trimmed)

	// 先精确
	for _, server := range c.MediaServers {
		if strings.ToLower(server.ID) == lowered || strings.ToLower(server.Name) == lowered {
			return server, true
		}
	}
	// 再包含 (用户可能只写了名字, 或写成了完整的"名字 + 路径")
	for _, server := range c.MediaServers {
		name := strings.ToLower(strings.TrimSpace(server.Name))
		if name == "" {
			continue
		}
		if strings.Contains(lowered, name) || strings.Contains(name, lowered) {
			return server, true
		}
	}
	return MediaServer{}, false
}

// names 返回介质服务器名称列表 (给提示词与报错用)。
func (c Catalog) MediaServerNames() []string {
	out := make([]string, 0, len(c.MediaServers))
	for _, server := range c.MediaServers {
		out = append(out, server.Display())
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 查询与缓存
// ---------------------------------------------------------------------------

// Provider 负责按 TTL 缓存清单, 避免每轮对话都去打外部接口。
type Provider struct {
	cfg    config.CatalogConfig
	client *http.Client

	mu sync.Mutex
	// lastGood / fetchedAt 记录"最近一次成功", hasGood 表示是否成功过。
	//
	// 必须与"什么时候该重新查"分开表达: 早期版本用同一个时间戳, 于是 Refresh 把时间
	// 清零之后, 查询失败时就丢失了"曾经成功过"这一信息, 只能退回内置清单, 而不是
	// 沿用上一次的真实环境清单。
	lastGood  Catalog
	hasGood   bool
	fetchedAt time.Time
	forceNext bool
}

// NewProvider 构造清单提供者。
func NewProvider(cfg config.CatalogConfig) *Provider {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Provider{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

// Configured 报告是否配置了外部清单接口。
//
// 只有配置了地址才算"配置了": 光把 enabled 设为 true 而没有地址, 等价于不查接口,
// 否则服务会一直去打一个空地址然后退回内置清单。
func (p *Provider) Configured() bool {
	if p == nil {
		return false
	}
	if p.cfg.Enabled != nil && !*p.cfg.Enabled {
		return false
	}
	return p.cfg.URL != "" || p.cfg.MediaServersURL != "" || p.cfg.ApplicationsURL != ""
}

// Builtin 返回内置兜底清单。
func (p *Provider) Builtin() Catalog { return Builtin() }

// Get 返回当前清单: 命中缓存就直接给, 否则查询接口, 失败则沿用上次成功的结果
// (从未成功过才退回内置清单)。
func (p *Provider) Get() Catalog {
	if !p.Configured() {
		return p.Builtin()
	}

	ttl := time.Duration(p.cfg.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.forceNext && p.hasGood && time.Since(p.fetchedAt) < ttl {
		return p.lastGood
	}

	fetched, err := p.fetch()
	if err != nil {
		p.forceNext = false
		if p.hasGood {
			// 沿用上次的真实清单: 环境没变, 只是接口抖了一下
			stale := p.lastGood
			stale.Warning = fmt.Sprintf("清单接口暂时不可用, 沿用上次结果 (%s): %v",
				p.fetchedAt.Format("15:04:05"), err)
			return stale
		}
		fallback := p.Builtin()
		fallback.Source = "builtin-fallback"
		fallback.Warning = fmt.Sprintf("清单接口不可用, 已退回内置清单: %v", err)
		return fallback
	}

	p.lastGood = fetched
	p.hasGood = true
	p.fetchedAt = time.Now()
	p.forceNext = false
	return fetched
}

// Refresh 忽略缓存强制重新查询 (界面上的"刷新清单"用)。
//
// 失败时仍会沿用上次成功的结果 —— 强制刷新不等于"宁可用空清单"。
func (p *Provider) Refresh() Catalog {
	p.mu.Lock()
	p.forceNext = true
	p.mu.Unlock()
	return p.Get()
}

// LastKnown 返回最近一次成功的清单; 从未成功过则返回内置清单 (不发起请求)。
func (p *Provider) LastKnown() Catalog {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hasGood {
		return p.lastGood
	}
	return p.Builtin()
}

func (p *Provider) fetch() (Catalog, error) {
	result := Catalog{Source: "api", FetchedAt: time.Now()}

	applicationsURL := p.cfg.ApplicationsURL
	mediaServersURL := p.cfg.MediaServersURL
	combinedURL := p.cfg.URL

	if applicationsURL != "" {
		body, err := p.get(applicationsURL)
		if err != nil {
			return Catalog{}, fmt.Errorf("查询应用清单失败: %w", err)
		}
		apps, err := decodeApplications(body)
		if err != nil {
			return Catalog{}, fmt.Errorf("解析应用清单失败: %w", err)
		}
		result.Applications = apps
	}
	if mediaServersURL != "" {
		body, err := p.get(mediaServersURL)
		if err != nil {
			return Catalog{}, fmt.Errorf("查询介质服务器失败: %w", err)
		}
		servers, err := decodeMediaServers(body)
		if err != nil {
			return Catalog{}, fmt.Errorf("解析介质服务器失败: %w", err)
		}
		result.MediaServers = servers
	}

	if combinedURL != "" {
		body, err := p.get(combinedURL)
		if err != nil {
			return Catalog{}, fmt.Errorf("查询清单失败: %w", err)
		}
		if result.Applications == nil {
			if apps, err := decodeApplications(body); err == nil {
				result.Applications = apps
			}
		}
		if result.MediaServers == nil {
			if servers, err := decodeMediaServers(body); err == nil {
				result.MediaServers = servers
			}
		}
		// 两个 URL 都没配时, 允许接口只提供其中一类
		if result.Applications == nil && result.MediaServers == nil {
			return Catalog{}, fmt.Errorf("接口返回里既没有应用清单也没有介质服务器清单")
		}
	}

	// 缺哪一类就用内置的补上, 而不是整份退回 —— 有外部应用清单但没配介质接口时,
	// 介质不至于变成空的
	builtin := p.Builtin()
	if len(result.Applications) == 0 {
		result.Applications = builtin.Applications
		result.Warning = appendWarning(result.Warning, "应用清单为空, 已用内置清单补全")
	}
	if len(result.MediaServers) == 0 {
		result.MediaServers = builtin.MediaServers
		result.Warning = appendWarning(result.Warning, "介质服务器清单为空, 已用内置清单补全")
	}
	return result, nil
}

func appendWarning(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func (p *Provider) get(url string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")

	if p.cfg.APIKeyEnv != "" {
		if key := strings.TrimSpace(getenv(p.cfg.APIKeyEnv)); key != "" {
			header := p.cfg.AuthHeader
			if header == "" {
				header = "Authorization"
			}
			value := key
			if strings.EqualFold(header, "Authorization") && !strings.HasPrefix(key, "Bearer ") {
				value = "Bearer " + key
			}
			request.Header.Set(header, value)
		}
	}
	for name, value := range p.cfg.Headers {
		request.Header.Set(name, value)
	}

	response, err := p.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	// 清单可能不小, 但仍要有上限, 免得接口异常时把内存吃光
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", response.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// 容错解析
//
// 各家的清单接口字段名不统一, 与其要求对方改接口, 不如在这里多认几种写法。
// 认不出时给出可读的报错, 而不是静默返回空清单 —— 空清单会让用户以为"没有可备份的应用"。
// ---------------------------------------------------------------------------

// unwrap 剥掉常见的外层包装: {"ok":true,"data":{...}} / {"data":[...]} / {"result":...}
func unwrap(body []byte) any {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	return unwrapValue(root, 0)
}

func unwrapValue(value any, depth int) any {
	if depth > 3 {
		return value
	}
	table, ok := value.(map[string]any)
	if !ok {
		return value
	}
	for _, key := range []string{"data", "result", "body", "payload"} {
		if inner, exists := table[key]; exists {
			switch inner.(type) {
			case map[string]any, []any:
				return unwrapValue(inner, depth+1)
			}
		}
	}
	return table
}

// firstList 按候选键名取出列表。
func firstList(node any, keys ...string) []any {
	table, ok := node.(map[string]any)
	if !ok {
		// 接口直接返回数组的情况
		if list, ok := node.([]any); ok {
			return list
		}
		return nil
	}
	for _, key := range keys {
		raw, exists := table[key]
		if !exists {
			continue
		}
		if list, ok := raw.([]any); ok {
			return list
		}
	}
	return nil
}

func firstString(table map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, exists := table[key]; exists {
			if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func stringList(table map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, exists := table[key]
		if !exists {
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			continue
		}
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	}
	return nil
}

func decodeApplications(body []byte) ([]Application, error) {
	node := unwrap(body)
	list := firstList(node, "applications", "apps", "sources", "engines", "application_list")
	if list == nil {
		return nil, fmt.Errorf("找不到应用清单字段 (期望 applications / apps / sources / engines)")
	}

	out := make([]Application, 0, len(list))
	for _, item := range list {
		table, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind := firstString(table, "kind", "type", "id", "code", "key")
		if kind == "" {
			continue
		}
		app := Application{
			Kind:       kind,
			Display:    firstString(table, "display", "name", "label", "title"),
			SourceType: firstString(table, "source_type", "category"),
			Aliases:    stringList(table, "aliases", "alias", "keywords"),
			Note:       firstString(table, "note", "description", "desc"),
		}
		if app.Display == "" {
			app.Display = kind
		}
		app.Options = decodeOptions(table)
		out = append(out, app)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("应用清单为空或缺少 kind 字段")
	}
	return out, nil
}

func decodeOptions(table map[string]any) []Option {
	list := firstList(table, "options", "params", "parameters")
	if list == nil {
		return nil
	}
	out := make([]Option, 0, len(list))
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		field := firstString(entry, "field", "key", "name", "id")
		if field == "" {
			continue
		}
		option := Option{
			Field:   field,
			Label:   firstString(entry, "label", "title", "display"),
			Kind:    firstString(entry, "kind", "type"),
			Choices: stringList(entry, "choices", "values", "options"),
			Example: firstString(entry, "example", "sample"),
			Suggest: stringList(entry, "suggest", "suggestions", "recommend"),
			Hint:    firstString(entry, "hint", "help", "tip"),
		}
		if raw, exists := entry["required"]; exists {
			if flag, ok := raw.(bool); ok {
				option.Required = &flag
			}
		}
		if raw, exists := entry["choices_map"]; exists {
			if mapping, ok := raw.(map[string]any); ok {
				option.ChoicesMap = map[string]string{}
				for label, code := range mapping {
					if text, ok := code.(string); ok {
						option.ChoicesMap[label] = text
					}
				}
			}
		}
		if raw, exists := entry["min"]; exists {
			if number, ok := toInt(raw); ok {
				option.Min = &number
			}
		}
		if raw, exists := entry["max"]; exists {
			if number, ok := toInt(raw); ok {
				option.Max = &number
			}
		}
		out = append(out, option)
	}
	return out
}

func decodeMediaServers(body []byte) ([]MediaServer, error) {
	node := unwrap(body)
	list := firstList(node, "media_servers", "mediaServers", "targets", "storage", "storages",
		"destinations", "media_server_list")
	if list == nil {
		return nil, fmt.Errorf(
			"找不到介质服务器字段 (期望 media_servers / targets / storage / destinations)")
	}

	out := make([]MediaServer, 0, len(list))
	for _, item := range list {
		table, ok := item.(map[string]any)
		if !ok {
			continue
		}
		server := MediaServer{
			ID:       firstString(table, "id", "code", "key"),
			Name:     firstString(table, "name", "display", "label", "title"),
			Type:     firstString(table, "type", "kind", "storage_type", "media_type"),
			Path:     firstString(table, "path", "dir", "directory", "mount", "mount_point"),
			Endpoint: firstString(table, "endpoint", "url", "host", "address"),
			Bucket:   firstString(table, "bucket", "bucket_name", "container"),
			Region:   firstString(table, "region", "zone", "location"),
			Note:     firstString(table, "note", "description", "desc"),
		}
		if server.ID == "" {
			server.ID = server.Name
		}
		if server.Name == "" {
			server.Name = server.ID
		}
		if server.ID == "" {
			continue
		}
		if server.Type == "" {
			server.Type = "other"
		}
		out = append(out, server)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("介质服务器清单为空或缺少 id/name 字段")
	}
	return out, nil
}

func toInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	}
	return 0, false
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
