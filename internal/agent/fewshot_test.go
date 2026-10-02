package agent

import (
	"strings"
	"testing"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// 示例对话里的输出必须只包含输入里出现过的信息。
//
// 这条为什么必须有: few-shot 示例里的"输入 -> 输出"会被模型当作"看到这句就这么填"。
// 曾经 oracle 那个示例的输入正是验收用例的原句, 而输出为了展示引擎参数把时刻、保留期、
// 目的地一并填了 —— 结果真实模型见到那句话就直接输出一套完整计划, 从不追问。
// demo 客户端不编造, 所以单元测试全绿, 只有打真实模型才暴露。
//
// 因此这里机械地检查: 示例输出里每一个有值的字段, 都能在输入文本里找到出处。
func TestFewShotExamplesDoNotFabricate(t *testing.T) {
	examples := fewShotExamples()
	if len(examples) == 0 {
		t.Fatal("没有示例")
	}

	for _, example := range examples {
		input := example.Input

		plan, ok := example.Plan.(*schema.BackupPlan)
		if !ok {
			continue // 恢复计划的检查见下
		}

		// ---- 调度 ----
		for name, spec := range map[string]*schema.ScheduleSpec{
			"全量": plan.Schedule.Full, "增量": plan.Schedule.Incremental, "差异": plan.Schedule.Differential,
		} {
			if spec == nil {
				continue
			}
			if spec.Time != "" {
				assertClockMentioned(t, input, name+"备份时刻", spec.Time)
			}
			if spec.Frequency == "weekly" && spec.Weekday != nil {
				assertWeekdayMentioned(t, input, name+"备份星期", *spec.Weekday)
			}
		}

		// ---- 保留期 ----
		if spec := plan.Retention.Full; spec != nil {
			assertMentioned(t, input, "保留期数值", itoa(spec.Value))
			assertMentioned(t, input, "保留期单位", unitKeyword(spec.Unit))
		}
		if spec := plan.Retention.Incremental; spec != nil {
			assertMentioned(t, input, "增量保留期数值", itoa(spec.Value))
		}

		// ---- 目的地 ----
		for _, destination := range plan.Destinations {
			if destination.Path != "" {
				assertMentioned(t, input, "备份路径", destination.Path)
			}
			if destination.Bucket != "" {
				assertMentioned(t, input, "桶名", destination.Bucket)
			}
			// 目的地类型要在输入里有对应说法
			assertEnumMentioned(t, input, "目的地类型", "destinations", destination.Type)
		}

		// ---- 引擎参数 ----
		for _, item := range plan.Source.Items {
			engine, exists := driver.Find(item.Kind)
			if !exists {
				continue
			}
			for _, slot := range engine.Slots {
				value, present := item.Options[slot.Field]
				if !present || len(slot.ChoicesMap) == 0 {
					continue
				}
				code, isText := value.(string)
				if !isText {
					continue
				}
				assertEnumMentioned(t, input, "参数 "+slot.Field, engine.Kind, code)
			}
			// 整实例这类会写进 databases 的取值
			for _, database := range item.Databases {
				if database == "*" {
					assertAnyMentioned(t, input, "整实例标记",
						[]string{"整个实例", "整实例", "全库", "全部库", "所有库", "整个库"})
				}
			}
		}

		// ---- 恢复演练 ----
		if plan.Verification.TestRestore {
			assertAnyMentioned(t, input, "恢复演练",
				[]string{"演练", "验证", "校验", "试恢复"})
		}
	}
}

// 恢复计划的示例同样要经得住检查。
func TestRestoreFewShotDoesNotFabricate(t *testing.T) {
	for _, example := range fewShotExamples() {
		plan, ok := example.Plan.(*schema.RestorePlan)
		if !ok {
			continue
		}
		input := example.Input

		if plan.Point.Value != "" {
			assertMentioned(t, input, "恢复时间点", plan.Point.Value)
		}
		if plan.Target.Host != "" {
			assertMentioned(t, input, "目标主机", plan.Target.Host)
		}
		if plan.SourceBackupRef != "" {
			t.Logf("备份来源引用 %q 是概括性描述, 不要求逐字出现", plan.SourceBackupRef)
		}
	}
}

// ---------------------------------------------------------------------------
// 断言小工具
// ---------------------------------------------------------------------------

func assertMentioned(t *testing.T, input, label, value string) {
	t.Helper()
	if strings.Contains(input, value) {
		return
	}
	t.Errorf("示例输出里的 %s=%q 在输入里找不到出处 —— 这是在教模型编造。输入: %q",
		label, value, input)
}

func assertAnyMentioned(t *testing.T, input, label string, candidates []string) {
	t.Helper()
	for _, candidate := range candidates {
		if strings.Contains(input, candidate) {
			return
		}
	}
	t.Errorf("示例输出里的 %s 在输入里找不到对应说法 (任选其一: %v)。输入: %q",
		label, candidates, input)
}

// assertClockMentioned 时刻可以写成 "01:00" 或 "凌晨1点"。
func assertClockMentioned(t *testing.T, input, label, clock string) {
	t.Helper()
	if strings.Contains(input, clock) {
		return
	}
	hour, minute := clock[:2], clock[3:]
	if strings.Contains(input, itoa(atoiSafe(hour))+"点") && minute == "00" {
		return
	}
	t.Errorf("示例输出里的 %s=%q 在输入里找不到出处。输入: %q", label, clock, input)
}

// assertWeekdayMentioned 星期几要在输入里有对应说法。
func assertWeekdayMentioned(t *testing.T, input, label string, weekday int) {
	t.Helper()
	names := map[int]string{1: "一", 2: "二", 3: "三", 4: "四", 5: "五", 6: "六", 7: "日"}
	day := names[weekday]
	for _, prefix := range []string{"周", "星期", "礼拜", "每"} {
		if strings.Contains(input, prefix+day) {
			return
		}
	}
	if weekday == 7 && strings.Contains(input, "周天") {
		return
	}
	t.Errorf("示例输出里的 %s=周%s 在输入里找不到出处。输入: %q", label, day, input)
}

// assertEnumMentioned 代码值要在输入里找到对应的中文说法。
func assertEnumMentioned(t *testing.T, input, label, kind, code string) {
	t.Helper()
	engine, exists := driver.Find(kind)
	if !exists {
		return
	}
	for _, slot := range engine.Slots {
		for text, value := range slot.ChoicesMap {
			if value != code {
				continue
			}
			// 标签里取最长的连续中文片段做关键词 (括号里的补充不一定出现在需求里)
			for _, fragment := range chineseFragments(text) {
				if len([]rune(fragment)) >= 3 && strings.Contains(input, fragment) {
					return
				}
			}
		}
	}
	if kind == "destinations" {
		// 目的地类型的中文标签在 catalog 里, 它才是这套词汇的所有者
		for _, fragment := range chineseFragments(catalog.DestTypeLabel(code)) {
			if len([]rune(fragment)) >= 2 && strings.Contains(input, fragment) {
				return
			}
		}
	}
	t.Errorf("示例输出里的 %s=%q 在输入里找不到对应说法。输入: %q", label, code, input)
}

// chineseFragments 把一段文本按非中文字符切开, 取出中文片段。
func chineseFragments(text string) []string {
	out := []string{}
	var current []rune
	for _, char := range text {
		if char >= 0x4e00 && char <= 0x9fff {
			current = append(current, char)
			continue
		}
		if len(current) > 0 {
			out = append(out, string(current))
			current = nil
		}
	}
	if len(current) > 0 {
		out = append(out, string(current))
	}
	return out
}

func unitKeyword(unit string) string {
	switch unit {
	case "days":
		return "天"
	case "weeks":
		return "周"
	case "months":
		return "月"
	case "years":
		return "年"
	case "hours":
		return "小时"
	}
	return unit
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func atoiSafe(text string) int {
	value := 0
	for _, char := range text {
		if char < '0' || char > '9' {
			return value
		}
		value = value*10 + int(char-'0')
	}
	return value
}
