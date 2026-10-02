// Package driver 是引擎扩展点: 新增一个数据库/平台 = 新增一个驱动文件 + 注册。
//
// 不需要改 schema、改提示词、改缺口分析、改接口 —— 提示词里的 options schema
// 与缺口分析的必填槽位都由本包的槽位表派生。
package driver

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// SlotKind 决定前端渲染什么控件、后端怎么解析用户的回答、JSON Schema 怎么写。
type SlotKind string

const (
	SlotTime     SlotKind = "time"
	SlotInt      SlotKind = "int"
	SlotEnum     SlotKind = "enum"
	SlotString   SlotKind = "string"
	SlotBool     SlotKind = "bool"
	SlotDuration SlotKind = "duration"
	SlotList     SlotKind = "list"
)

// SlotTarget 决定槽位落在 options 里还是 SourceItem 的通用字段上。
type SlotTarget string

const (
	TargetOptions SlotTarget = "options"
	TargetItem    SlotTarget = "item"
)

// Slot 是一个必填/可选参数, 也是追问的原子单位。
//
// 它是本项目的核心数据结构: 同时驱动
//   - JSON Schema 生成 (driver/schema.go) —— 让 LLM 知道该填哪些键
//   - 取值校验 (driver/validate.go) —— 模型填错时报出精确路径
//   - 缺口分析与追问 (agent/gaps.go) —— 缺什么就问什么
type Slot struct {
	Field    string     // 参数名 (options 的键名, 或 SourceItem 的字段名)
	Question string     // 追问用的中文问句
	Kind     SlotKind   // 取值类型
	Target   SlotTarget // 落点

	// Desc 是写进 JSON Schema 的字段说明。留空时退回 Question。
	// 建议写清"各代码值是什么意思" —— 这份描述会原样出现在提示词里,
	// 模型据此把 "系统认证" 映射成 os_auth, 比在别处堆术语表可靠得多。
	Desc string

	Choices    []string          // 给用户看的候选值 (中文标签)
	ChoicesMap map[string]string // 中文标签 -> 代码值
	// Suggest 是给用户看的推荐值, 与 Choices 的区别是它不属于封闭集合 ——
	// 用户既可以点它, 也可以自己填别的。适用于时刻、保留期这类没有固定候选、
	// 但绝大多数人只会从几个常见值里选的参数。
	Suggest []string
	Example string
	Hint    string

	// Optional 为 true 表示缺失不阻塞 ready, 只出提示。
	// 用"可选"而不是"必填"命名是有意的: Go 零值是 false, 忘记声明时槽位会变成
	// 必填 (吵闹但安全), 而不是悄悄变成可选。
	Optional bool
	// Default 是 Optional 槽位的兜底取值, 用于展示与警告文案。
	Default any

	Min *int // Kind=SlotInt 时的下界
	Max *int // Kind=SlotInt 时的上界

	// SatisfiedWhen 支持条件必填: 传回 true 表示当前参数组合下该槽位已被满足。
	// 例: PostgreSQL 选物理备份时 databases 无意义。
	SatisfiedWhen func(options map[string]any) bool
}

// ChoiceLabels 返回给前端/提示词展示的候选标签。
func (s Slot) ChoiceLabels() []string {
	if len(s.Choices) > 0 {
		return s.Choices
	}
	labels := make([]string, 0, len(s.ChoicesMap))
	for label := range s.ChoicesMap {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// Example 是驱动的示例对话。
type Example struct {
	Input  string
	Output string // 计划 JSON 字符串
}

// Driver 描述一个备份引擎的完整支持面。
type Driver struct {
	Kind        string
	Display     string
	SourceType  string // 对应 schema.SourceTypes 之一
	Aliases     []string
	Slots       []Slot
	PromptHints string
	OptionsDesc string
	FewShot     []Example
}

// RequiredSlots 返回阻塞 ready 的槽位。
func (d *Driver) RequiredSlots() []Slot {
	out := make([]Slot, 0, len(d.Slots))
	for _, slot := range d.Slots {
		if !slot.Optional {
			out = append(out, slot)
		}
	}
	return out
}

// FindSlot 按字段名查槽位。
func (d *Driver) FindSlot(field string) (Slot, bool) {
	for _, slot := range d.Slots {
		if slot.Field == field {
			return slot, true
		}
	}
	return Slot{}, false
}

// ChoiceLabel 把代码值翻回中文标签 (给追问文案与展示用)。
func (d *Driver) ChoiceLabel(value string) string {
	for _, slot := range d.Slots {
		for label, code := range slot.ChoicesMap {
			if code == value {
				return label
			}
		}
	}
	return value
}

// genericKind 是兜底驱动的 kind。
const genericKind = "other"

// IntPtr 便于在槽位里声明取值范围。
func IntPtr(value int) *int { return &value }

// ---------------------------------------------------------------------------
// 注册表
// ---------------------------------------------------------------------------

var registry = map[string]*Driver{}

// Register 注册驱动; 重复注册会 panic (启动期错误应当立刻暴露)。
func Register(d *Driver) {
	if d == nil || d.Kind == "" {
		panic("driver: 注册了空驱动")
	}
	if _, exists := registry[d.Kind]; exists {
		panic("driver: 重复注册 " + d.Kind)
	}
	registry[d.Kind] = d
}

// Find 精确查找, 不做兜底。用于判断"该 kind 是否已被支持"。
func Find(kind string) (*Driver, bool) {
	d, ok := registry[kind]
	return d, ok
}

// Get 查找并兜底到 generic —— 未知引擎不该中断流程。
func Get(kind string) *Driver {
	if d, ok := registry[kind]; ok {
		return d
	}
	return registry[genericKind]
}

// All 返回全部驱动, 按 Kind 排序以保证接口输出与提示词稳定。
func All() []*Driver {
	kinds := Kinds()
	out := make([]*Driver, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, registry[kind])
	}
	return out
}

// Kinds 返回全部已注册的 kind (已排序)。
func Kinds() []string {
	kinds := make([]string, 0, len(registry))
	for kind := range registry {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Registered 判断 kind 是否已注册。
func Registered(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// ---------------------------------------------------------------------------
// 别名解析
// ---------------------------------------------------------------------------

type aliasEntry struct {
	alias  string
	driver *Driver
	re     *regexp.Regexp
}

var (
	aliasIndex []aliasEntry
	aliasOnce  sync.Once
)

// buildAliasIndex 惰性构建别名索引。
//
// 不能放在 init 里: Go 按文件名顺序执行同包 init, driver.go 会早于 oracle.go
// 执行, 那时驱动还没注册完。
func buildAliasIndex() {
	aliasIndex = make([]aliasEntry, 0, 32)
	for _, d := range registry {
		for _, alias := range d.Aliases {
			if alias == "" {
				continue
			}
			lowered := strings.ToLower(alias)
			entry := aliasEntry{alias: lowered, driver: d}
			if isASCII(lowered) {
				// ASCII 别名要求词边界, 避免 "pg" 命中 "gpg"/"page"
				entry.re = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(lowered) + `\b`)
			}
			aliasIndex = append(aliasIndex, entry)
		}
	}
	// 长别名优先: "postgresql" 胜过 "pg", "文件服务器" 胜过 "文件"
	sort.SliceStable(aliasIndex, func(i, j int) bool {
		return len(aliasIndex[i].alias) > len(aliasIndex[j].alias)
	})
}

// Resolve 按别名从自然语言里认出引擎; 认不出返回 nil。
//
// 这是 demo 客户端与"模型返回了未注册 kind 时"的补救路径。真实抽取以模型为主,
// 但引擎识别这一步没必要交给概率模型 —— 确定性规则更稳。
func Resolve(text string) *Driver {
	lowered := strings.ToLower(text)
	if strings.TrimSpace(lowered) == "" {
		return nil
	}
	aliasOnce.Do(buildAliasIndex)
	for _, entry := range aliasIndex {
		if entry.re != nil {
			if entry.re.MatchString(lowered) {
				return entry.driver
			}
			continue
		}
		if strings.Contains(lowered, entry.alias) {
			return entry.driver
		}
	}
	return nil
}

func isASCII(value string) bool {
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	return true
}

// OptionsSchemas 返回 kind -> options 的 JSON Schema 片段, 供提示词拼装。
func OptionsSchemas() map[string]any {
	out := map[string]any{}
	for _, kind := range Kinds() {
		d := registry[kind]
		if len(d.Slots) == 0 {
			continue
		}
		out[kind] = d.OptionsJSONSchema()
	}
	return out
}

// KindList 返回给提示词用的 kind 清单描述。
func KindList() string {
	return strings.Join(Kinds(), " / ")
}
