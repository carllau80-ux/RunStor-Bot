package agent

import (
	"backup-restore-agent/internal/catalog"
	"strings"
	"testing"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// 完成后的总结: 给人看的中文行, 而不是让用户自己去读 JSON 里的枚举代码值。
func TestSummaryForCompleteBackupPlan(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "Oracle 实例周期性全备+增量备份"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle 实例", Kind: "oracle",
		Databases: []string{"*"},
		Options: map[string]any{
			"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4,
			"archivelog": true,
		},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{
		Frequency: "weekly", Time: "01:00", Weekday: driver.IntPtr(7), Timezone: "Asia/Shanghai",
	}
	plan.Schedule.Incremental = &schema.ScheduleSpec{
		Frequency: "daily", Time: "01:00", Timezone: "Asia/Shanghai",
	}
	plan.Retention.Full = &schema.RetentionSpec{Value: 14, Unit: "days"}
	plan.Retention.MinCopies = 1
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local", Path: "/u01/backup/orcl"}}
	plan.Verification = schema.Verification{TestRestore: true, Frequency: "weekly"}
	plan.Confidence = 0.9
	plan.Warnings = []string{"只有一个备份目的地, 建议增加异地副本"}

	summary := summarize(schema.KindBackup, plan, catalog.Builtin())
	if summary == nil {
		t.Fatal("应产出总结")
	}
	if summary.Title != plan.TaskName {
		t.Errorf("标题不符: %q", summary.Title)
	}
	if len(summary.Warnings) != 1 {
		t.Errorf("警告应逐条带出: %v", summary.Warnings)
	}

	byLabel := map[string]schema.SummaryRow{}
	for _, row := range summary.Rows {
		byLabel[row.Label] = row
	}

	// 中文而不是代码值; 标签里的长括号补充会被去掉, 让总结更紧凑
	if row := byLabel["认证方式"]; !strings.Contains(row.Value, "操作系统认证") ||
		strings.Contains(row.Value, "os_auth") {
		t.Errorf("认证方式应是中文标签而非代码值, 实际 %q", row.Value)
	}
	if row := byLabel["备份范围"]; !strings.Contains(row.Value, "整个实例") {
		t.Errorf("备份范围应翻成中文, 实际 %q", row.Value)
	}
	if row := byLabel["全量备份"]; !strings.Contains(row.Value, "每周") ||
		!strings.Contains(row.Value, "周日") || !strings.Contains(row.Value, "01:00") {
		t.Errorf("全量调度应含频率/星期/时刻, 实际 %q", row.Value)
	}
	if row := byLabel["增量备份"]; row.Value != "每天 01:00" {
		t.Errorf("增量调度不符: %q", row.Value)
	}
	if row := byLabel["保留策略"]; row.Value != "全量 14 天" {
		t.Errorf("保留策略不符: %q", row.Value)
	}
	if row := byLabel["备份目的地"]; !strings.Contains(row.Value, "本地磁盘") ||
		!strings.Contains(row.Value, "/u01/backup/orcl") {
		t.Errorf("目的地不符: %q", row.Value)
	}
	if row := byLabel["并发度"]; row.Value != "4" {
		t.Errorf("并发度不符: %q", row.Value)
	}
	if row := byLabel["归档日志"]; row.Value != "是" {
		t.Errorf("布尔项应显示是/否, 实际 %q", row.Value)
	}
	if row := byLabel["恢复演练"]; row.Value != "每周一次" {
		t.Errorf("恢复演练不符: %q", row.Value)
	}

	// 可修改的行必须带槽位路径, 界面才能"修改这一项"
	for _, label := range []string{"认证方式", "全量备份", "保留策略", "备份目的地"} {
		if byLabel[label].Slot == "" {
			t.Errorf("%s 应可修改（带 slot 路径）", label)
		}
	}
	// 没有对应槽位的行不该硬编一个路径
	if byLabel["任务名称"].Slot != "" && byLabel["任务名称"].Label == "" {
		t.Error("任务名称不该带槽位")
	}
}

func TestSummaryForCompleteRestorePlan(t *testing.T) {
	plan := schema.NewRestorePlan()
	plan.TaskName = "Oracle 恢复到故障前时刻"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "Oracle", Kind: "oracle", Options: map[string]any{},
	}}
	plan.SourceBackupRef = "生产Oracle实例周全备"
	plan.Target = schema.RestoreTarget{Mode: "alternate_host", Host: "192.0.2.11"}
	plan.Point = schema.RestorePoint{Type: "timestamp", Value: "2026-09-25 10:30:00", Timezone: "Asia/Shanghai"}
	plan.Scope = schema.RestoreScope{Level: "tablespace", Items: []string{"USERS"}}
	plan.Verification = schema.RestoreVerification{ValidateBackup: true, OpenDatabase: true}

	summary := summarize(schema.KindRestore, plan, catalog.Builtin())
	byLabel := map[string]schema.SummaryRow{}
	for _, row := range summary.Rows {
		byLabel[row.Label] = row
	}

	if row := byLabel["备份来源"]; row.Value != "生产Oracle实例周全备" {
		t.Errorf("备份来源不符: %q", row.Value)
	}
	if row := byLabel["恢复时间点"]; !strings.Contains(row.Value, "指定时刻") ||
		!strings.Contains(row.Value, "2026-09-25 10:30:00") {
		t.Errorf("时间点不符: %q", row.Value)
	}
	if row := byLabel["恢复目标"]; !strings.Contains(row.Value, "异机恢复") ||
		!strings.Contains(row.Value, "192.0.2.11") {
		t.Errorf("目标不符: %q", row.Value)
	}
	if row := byLabel["恢复范围"]; !strings.Contains(row.Value, "表空间") ||
		!strings.Contains(row.Value, "USERS") {
		t.Errorf("范围不符: %q", row.Value)
	}
	if row := byLabel["恢复后动作"]; !strings.Contains(row.Value, "校验") {
		t.Errorf("恢复后动作不符: %q", row.Value)
	}
}

// 未完成的计划不给总结 —— 那时还没有可交付的东西。
func TestSummaryOnlyWhenReady(t *testing.T) {
	agent := newDemoAgent(t, 10)
	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if first.Summary != nil {
		t.Error("信息未完整时不该给总结")
	}

	ready, err := agent.Answer(first.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘 /u01/backup/orcl",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}
	if ready.Status != schema.StatusReady {
		t.Fatalf("应已完成: %q, 剩余 %v", ready.Status, questionSlots(ready.Questions))
	}
	if ready.Summary == nil {
		t.Fatal("完成时应给出总结")
	}
	if len(ready.Summary.Rows) < 5 {
		t.Errorf("总结行太少: %v", ready.Summary.Rows)
	}
}

// 提交 = 用户确认这份 JSON 定稿 (MVP 不执行备份)。
func TestSubmitMarksSessionFinal(t *testing.T) {
	agent := newDemoAgent(t, 10)
	first, _ := agent.Start(Request{Text: acceptanceText})
	ready, err := agent.Answer(first.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘 /u01/backup/orcl",
	}, "")
	if err != nil || ready.Status != schema.StatusReady {
		t.Fatalf("应先完成: %v %v", err, ready.Status)
	}

	result, err := agent.Submit(first.SessionID)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if result.Summary == nil {
		t.Error("提交后仍应带总结 (界面要显示已定稿的清单)")
	}
	if len(result.Notices) == 0 {
		t.Error("应提示已定稿")
	}

	// 历史列表要能看出已提交
	for _, item := range agent.ListSessions() {
		if item["id"] == first.SessionID {
			if item["submitted"] != true {
				t.Errorf("列表应标记已提交: %v", item)
			}
			return
		}
	}
	t.Fatal("未找到会话")
}

// 信息不完整时不能提交。
func TestSubmitRefusesIncompleteSession(t *testing.T) {
	agent := newDemoAgent(t, 10)
	first, _ := agent.Start(Request{Text: acceptanceText})

	if _, err := agent.Submit(first.SessionID); err == nil {
		t.Fatal("信息不完整时不该允许提交")
	} else if !strings.Contains(err.Error(), "待补充") {
		t.Errorf("报错应说明还差什么: %v", err)
	}
	if _, err := agent.Submit("s-nope"); err == nil {
		t.Fatal("未知会话应报错")
	}
}

// 定稿之后再改动, 原来的确认就失效了 —— 交付物已经变了。
func TestSubmitInvalidatedByLaterChange(t *testing.T) {
	agent := newDemoAgent(t, 10)
	first, _ := agent.Start(Request{Text: acceptanceText})
	if _, err := agent.Answer(first.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘 /u01/backup/orcl",
	}, ""); err != nil {
		t.Fatalf("续答失败: %v", err)
	}
	if _, err := agent.Submit(first.SessionID); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 再改一处
	if _, err := agent.Continue(first.SessionID, "保留期改成90天"); err != nil {
		t.Fatalf("修改失败: %v", err)
	}

	for _, item := range agent.ListSessions() {
		if item["id"] == first.SessionID {
			if item["submitted"] == true {
				t.Error("计划改动后不该仍标记为已提交")
			}
			return
		}
	}
	t.Fatal("未找到会话")
}

// 总结里的中文标签来自槽位表, 不能是另写一份的字面量。
func TestSummaryLabelsComeFromSlotTables(t *testing.T) {
	// 目的地类型的中文标签应能在槽位表里找到出处
	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{ID: "s", Name: "n", Kind: "oracle", Options: map[string]any{}}}
	plan.Destinations = []schema.Destination{{ID: "d", Type: "aliyun_oss", Bucket: "backup-prod"}}

	rows := backupSummaryRows(mustMap(t, plan), catalog.Builtin())
	found := false
	for _, row := range rows {
		if row.Label == "备份目的地" {
			found = true
			if !strings.Contains(row.Value, "阿里云 OSS") {
				t.Errorf("应使用槽位表登记的中文标签: %q", row.Value)
			}
		}
	}
	if !found {
		t.Fatal("缺少目的地行")
	}
}

func mustMap(t *testing.T, plan any) map[string]any {
	t.Helper()
	out, err := schema.PlanToMap(plan)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return out
}

// 可编辑的行要带上该槽位的可选项, 界面才能直接给出"改成什么"。
func TestSummaryRowsCarryOptions(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{
		ID: "s", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{
		Frequency: "weekly", Time: "01:00", Weekday: driver.IntPtr(7), Timezone: "Asia/Shanghai",
	}
	plan.Retention.Full = &schema.RetentionSpec{Value: 14, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "d", Type: "local", Path: "/u01/backup"}}

	summary := summarize(schema.KindBackup, plan, catalog.Builtin())
	byLabel := map[string]schema.SummaryRow{}
	for _, row := range summary.Rows {
		byLabel[row.Label] = row
	}

	// 认证方式是封闭候选: 应给出三个选项
	if row := byLabel["认证方式"]; len(row.Choices) != 3 {
		t.Errorf("认证方式应带 3 个候选值, 实际 %v", row.Choices)
	}
	// 保留期是开放推荐: 应给出常用值
	if row := byLabel["保留策略"]; len(row.Suggestions) == 0 {
		t.Errorf("保留策略应带推荐值: %v", row)
	}
	// 并发度也要有推荐值
	if row := byLabel["并发度"]; len(row.Suggestions) == 0 {
		t.Errorf("并发度应带推荐值: %v", row)
	}
	// 不可编辑的行不该带可选项
	if row := byLabel["任务名称"]; len(row.Choices)+len(row.Suggestions) != 0 {
		t.Errorf("任务名称不该带可选项: %v", row)
	}
}

// 每行的可选项都必须能被该槽位解析 —— 否则"修改这一项"点下去会失败。
func TestSummaryRowOptionsAreParseable(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{
		ID: "s", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 4},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{
		Frequency: "weekly", Time: "01:00", Weekday: driver.IntPtr(7), Timezone: "Asia/Shanghai",
	}
	plan.Retention.Full = &schema.RetentionSpec{Value: 14, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "d", Type: "local", Path: "/u01/backup"}}

	planMap := mustMap(t, plan)
	checked := 0
	for _, row := range summarize(schema.KindBackup, plan, catalog.Builtin()).Rows {
		if row.Slot == "" {
			continue
		}
		meta, ok := LookupSlotMeta(planMap, row.Slot, catalog.Builtin())
		if !ok {
			t.Errorf("%s: 行带了槽位 %q 却查不到元信息", row.Label, row.Slot)
			continue
		}
		for _, option := range append(append([]string{}, row.Choices...), row.Suggestions...) {
			checked++
			if _, err := ParseAnswer(meta, option); err != nil {
				t.Errorf("%s: 可选项 %q 解析失败: %v", row.Label, option, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何可选项")
	}
	t.Logf("已校验 %d 个字总结可选项", checked)
}

// 通配符是契约写法, 不能直接摆给用户看。
func TestSummaryRendersWildcardDatabaseAsChinese(t *testing.T) {
	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{
		ID: "s", Name: "MySQL", Kind: "mysql", Databases: []string{"*"},
		Options: map[string]any{"method": "logical", "databases": []string{"*"}},
	}}

	summary := summarize(schema.KindBackup, plan, catalog.Builtin())
	for _, row := range summary.Rows {
		if row.Label != "数据库" {
			continue
		}
		if row.Value != "全部数据库" {
			t.Fatalf("通配符应翻成中文, 实际 %q", row.Value)
		}
		return
	}
	t.Fatal("缺少数据库行")
}

func TestDatabaseListText(t *testing.T) {
	cases := []struct {
		want  string
		input []string
	}{
		{"全部数据库", []string{"*"}},
		{"全部数据库", []string{"*", "other"}}, // 通配符涵盖其余项
		{"db1、db2", []string{"db1", "db2"}},
	}
	for _, entry := range cases {
		want, input := entry.want, entry.input
		if got := databaseListText(input); got != want {
			t.Errorf("databaseListText(%v) = %q, 期望 %q", input, got, want)
		}
	}
}
