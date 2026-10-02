package driver

import (
	"sort"
	"strings"

	"backup-restore-agent/internal/schema"
)

// OptionsJSONSchema 由槽位表生成 options 的 JSON Schema 片段。
//
// 这是"单一事实源"的兑现方式: 槽位表既定义了模型该填什么键 (这里),
// 也定义了缺什么要问用户 (agent/gaps.go), 还定义了怎么校验 (validate.go)。
// 新增引擎时三处自动同步, 不存在"加了参数但忘了改提示词"。
func (d *Driver) OptionsJSONSchema() map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(d.Slots))

	for _, slot := range d.Slots {
		properties[slot.Field] = slot.jsonSchema()
		if !slot.Optional {
			required = append(required, slot.Field)
		}
	}
	sort.Strings(required)

	description := d.OptionsDesc
	if description == "" {
		description = d.Display + " 专属参数。只填用户明确提到的, 不要编造"
	}

	out := map[string]any{
		"type":                 "object",
		"description":          description,
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// jsonSchema 把单个槽位翻译成 JSON Schema。
func (s Slot) jsonSchema() map[string]any {
	out := map[string]any{"description": s.description()}

	switch s.Kind {
	case SlotTime:
		out["type"] = "string"
		out["pattern"] = `^([01]\d|2[0-3]):[0-5]\d$`
	case SlotInt:
		out["type"] = "integer"
		if s.Min != nil {
			out["minimum"] = *s.Min
		}
		if s.Max != nil {
			out["maximum"] = *s.Max
		}
	case SlotEnum:
		// 枚举给出的是代码值 —— 存进 JSON 的就是它。中文标签与代码的对应关系
		// 写在 description 里, 模型据此完成映射。
		out["type"] = "string"
		if codes := s.sortedCodes(); len(codes) > 0 {
			out["enum"] = codes
		}
	case SlotBool:
		out["type"] = "boolean"
	case SlotList:
		out["type"] = "array"
		out["items"] = map[string]any{"type": "string"}
	case SlotDuration:
		out["type"] = "object"
		out["properties"] = map[string]any{
			"value": map[string]any{"type": "integer", "minimum": 1},
			"unit":  map[string]any{"type": "string", "enum": schema.RetentionUnits},
		}
		out["required"] = []string{"value", "unit"}
	default:
		out["type"] = "string"
	}
	return out
}

// description 组装字段说明, 并补上 Desc 里没提到的"中文标签 -> 代码值"映射。
//
// 只补缺失的映射: Desc 里若已逐条写明 (如 Oracle scope 的 "whole_instance=整个实例"),
// 再追加一遍只是浪费提示词预算。
func (s Slot) description() string {
	base := s.Desc
	if base == "" {
		base = s.Question
	}
	if len(s.ChoicesMap) == 0 {
		return base
	}

	labels := make([]string, 0, len(s.ChoicesMap))
	for label := range s.ChoicesMap {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	missing := make([]string, 0, len(labels))
	for _, label := range labels {
		if strings.Contains(base, s.ChoicesMap[label]) {
			continue // 该代码值已在说明里出现, 不必重复
		}
		missing = append(missing, label+" -> "+s.ChoicesMap[label])
	}
	if len(missing) == 0 {
		return base
	}
	return base + " 取值对应关系: " + strings.Join(missing, "; ")
}
