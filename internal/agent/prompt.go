package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// 本文件负责组装提示词。
//
// 两个刻意的做法:
//  1. schema 是从结构体与驱动槽位表动态生成的 (schema.PlanSchema + driver.OptionsSchemas),
//     所以新增引擎时提示词自动跟上, 不需要手改。
//  2. few-shot 示例用 Go 结构体构造而不是硬编码 JSON 字符串 —— 编译器与
//     test_prompt_examples_are_valid 一起保证示例永远不偏离 schema。

// fewShot 是一条示例对话。
type fewShot struct {
	Input string
	Plan  any
}

// fewShotExamples 返回示例集。
//
// 覆盖四种典型形态: 信息完整 / 信息严重缺失 (教它不要编造) /
// 引擎参数丰富 (教它 options 怎么写) / 恢复意图。
func fewShotExamples() []fewShot {
	weekday := 7
	day := 30
	channels := 4

	// 示例的输出必须只包含输入里出现过的信息。曾经这里写了桶名、区域、路径与
	// 备份方式, 而输入里都没有 —— 那等于在教模型编造, 真实模型会照抄。
	mysqlComplete := &schema.BackupPlan{
		SchemaVersion: schema.SchemaVersion,
		Kind:          schema.KindBackup,
		TaskName:      "生产MySQL每日全量备份",
		Source: schema.BackupSource{
			Type: "database",
			Items: []schema.SourceItem{{
				ID: "src-1", Name: "生产MySQL", Kind: "mysql",
				Databases: []string{"*"},
				Options:   map[string]any{"method": "logical", "databases": []string{"*"}},
			}},
		},
		Schedule: schema.Schedule{
			Full: &schema.ScheduleSpec{Frequency: "daily", Time: "02:00", Timezone: "Asia/Shanghai"},
		},
		Retention:    schema.Retention{Full: &schema.RetentionSpec{Value: day, Unit: "days"}},
		Destinations: []schema.Destination{{ID: "dest-1", Type: "aliyun_oss"}},
		Verification: schema.Verification{TestRestore: true, Frequency: "weekly"},
		Confidence:   0.85,
		Warnings:     []string{},
	}

	vague := &schema.BackupPlan{
		SchemaVersion: schema.SchemaVersion,
		Kind:          schema.KindBackup,
		TaskName:      "数据库备份",
		Source: schema.BackupSource{
			Type:  "database",
			Items: []schema.SourceItem{{ID: "src-1", Name: "未明确的数据库", Kind: "other", Options: map[string]any{}}},
		},
		Destinations: []schema.Destination{},
		Confidence:   0.2,
		Warnings:     []string{},
	}

	// 这条示例的输入与验收用例同句: 它只说了范围、认证方式与并发度, 没说时刻、
	// 没说保留期、没说存到哪里 —— 因此输出必须把这三项留空。
	// 曾经这里顺手把 01:00 / 14 天 / 本地路径都填上了, 于是真实模型见到这句话
	// 就直接输出一套完整计划, 从不追问 (demo 客户端不编造, 所以测试全绿,
	// 只有打真实模型才暴露)。
	oracle := &schema.BackupPlan{
		SchemaVersion: schema.SchemaVersion,
		Kind:          schema.KindBackup,
		TaskName:      "Oracle 实例周期性全备+增量备份",
		Source: schema.BackupSource{
			Type: "database",
			Items: []schema.SourceItem{{
				ID: "src-1", Name: "Oracle 实例", Kind: "oracle",
				Options: map[string]any{
					"scope":     "whole_instance",
					"auth_mode": "os_auth",
					"channels":  channels,
				},
			}},
		},
		Schedule: schema.Schedule{
			Full:        &schema.ScheduleSpec{Frequency: "weekly", Weekday: &weekday, Timezone: "Asia/Shanghai"},
			Incremental: &schema.ScheduleSpec{Frequency: "daily", Timezone: "Asia/Shanghai"},
		},
		Destinations: []schema.Destination{},
		Confidence:   0.5,
		Warnings:     []string{},
	}

	restore := &schema.RestorePlan{
		SchemaVersion:   schema.SchemaVersion,
		Kind:            schema.KindRestore,
		TaskName:        "Oracle 实例恢复到故障前时刻",
		Source:          oracle.Source,
		SourceBackupRef: "Oracle 实例周期性全备+增量备份",
		Target:          schema.RestoreTarget{Mode: "in_place"},
		Point:           schema.RestorePoint{Type: "timestamp", Value: "2026-09-25 10:30:00", Timezone: "Asia/Shanghai"},
		Scope:           schema.RestoreScope{Level: "whole", Items: []string{}},
		Alternate:       schema.AlternateLocation{},
		Verification:    schema.RestoreVerification{OpenDatabase: true},
		Confidence:      0.8,
		Warnings:        []string{},
	}

	return []fewShot{
		{Input: "每天凌晨2点对生产环境的MySQL做逻辑备份，全部库都备，上传到阿里云OSS，保留30天，每周五做一次恢复演练", Plan: mysqlComplete},
		{Input: "把数据库的重要数据备份一下", Plan: vague},
		{Input: "周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4", Plan: oracle},
		{Input: "Oracle数据库出故障了，恢复到2026-09-25 10:30:00这个时间点", Plan: restore},
	}
}

// SystemPrompt 组装 system 消息。
func SystemPrompt(kind string, cat catalog.Catalog) (string, error) {
	optionsSchemas := driver.OptionsSchemas()
	planSchema := schema.PlanSchema(kind, optionsSchemas)

	planSchemaJSON, err := schema.MarshalIndentNoEscape(planSchema)
	if err != nil {
		return "", fmt.Errorf("序列化计划 schema 失败: %w", err)
	}

	var builder strings.Builder
	if kind == schema.KindRestore {
		builder.WriteString(`你是一个"数据库与文件恢复需求解析器"。用户会用自然语言描述恢复诉求，你需要把它转换成结构化的 JSON 输出。`)
	} else {
		builder.WriteString(`你是一个"备份需求解析器"。用户会用自然语言描述备份需求，你需要把它转换成结构化的 JSON 输出。`)
	}
	builder.WriteString("\n\n输出要求:\n")
	builder.WriteString(`1. 只输出一个合法的 JSON 对象，不要输出任何解释文字、代码块标记或其他内容。
2. 必须严格符合下面给出的 JSON Schema。枚举字段只能使用 Schema 中定义的取值。
3. 用户没有提到的信息一律留空 (省略该字段、null 或空数组)，绝对不要编造。宁可留空让系统去追问，也不要猜。
4. 不要输出任何"待确认问题"。缺失的信息由系统统一追问，你的职责只是忠实抽取。
5. 描述中模糊、有歧义或自相矛盾之处，写进 warnings 数组，每条一句中文说明。
6. confidence 表示整体置信度 (0~1)：信息完整给 0.8 以上，缺失较多时给低值。
7. 密码、密钥、口令等敏感信息一律不要输出，只填 credential_ref 引用占位 (如 "secret://cred/xxx")。
8. 时间用 24 小时制 "HH:MM"；未指定时区默认 Asia/Shanghai。
9. 引擎专属参数放在 source.items[].options 里，键名必须严格使用该 kind 的 options schema 中定义的键。options 里没有的键不要写。
10. 用户提到"全量/全备"填 schedule.full，提到"增量"填 schedule.incremental，提到"差异"填 schedule.differential。
`)
	builder.WriteString("\n可用的备份对象与目标介质 (来自运行时清单, 请只从中选择):\n")
	builder.WriteString(catalogPromptText(cat))
	builder.WriteString("\n\n术语映射 (请据此把用户的中文说法转成代码值):\n")
	for _, engine := range driver.All() {
		if engine.PromptHints == "" {
			continue
		}
		builder.WriteString("- ")
		builder.WriteString(engine.Display)
		builder.WriteString(": ")
		builder.WriteString(engine.PromptHints)
		builder.WriteString("\n")
	}

	builder.WriteString("\n输出必须符合的 JSON Schema:\n")
	builder.Write(planSchemaJSON)

	builder.WriteString("\n\n示例:\n")
	for _, example := range fewShotExamples() {
		planJSON, err := json.Marshal(example.Plan)
		if err != nil {
			return "", fmt.Errorf("序列化示例失败: %w", err)
		}
		builder.WriteString("输入: ")
		builder.WriteString(example.Input)
		builder.WriteString("\n输出: ")
		builder.Write(planJSON)
		builder.WriteString("\n\n")
	}

	return builder.String(), nil
}

// BuildMessages 组装完整的对话消息。
//
// 分块标记不只是给人看的标题, 客户端 (尤其是 demo) 依赖它定位用户文本, 见 llm.MarkRequirement。
func BuildMessages(kind, requirement, qa, draft string, feedback []string, cat catalog.Catalog) ([]llm.Message, error) {
	system, err := SystemPrompt(kind, cat)
	if err != nil {
		return nil, err
	}

	var user strings.Builder
	// 显式声明本轮要产出哪种计划。真实模型据 system prompt 也能判断,
	// 但写明可避免歧义; demo 客户端则完全依赖这一行。
	user.WriteString(llm.MarkKind)
	user.WriteString(kind)
	user.WriteString("\n\n")
	user.WriteString(llm.MarkRequirement)
	user.WriteString("\n")
	user.WriteString(strings.TrimSpace(requirement))

	if strings.TrimSpace(qa) != "" {
		user.WriteString("\n\n")
		user.WriteString(llm.MarkQA)
		user.WriteString("\n")
		user.WriteString(strings.TrimSpace(qa))
	}
	if strings.TrimSpace(draft) != "" {
		user.WriteString("\n\n")
		user.WriteString(llm.MarkDraft)
		user.WriteString("\n")
		user.WriteString(strings.TrimSpace(draft))
	}
	if len(feedback) > 0 {
		user.WriteString("\n\n")
		user.WriteString(llm.MarkFeedback)
		user.WriteString("\n")
		user.WriteString(strings.Join(feedback, "\n"))
	}

	return []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: user.String()},
	}, nil
}

// catalogPromptText 把运行时清单渲染成提示词里的一段说明。
//
// 这是"能备份哪些应用 / 能备份到哪些介质"变成动态查询后的关键落点: 模型只能从
// 这里列出的取值里挑, 否则它会凭常识编出环境里并不存在的东西。
func catalogPromptText(cat catalog.Catalog) string {
	var builder strings.Builder

	if len(cat.Applications) == 0 {
		builder.WriteString("- 可备份应用: " + driver.KindList() + "\n")
	} else {
		builder.WriteString("- 可备份应用 (source.items[].kind 只能取这些值):\n")
		for _, app := range cat.Applications {
			builder.WriteString("  · " + app.Kind)
			if app.Display != "" && app.Display != app.Kind {
				builder.WriteString("（" + app.Display + "）")
			}
			if len(app.Options) > 0 {
				fields := make([]string, 0, len(app.Options))
				for _, option := range app.Options {
					fields = append(fields, option.Field)
				}
				builder.WriteString("; options 可用键: " + strings.Join(fields, ", "))
			}
			if app.Note != "" {
				builder.WriteString("; " + app.Note)
			}
			builder.WriteString("\n")
		}
	}

	if len(cat.MediaServers) > 0 {
		builder.WriteString("- 可用的目标介质服务器 (destinations 请优先从中选择, " +
			"并把服务器名填进 media_server):\n")
		for _, server := range cat.MediaServers {
			builder.WriteString("  · " + server.Display() + " (type=" + server.Type)
			if server.ID != "" && server.ID != server.Name {
				builder.WriteString(", id=" + server.ID)
			}
			builder.WriteString(")")
			if server.Note != "" {
				builder.WriteString("; " + server.Note)
			}
			builder.WriteString("\n")
		}
	}

	builder.WriteString("- 清单来源: " + catalogSourceText(cat) + "\n")
	return builder.String()
}

// catalogSourceText 说明清单从哪来 (界面与报错都要能讲清楚这一点)。
func catalogSourceText(cat catalog.Catalog) string {
	switch cat.Source {
	case "api":
		return "运行时接口"
	case "builtin-fallback":
		return "内置清单(接口不可用)"
	default:
		return "内置清单"
	}
}
