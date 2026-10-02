package agent

import (
	"backup-restore-agent/internal/catalog"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// fakeClient 按预设顺序返回响应, 并记录调用次数与收到的 user 消息。
type fakeClient struct {
	responses []string
	err       error
	calls     int
	users     []string
}

func (f *fakeClient) Name() string { return "fake" }

func (f *fakeClient) Chat(messages []llm.Message) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if len(messages) > 0 {
		f.users = append(f.users, messages[len(messages)-1].Content)
	}
	index := f.calls - 1
	if index >= len(f.responses) {
		index = len(f.responses) - 1
	}
	return f.responses[index], nil
}

// completePlanJSON 返回一份通过全部校验的计划。
func completePlanJSON(t *testing.T, overrides map[string]any) string {
	t.Helper()
	weekday := 7
	plan := map[string]any{
		"schema_version": schema.SchemaVersion,
		"kind":           "backup",
		"task_name":      "测试备份",
		"source": map[string]any{
			"type": "database",
			"items": []any{map[string]any{
				"id": "src-1", "name": "MySQL", "kind": "mysql",
				"databases": []any{"*"},
				"options":   map[string]any{"method": "logical", "databases": []any{"*"}},
			}},
		},
		"schedule": map[string]any{
			"full": map[string]any{
				"frequency": "weekly", "time": "02:00", "weekday": weekday,
				"timezone": "Asia/Shanghai",
			},
		},
		"retention":    map[string]any{"full": map[string]any{"value": 30, "unit": "days"}},
		"destinations": []any{map[string]any{"id": "dest-1", "type": "local", "path": "/backup"}},
		"confidence":   0.9,
	}
	for key, value := range overrides {
		plan[key] = value
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("构造用例计划失败: %v", err)
	}
	return string(raw)
}

func TestExtractSucceedsOnFirstAttempt(t *testing.T) {
	client := &fakeClient{responses: []string{completePlanJSON(t, nil)}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "每天凌晨2点备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if client.calls != 1 {
		t.Errorf("应只调用一次, 实际 %d", client.calls)
	}
	if result.Plan == nil {
		t.Fatal("应返回计划")
	}
	if _, ok := result.Plan.(*schema.BackupPlan); !ok {
		t.Fatalf("计划类型不对: %T", result.Plan)
	}
}

// 模型偶尔会带上代码块围栏或解释文字, 应能抢救出 JSON。
func TestExtractSalvagesWrappedOutput(t *testing.T) {
	wrapped := "好的, 结果如下:\n```json\n" + completePlanJSON(t, nil) + "\n```\n以上。"
	client := &fakeClient{responses: []string{wrapped}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("应能抢救出 JSON: %v", err)
	}
	if client.calls != 1 {
		t.Errorf("抢救成功不该重试, 实际调用 %d 次", client.calls)
	}
	if result.Plan == nil {
		t.Fatal("应返回计划")
	}
}

func TestExtractRetriesOnUnparseableOutput(t *testing.T) {
	client := &fakeClient{responses: []string{
		"这不是 JSON",
		completePlanJSON(t, nil),
	}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("第二次应成功: %v", err)
	}
	if client.calls != 2 {
		t.Errorf("应重试到第二次成功, 实际 %d 次", client.calls)
	}
	if len(result.Notices) == 0 {
		t.Error("应记录重试过程")
	}
	if len(client.users) < 2 || !strings.Contains(client.users[1], "无法解析") {
		t.Errorf("重试时应把失败原因告诉模型: %v", client.users)
	}
}

// 三次都拿不到可解析计划才算彻底失败。
func TestExtractGivesUpAfterMaxAttempts(t *testing.T) {
	client := &fakeClient{responses: []string{"坏输出一"}}

	_, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err == nil {
		t.Fatal("应返回错误")
	}
	var extraction *ExtractionError
	if !errors.As(err, &extraction) {
		t.Fatalf("错误类型不对: %T", err)
	}
	if extraction.Attempts != MaxAttempts {
		t.Errorf("尝试次数应为 %d, 实际 %d", MaxAttempts, extraction.Attempts)
	}
	if client.calls != MaxAttempts {
		t.Errorf("调用次数应为 %d, 实际 %d", MaxAttempts, client.calls)
	}
	if !strings.Contains(extraction.Error(), "换") &&
		!strings.Contains(extraction.Error(), "模型") {
		t.Errorf("报错应给出可操作建议: %s", extraction.Error())
	}
}

// 结构性问题 (枚举非法/类型不符) 是模型的锅, 应重试。
func TestExtractRetriesOnStructuralErrors(t *testing.T) {
	client := &fakeClient{responses: []string{
		completePlanJSON(t, map[string]any{"confidence": "很高"}),
		completePlanJSON(t, nil),
	}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("第二次应成功: %v", err)
	}
	if client.calls != 2 {
		t.Errorf("结构错误应重试, 实际调用 %d 次", client.calls)
	}
	if result.Plan == nil {
		t.Fatal("应返回计划")
	}
}

// 字段缺失只重试一次 (确认不是漏读), 之后交给追问, 不浪费调用。
//
// 用"每周却没给星期几"来触发: 这是校验器会判错的字段缺失 (Issue.Slot 非空),
// 代表"用户可能说了但模型漏读"。而"目的地为空"之类属于缺口, 由缺口分析负责,
// 根本不走重试路径 —— 两者刻意分开, 避免重复追问。
func TestExtractRetriesMissingFieldsOnlyOnce(t *testing.T) {
	weeklyWithoutWeekday := completePlanJSON(t, map[string]any{
		"schedule": map[string]any{
			"full": map[string]any{
				"frequency": "weekly", "time": "02:00", "timezone": "Asia/Shanghai",
			},
		},
	})
	client := &fakeClient{responses: []string{weeklyWithoutWeekday}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "每周日备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("有可解析计划时不该报错: %v", err)
	}
	if client.calls != 2 {
		t.Errorf("字段缺失应重试一次 (共 2 次调用), 实际 %d 次", client.calls)
	}
	if result.Plan == nil {
		t.Fatal("应返回已有计划供继续追问")
	}
	if len(result.Notices) == 0 {
		t.Error("应说明为何重试")
	}
	if len(client.users) < 2 || !strings.Contains(client.users[1], "再仔细读") {
		t.Errorf("重试提示应要求复查需求文本: %v", client.users)
	}
	// 仍缺星期几时, 必须把该问题标记为可追问
	hasWeekdayIssue := false
	for _, issue := range result.Issues {
		if issue.Slot == "schedule.full.weekday" {
			hasWeekdayIssue = true
		}
	}
	if !hasWeekdayIssue {
		t.Error("未补齐的 weekday 应保留为可追问项")
	}
}

// 用户确实没提的必填选项, 不该被反复追问式重试耗尽尝试次数。
func TestExtractMissingDriverSlotStillReturnsPlan(t *testing.T) {
	// 去掉 oracle 的 auth_mode / channels —— 用户没说, 模型也填不出来
	withoutOptions := completePlanJSON(t, map[string]any{
		"source": map[string]any{
			"type": "database",
			"items": []any{map[string]any{
				"id": "src-1", "name": "Oracle", "kind": "oracle",
				"options": map[string]any{"scope": "whole_instance"},
			}},
		},
	})
	client := &fakeClient{responses: []string{withoutOptions}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份oracle", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if result.Plan == nil {
		t.Fatal("应返回计划")
	}
	// 缺失项必须带槽位标记, 供上层转成追问
	hasSlotIssue := false
	for _, issue := range result.Issues {
		if issue.Slot != "" {
			hasSlotIssue = true
		}
	}
	if !hasSlotIssue {
		t.Error("缺失的引擎参数应带槽位标记, 才能转成追问")
	}
}

// 调用失败 (Key 错/限流/连不上) 重试也一样, 应立刻上报。
func TestExtractDoesNotRetryClientErrors(t *testing.T) {
	client := &fakeClient{err: llm.NewError(llm.ErrAuth, "认证失败", "")}

	_, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if client.calls != 1 {
		t.Errorf("调用类错误不该重试, 实际调用 %d 次", client.calls)
	}
	if llm.AsError(err).Kind != llm.ErrAuth {
		t.Errorf("错误类型应原样透出: %v", err)
	}
}

// 模型写了未知字段时, 报错要指明字段名, 模型才能自己修。
func TestExtractFeedbackNamesUnknownField(t *testing.T) {
	bad := completePlanJSON(t, map[string]any{"backup_time": "02:00"})
	client := &fakeClient{responses: []string{bad, completePlanJSON(t, nil)}}

	if _, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""}); err != nil {
		t.Fatalf("第二次应成功: %v", err)
	}
	if len(client.users) < 2 || !strings.Contains(client.users[1], "backup_time") {
		t.Errorf("反馈应指明未知字段名: %v", client.users)
	}
}

// schema_version 与 kind 由后端统一填, 模型写错不该导致失败。
func TestExtractNormalizesSchemaVersionAndKind(t *testing.T) {
	wrong := completePlanJSON(t, map[string]any{
		"schema_version": "9.9", "kind": "whatever",
	})
	client := &fakeClient{responses: []string{wrong}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	plan := result.Plan.(*schema.BackupPlan)
	if plan.SchemaVersion != schema.SchemaVersion {
		t.Errorf("schema_version 应被规范为 %s, 实际 %q",
			schema.SchemaVersion, plan.SchemaVersion)
	}
	if plan.Kind != schema.KindBackup {
		t.Errorf("kind 应被规范为 backup, 实际 %q", plan.Kind)
	}
}

func TestExtractRestoreKind(t *testing.T) {
	restorePlan := map[string]any{
		"schema_version": schema.SchemaVersion,
		"kind":           "restore",
		"task_name":      "恢复测试",
		"source": map[string]any{
			"type": "database",
			"items": []any{map[string]any{
				"id": "src-1", "name": "Oracle", "kind": "oracle",
				"options": map[string]any{},
			}},
		},
		"target":     map[string]any{"mode": "in_place"},
		"point":      map[string]any{"type": "latest", "timezone": "Asia/Shanghai"},
		"scope":      map[string]any{"level": "whole", "items": []any{}},
		"confidence": 0.8,
	}
	raw, _ := json.Marshal(restorePlan)
	client := &fakeClient{responses: []string{string(raw)}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindRestore, Requirement: "恢复Oracle", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	if _, ok := result.Plan.(*schema.RestorePlan); !ok {
		t.Fatalf("计划类型不对: %T", result.Plan)
	}
}

// 未注册的引擎不该中断流程, 只给提示。
func TestValidatePlanWarnsUnregisteredEngine(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "达梦备份"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "达梦", Kind: "dameng", Options: map[string]any{},
	}}

	report := ValidatePlan(plan, catalog.Builtin())
	if len(report.Errors()) != 0 {
		t.Errorf("未注册引擎不该报错: %v", report.Messages(schema.LevelError))
	}
	warnings := strings.Join(report.Messages(schema.LevelWarning), "\n")
	if !strings.Contains(warnings, "dameng") {
		t.Errorf("应提示引擎未注册: %s", warnings)
	}
}

func TestValidatePlanChecksDriverOptions(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "Oracle 备份"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 99},
	}}

	messages := strings.Join(ValidatePlan(plan, catalog.Builtin()).Messages(schema.LevelError), "\n")
	if !strings.Contains(messages, "channels") {
		t.Fatalf("应报出 channels 越界: %s", messages)
	}
	if !strings.Contains(messages, "不能大于") {
		t.Errorf("报错应说明具体越界: %s", messages)
	}
}

// 模型不会写 min_copies, 但交付物里出现 "min_copies": 0 等于"最少保留 0 份副本"。
func TestExtractNormalizesMinCopies(t *testing.T) {
	client := &fakeClient{responses: []string{completePlanJSON(t, nil)}}

	result, err := Extract(client, ExtractRequest{Kind: schema.KindBackup, Requirement: "每天凌晨2点备份MySQL", QA: "", Draft: ""})
	if err != nil {
		t.Fatalf("抽取失败: %v", err)
	}
	plan := result.Plan.(*schema.BackupPlan)
	if plan.Retention.MinCopies < 1 {
		t.Fatalf("min_copies 应被补齐为 >= 1, 实际 %d", plan.Retention.MinCopies)
	}
}

// 模型与引擎各自发现同一个风险时, 措辞不同也该合并成一条。
func TestMergeWarningsDedupesByTopic(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "MySQL", Kind: "mysql",
		Options: map[string]any{"method": "logical", "databases": []string{"*"}},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "daily", Time: "02:00", Timezone: "Asia/Shanghai"}
	plan.Schedule.Incremental = &schema.ScheduleSpec{Frequency: "hourly", Time: "02:00", Timezone: "Asia/Shanghai"}
	plan.Retention.Full = &schema.RetentionSpec{Value: 7, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local"}}
	// 模型自己先写了一条, 覆盖了引擎会在意的同一件事
	plan.Warnings = []string{"已配置增量备份, 但未单独指定增量保留策略, 暂按全量保留 7 天处理"}

	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	report := ValidatePlan(plan, catalog.Builtin())
	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	warnings := mergeWarnings(planMap, report, gaps, &Session{})

	count := 0
	for _, warning := range warnings {
		if strings.Contains(warning, "未单独指定增量保留策略") {
			count++
		}
	}
	if count > 1 {
		t.Fatalf("同一件事应只保留一条, 实际 %d 条: %v", count, warnings)
	}
	if count == 0 {
		t.Fatalf("该风险仍应被提示: %v", warnings)
	}
}

func TestWarningTopic(t *testing.T) {
	cases := map[string]string{
		"已配置增量备份, 但未单独指定增量保留策略": "已配置增量备份",
		"未提及 RPO/RTO，建议明确":      "未提及 RPO/RTO",
		"只有一个备份目的地; 建议增加异地副本":   "只有一个备份目的地",
		"没有任何分隔符的短句":            "没有任何分隔符的短句",
	}
	for input, want := range cases {
		if got := warningTopic(input); got != want {
			t.Errorf("warningTopic(%q) = %q, 期望 %q", input, got, want)
		}
	}
}
