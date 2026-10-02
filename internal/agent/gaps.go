package agent

import (
	"fmt"
	"strings"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// gapResult 是一次缺口分析的结果。
type gapResult struct {
	Questions []schema.Question
	Warnings  []string
	// Errors 是无法转成追问的残留问题 —— 只能由模型修正, 用户答不了。
	Errors []string
}

// AnalyzeGaps 分析计划缺口并产出待追问的问题。
//
// 这是"追问直到 JSON 完整"的核心。关键设计: 待问的问题完全由确定性检查产出,
// 不由模型自由生成。因此问题集合是有限且单调收敛的 (每填一个槽位就少一个问题),
// 闭环必然终止; 而模型生成的 questions 只作为参考提示, 不进入追问列表。
//
// answered 是本会话已答过的槽位, 用于在问题文案里附上"上轮没听懂"的说明 ——
// 静默重复同一个问题是最让人恼火的交互。
func AnalyzeGaps(kind string, plan any, answers map[string]string, cat catalog.Catalog) gapResult {
	result := gapResult{
		Questions: []schema.Question{},
		Warnings:  []string{},
		Errors:    []string{},
	}

	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		result.Errors = append(result.Errors, "计划无法序列化: "+err.Error())
		return result
	}

	missing := collectMissingSlots(kind, planMap, cat)
	if len(missing) > 0 {
		seen := map[string]bool{}
		for _, path := range missing {
			if seen[path] {
				continue
			}
			seen[path] = true

			meta, ok := LookupSlotMeta(planMap, path, cat)
			if !ok {
				// 没有元信息就无法追问 —— 这类问题只能反馈给模型去修
				result.Errors = append(result.Errors,
					fmt.Sprintf("%s: 缺少取值, 且该字段不支持追问", path))
				continue
			}
			result.Questions = append(result.Questions, buildQuestion(meta, answers))
		}
	}

	return result
}

// collectMissingSlots 收集所有"缺了就不完整"的槽位路径。
//
// 来源有两类, 缺一不可:
//  1. 存在性检查 —— 校验器不认为是错误、但计划明显不完整的字段
//     (如完全没有全量调度、没有保留策略、没有目的地)
//  2. 校验器报出的、可通过追问补齐的错误 (Issue.Slot 非空)
func collectMissingSlots(kind string, planMap map[string]any, cat catalog.Catalog) []string {
	out := []string{}

	items, _ := GetPath(planMap, "source.items")
	itemList, _ := items.([]any)
	if len(itemList) == 0 {
		out = append(out, "source.items")
		return out
	}

	// ---- 备份对象类型 ----
	for index := range itemList {
		path := fmt.Sprintf("source.items[%d].kind", index)
		value, _ := GetPath(planMap, path)
		text, _ := value.(string)
		if strings.TrimSpace(text) == "" {
			out = append(out, path)
		}
	}

	if kind == schema.KindRestore {
		return append(out, collectRestoreGaps(planMap)...)
	}

	// ---- 调度 ----
	out = append(out, collectScheduleGaps(planMap, "full")...)
	if incremental, ok := GetPath(planMap, "schedule.incremental"); ok && incremental != nil {
		out = append(out, collectScheduleGaps(planMap, "incremental")...)
	}

	// ---- 保留 ----
	if !hasRetention(planMap) {
		out = append(out, "retention")
	}

	// ---- 目的地 ----
	destinations, _ := GetPath(planMap, "destinations")
	destinationList, _ := destinations.([]any)
	if len(destinationList) == 0 {
		out = append(out, "destinations")
	}

	// ---- 引擎专属参数 (由驱动的槽位表判定) ----
	for index := range itemList {
		prefix := fmt.Sprintf("source.items[%d]", index)
		kindValue, _ := GetPath(planMap, prefix+".kind")
		kindText, _ := kindValue.(string)
		engine := driver.Get(kindText)

		options := map[string]any{}
		if raw, ok := GetPath(planMap, prefix+".options"); ok {
			if table, ok := raw.(map[string]any); ok {
				options = table
			}
		}
		// 清单接口为应用声明了参数时以它为准 —— 那是环境里真正支持的那些参数;
		// 否则用内置驱动的槽位表。两条路都必须走, 否则"接口声明的必填参数"
		// 永远不会被追问。
		if app, ok := cat.ApplicationFor(kindText); ok && len(app.Options) > 0 {
			for _, option := range app.Options {
				if !option.IsRequired() {
					continue
				}
				path := prefix + ".options." + option.Field
				value, present := GetPath(planMap, path)
				if !present || isBlankValue(value) {
					out = append(out, path)
				}
			}
			continue
		}

		for _, slot := range engine.Slots {
			if slot.Optional {
				continue
			}
			if slot.SatisfiedWhen != nil && slot.SatisfiedWhen(options) {
				continue
			}
			path := prefix + ".options." + slot.Field
			if slot.Target == driver.TargetItem {
				path = prefix + "." + slot.Field
			}
			value, present := GetPath(planMap, path)
			if !present || isBlankValue(value) {
				out = append(out, path)
			}
		}
	}
	return out
}

// collectScheduleGaps 检查某一类调度是否完整。
func collectScheduleGaps(planMap map[string]any, name string) []string {
	prefix := "schedule." + name
	spec, present := GetPath(planMap, prefix)
	if !present || spec == nil {
		if name == "full" {
			return []string{"schedule.full"}
		}
		return nil
	}

	out := []string{}
	frequency, _ := GetPath(planMap, prefix+".frequency")
	frequencyText, _ := frequency.(string)

	// 时刻: 只给了频率没给时刻, 计划还不能真正落地
	if !hasNonEmpty(planMap, prefix+".time") {
		out = append(out, prefix+".time")
	}
	switch frequencyText {
	case "weekly":
		if !hasNonEmpty(planMap, prefix+".weekday") {
			out = append(out, prefix+".weekday")
		}
	case "monthly":
		if !hasNonEmpty(planMap, prefix+".day_of_month") {
			out = append(out, prefix+".day_of_month")
		}
	}
	return out
}

func collectRestoreGaps(planMap map[string]any) []string {
	out := []string{}

	pointType, _ := GetPath(planMap, "point.type")
	pointTypeText, _ := pointType.(string)
	if pointTypeText != "" && pointTypeText != "latest" && !hasNonEmpty(planMap, "point.value") {
		out = append(out, "point.value")
	}

	mode, _ := GetPath(planMap, "target.mode")
	modeText, _ := mode.(string)
	if modeText == "alternate_host" {
		if !hasNonEmpty(planMap, "target.host") {
			out = append(out, "target.host")
		}
		if !hasNonEmpty(planMap, "target.instance") {
			out = append(out, "target.instance")
		}
	}

	level, _ := GetPath(planMap, "scope.level")
	levelText, _ := level.(string)
	if levelText != "" && levelText != "whole" && !hasNonEmpty(planMap, "scope.items") {
		out = append(out, "scope.items")
	}

	return out
}

// hasRetention 判断是否给出了保留策略 (目的地级保留也算)。
func hasRetention(planMap map[string]any) bool {
	for _, path := range []string{
		"retention.full", "retention.incremental", "retention.differential",
	} {
		if value, ok := GetPath(planMap, path); ok && value != nil {
			return true
		}
	}
	destinations, _ := GetPath(planMap, "destinations")
	if list, ok := destinations.([]any); ok {
		for index := range list {
			if value, ok := GetPath(planMap, fmt.Sprintf("destinations[%d].retention", index)); ok && value != nil {
				return true
			}
		}
	}
	return false
}

func hasNonEmpty(planMap map[string]any, path string) bool {
	value, ok := GetPath(planMap, path)
	if !ok {
		return false
	}
	return !isBlankValue(value)
}

func isBlankValue(value any) bool {
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

// buildQuestion 把槽位元信息渲染成一条追问。
func buildQuestion(meta SlotMeta, answers map[string]string) schema.Question {
	hint := meta.Hint
	if previous, asked := answers[meta.Slot]; asked && strings.TrimSpace(previous) != "" {
		// 上一轮答过但没落进计划 —— 说明没听懂, 必须让用户知道
		hint = fmt.Sprintf("上一轮的回答 %q 未能识别, 请换个说法", truncateText(previous, 40))
	}

	question := schema.Question{
		Slot:        meta.Slot,
		Question:    meta.Question,
		Kind:        meta.Kind,
		Required:    true,
		Example:     meta.Example,
		Choices:     meta.Choices,
		Suggestions: outsideChoices(meta.Choices, meta.Suggest),
		Scope:       scopeOf(meta.Slot),
		Hint:        hint,
	}
	if question.Choices == nil {
		question.Choices = []string{}
	}
	if question.Suggestions == nil {
		question.Suggestions = []string{}
	}
	return question
}

// outsideChoices 去掉与封闭候选值重复的推荐值。
//
// 同一个值既出现在 choices 又出现在 suggestions 里, 界面上会显示成两个一模一样的
// 按钮, 看起来像 bug。
func outsideChoices(choices, suggest []string) []string {
	if len(suggest) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, choice := range choices {
		seen[choice] = true
	}
	out := make([]string, 0, len(suggest))
	for _, item := range suggest {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// scopeOf 给问题分组, 让界面能按"调度/保留/目的地"归类展示。
func scopeOf(slot string) string {
	switch {
	case strings.HasPrefix(slot, "source.items") && strings.Contains(slot, ".options."):
		field := slot[strings.LastIndex(slot, ".")+1:]
		return "引擎参数 (" + field + ")"
	case strings.HasPrefix(slot, "source.items"):
		return "备份对象"
	case strings.HasPrefix(slot, "schedule"):
		return "调度"
	case strings.HasPrefix(slot, "retention"):
		return "保留策略"
	case strings.HasPrefix(slot, "destinations"):
		return "存储目的地"
	case strings.HasPrefix(slot, "target"), strings.HasPrefix(slot, "alternate"):
		return "恢复目标"
	case strings.HasPrefix(slot, "point"):
		return "恢复时间点"
	case strings.HasPrefix(slot, "scope"):
		return "恢复范围"
	case slot == "source_backup_ref":
		return "备份来源"
	default:
		return "其他"
	}
}

func truncateText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
