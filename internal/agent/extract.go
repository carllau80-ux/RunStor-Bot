package agent

import (
	"fmt"
	"sort"
	"strings"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// MaxAttempts 是单次抽取的最大尝试次数。
const MaxAttempts = 3

// ExtractionError 表示连一次可解析的计划都没拿到。
type ExtractionError struct {
	Message  string
	Attempts int
}

func (e *ExtractionError) Error() string { return e.Message }

// ExtractResult 是一次抽取的产出。
type ExtractResult struct {
	Plan     any
	Issues   []schema.Issue
	Notices  []string
	Attempts int
}

// ResidualErrors 返回无法由用户补充、只能由模型修正的残留错误。
func (r ExtractResult) ResidualErrors() []schema.Issue {
	out := []schema.Issue{}
	for _, issue := range r.Issues {
		if issue.Level == schema.LevelError && issue.Slot == "" {
			out = append(out, issue)
		}
	}
	return out
}

// ValidatePlan 做完整校验: 通用结构/枚举 + 各源对象的引擎专属参数 + 清单一致性。
func ValidatePlan(plan any, cat catalog.Catalog) schema.Report {
	report := schema.Validate(plan)

	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		return report
	}
	items, _ := GetPath(planMap, "source.items")
	list, _ := items.([]any)
	for index := range list {
		kind := itemKindOf(planMap, index)
		engine := driver.Get(kind)

		options := map[string]any{}
		if raw, ok := GetPath(planMap, fmt.Sprintf("source.items[%d].options", index)); ok {
			if table, ok := raw.(map[string]any); ok {
				options = table
			}
		}
		prefix := fmt.Sprintf("source.items[%d].options", index)
		// 清单接口给了该应用的参数定义时, 按清单校验; 否则按内置驱动的槽位表
		if app, ok := cat.ApplicationFor(kind); ok && len(app.Options) > 0 {
			report.Issues = append(report.Issues, validateCatalogOptions(app, options, prefix).Issues...)
		} else {
			report.Issues = append(report.Issues, engine.ValidateOptions(options, prefix).Issues...)
		}

		// 清单里没有这个应用时给提示: 要么是清单接口没覆盖, 要么是抽错了对象类型
		if len(cat.Applications) > 0 && kind != "" && kind != "other" {
			if _, known := cat.ApplicationFor(kind); !known {
				report.AddWarning(fmt.Sprintf("source.items[%d].kind", index),
					"应用 %q 不在当前清单中 (清单来源: %s), 请确认该对象确实可备份",
					kind, catalogSourceText(cat))
			}
		}
		// 未注册的引擎给提示, 但不阻塞 —— 用户可能就是要备一个我们还没写驱动的库
		if !driver.Registered(kind) && kind != "other" && strings.TrimSpace(kind) != "" && !cat.FromAPI() {
			report.AddWarning(fmt.Sprintf("source.items[%d].kind", index),
				"引擎 %q 尚未注册驱动, 其专属参数不会被校验", kind)
		}
	}
	return report
}

// Extract 调模型抽取计划, 并按错误性质决定是否重试。
//
// 重试策略 (这是本函数最关键的决策):
//
//	结构性错误 (类型不符/枚举非法/未知字段/JSON 不合法)  -> 是模型的锅, 重试
//	字段缺失 (Issue.Slot 非空, 用户可补)                  -> 先重试一次确认不是模型漏读,
//	                                                     再重试无果就交给追问, 不再浪费时间
//
// 区分这两类的价值: 用户没提到的字段永远缺席, 若一律重试就是每轮白打三次模型;
// 而模型漏读用户明说过的信息又确实值得再试一次。
//
// 三次都拿不到可解析的计划才返回 ExtractionError; 只要能解析出计划, 就返回它
// 连同残留问题 —— 让上层决定是追问还是报错, 而不是在这里把流程判死。
// ExtractRequest 是一次抽取的全部输入。
//
// 用结构体而不是一长串位置参数: 输入项还会增加 (清单、上下文…), 位置参数很快就会
// 变成"第 4 个 string 是哪个"的猜谜游戏。
type ExtractRequest struct {
	Kind        string
	Requirement string
	QA          string
	Draft       string
	// Catalog 是本轮的应用与介质清单, 决定提示词里给出哪些可选值。
	Catalog catalog.Catalog
}

func Extract(client llm.Client, req ExtractRequest) (*ExtractResult, error) {
	kind := req.Kind
	var (
		feedback       []string
		bestPlan       any
		bestIssues     []schema.Issue
		notices        []string
		missingRetried bool
	)

	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		messages, err := BuildMessages(kind, req.Requirement, req.QA, req.Draft, feedback, req.Catalog)
		if err != nil {
			return nil, fmt.Errorf("组装提示词失败: %w", err)
		}

		raw, err := client.Chat(messages)
		if err != nil {
			// 调用失败不重试 (Key 错、限流、连不上, 重试也一样), 直接上报
			return nil, err
		}

		plan, parseErr := parsePlan(kind, raw)
		if parseErr != nil {
			feedback = []string{
				fmt.Sprintf("第 %d 次输出无法解析为合法计划: %v", attempt, parseErr),
				"请只输出一个 JSON 对象, 不要包含解释文字或代码块标记。",
			}
			notices = append(notices, fmt.Sprintf("第 %d 次输出解析失败, 已要求模型修正", attempt))
			continue
		}

		report := ValidatePlan(plan, req.Catalog)
		structural := issuesWithoutSlot(report.Errors())
		missing := issuesWithSlot(report.Errors())

		if len(structural) == 0 && len(missing) == 0 {
			return &ExtractResult{
				Plan: plan, Issues: report.Issues, Notices: notices, Attempts: attempt,
			}, nil
		}

		// 记住最好的一次结果: 有计划总比没有强, 缺失的字段可以追问
		bestPlan, bestIssues = plan, report.Issues

		if len(structural) > 0 {
			feedback = append([]string{
				fmt.Sprintf("第 %d 次输出未通过校验:", attempt),
			}, report.Messages(schema.LevelError)...)
			notices = append(notices, fmt.Sprintf("第 %d 次输出有 %d 处校验错误, 已要求模型修正",
				attempt, len(structural)))
			continue
		}

		// 只剩字段缺失: 重试一次确认不是漏读, 再缺就交给追问
		if !missingRetried {
			missingRetried = true
			feedback = append([]string{
				fmt.Sprintf("第 %d 次输出缺少以下字段。用户的需求文本里可能已经提到, 请再仔细读一遍;",
					attempt),
				"确实没有提到的, 保持为空, 不要编造:",
			}, report.Messages(schema.LevelError)...)
			notices = append(notices, "字段缺失, 已要求模型复查需求文本")
			continue
		}

		return &ExtractResult{
			Plan: plan, Issues: report.Issues, Notices: notices, Attempts: attempt,
		}, nil
	}

	if bestPlan != nil {
		notices = append(notices, fmt.Sprintf(
			"已达最大尝试次数 (%d), 返回当前可用结果供继续追问", MaxAttempts))
		return &ExtractResult{
			Plan: bestPlan, Issues: bestIssues, Notices: notices, Attempts: MaxAttempts,
		}, nil
	}

	return nil, &ExtractionError{
		Message: fmt.Sprintf(
			"连续 %d 次未能从模型输出中解析出计划。请检查模型是否支持 JSON 输出, 或改用能力更强的模型",
			MaxAttempts),
		Attempts: MaxAttempts,
	}
}

// parsePlan 把模型输出解析成对应类型的计划。
func parsePlan(kind string, raw string) (any, error) {
	decoded, err := schema.JSONDecode(raw)
	if err != nil {
		return nil, err
	}
	// 统一 schema_version 与 kind, 免得模型写成别的值导致校验失败
	decoded["schema_version"] = schema.SchemaVersion
	decoded["kind"] = kind

	plan, err := schema.PlanFromMap(kind, decoded)
	if err != nil {
		return nil, err
	}
	normalizePlan(plan)
	return plan, nil
}

// normalizePlan 补齐模型没提到、但交付物里不该留空的字段。
//
// 为什么要单独补: JSON 里只要出现了父对象 (如 "retention"), 反序列化就会整个替换
// 那个结构体, NewBackupPlan 里设的默认值随之丢失。模型不会写 min_copies, 于是
// 交付的 JSON 里会出现 "min_copies": 0 —— 语义上等于"最少保留 0 份副本"。
func normalizePlan(plan any) {
	backup, ok := plan.(*schema.BackupPlan)
	if !ok {
		return
	}
	if backup.Retention.MinCopies < 1 {
		backup.Retention.MinCopies = 1
	}
}

func issuesWithSlot(issues []schema.Issue) []schema.Issue {
	out := []schema.Issue{}
	for _, issue := range issues {
		if issue.Slot != "" {
			out = append(out, issue)
		}
	}
	return out
}

func issuesWithoutSlot(issues []schema.Issue) []schema.Issue {
	out := []schema.Issue{}
	for _, issue := range issues {
		if issue.Slot == "" {
			out = append(out, issue)
		}
	}
	return out
}

// validateCatalogOptions 按清单接口给出的参数定义校验 options。
//
// 取值语义复用驱动的 Slot.ValidateValue: 同一份配置不该在一处通过、在另一处被拒。
func validateCatalogOptions(app catalog.Application, options map[string]any, prefix string) schema.Report {
	var report schema.Report
	if prefix != "" {
		prefix += "."
	}

	known := map[string]bool{}
	for _, option := range app.Options {
		known[option.Field] = true
		path := prefix + option.Field

		value, present := options[option.Field]
		if !present || isBlankValue(value) {
			if option.IsRequired() {
				// 交给缺口分析转成追问 (Slot 非空即表示用户可补)
				report.AddError(path, path, "缺少 %s", option.Field)
			}
			continue
		}

		slot := driver.Slot{
			Field:      option.Field,
			Kind:       driver.SlotKind(option.Kind),
			ChoicesMap: option.ChoicesMap,
			Min:        option.Min,
			Max:        option.Max,
		}
		if problem := slot.ValidateValue(value); problem != "" {
			report.AddError(path, path, "%s", problem)
		}
	}

	unknown := make([]string, 0, len(options))
	for key := range options {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		report.AddWarning(prefix+"options",
			"以下参数不在清单为应用 %s 声明的参数中: %s",
			app.Kind, strings.Join(unknown, ", "))
	}
	return report
}
