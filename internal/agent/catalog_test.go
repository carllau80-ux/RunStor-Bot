package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// catalogServer 起一个 mock 清单接口, 返回给定的应用与介质服务器。
func catalogServer(t *testing.T, applications, mediaServers []map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{}
		if applications != nil {
			payload["applications"] = applications
		}
		if mediaServers != nil {
			payload["media_servers"] = mediaServers
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

// agentWithCatalog 构造一个清单来自接口的 Agent。
func agentWithCatalog(t *testing.T, url string) *Agent {
	t.Helper()
	enabled := true
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	return New(llm.NewRegistry(cfg), Options{
		MaxTurns: 10, SessionTTL: time.Minute,
		Catalog: config.CatalogConfig{
			Enabled: &enabled, URL: url, TimeoutSeconds: 3, TTLSeconds: 60,
		},
	})
}

// 目标介质来自接口: 目的地槽位的候选项就是接口返回的那些服务器。
func TestDestinationsComeFromCatalogAPI(t *testing.T) {
	server := catalogServer(t, nil, []map[string]any{
		{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
		{"id": "tape-01", "name": "磁带库01", "type": "tape"},
	})
	agent := agentWithCatalog(t, server.URL)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	var destinations schema.Question
	for _, question := range result.Questions {
		if question.Slot == "destinations" {
			destinations = question
		}
	}
	if destinations.Slot == "" {
		t.Fatalf("应有目的地追问: %v", questionSlots(result.Questions))
	}

	joined := strings.Join(destinations.Choices, "|")
	if !strings.Contains(joined, "本地磁盘池A") || !strings.Contains(joined, "磁带库01") {
		t.Fatalf("候选项应来自接口清单, 实际: %v", destinations.Choices)
	}
	if !strings.Contains(joined, "/u01/backup") {
		t.Errorf("候选项应带上落点, 用户才知道选的是哪个目录: %v", destinations.Choices)
	}
	// 内置的目的地类型不该再出现 (清单接口才是权威)
	if strings.Contains(joined, "阿里云 OSS") {
		t.Errorf("接口给出了清单时不该混入内置候选: %v", destinations.Choices)
	}
}

// 选中一台介质服务器后, 计划里的落点应取自该服务器的定义。
func TestSelectingMediaServerFillsDestinationFromCatalog(t *testing.T) {
	server := catalogServer(t, nil, []map[string]any{
		{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
	})
	agent := agentWithCatalog(t, server.URL)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 模拟界面点选候选项
	answer := "本地磁盘池A /u01/backup"
	second, err := agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              answer,
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}
	if second.Status != schema.StatusReady {
		t.Fatalf("应已完成: %q 剩余 %v", second.Status, questionSlots(second.Questions))
	}

	planMap := planMapOf(t, second)
	// 同类型替换: 模型从原文抽出的那条本地目的地的落点被答案覆盖, 不该变成两条
	if length := listLength(planMap, "destinations"); length != 1 {
		t.Fatalf("同类型的落点应替换而不是追加, 实际 %d 条", length)
	}
	assertPath(t, planMap, "destinations[0].type", "local")
	assertPath(t, planMap, "destinations[0].path", "/u01/backup")
	assertPath(t, planMap, "destinations[0].media_server", "本地磁盘池A")
}

// 不同类型的落点是"增加一处副本", 应该追加而不是把原来那处覆盖掉。
func TestDifferentDestinationTypeAppends(t *testing.T) {
	server := catalogServer(t, nil, []map[string]any{
		{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
		{"id": "dr-s3", "name": "异地对象存储", "type": "s3", "bucket": "dr-backup"},
	})
	agent := agentWithCatalog(t, server.URL)

	planMap := oraclePlanMap(t)
	if err := SetPath(planMap, "destinations", []any{
		map[string]any{"id": "dest-1", "type": "local", "path": "/u01/backup"},
	}); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	plan, _, problems := ApplyAnswers(schema.KindBackup, planMap,
		map[string]string{"destinations": "异地对象存储"},
		agent.Catalog(), lookup(planMap))
	if len(problems) != 0 {
		t.Fatalf("不该有问题: %v", problems)
	}

	typed := plan.(*schema.BackupPlan)
	if len(typed.Destinations) != 2 {
		t.Fatalf("不同类型应追加为第二处副本, 实际 %d 条: %+v",
			len(typed.Destinations), typed.Destinations)
	}
	offsite := typed.Destinations[1]
	if offsite.Type != "s3" || offsite.Bucket != "dr-backup" {
		t.Errorf("异地副本应取自清单里的服务器定义: %+v", offsite)
	}
	if typed.Destinations[0].Path != "/u01/backup" {
		t.Errorf("原有的本地落点不该被覆盖: %+v", typed.Destinations[0])
	}
}

// 用户只写服务器名时也要能对上, 不必连路径一起抄。
func TestSelectingMediaServerByNameOnly(t *testing.T) {
	server := catalogServer(t, nil, []map[string]any{
		{"id": "nfs-1", "name": "共享目录1", "type": "nfs", "path": "/mnt/nfs/backup"},
	})
	agent := agentWithCatalog(t, server.URL)

	result, _ := agent.Start(Request{Text: acceptanceText})
	second, err := agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "共享目录1",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	planMap := planMapOf(t, second)
	assertPath(t, planMap, "destinations[0].type", "nfs")
	assertPath(t, planMap, "destinations[0].path", "/mnt/nfs/backup")
}

// 接口提供的应用 (内置驱动没有的) 也要能被识别、追问与校验。
func TestApplicationOnlyDefinedByCatalog(t *testing.T) {
	server := catalogServer(t, []map[string]any{{
		"kind": "dameng", "display": "达梦 DM8", "source_type": "database",
		"aliases": []string{"达梦"},
		"options": []map[string]any{{
			"field": "mode", "label": "备份方式", "kind": "enum",
			"choices":     []string{"全库", "增量"},
			"choices_map": map[string]string{"全库": "full", "增量": "incremental"},
		}},
	}}, nil)
	agent := agentWithCatalog(t, server.URL)

	// 只给对象类型, 让缺口分析按清单里声明的参数追问
	plan := schema.NewBackupPlan()
	plan.TaskName = "达梦备份"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "达梦", Kind: "dameng", Options: map[string]any{},
	}}
	plan.Schedule.Full = &schema.ScheduleSpec{Frequency: "daily", Time: "02:00", Timezone: "Asia/Shanghai"}
	plan.Retention.Full = &schema.RetentionSpec{Value: 7, Unit: "days"}
	plan.Destinations = []schema.Destination{{ID: "dest-1", Type: "local"}}

	gaps := AnalyzeGaps(schema.KindBackup, plan, map[string]string{}, agent.Catalog())
	found := false
	for _, question := range gaps.Questions {
		if question.Slot != "source.items[0].options.mode" {
			continue
		}
		found = true
		if question.Question != "备份方式" {
			t.Errorf("问句应取自清单里的 label, 实际 %q", question.Question)
		}
		if len(question.Choices) != 2 {
			t.Errorf("候选项应取自清单: %v", question.Choices)
		}
	}
	if !found {
		t.Fatalf("应按清单声明的参数追问 mode: %v", questionSlots(gaps.Questions))
	}

	// 按清单校验: 非法取值要报错
	plan.Source.Items[0].Options = map[string]any{"mode": "whatever"}
	report := ValidatePlan(plan, agent.Catalog())
	messages := strings.Join(report.Messages(schema.LevelError), "\n")
	if !strings.Contains(messages, "options.mode") {
		t.Errorf("应报出非法取值: %s", messages)
	}
}

// 清单里没有的应用要给提示 (可能是抽错了, 也可能是清单没覆盖)。
func TestUnknownApplicationWarns(t *testing.T) {
	server := catalogServer(t, []map[string]any{
		{"kind": "oracle", "display": "Oracle"},
	}, nil)
	agent := agentWithCatalog(t, server.URL)

	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{
		ID: "src-1", Name: "某库", Kind: "db2", Options: map[string]any{},
	}}

	report := ValidatePlan(plan, agent.Catalog())
	warnings := strings.Join(report.Messages(schema.LevelWarning), "\n")
	if !strings.Contains(warnings, "db2") {
		t.Fatalf("应提示应用不在清单中: %s", warnings)
	}
	if !strings.Contains(warnings, "运行时接口") {
		t.Errorf("提示里应说明清单来源: %s", warnings)
	}
}

// 提示词里必须带上清单, 否则模型会凭常识编出环境里没有的东西。
func TestPromptCarriesCatalog(t *testing.T) {
	server := catalogServer(t,
		[]map[string]any{{"kind": "dameng", "display": "达梦 DM8"}},
		[]map[string]any{{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"}})
	agent := agentWithCatalog(t, server.URL)

	system, err := SystemPrompt(schema.KindBackup, agent.Catalog())
	if err != nil {
		t.Fatalf("组装提示词失败: %v", err)
	}
	for _, want := range []string{"dameng", "达梦 DM8", "本地磁盘池A", "/u01/backup", "运行时接口"} {
		if !strings.Contains(system, want) {
			t.Errorf("提示词里缺少 %q", want)
		}
	}
	if !strings.Contains(system, "media_server") {
		t.Error("提示词应说明把服务器名填进 media_server")
	}
}

// 接口没配时走内置清单, 行为与之前一致。
func TestBuiltinCatalogKeepsPreviousBehavior(t *testing.T) {
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	agent := New(llm.NewRegistry(cfg), Options{MaxTurns: 10, SessionTTL: time.Minute})

	cat := agent.Catalog()
	if cat.Source != "builtin" {
		t.Errorf("未配置时应使用内置清单, 实际 %q", cat.Source)
	}

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var destinations schema.Question
	for _, question := range result.Questions {
		if question.Slot == "destinations" {
			destinations = question
		}
	}
	if !strings.Contains(strings.Join(destinations.Choices, "|"), "本地磁盘") {
		t.Errorf("内置清单的目的地候选不符: %v", destinations.Choices)
	}
}

// 清单接口挂了不能影响可用性: 退回内置清单, 流程照常。
func TestCatalogFailureDoesNotBreakFlow(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	agent := agentWithCatalog(t, dead.URL)
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("清单接口挂了不该让解析失败: %v", err)
	}
	if result.Status != schema.StatusNeedsClarification {
		t.Errorf("应正常进入追问: %q", result.Status)
	}

	cat := agent.Catalog()
	if cat.Source != "builtin-fallback" {
		t.Errorf("应退回内置清单, 实际 %q", cat.Source)
	}
	if cat.Warning == "" {
		t.Error("应说明退化原因, 否则用户不知道清单不完整")
	}
}

// 汇总里显示的是介质服务器名, 而不是 local/s3 这类代码值。
func TestSummaryShowsMediaServerName(t *testing.T) {
	server := catalogServer(t, nil, []map[string]any{
		{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
	})
	agent := agentWithCatalog(t, server.URL)

	result, _ := agent.Start(Request{Text: acceptanceText})
	ready, err := agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘池A /u01/backup",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}
	if ready.Summary == nil {
		t.Fatal("完成时应给出总结")
	}
	for _, row := range ready.Summary.Rows {
		if row.Label != "备份目的地" {
			continue
		}
		if !strings.Contains(row.Value, "本地磁盘池A") {
			t.Fatalf("总结应显示介质服务器名: %q", row.Value)
		}
		if strings.Contains(row.Value, `"local"`) {
			t.Errorf("总结里不该出现代码值: %q", row.Value)
		}
		return
	}
	t.Fatal("缺少备份目的地行")
}

// 取值标签也要以清单为准: 否则会出现"问题标签来自接口、值标签来自内置"的不一致。
func TestSummaryValueLabelFollowsCatalog(t *testing.T) {
	server := catalogServer(t, []map[string]any{{
		"kind": "oracle", "display": "Oracle",
		"options": []map[string]any{{
			"field": "auth_mode", "label": "认证方式", "kind": "enum",
			// 清单里的标签比内置的更贴合这家环境
			"choices":     []string{"系统认证", "账号密码"},
			"choices_map": map[string]string{"系统认证": "os_auth", "账号密码": "password"},
		}},
	}}, nil)
	agent := agentWithCatalog(t, server.URL)

	plan := schema.NewBackupPlan()
	plan.TaskName = "t"
	plan.Source.Items = []schema.SourceItem{{
		ID: "s", Name: "Oracle", Kind: "oracle",
		Options: map[string]any{"auth_mode": "os_auth"},
	}}

	summary := summarize(schema.KindBackup, plan, agent.Catalog())
	for _, row := range summary.Rows {
		if row.Label != "认证方式" {
			continue
		}
		if row.Value != "系统认证" {
			t.Fatalf("取值标签应取自清单 (期望 系统认证), 实际 %q", row.Value)
		}
		return
	}
	t.Fatal("缺少认证方式行")
}
