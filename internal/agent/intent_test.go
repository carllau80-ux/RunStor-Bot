package agent

import (
	"strings"
	"testing"

	"backup-restore-agent/internal/schema"
)

func TestDetectIntentBackup(t *testing.T) {
	cases := []string{
		"周天全备份oracle数据库，每天增量备份",
		"每天凌晨2点备份MySQL到本地磁盘",
		"给文件服务器 /data 做每周全备，保留30天",
		"备份达梦数据库",
		"把数据库的重要数据备份一下",
	}
	for _, text := range cases {
		if got := DetectIntent(text); got != schema.KindBackup {
			t.Errorf("DetectIntent(%q) = %q, 期望 backup", text, got)
		}
	}
}

func TestDetectIntentRestore(t *testing.T) {
	cases := []string{
		"Oracle数据库出故障了，恢复到2026-09-25 10:30:00",
		"把这个库还原到昨天下午3点",
		"回滚到上一个备份点",
		"从生产库的周全备恢复到另一台机器 192.0.2.11",
		"需要把数据恢复回来",
		"库挂了，帮我恢复",
	}
	for _, text := range cases {
		if got := DetectIntent(text); got != schema.KindRestore {
			t.Errorf("DetectIntent(%q) = %q, 期望 restore", text, got)
		}
	}
}

// 这是最容易误判的一类: 备份需求里常提到"恢复演练/恢复校验",
// 不能因为出现"恢复"二字就判成恢复意图。
func TestDetectIntentIgnoresDrillMentions(t *testing.T) {
	cases := []string{
		"每天凌晨2点全量备份MySQL，每周五做一次恢复演练",
		"备份Oracle并做恢复验证",
		"每天增量备份，每月做一次恢复测试",
		"全备后做恢复演练，保留30天",
	}
	for _, text := range cases {
		if got := DetectIntent(text); got != schema.KindBackup {
			t.Errorf("DetectIntent(%q) = %q, 期望 backup (演练不算恢复意图)", text, got)
		}
	}
}

// 同时提到备份与恢复时, 按备份处理 —— 备份需求里常顺带提到可恢复性。
func TestDetectIntentPrefersBackupWhenBothMentioned(t *testing.T) {
	text := "备份Oracle数据库，保留30天，要能恢复到任意时间点"
	if got := DetectIntent(text); got != schema.KindBackup {
		t.Errorf("两者都提到时应按备份处理, 得到 %q", got)
	}
}

func TestNormalizeKind(t *testing.T) {
	cases := map[string]string{
		"backup":    schema.KindBackup,
		"BACKUP":    schema.KindBackup,
		"备份":        schema.KindBackup,
		"restore":   schema.KindRestore,
		"恢复":        schema.KindRestore,
		"还原":        schema.KindRestore,
		" Restore ": schema.KindRestore,
		"":          "",
		"whatever":  "",
	}
	for input, want := range cases {
		if got := NormalizeKind(input); got != want {
			t.Errorf("NormalizeKind(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 恢复闭环
// ---------------------------------------------------------------------------

const restoreText = "Oracle数据库出故障了，从生产Oracle实例周全备恢复到2026-09-25 10:30:00这个时间点"

func TestRestoreFlowProducesRestorePlan(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: restoreText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if result.Kind != schema.KindRestore {
		t.Fatalf("意图应识别为 restore, 得到 %q", result.Kind)
	}

	plan, ok := result.Plan.(*schema.RestorePlan)
	if !ok {
		t.Fatalf("计划类型不对: %T", result.Plan)
	}
	if plan.Point.Type != "timestamp" {
		t.Errorf("时间点类型不对: %q", plan.Point.Type)
	}
	if plan.Point.Value != "2026-09-25 10:30:00" {
		t.Errorf("时间点取值不对: %q", plan.Point.Value)
	}
	if plan.Target.Mode != "in_place" {
		t.Errorf("未提异机时应为原机恢复: %q", plan.Target.Mode)
	}
}

func TestRestoreExplicitKindOverride(t *testing.T) {
	agent := newDemoAgent(t, 10)

	// 文本里没有任何恢复词, 靠 kind 参数强制指定
	result, err := agent.Start(Request{
		Text: "Oracle 生产库，从上周的全备恢复", Kind: "restore",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if result.Kind != schema.KindRestore {
		t.Fatalf("显式 kind 应生效, 得到 %q", result.Kind)
	}
}

func TestRestoreAlternateHostAsksForHost(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: "把Oracle库异机恢复，恢复到2026-09-25 10:30:00"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if result.Status != schema.StatusNeedsClarification {
		t.Fatalf("异机恢复缺目标主机, 应追问, 得到 %q", result.Status)
	}

	found := false
	for _, question := range result.Questions {
		if question.Slot == "target.host" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应追问目标主机: %v", questionSlots(result.Questions))
	}

	// 补上主机后应 ready
	second, err := agent.Answer(result.SessionID, map[string]string{
		"target.host": "192.0.2.11",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	planMap := planMapOf(t, second)
	assertPath(t, planMap, "target.host", "192.0.2.11")
	assertPath(t, planMap, "target.mode", "alternate_host")
	if second.Status != schema.StatusReady {
		t.Logf("剩余追问: %v", questionSlots(second.Questions))
	}
}

func TestRestoreSCN(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{
		Text: "Oracle库需要恢复，恢复到SCN 1234567",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	plan, ok := result.Plan.(*schema.RestorePlan)
	if !ok {
		t.Fatalf("计划类型不对: %T", result.Plan)
	}
	if plan.Point.Type != "scn" || plan.Point.Value != "1234567" {
		t.Fatalf("SCN 解析不对: %+v", plan.Point)
	}
}

// 恢复计划里不该出现备份侧的字段。
func TestRestorePlanHasNoBackupFields(t *testing.T) {
	agent := newDemoAgent(t, 10)
	result, err := agent.Start(Request{Text: restoreText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	planMap := planMapOf(t, result)

	for _, forbidden := range []string{"schedule", "retention", "destinations"} {
		if _, exists := planMap[forbidden]; exists {
			t.Errorf("恢复计划不该包含 %s", forbidden)
		}
	}
	for _, required := range []string{"point", "target", "scope", "alternate"} {
		if _, exists := planMap[required]; !exists {
			t.Errorf("恢复计划应包含 %s", required)
		}
	}
}

// 未指定时间点时给出提示, 而不是悄悄按"最近可用"处理。
func TestRestoreLatestWarnsAboutDataLoss(t *testing.T) {
	agent := newDemoAgent(t, 10)
	result, err := agent.Start(Request{Text: "把Oracle数据库恢复一下"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "未指定恢复时间点") {
		t.Fatalf("应提示未指定时间点的风险: %v", result.Warnings)
	}
}
