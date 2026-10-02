package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// 验收用例: 直接使用需求方给出的原句。
//
//	"周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4"
//
// 该句信息不完整 (没给时刻、保留期、目的地), 因此期望的行为是:
// 已给出的部分准确落进 JSON, 缺的部分恰好被追问, 补齐后 status=ready。
const acceptanceText = "周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4"

// newDemoAgent 构造一个无任何模型 Key 的 Agent, 即 demo 模式。
func newDemoAgent(t *testing.T, maxTurns int) *Agent {
	t.Helper()
	cfg := &config.Config{
		Server:   config.Server{MaxTurns: maxTurns, PromptLimit: 20000},
		Profiles: []*config.Profile{},
	}
	return New(llm.NewRegistry(cfg), Options{MaxTurns: maxTurns, SessionTTL: time.Minute})
}

// planMapOf 把回合结果里的计划转成 map, 便于按路径断言。
func planMapOf(t *testing.T, result *schema.TurnResult) map[string]any {
	t.Helper()
	if result.Plan == nil {
		t.Fatal("回合结果里没有计划")
	}
	planMap, err := schema.PlanToMap(result.Plan)
	if err != nil {
		t.Fatalf("计划序列化失败: %v", err)
	}
	return planMap
}

// normalize 把数字统一成字符串再比较。
//
// PlanToMap 用 UseNumber 保留整数精度 (端口/并发度不该经过浮点), 所以从 map 里
// 取出的数字是 json.Number 而不是 float64, 直接比较类型会不相等。
func normalize(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", value)
	}
}

func assertPath(t *testing.T, planMap map[string]any, path string, want any) {
	t.Helper()
	got, ok := GetPath(planMap, path)
	if !ok {
		t.Errorf("字段 %s 不存在", path)
		return
	}
	if normalize(got) != normalize(want) {
		t.Errorf("字段 %s = %#v, 期望 %#v", path, got, want)
	}
}

func questionSlots(questions []schema.Question) []string {
	slots := make([]string, 0, len(questions))
	for _, question := range questions {
		slots = append(slots, question.Slot)
	}
	return slots
}

// ---------------------------------------------------------------------------
// 验收 A: 原句 -> 准确抽取 + 恰好追问 + 补齐后 ready
// ---------------------------------------------------------------------------

func TestAcceptanceOracleExtraction(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if result.Kind != schema.KindBackup {
		t.Fatalf("意图应为 backup, 得到 %q", result.Kind)
	}

	planMap := planMapOf(t, result)
	assertPath(t, planMap, "source.items[0].kind", "oracle")
	assertPath(t, planMap, "source.items[0].options.auth_mode", "os_auth")
	assertPath(t, planMap, "source.items[0].options.scope", "whole_instance")
	assertPath(t, planMap, "source.items[0].options.channels", float64(4))
	assertPath(t, planMap, "schedule.full.frequency", "weekly")
	assertPath(t, planMap, "schedule.full.weekday", float64(7))
	assertPath(t, planMap, "schedule.incremental.frequency", "daily")
}

func TestAcceptanceAsksExactlyFourQuestions(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if result.Status != schema.StatusNeedsClarification {
		t.Fatalf("信息不完整, 应为 needs_clarification, 得到 %q", result.Status)
	}

	// 恰好问这四件事: 全备时刻、增量时刻、保留期、目的地。
	// 不多问 (已给的信息不该重复问), 不少问 (缺的必须问出来)。
	want := []string{
		"schedule.full.time",
		"schedule.incremental.time",
		"retention",
		"destinations",
	}
	got := questionSlots(result.Questions)
	if len(got) != len(want) {
		t.Fatalf("追问数量应为 %d, 实际 %d: %v", len(want), len(got), got)
	}
	for index, slot := range want {
		if got[index] != slot {
			t.Errorf("第 %d 个追问应为 %s, 实际 %s", index+1, slot, got[index])
		}
	}

	// 已给出的信息不该被重新问起
	for _, slot := range got {
		if strings.Contains(slot, "auth_mode") || strings.Contains(slot, "scope") ||
			strings.Contains(slot, "channels") || strings.Contains(slot, "weekday") {
			t.Errorf("已提供的信息不该追问: %s", slot)
		}
	}
}

func TestAcceptanceBecomesReadyAfterAnswers(t *testing.T) {
	agent := newDemoAgent(t, 10)

	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	second, err := agent.Answer(first.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘 /u01/backup/orcl",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	if second.Status != schema.StatusReady {
		t.Fatalf("补齐后应为 ready, 得到 %q, 剩余追问: %v",
			second.Status, questionSlots(second.Questions))
	}
	if len(second.Questions) != 0 {
		t.Fatalf("ready 时不该有追问: %v", questionSlots(second.Questions))
	}

	planMap := planMapOf(t, second)
	// 回答必须精确落进对应字段
	assertPath(t, planMap, "schedule.full.time", "01:00")
	assertPath(t, planMap, "schedule.incremental.time", "01:00")
	assertPath(t, planMap, "retention.full.value", float64(14))
	assertPath(t, planMap, "retention.full.unit", "days")
	// 起点信息不能被后续回合丢掉
	assertPath(t, planMap, "source.items[0].options.auth_mode", "os_auth")
	assertPath(t, planMap, "source.items[0].options.channels", float64(4))
	assertPath(t, planMap, "schedule.full.weekday", float64(7))
	if length := listLength(planMap, "destinations"); length != 1 {
		t.Errorf("应有 1 个目的地, 实际 %d", length)
	}
	if second.Turns != 2 {
		t.Errorf("回合数应为 2, 实际 %d", second.Turns)
	}
}

// 追问必须收敛, 不能反复问同一个问题。
func TestAcceptanceQuestionsDoNotRepeat(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	firstSlots := questionSlots(result.Questions)

	// 只回答时刻, 保留期与目的地仍缺
	second, err := agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time": "凌晨1点",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}
	secondSlots := questionSlots(second.Questions)

	for _, slot := range secondSlots {
		if slot == "schedule.full.time" {
			t.Error("已答的 schedule.full.time 不该再次追问")
		}
	}
	if len(secondSlots) >= len(firstSlots) {
		t.Errorf("追问数量应减少: 首轮 %v, 次轮 %v", firstSlots, secondSlots)
	}
}

// ---------------------------------------------------------------------------
// 终止性: 无论用户怎么答, 都必须能收敛
// ---------------------------------------------------------------------------

// 用户一直给出无法识别的回答时, 靠轮次上限熔断, 而不是无限追问。
func TestLoopTerminatesOnUnparseableAnswers(t *testing.T) {
	agent := newDemoAgent(t, 4)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 连续给出无法解析的回答
	for turn := 0; turn < 8 && result.Status == schema.StatusNeedsClarification; turn++ {
		answers := map[string]string{}
		for _, question := range result.Questions {
			answers[question.Slot] = "((("
		}
		result, err = agent.Answer(result.SessionID, answers, "")
		if err != nil {
			t.Fatalf("第 %d 轮续答失败: %v", turn, err)
		}
	}

	if result.Status != schema.StatusReady {
		t.Fatalf("轮次用尽后应交付结果, 得到 %q", result.Status)
	}
	if result.Turns > 4 {
		t.Errorf("回合数不该超过上限 4, 实际 %d", result.Turns)
	}

	hasCapNotice := false
	for _, notice := range result.Notices {
		if strings.Contains(notice, "最大追问轮次") {
			hasCapNotice = true
		}
	}
	if !hasCapNotice {
		t.Errorf("熔断时应给出提示: %v", result.Notices)
	}
	hasCapWarning := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "上限") {
			hasCapWarning = true
		}
	}
	if !hasCapWarning {
		t.Errorf("熔断时应写入警告, 让交付物自带风险说明: %v", result.Warnings)
	}
}

// 已经答过的槽位不该在下一轮被要求重答 —— 即便模型没把它填进计划。
func TestAnsweredSlotsStayAnswered(t *testing.T) {
	agent := newDemoAgent(t, 10)

	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	result, err = agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time": "凌晨1点",
	}, "")
	if err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	planMap := planMapOf(t, result)
	if got, _ := GetPath(planMap, "schedule.full.time"); got != "01:00" {
		t.Fatalf("回答未落进计划: %v", got)
	}
}
