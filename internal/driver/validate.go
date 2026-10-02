package driver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"backup-restore-agent/internal/schema"
)

// ValidateOptions 按槽位表校验某个源对象的 options。
//
// 报错信息带完整 JSON 路径, 因此既能直接展示给用户, 也能原样回填给模型做自纠错。
func (d *Driver) ValidateOptions(options map[string]any, prefix string) schema.Report {
	var report schema.Report
	if prefix != "" && !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}

	for _, slot := range d.Slots {
		path := prefix + slot.Field
		value, present := options[slot.Field]

		if !present || isBlank(value) {
			if slot.Optional {
				continue
			}
			if slot.SatisfiedWhen != nil && slot.SatisfiedWhen(options) {
				continue
			}
			// 交给缺口分析转成追问; 这里只声明"确实缺"
			report.AddError(path, path, "缺少 %s", slot.Field)
			continue
		}

		if problem := slot.ValidateValue(value); problem != "" {
			report.AddError(path, path, "%s", problem)
		}
	}

	// 未在槽位表里声明的键: 不算错 (用户可能有额外诉求), 但要提示可能不被识别
	if unknown := d.unknownKeys(options); len(unknown) > 0 {
		report.AddWarning(prefix+"options",
			"以下参数不在 %s 驱动的已知字段中, 不会被处理: %s",
			d.Kind, strings.Join(unknown, ", "))
	}
	return report
}

// ValidateValue 校验单个取值, 返回空串表示通过。
//
// 导出是为了让"清单接口定义的参数"与"内置驱动的参数"共用同一套取值语义 ——
// 两处各写一份必然分叉, 而分叉的表现是同一份配置在一处通过、在另一处被拒。
func (s Slot) ValidateValue(value any) string {
	switch s.Kind {
	case SlotTime:
		text, ok := asString(value)
		if !ok {
			return fmt.Sprintf("需要 HH:MM 格式的字符串, 实际是 %s", describe(value))
		}
		if _, _, valid := schema.ParseTime(text); !valid {
			return fmt.Sprintf("时刻格式不对: %q, 期望 24 小时制 HH:MM", text)
		}

	case SlotInt:
		number, ok := asInt(value)
		if !ok {
			return fmt.Sprintf("需要整数, 实际是 %s", describe(value))
		}
		if s.Min != nil && number < *s.Min {
			return fmt.Sprintf("不能小于 %d, 实际是 %d", *s.Min, number)
		}
		if s.Max != nil && number > *s.Max {
			return fmt.Sprintf("不能大于 %d, 实际是 %d", *s.Max, number)
		}

	case SlotEnum:
		text, ok := asString(value)
		if !ok {
			return fmt.Sprintf("需要字符串, 实际是 %s", describe(value))
		}
		if len(s.ChoicesMap) == 0 {
			return ""
		}
		if _, found := s.codeFor(text); !found {
			return fmt.Sprintf("取值必须是 %s 之一, 实际是 %q",
				strings.Join(s.sortedCodes(), "/"), text)
		}

	case SlotBool:
		if _, ok := value.(bool); !ok {
			return fmt.Sprintf("需要布尔值 (true/false), 实际是 %s", describe(value))
		}

	case SlotList:
		list, ok := value.([]any)
		if !ok {
			return fmt.Sprintf("需要字符串数组, 实际是 %s", describe(value))
		}
		for index, entry := range list {
			if _, ok := asString(entry); !ok {
				return fmt.Sprintf("第 %d 项需要字符串, 实际是 %s", index, describe(entry))
			}
		}

	case SlotDuration:
		table, ok := value.(map[string]any)
		if !ok {
			return fmt.Sprintf("需要 {value, unit} 对象, 实际是 %s", describe(value))
		}
		amount, ok := asInt(table["value"])
		if !ok || amount < 1 {
			return "value 需要 >= 1 的整数"
		}
		unit, ok := asString(table["unit"])
		if !ok || !schema.Contains(schema.RetentionUnits, unit) {
			return fmt.Sprintf("unit 必须是 %s 之一",
				strings.Join(schema.RetentionUnits, "/"))
		}

	default:
		if _, ok := asString(value); !ok {
			return fmt.Sprintf("需要字符串, 实际是 %s", describe(value))
		}
	}
	return ""
}

// codeFor 把取值解析成规范代码值。
func (s Slot) codeFor(value string) (string, bool) {
	return MatchChoice(s.ChoicesMap, value)
}

// MatchChoice 把取值解析成规范代码值。
//
// 三层匹配, 由严到宽: 代码值精确 -> 标签精确 -> 标签与取值互相包含且唯一命中。
// 最后一层是必要的: 模型常把用户原话 "整个实例" 回填进来, 而登记的标签是
// "整个实例(全库+归档+控制文件+参数文件)", 精确匹配会漏。能认就认, 不该因此报错。
//
// 导出是因为槽位答案解析要用同一套规则 —— 两处各写一份必然漂移。
func MatchChoice(choicesMap map[string]string, value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}

	for _, code := range choicesMap {
		if strings.EqualFold(code, trimmed) {
			return code, true
		}
	}
	if code, ok := choicesMap[trimmed]; ok {
		return code, true
	}

	matched := map[string]bool{}
	best := ""
	for label, code := range choicesMap {
		if strings.Contains(label, trimmed) || strings.Contains(trimmed, label) {
			matched[code] = true
			best = code
		}
	}
	if len(matched) == 1 {
		return best, true
	}
	return "", false
}

// sortedCodes 返回去重排序后的代码值。
//
// 去重是必需的: 同一个代码可以登记多个中文别名 (如 "整个实例" 与 "整实例"),
// 不去重会让 JSON Schema 的 enum 出现重复项。
func (s Slot) sortedCodes() []string {
	seen := map[string]bool{}
	codes := make([]string, 0, len(s.ChoicesMap))
	for _, code := range s.ChoicesMap {
		if seen[code] {
			continue
		}
		seen[code] = true
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// unknownKeys 返回 options 里未被槽位声明的键。
func (d *Driver) unknownKeys(options map[string]any) []string {
	known := map[string]bool{}
	for _, slot := range d.Slots {
		known[slot.Field] = true
	}
	unknown := make([]string, 0, len(options))
	for key := range options {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// ---------------------------------------------------------------------------
// 取值转换 —— 兼容 JSON 解码可能产生的几种表示
// ---------------------------------------------------------------------------

func asString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	default:
		return "", false
	}
}

func asInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func isBlank(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func describe(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("字符串 %q", typed)
	case bool:
		return fmt.Sprintf("布尔值 %v", typed)
	case json.Number:
		return "数字 " + typed.String()
	case map[string]any:
		return "对象"
	case []any:
		return "数组"
	default:
		return fmt.Sprintf("%T", value)
	}
}
