package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 防漂移: 结构体字段与 JSON Schema 必须一致
//
// Go 没有 Pydantic 那种从模型自动生成 schema 的能力, 所以存在"加了字段却忘了
// 写进 schema"的风险 —— 后果是模型永远不知道该填这个字段, 而且不会有任何报错。
// 这个测试就是那道防线: 用反射列出结构体的 JSON 字段, 逐个到 schema 里找。
// ---------------------------------------------------------------------------

// defNames 把共享类型映射到 $defs 里的名字。
var defNames = map[reflect.Type]string{
	reflect.TypeOf(ScheduleSpec{}):  "schedule_spec",
	reflect.TypeOf(Schedule{}):      "schedule",
	reflect.TypeOf(RetentionSpec{}): "retention_spec",
	reflect.TypeOf(Retention{}):     "retention",
	reflect.TypeOf(Destination{}):   "destination",
	reflect.TypeOf(SLA{}):           "sla",
	reflect.TypeOf(Verification{}):  "verification",
	reflect.TypeOf(Notification{}):  "notification",
	reflect.TypeOf(SourceItem{}):    "source_item",
	reflect.TypeOf(BackupSource{}):  "backup_source",
}

// jsonFieldNames 列出一个结构体的全部 JSON 字段名。
func jsonFieldNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	names := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assertCovered 断言结构体的每个字段都出现在 properties 中。
func assertCovered(t *testing.T, typ reflect.Type, properties map[string]any, label string) {
	t.Helper()
	for _, name := range jsonFieldNames(t, typ) {
		if _, exists := properties[name]; !exists {
			t.Errorf("%s: 字段 %q 未出现在 JSON Schema 中 —— 模型将不知道这个字段存在", label, name)
		}
	}
}

// assertNoExtra 反向检查: schema 里的字段必须在结构体里真实存在。
func assertNoExtra(t *testing.T, typ reflect.Type, properties map[string]any, label string) {
	t.Helper()
	known := map[string]bool{}
	for _, name := range jsonFieldNames(t, typ) {
		known[name] = true
	}
	for name := range properties {
		if !known[name] {
			t.Errorf("%s: JSON Schema 声明了字段 %q, 但结构体里没有 —— 模型会填一个被丢弃的键", label, name)
		}
	}
}

func TestSharedDefsCoverStructs(t *testing.T) {
	defs := sharedDefs()
	if len(defs) != len(defNames) {
		t.Errorf("$defs 数量不符: 有 %d 个定义, 测试登记了 %d 个", len(defs), len(defNames))
	}
	for typ, name := range defNames {
		raw, exists := defs[name]
		if !exists {
			t.Errorf("$defs 缺少 %q", name)
			continue
		}
		def, ok := raw.(Obj)
		if !ok {
			t.Errorf("$defs[%q] 不是对象", name)
			continue
		}
		properties, ok := def["properties"].(Obj)
		if !ok {
			t.Errorf("$defs[%q] 缺少 properties", name)
			continue
		}
		assertCovered(t, typ, properties, "$defs."+name)
		assertNoExtra(t, typ, properties, "$defs."+name)
	}
}

func TestBackupPlanSchemaCoversTopLevel(t *testing.T) {
	planSchema := BackupPlanSchema(map[string]any{})
	properties, ok := planSchema["properties"].(Obj)
	if !ok {
		t.Fatal("备份计划 schema 缺少 properties")
	}
	assertCovered(t, reflect.TypeOf(BackupPlan{}), properties, "BackupPlan")
	assertNoExtra(t, reflect.TypeOf(BackupPlan{}), properties, "BackupPlan")

	// 必备字段必须在 required 里, 否则模型可能整个省掉
	required, _ := planSchema["required"].([]string)
	for _, name := range []string{"task_name", "source", "schedule", "retention", "destinations"} {
		if !contains(required, name) {
			t.Errorf("%s 应为必填", name)
		}
	}
}

func TestRestorePlanSchemaCoversTopLevel(t *testing.T) {
	planSchema := RestorePlanSchema(map[string]any{})
	properties, ok := planSchema["properties"].(Obj)
	if !ok {
		t.Fatal("恢复计划 schema 缺少 properties")
	}
	assertCovered(t, reflect.TypeOf(RestorePlan{}), properties, "RestorePlan")
	assertNoExtra(t, reflect.TypeOf(RestorePlan{}), properties, "RestorePlan")

	defs, _ := planSchema["$defs"].(Obj)
	for _, name := range []string{"restore_point", "restore_target", "restore_scope",
		"alternate_location", "restore_verification"} {
		if _, exists := defs[name]; !exists {
			t.Errorf("恢复计划 schema 缺少 $defs.%s", name)
		}
	}
}

// 枚举表被 schema 与校验共用; 枚举漏进 schema 会让模型无从选择。
func TestEnumTablesAppearInSchema(t *testing.T) {
	defs := sharedDefs()
	retentionSpec := defs["retention_spec"].(Obj)["properties"].(Obj)
	unit := retentionSpec["unit"].(Obj)
	values, _ := unit["enum"].([]string)
	if len(values) != len(RetentionUnits) {
		t.Fatalf("retention_spec.unit 的 enum 应覆盖全部单位, 得到 %v", values)
	}
	for _, want := range RetentionUnits {
		if !contains(values, want) {
			t.Errorf("retention_spec.unit 缺少 %q", want)
		}
	}
}

func TestSchemaIsJSONSerializable(t *testing.T) {
	// schema 是要原样拼进提示词的, 必须能序列化
	for _, kind := range []string{KindBackup, KindRestore} {
		if _, err := json.Marshal(PlanSchema(kind, map[string]any{"oracle": map[string]any{}})); err != nil {
			t.Fatalf("%s schema 无法序列化: %v", kind, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 校验
// ---------------------------------------------------------------------------

func validBackupPlan() *BackupPlan {
	weekday := 7
	plan := NewBackupPlan()
	plan.TaskName = "Oracle 周备"
	plan.Source = BackupSource{
		Type:  "database",
		Items: []SourceItem{{ID: "src-1", Name: "Oracle 实例", Kind: "oracle", Options: map[string]any{}}},
	}
	plan.Schedule.Full = &ScheduleSpec{Frequency: "weekly", Time: "01:00", Weekday: &weekday, Timezone: "Asia/Shanghai"}
	plan.Retention.Full = &RetentionSpec{Value: 14, Unit: "days"}
	plan.Destinations = []Destination{{ID: "dest-1", Type: "local", Path: "/u01/backup"}}
	plan.Confidence = 0.9
	return plan
}

func TestValidateAcceptsCompletePlan(t *testing.T) {
	report := Validate(validBackupPlan())
	if errors := report.Errors(); len(errors) != 0 {
		t.Fatalf("完整计划不该报错: %v", report.Messages(LevelError))
	}
}

func TestValidateReportsMissingSource(t *testing.T) {
	plan := validBackupPlan()
	plan.Source.Items = nil
	messages := strings.Join(Validate(plan).Messages(LevelError), "\n")
	if !strings.Contains(messages, "source.items") || !strings.Contains(messages, "备份源") {
		t.Fatalf("应报出缺少备份源: %s", messages)
	}
}

func TestValidateWeeklyRequiresWeekday(t *testing.T) {
	plan := validBackupPlan()
	plan.Schedule.Full.Weekday = nil

	report := Validate(plan)
	messages := strings.Join(report.Messages(LevelError), "\n")
	if !strings.Contains(messages, "schedule.full.weekday") {
		t.Fatalf("应报出缺少 weekday: %s", messages)
	}
	// 该字段用户能补, 必须带上槽位路径以便降级为追问
	for _, issue := range report.Errors() {
		if issue.Path == "schedule.full.weekday" && issue.Slot != "schedule.full.weekday" {
			t.Errorf("weekday 缺失应标记为可追问槽位, 得到 Slot=%q", issue.Slot)
		}
	}
}

func TestValidateRejectsBadTimeFormat(t *testing.T) {
	for _, bad := range []string{"25:00", "1:00", "01:60", "0100", "01:0"} {
		plan := validBackupPlan()
		plan.Schedule.Full.Time = bad
		messages := strings.Join(Validate(plan).Messages(LevelError), "\n")
		if !strings.Contains(messages, "时刻格式") {
			t.Errorf("%q 应被判为格式错误: %s", bad, messages)
		}
	}
	for _, good := range []string{"00:00", "01:00", "23:59"} {
		plan := validBackupPlan()
		plan.Schedule.Full.Time = good
		if errors := Validate(plan).Errors(); len(errors) != 0 {
			t.Errorf("%q 应被接受: %v", good, Validate(plan).Messages(LevelError))
		}
	}
}

func TestValidateRejectsDuplicateDestinationIDs(t *testing.T) {
	plan := validBackupPlan()
	plan.Destinations = []Destination{{ID: "dup", Type: "local"}, {ID: "dup", Type: "s3"}}
	messages := strings.Join(Validate(plan).Messages(LevelError), "\n")
	if !strings.Contains(messages, "重复") {
		t.Fatalf("应报出 id 重复: %s", messages)
	}
}

func TestValidateRejectsInvalidEnums(t *testing.T) {
	plan := validBackupPlan()
	plan.Schedule.Full.Frequency = "sometimes"
	plan.Retention.Full.Unit = "fortnights"
	plan.Destinations[0].Type = "ftp"

	messages := strings.Join(Validate(plan).Messages(LevelError), "\n")
	for _, want := range []string{"schedule.full.frequency", "retention.full.unit", "destinations[0].type"} {
		if !strings.Contains(messages, want) {
			t.Errorf("应报出 %s: %s", want, messages)
		}
	}
}

func TestValidateRestorePoint(t *testing.T) {
	base := func() *RestorePlan {
		plan := NewRestorePlan()
		plan.TaskName = "恢复"
		plan.Source = BackupSource{
			Type:  "database",
			Items: []SourceItem{{ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{}}},
		}
		return plan
	}

	latest := base()
	if errors := Validate(latest).Errors(); len(errors) != 0 {
		t.Fatalf("point=latest 不该报错: %v", Validate(latest).Messages(LevelError))
	}

	scnWithoutValue := base()
	scnWithoutValue.Point.Type = "scn"
	if messages := strings.Join(Validate(scnWithoutValue).Messages(LevelError), "\n"); !strings.Contains(messages, "point.value") {
		t.Errorf("scn 缺取值应报错: %s", messages)
	}

	badTimestamp := base()
	badTimestamp.Point.Type = "timestamp"
	badTimestamp.Point.Value = "2026/09/25 10:00"
	if messages := strings.Join(Validate(badTimestamp).Messages(LevelError), "\n"); !strings.Contains(messages, "时间格式") {
		t.Errorf("时间格式错误应被报出: %s", messages)
	}

	goodTimestamp := base()
	goodTimestamp.Point.Type = "timestamp"
	goodTimestamp.Point.Value = "2026-09-25 10:00:00"
	if errors := Validate(goodTimestamp).Errors(); len(errors) != 0 {
		t.Errorf("合法时间点应被接受: %v", Validate(goodTimestamp).Messages(LevelError))
	}
}

// 异机恢复缺主机属于"用户没说", 必须带上槽位以便转成追问而不是判死。
func TestValidateAlternateHostIsAnswerable(t *testing.T) {
	plan := NewRestorePlan()
	plan.TaskName = "异机恢复"
	plan.Source = BackupSource{
		Type:  "database",
		Items: []SourceItem{{ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{}}},
	}
	plan.Target.Mode = "alternate_host"

	found := false
	for _, issue := range Validate(plan).Errors() {
		if issue.Path == "target.host" && issue.Slot == "target.host" {
			found = true
		}
	}
	if !found {
		t.Fatal("target.host 缺失应标记为可追问槽位")
	}
}

// ---------------------------------------------------------------------------
// JSON 解析容错
// ---------------------------------------------------------------------------

func TestJSONDecodeSalvagesWrappedOutput(t *testing.T) {
	cases := []string{
		`{"a": 1}`,
		"```json\n{\"a\": 1}\n```",
		"```\n{\"a\": 1}\n```",
		"好的, 这是结果:\n{\"a\": 1}\n以上。",
		"  \n {\"a\": 1}  ",
	}
	for _, raw := range cases {
		out, err := JSONDecode(raw)
		if err != nil {
			t.Errorf("应能解析 %q: %v", raw, err)
			continue
		}
		if out["a"] == nil {
			t.Errorf("解析结果不对: %q -> %v", raw, out)
		}
	}
}

func TestJSONDecodeRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "not json", "[]", "42"} {
		if _, err := JSONDecode(raw); err == nil {
			t.Errorf("%q 应解析失败", raw)
		}
	}
}

func TestUnmarshalStrictRejectsUnknownField(t *testing.T) {
	raw := []byte(`{"task_name":"x","backup_time":"02:00"}`)
	var plan BackupPlan
	err := UnmarshalStrict(raw, &plan)
	if err == nil {
		t.Fatal("未知字段应报错")
	}
	if !strings.Contains(err.Error(), "backup_time") {
		t.Fatalf("报错应指明字段名: %v", err)
	}
}

func TestUnmarshalStrictReportsTypeMismatch(t *testing.T) {
	raw := []byte(`{"task_name":"x","confidence":"高"}`)
	var plan BackupPlan
	err := UnmarshalStrict(raw, &plan)
	if err == nil {
		t.Fatal("类型不符应报错")
	}
	if !strings.Contains(err.Error(), "期望") {
		t.Fatalf("报错应说明期望类型: %v", err)
	}
}

func TestPlanMapRoundTrip(t *testing.T) {
	original := validBackupPlan()
	asMap, err := PlanToMap(original)
	if err != nil {
		t.Fatalf("转 map 失败: %v", err)
	}
	restored, err := PlanFromMap(KindBackup, asMap)
	if err != nil {
		t.Fatalf("转回计划失败: %v", err)
	}
	back, ok := restored.(*BackupPlan)
	if !ok {
		t.Fatalf("类型不符: %T", restored)
	}
	if back.Schedule.Full.Time != "01:00" || back.Schedule.Full.Weekday == nil || *back.Schedule.Full.Weekday != 7 {
		t.Fatalf("往返后数据损坏: %+v", back.Schedule.Full)
	}
	if back.Retention.Full.Value != 14 {
		t.Fatalf("保留期往返后损坏: %+v", back.Retention.Full)
	}
}

// 槽位机制依赖 map 往返, 数字必须保持精确 —— 端口/并发度不该经过浮点。
func TestPlanMapKeepsIntegersExact(t *testing.T) {
	plan := validBackupPlan()
	port := 1521
	plan.Source.Items[0].Port = &port
	plan.Source.Items[0].Options = map[string]any{"channels": 4}

	asMap, err := PlanToMap(plan)
	if err != nil {
		t.Fatalf("转 map 失败: %v", err)
	}
	item := asMap["source"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if got := item["port"]; got != json.Number("1521") {
		t.Fatalf("port 应为 json.Number(1521), 得到 %#v", got)
	}
	options := item["options"].(map[string]any)
	if got := options["channels"]; got != json.Number("4") {
		t.Fatalf("channels 应为 json.Number(4), 得到 %#v", got)
	}
}
