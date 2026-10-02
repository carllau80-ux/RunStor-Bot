package agent

import (
	"backup-restore-agent/internal/catalog"
	"encoding/json"
	"strings"
	"testing"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// ---------------------------------------------------------------------------
// 路径存取
// ---------------------------------------------------------------------------

func TestPathRoundTrip(t *testing.T) {
	data := map[string]any{
		"source": map[string]any{
			"items": []any{
				map[string]any{
					"kind":    "oracle",
					"options": map[string]any{"channels": 4},
				},
			},
		},
	}

	value, ok := GetPath(data, "source.items[0].options.channels")
	if !ok || value != 4 {
		t.Fatalf("读取失败: %v %v", value, ok)
	}

	if err := SetPath(data, "source.items[0].options.channels", 8); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	value, _ = GetPath(data, "source.items[0].options.channels")
	if value != 8 {
		t.Fatalf("写入未生效: %v", value)
	}
}

func TestGetPathMissingReturnsFalse(t *testing.T) {
	data := map[string]any{"a": map[string]any{"b": 1}}
	for _, path := range []string{
		"a.c", "x.y", "a.b.c", "a[0]", "", "!!!",
	} {
		if _, ok := GetPath(data, path); ok {
			t.Errorf("路径 %q 不该取到值", path)
		}
	}
}

func TestSetPathMissingParentReportsError(t *testing.T) {
	data := map[string]any{"a": map[string]any{}}
	if err := SetPath(data, "a.b.c", 1); err == nil {
		t.Fatal("父级不存在时应报错, 而不是凭空创建")
	}
}

func TestSetPathOutOfRangeIndex(t *testing.T) {
	data := map[string]any{"items": []any{map[string]any{"x": 1}}}
	if err := SetPath(data, "items[5].x", 2); err == nil {
		t.Fatal("越界索引应报错")
	}
}

// ---------------------------------------------------------------------------
// 槽位元信息查找
// ---------------------------------------------------------------------------

func oraclePlanMap(t *testing.T) map[string]any {
	t.Helper()
	plan := schema.NewBackupPlan()
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{},
	}}
	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return planMap
}

// 驱动槽位: 由 kind 查驱动表, 问句与候选值都来自驱动定义。
func TestLookupSlotMetaForDriverSlot(t *testing.T) {
	planMap := oraclePlanMap(t)
	meta, ok := LookupSlotMeta(planMap, "source.items[0].options.auth_mode", catalog.Builtin())
	if !ok {
		t.Fatal("未找到驱动槽位")
	}
	if meta.Kind != string(driver.SlotEnum) {
		t.Errorf("取值类型不符: %q", meta.Kind)
	}
	if len(meta.ChoicesMap) == 0 {
		t.Error("应带出候选值映射")
	}
	if !strings.Contains(meta.Question, "认证") {
		t.Errorf("问句不符: %q", meta.Question)
	}
}

// 不同驱动对同一个字段名可以有不同的问句与类型。
func TestLookupSlotMetaRespectsDriverKind(t *testing.T) {
	planMap := oraclePlanMap(t)
	planMap["source"].(map[string]any)["items"].([]any)[0].(map[string]any)["kind"] = "mysql"

	meta, ok := LookupSlotMeta(planMap, "source.items[0].options.method", catalog.Builtin())
	if !ok {
		t.Fatal("未找到 MySQL method 槽位")
	}
	if !strings.Contains(meta.Question, "MySQL") {
		t.Errorf("问句应来自 MySQL 驱动: %q", meta.Question)
	}

	// Oracle 没有 method 这个槽位
	if _, ok := LookupSlotMeta(planMap, "source.items[0].options.auth_mode", catalog.Builtin()); ok {
		t.Error("kind 改成 mysql 后不该还能查到 oracle 的槽位")
	}
}

func TestLookupSlotMetaForGenericSlot(t *testing.T) {
	planMap := oraclePlanMap(t)
	meta, ok := LookupSlotMeta(planMap, "schedule.full.time", catalog.Builtin())
	if !ok {
		t.Fatal("未找到通用槽位")
	}
	if meta.Kind != string(driver.SlotTime) {
		t.Errorf("取值类型不符: %q", meta.Kind)
	}
}

func TestLookupSlotMetaUnknownPath(t *testing.T) {
	planMap := oraclePlanMap(t)
	if _, ok := LookupSlotMeta(planMap, "no.such.slot", catalog.Builtin()); ok {
		t.Error("未知路径不该返回元信息")
	}
}

// ---------------------------------------------------------------------------
// 答案解析
// ---------------------------------------------------------------------------

func TestParseAnswerByKind(t *testing.T) {
	cases := []struct {
		slot string
		text string
		want any
	}{
		{"schedule.full.time", "凌晨1点", "01:00"},
		{"schedule.full.time", "23:30", "23:30"},
		{"schedule.full.weekday", "周日", 7},
		{"schedule.full.weekday", "星期天", 7},
		{"schedule.full.weekday", "每周三", 3},
		{"source.items[0].options.channels", "4", 4},
		{"source.items[0].options.channels", "并发设为8", 8},
	}
	for _, entry := range cases {
		meta, ok := LookupSlotMeta(oraclePlanMap(t), entry.slot, catalog.Builtin())
		if !ok {
			t.Fatalf("未找到槽位 %s", entry.slot)
		}
		got, err := ParseAnswer(meta, entry.text)
		if err != nil {
			t.Errorf("%s = %q 解析失败: %v", entry.slot, entry.text, err)
			continue
		}
		if got != entry.want {
			t.Errorf("%s = %q 得到 %#v, 期望 %#v", entry.slot, entry.text, got, entry.want)
		}
	}
}

func TestParseAnswerDuration(t *testing.T) {
	meta := genericSlots["retention"]
	value, err := ParseAnswer(meta, "保留 2 周")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	table, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("时长应解析成对象: %#v", value)
	}
	if table["value"] != 2 || table["unit"] != "weeks" {
		t.Fatalf("解析结果不符: %#v", table)
	}
}

func TestParseAnswerListAllBecomesWildcard(t *testing.T) {
	meta := genericSlots["scope.items"]
	for _, text := range []string{"全部", "所有库", "整个实例"} {
		value, err := ParseAnswer(meta, text)
		if err != nil {
			t.Fatalf("%q 解析失败: %v", text, err)
		}
		list, ok := value.([]string)
		if !ok || len(list) != 1 || list[0] != "*" {
			t.Errorf("%q 应解析为通配符: %#v", text, value)
		}
	}
}

func TestParseAnswerListSplits(t *testing.T) {
	meta := genericSlots["scope.items"]
	value, err := ParseAnswer(meta, "users、orders, items")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	list := value.([]string)
	if len(list) != 3 {
		t.Fatalf("应切成 3 项: %#v", list)
	}
}

func TestParseAnswerEnumRejectsUnknown(t *testing.T) {
	meta := genericSlots["target.mode"]
	if _, err := ParseAnswer(meta, "随便吧"); err == nil {
		t.Fatal("无法匹配候选值时应报错")
	}
}

func TestParseAnswerRejectsEmpty(t *testing.T) {
	for _, slot := range []string{"schedule.full.time", "retention", "destinations"} {
		meta := genericSlots[slot]
		if _, err := ParseAnswer(meta, "   "); err == nil {
			t.Errorf("%s 空回答应报错", slot)
		}
	}
}

func TestParseAnswerIntRangeEnforced(t *testing.T) {
	meta := genericSlots["schedule.full.day_of_month"]
	if _, err := ParseAnswer(meta, "45号"); err == nil {
		t.Fatal("超出范围的日期应报错")
	}
	if value, err := ParseAnswer(meta, "15号"); err != nil || value != 15 {
		t.Fatalf("15号 应解析为 15: %#v %v", value, err)
	}
}

// ---------------------------------------------------------------------------
// 复合槽位回填
// ---------------------------------------------------------------------------

func lookup(planMap map[string]any) func(string) (SlotMeta, bool) {
	return func(path string) (SlotMeta, bool) { return LookupSlotMeta(planMap, path, catalog.Builtin()) }
}

func TestApplyAnswersFrequencyCreatesSchedule(t *testing.T) {
	planMap := oraclePlanMap(t)
	plan, _, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"schedule.full": "每周日凌晨1点"}, catalog.Builtin(), lookup(planMap))

	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}
	if plan == nil {
		t.Fatal("应返回更新后的计划")
	}
	typed := plan.(*schema.BackupPlan)
	if typed.Schedule.Full == nil {
		t.Fatal("schedule.full 应被创建")
	}
	if typed.Schedule.Full.Frequency != "weekly" || *typed.Schedule.Full.Weekday != 7 {
		t.Errorf("频率解析不对: %+v", typed.Schedule.Full)
	}
	if typed.Schedule.Full.Time != "01:00" {
		t.Errorf("时刻解析不对: %q", typed.Schedule.Full.Time)
	}
	if typed.Schedule.Full.Timezone == "" {
		t.Error("应补上默认时区")
	}
}

func TestApplyAnswersFrequencyMergesInsteadOfOverwriting(t *testing.T) {
	planMap := oraclePlanMap(t)
	// 先把周日定下来, 再单独补时刻 —— 时刻不该把周日抹掉
	if err := SetPath(planMap, "schedule.full", map[string]any{
		"frequency": "weekly", "weekday": 7, "timezone": "Asia/Shanghai",
	}); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	plan, _, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"schedule.full": "凌晨2点"}, catalog.Builtin(), lookup(planMap))
	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}

	typed := plan.(*schema.BackupPlan)
	if typed.Schedule.Full.Weekday == nil || *typed.Schedule.Full.Weekday != 7 {
		t.Errorf("合并后应保留周日: %+v", typed.Schedule.Full)
	}
	if typed.Schedule.Full.Time != "02:00" {
		t.Errorf("应更新为 02:00: %q", typed.Schedule.Full.Time)
	}
}

func TestApplyAnswersRetentionWritesFull(t *testing.T) {
	planMap := oraclePlanMap(t)
	plan, _, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"retention": "保留14天"}, catalog.Builtin(), lookup(planMap))
	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}

	typed := plan.(*schema.BackupPlan)
	if typed.Retention.Full == nil || typed.Retention.Full.Value != 14 ||
		typed.Retention.Full.Unit != "days" {
		t.Fatalf("保留期不对: %+v", typed.Retention.Full)
	}
}

// 目的地回答是自然语言, 必须构造成对象而不是把数组替换成字符串。
func TestApplyAnswersDestinationBuildsObject(t *testing.T) {
	planMap := oraclePlanMap(t)
	plan, _, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"destinations": "本地磁盘 /u01/backup/orcl"}, catalog.Builtin(), lookup(planMap))
	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}

	typed := plan.(*schema.BackupPlan)
	if len(typed.Destinations) != 1 {
		t.Fatalf("应有 1 个目的地: %#v", typed.Destinations)
	}
	destination := typed.Destinations[0]
	if destination.Type != "local" {
		t.Errorf("类型不对: %q", destination.Type)
	}
	if destination.Path != "/u01/backup/orcl" {
		t.Errorf("路径不对: %q", destination.Path)
	}
	if destination.ID == "" {
		t.Error("应自动生成 id")
	}
}

func TestApplyAnswersDestinationObjectStorage(t *testing.T) {
	planMap := oraclePlanMap(t)
	plan, _, _ := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"destinations": "阿里云OSS，桶名 backup-prod"}, catalog.Builtin(), lookup(planMap))

	typed := plan.(*schema.BackupPlan)
	if len(typed.Destinations) != 1 {
		t.Fatalf("应有 1 个目的地: %#v", typed.Destinations)
	}
	if typed.Destinations[0].Type != "aliyun_oss" {
		t.Errorf("类型不对: %q", typed.Destinations[0].Type)
	}
	if typed.Destinations[0].Bucket != "backup-prod" {
		t.Errorf("桶名不对: %q", typed.Destinations[0].Bucket)
	}
}

func TestApplyAnswersDestinationUnrecognized(t *testing.T) {
	planMap := oraclePlanMap(t)
	_, applied, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"destinations": "反正找个地方放着"}, catalog.Builtin(), lookup(planMap))

	if len(applied) != 0 {
		t.Errorf("无法识别时不该计入已应用: %v", applied)
	}
	if len(problems) == 0 {
		t.Error("应报告无法识别, 以便把原始回答交给模型")
	}
}

// 同类型同落点的目的地不该重复添加 —— 模型路径与确定性路径都会填。
func TestApplyAnswersDestinationDedupes(t *testing.T) {
	planMap := oraclePlanMap(t)
	answers := map[string]string{"destinations": "本地磁盘 /u01/backup/orcl"}

	plan, _, _ := ApplyAnswers(schema.KindBackup, planMap, answers, catalog.Builtin(), lookup(planMap))
	planMap2, err := schema.PlanToMap(plan)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	plan2, _, _ := ApplyAnswers(schema.KindBackup, planMap2, answers, catalog.Builtin(), lookup(planMap2))

	typed := plan2.(*schema.BackupPlan)
	if len(typed.Destinations) != 1 {
		t.Fatalf("重复应用不该产生重复目的地: %#v", typed.Destinations)
	}
}

// 一条坏答案不该拖垮其它已经解析成功的答案。
func TestApplyAnswersKeepsGoodOnesWhenOneFails(t *testing.T) {
	planMap := oraclePlanMap(t)
	// 引擎只在 schedule.full 已存在时才追问 full.time, 这里保持同样的前提
	if err := SetPath(planMap, "schedule.full", map[string]any{
		"frequency": "weekly", "weekday": 7, "timezone": "Asia/Shanghai",
	}); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	plan, applied, problems := ApplyAnswers(schema.KindBackup, planMap, map[string]string{
		"schedule.full.time": "凌晨1点",
		"retention":          "(((",
	}, catalog.Builtin(), lookup(planMap))

	if len(problems) == 0 {
		t.Error("应报告解析失败的项")
	}
	if len(applied) != 1 {
		t.Errorf("应只应用成功的那一项: %v", applied)
	}
	if plan == nil {
		t.Fatal("应返回更新后的计划")
	}
	typed := plan.(*schema.BackupPlan)
	if typed.Schedule.Full == nil || typed.Schedule.Full.Time != "01:00" {
		t.Errorf("成功的答案应落盘: %+v", typed.Schedule.Full)
	}
}

func TestApplyAnswersIgnoresUnknownSlots(t *testing.T) {
	planMap := oraclePlanMap(t)
	_, applied, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"totally.unknown.slot": "x"}, catalog.Builtin(), lookup(planMap))

	if len(applied) != 0 || len(problems) != 0 {
		t.Errorf("未知槽位应被静默忽略: applied=%v problems=%v", applied, problems)
	}
}

// 父级对象缺失时, 该字段的答案无法确定性落盘 —— 这时不应静默丢弃,
// 而要报告出来, 让原始问答继续走模型路径。
//
// 这属于设计约定: 引擎在 schedule.<name> 整体缺失时问的是复合槽位
// schedule.<name> (由 applyFrequency 负责建对象), 而不会问它的子字段。
func TestApplyAnswersReportsMissingParent(t *testing.T) {
	planMap := oraclePlanMap(t)
	_, applied, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"schedule.full.time": "凌晨1点"}, catalog.Builtin(), lookup(planMap))

	if len(applied) != 0 {
		t.Errorf("父级缺失时不该计入已应用: %v", applied)
	}
	if len(problems) == 0 {
		t.Error("应报告无法落盘, 以便交由模型理解")
	}
}

func TestApplyAnswersEmptyAnswerIgnored(t *testing.T) {
	planMap := oraclePlanMap(t)
	_, applied, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"schedule.full.time": "   "}, catalog.Builtin(), lookup(planMap))
	if len(applied) != 0 || len(problems) != 0 {
		t.Errorf("空回答应被跳过: applied=%v problems=%v", applied, problems)
	}
}

// ---------------------------------------------------------------------------
// 缺口分析的幂等性
// ---------------------------------------------------------------------------

func TestGapsShrinkAsAnswersArrive(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{
		Frequency: "weekly", Timezone: "Asia/Shanghai", Weekday: driver.IntPtr(7),
	}

	first := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	firstSlots := questionSlots(first.Questions)
	want := []string{"schedule.full.time", "retention", "destinations"}
	if strings.Join(firstSlots, ",") != strings.Join(want, ",") {
		t.Fatalf("缺口不符: %v, 期望 %v", firstSlots, want)
	}

	// 补上时刻后, 该问题应消失, 其余保持
	plan.Schedule.Full.Time = "01:00"
	second := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	secondSlots := questionSlots(second.Questions)
	if len(secondSlots) != 2 {
		t.Fatalf("补一项后应只剩 2 个缺口: %v", secondSlots)
	}
	for _, slot := range secondSlots {
		if slot == "schedule.full.time" {
			t.Error("已填的字段不该再问")
		}
	}
}

// 每周却缺星期几时必须问出来, 且问题要能落到槽位上。
func TestGapsWeeklyNeedsWeekday(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "MySQL", Kind: "mysql",
		Options: map[string]any{"method": "logical", "databases": []string{"*"}},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "weekly", Time: "01:00"}
	plan.Retention.Full = &schema.RetentionSpec{Value: 7, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local"}}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	found := false
	for _, question := range gaps.Questions {
		if question.Slot == "schedule.full.weekday" {
			found = true
			if !strings.Contains(question.Question, "周") {
				t.Errorf("问句不符: %q", question.Question)
			}
		}
	}
	if !found {
		t.Fatalf("应追问星期几: %v", questionSlots(gaps.Questions))
	}
}

// 上一轮答过但没落盘的槽位, 再问时必须说明原因, 不能静默重复。
func TestGapsExplainUnresolvedPreviousAnswer(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{ID: "src-1", Name: "MySQL", Kind: "mysql", Options: map[string]any{}}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "daily", Timezone: "Asia/Shanghai"}
	plan.Retention.Full = &schema.RetentionSpec{Value: 7, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local"}}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{"schedule.full.time": "大概是晚上"}, catalog.Builtin())

	for _, question := range gaps.Questions {
		if question.Slot != "schedule.full.time" {
			continue
		}
		if !strings.Contains(question.Hint, "未能识别") {
			t.Fatalf("应说明上轮回答未被识别: %q", question.Hint)
		}
		return
	}
	t.Fatal("缺时刻时应继续追问")
}

func TestGapsRestoreFields(t *testing.T) {
	plan := schema.NewRestorePlan()
	plan.TaskName = "恢复"
	plan.Source.Items = []schema.SourceItem{{ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{}}}
	plan.Target.Mode = "alternate_host"
	plan.Point.Type = "scn"
	plan.Scope.Level = "table"

	gaps := AnalyzeGaps(schema.KindRestore, plan, map[string]string{}, catalog.Builtin())
	found := map[string]bool{}
	for _, question := range gaps.Questions {
		found[question.Slot] = true
	}
	for _, want := range []string{"target.host", "point.value", "scope.items"} {
		if !found[want] {
			t.Errorf("恢复缺口应包含 %s, 实际 %v", want, questionSlots(gaps.Questions))
		}
	}
}

func TestGapsWarningsAreStructured(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{ID: "src-1", Name: "MySQL", Kind: "mysql", Options: map[string]any{}}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "daily", Time: "02:00", Timezone: "Asia/Shanghai"}
	plan.Retention.Full = &schema.RetentionSpec{Value: 7, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local"}}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	for _, question := range gaps.Questions {
		if question.Slot == "" {
			t.Error("每条追问都必须带槽位标识, 否则前端无法回传答案")
		}
		if question.Kind == "" {
			t.Error("每条追问都必须带取值类型, 否则前端不知道该渲染什么控件")
		}
		if question.Question == "" {
			t.Error("每条追问都必须有问句")
		}
		if !question.Required {
			t.Error("缺口追问都应是必填")
		}
	}
}

// 序列化出来的问题必须能被前端消费 (字段名稳定)。
func TestQuestionsSerializeWithStableFieldNames(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{ID: "src-1", Name: "MySQL", Kind: "mysql", Options: map[string]any{}}}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	raw, err := json.Marshal(gaps.Questions)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	text := string(raw)
	for _, field := range []string{`"slot"`, `"question"`, `"kind"`, `"required"`, `"choices"`} {
		if !strings.Contains(text, field) {
			t.Errorf("问题 JSON 缺少字段 %s: %s", field, text)
		}
	}
}

// ---------------------------------------------------------------------------
// 推荐值
// ---------------------------------------------------------------------------

// 这是推荐值最关键的一条约束: 点一下就提交, 所以每个推荐值都必须能被该槽位的
// 解析器接受。一旦有推荐值解析不了, 用户点它会得到"无法识别"——比没有推荐更糟。
func TestEverySuggestionIsParseable(t *testing.T) {
	checked := 0

	check := func(label string, meta SlotMeta, suggestions []string) {
		for _, suggestion := range suggestions {
			checked++
			if _, err := ParseAnswer(meta, suggestion); err != nil {
				t.Errorf("%s: 推荐值 %q 无法解析: %v", label, suggestion, err)
			}
		}
	}

	// 通用槽位
	for slot, meta := range genericSlots {
		check("通用槽位 "+slot, meta, meta.Suggest)
	}

	// 驱动槽位
	for _, engine := range driver.All() {
		for _, slot := range engine.Slots {
			meta := slotMetaFrom(engine, slot, slot.Field)
			check(engine.Kind+"."+slot.Field, meta, slot.Suggest)
		}
	}

	if checked == 0 {
		t.Fatal("没有检查到任何推荐值 —— 断言本身失效了")
	}
	t.Logf("已校验 %d 个推荐值", checked)
}

// 推荐值不应与封闭候选值重复, 否则界面上会出现两个一模一样的按钮。
func TestSuggestionsDoNotDuplicateChoices(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "weekly", Timezone: "Asia/Shanghai"}

	// 构造一个 choices 与 suggest 有交集的槽位元信息
	meta := SlotMeta{
		Slot: "x", Question: "q", Kind: string(driver.SlotEnum),
		Choices: []string{"每天", "每周"}, Suggest: []string{"每天", "23:30"},
	}
	kept := outsideChoices(meta.Choices, meta.Suggest)
	if len(kept) != 1 || kept[0] != "23:30" {
		t.Fatalf("应只保留不在候选值里的推荐值, 得到 %v", kept)
	}
}

// 追问必须带上推荐值 —— 这正是本次改动的要求。
func TestQuestionsCarrySuggestions(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "测试"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{
		Frequency: "weekly", Timezone: "Asia/Shanghai", Weekday: driver.IntPtr(7),
	}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, catalog.Builtin())
	bySlot := map[string]schema.Question{}
	for _, question := range gaps.Questions {
		bySlot[question.Slot] = question
	}

	// 时刻与保留期过去只有输入框, 没有任何可点的东西
	for _, slot := range []string{"schedule.full.time", "retention", "destinations"} {
		question, ok := bySlot[slot]
		if !ok {
			t.Errorf("缺少预期追问 %s", slot)
			continue
		}
		total := len(question.Choices) + len(question.Suggestions)
		if total == 0 {
			t.Errorf("%s 既无候选值也无推荐值, 用户只能手打", slot)
		}
	}
}

// 没有推荐值也应该能追问 (例如目标主机必须填真实地址)。
func TestQuestionWithoutSuggestionsStillValid(t *testing.T) {
	plan := schema.NewRestorePlan()
	plan.TaskName = "恢复"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{},
	}}
	plan.Target.Mode = "alternate_host"

	gaps := AnalyzeGaps(schema.KindRestore, plan, map[string]string{}, catalog.Builtin())
	for _, question := range gaps.Questions {
		if question.Slot != "target.host" {
			continue
		}
		if len(question.Suggestions) != 0 {
			t.Errorf("目标主机不该有推荐值 (每台机器都不同): %v", question.Suggestions)
		}
		if question.Question == "" {
			t.Error("没有推荐值时问句仍须完整")
		}
		return
	}
	t.Fatal("应追问 target.host")
}

// 推荐值要能真的走完回填 —— 解析出来只是一半, 写进计划才算数。
func TestSuggestionsApplyToPlan(t *testing.T) {
	planMap := oraclePlanMap(t)
	if err := SetPath(planMap, "schedule.full", map[string]any{
		"frequency": "daily", "timezone": "Asia/Shanghai",
	}); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	// 全部推荐值都应能应用成功
	answers := map[string]string{}
	for _, pair := range []struct{ slot, value string }{
		{"schedule.full.time", "凌晨1点"},
		{"retention", "保留30天"},
		{"source.items[0].options.channels", "4"},
		{"source.items[0].options.scope", "整个实例"},
		{"destinations", "本地磁盘 /u01/backup"},
	} {
		meta, ok := LookupSlotMeta(planMap, pair.slot, catalog.Builtin())
		if !ok {
			t.Fatalf("未找到槽位 %s", pair.slot)
		}
		if _, err := ParseAnswer(meta, pair.value); err != nil {
			t.Errorf("%s 的推荐值 %q 解析失败: %v", pair.slot, pair.value, err)
		}
		answers[pair.slot] = pair.value
	}

	plan, applied, problems := ApplyAnswers(schema.KindBackup, planMap, answers, catalog.Builtin(), lookup(planMap))
	if len(problems) != 0 {
		t.Fatalf("推荐值不该有应用失败: %v", problems)
	}
	if len(applied) != len(answers) {
		t.Fatalf("应全部应用成功: %v", applied)
	}
	typed := plan.(*schema.BackupPlan)
	if typed.Schedule.Full.Time != "01:00" {
		t.Errorf("时刻未落盘: %+v", typed.Schedule.Full)
	}
	if typed.Retention.Full == nil || typed.Retention.Full.Value != 30 {
		t.Errorf("保留期未落盘: %+v", typed.Retention.Full)
	}
}
