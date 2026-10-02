package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// newAgentWithStore 构造一个会话落盘到临时文件的 Agent。
func newAgentWithStore(t *testing.T, path string) *Agent {
	t.Helper()
	cfg := &config.Config{
		Server:   config.Server{MaxTurns: 10, PromptLimit: 20000},
		Profiles: []*config.Profile{},
	}
	return New(llm.NewRegistry(cfg), Options{
		MaxTurns: 10, SessionTTL: time.Hour, SessionPath: path, MaxSessions: 30,
	})
}

func TestSessionPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	first := newAgentWithStore(t, path)
	result, err := first.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, err := first.Answer(result.SessionID, map[string]string{
		"schedule.full.time": "凌晨1点",
	}, ""); err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	// 模拟重启: 新建一个指向同一文件的 Agent
	reopened := newAgentWithStore(t, path)
	detail, ok := reopened.SessionDetail(result.SessionID)
	if !ok {
		t.Fatal("重启后历史会话应仍在")
	}
	if detail["text"] != acceptanceText {
		t.Errorf("需求原文未持久化: %v", detail["text"])
	}
	if detail["turns"] != 2 {
		t.Errorf("回合数应为 2, 实际 %v", detail["turns"])
	}

	transcript, _ := detail["transcript"].([]Record)
	if len(transcript) < 3 {
		t.Fatalf("对话记录应至少 3 条 (需求/回答/两轮 agent), 实际 %d", len(transcript))
	}
	if transcript[0].Role != RoleUser {
		t.Errorf("第一条应是用户发言, 实际 %q", transcript[0].Role)
	}
}

func TestSessionFileIs0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	agent := newAgentWithStore(t, path)

	if _, err := agent.Start(Request{Text: acceptanceText}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("会话文件未落盘: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("权限应为 0600, 实际 %04o", mode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("内容不是合法 JSON: %v", err)
	}
}

func TestSessionCorruptFileDoesNotBreakStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	// 会话历史损坏不该让服务起不来
	agent := newAgentWithStore(t, path)
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("损坏的历史不该影响新会话: %v", err)
	}
	if result.SessionID == "" {
		t.Fatal("应正常创建会话")
	}
}

func TestSessionTitleFromFirstLine(t *testing.T) {
	session := &Session{Text: "周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4"}
	title := session.Title()
	if title == "" {
		t.Fatal("标题不该为空")
	}
	if len([]rune(title)) > 25 {
		t.Errorf("标题应被截断: %q (%d 字)", title, len([]rune(title)))
	}
	if !strings.HasPrefix(title, "周天全备份") {
		t.Errorf("标题应取需求开头: %q", title)
	}

	multiline := &Session{Text: "第一行需求\n第二行需求"}
	if got := multiline.Title(); got != "第一行需求" {
		t.Errorf("多行需求应只取首行: %q", got)
	}

	if got := (&Session{}).Title(); got != "(空需求)" {
		t.Errorf("空需求应有兜底标题: %q", got)
	}
}

func TestSessionTitleTruncatesLongChinese(t *testing.T) {
	long := strings.Repeat("备", 60)
	title := (&Session{Text: long}).Title()
	if len([]rune(title)) != 25 { // 24 字 + 省略号
		t.Fatalf("应按字符截断到 24 字加省略号, 实际 %d: %q", len([]rune(title)), title)
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("截断后应有省略号: %q", title)
	}
}

// 自由文本补充: 用户可以一句话补多个信息, 不必逐格填空。
func TestContinueWithFreeText(t *testing.T) {
	agent := newDemoAgent(t, 10)

	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if first.Status != schema.StatusNeedsClarification {
		t.Fatalf("应先进入追问: %q", first.Status)
	}

	second, err := agent.Continue(first.SessionID, "都是凌晨1点执行，保留14天，备份到本地 /u01/backup/orcl")
	if err != nil {
		t.Fatalf("自由补充失败: %v", err)
	}
	if second.Status != schema.StatusReady {
		t.Logf("剩余追问: %v", questionSlots(second.Questions))
	}

	planMap := planMapOf(t, second)
	// 起点信息不能因为补充而丢失
	assertPath(t, planMap, "source.items[0].options.auth_mode", "os_auth")
	assertPath(t, planMap, "source.items[0].options.channels", float64(4))
	assertPath(t, planMap, "schedule.full.weekday", float64(7))
}

func TestContinueRejectsEmptyText(t *testing.T) {
	agent := newDemoAgent(t, 10)
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, err := agent.Continue(result.SessionID, "   "); err == nil {
		t.Fatal("空补充应被拒绝")
	}
	if _, err := agent.Continue("s-nope", "补充"); err == nil {
		t.Fatal("未知会话应被拒绝")
	}
}

// 自由补充会累积进需求文本, 因此下一轮仍能看到。
func TestContinueAccumulatesFollowUps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	agent := newAgentWithStore(t, path)

	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, err := agent.Continue(first.SessionID, "保留7天"); err != nil {
		t.Fatalf("第一次补充失败: %v", err)
	}
	if _, err := agent.Continue(first.SessionID, "存到本地 /data/backup"); err != nil {
		t.Fatalf("第二次补充失败: %v", err)
	}

	detail, ok := agent.SessionDetail(first.SessionID)
	if !ok {
		t.Fatal("应能取到会话详情")
	}
	followUps, _ := detail["follow_ups"].([]string)
	if len(followUps) != 2 {
		t.Fatalf("应累积两条补充, 实际 %v", followUps)
	}
}

func TestListSessionsSortedByActivity(t *testing.T) {
	agent := newDemoAgent(t, 10)

	first, err := agent.Start(Request{Text: "每天凌晨2点备份MySQL到本地磁盘 /u01/backup，保留7天"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := agent.Start(Request{Text: "把Oracle数据库恢复一下"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	sessions := agent.ListSessions()
	if len(sessions) != 2 {
		t.Fatalf("应有 2 条历史, 实际 %d", len(sessions))
	}
	// 最近活动的排最前
	if sessions[0]["id"] != second.SessionID {
		t.Errorf("最近活动的应排最前, 得到 %v", sessions[0]["id"])
	}
	if sessions[1]["id"] != first.SessionID {
		t.Errorf("顺序不符: %v", sessions[1]["id"])
	}

	// 摘要里要有侧栏需要的字段
	for _, field := range []string{"id", "title", "kind", "status", "turns", "pending", "updated_at"} {
		if _, ok := sessions[0][field]; !ok {
			t.Errorf("摘要缺少字段 %s", field)
		}
	}
	if sessions[0]["kind"] != schema.KindRestore {
		t.Errorf("意图应为 restore: %v", sessions[0]["kind"])
	}
}

func TestDeleteSession(t *testing.T) {
	agent := newDemoAgent(t, 10)
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if !agent.DeleteSession(result.SessionID) {
		t.Fatal("删除应成功")
	}
	if agent.DeleteSession(result.SessionID) {
		t.Fatal("重复删除应返回 false")
	}
	if len(agent.ListSessions()) != 0 {
		t.Error("删除后历史应为空")
	}
	if _, ok := agent.SessionDetail(result.SessionID); ok {
		t.Error("删除后不该还能取到详情")
	}
}

func TestSessionDetailHasTranscriptAndPlan(t *testing.T) {
	agent := newDemoAgent(t, 10)
	result, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 补齐让它 ready, 这样详情里应有计划
	if _, err := agent.Answer(result.SessionID, map[string]string{
		"schedule.full.time":        "凌晨1点",
		"schedule.incremental.time": "凌晨1点",
		"retention":                 "保留14天",
		"destinations":              "本地磁盘 /u01/backup/orcl",
	}, ""); err != nil {
		t.Fatalf("续答失败: %v", err)
	}

	detail, ok := agent.SessionDetail(result.SessionID)
	if !ok {
		t.Fatal("应能取到详情")
	}
	if detail["status"] != schema.StatusReady {
		t.Errorf("状态应为 ready: %v", detail["status"])
	}
	if detail["plan"] == nil {
		t.Error("详情应带最近一次的计划")
	}

	transcript, _ := detail["transcript"].([]Record)
	if len(transcript) == 0 {
		t.Fatal("应带对话记录")
	}
	// 最后一条 agent 记录应带状态, 供界面区分"这一轮问了什么"
	last := transcript[len(transcript)-1]
	if last.Role != RoleAgent {
		t.Errorf("最后一条应是 agent 记录: %q", last.Role)
	}
	if last.Status == "" {
		t.Error("agent 记录应带状态")
	}
}

// 上游创建的旧会话没有逐轮记录时, 详情仍需能用 (靠需求原文兜底)。
func TestSessionDetailHandlesLegacyWithoutTranscript(t *testing.T) {
	session := &Session{
		ID: "s-legacy", Kind: schema.KindBackup, Text: "旧会话需求",
		Answers: map[string]string{}, Asked: map[string]string{},
	}
	detail := session.Detail()
	if detail["text"] != "旧会话需求" {
		t.Errorf("应回显需求原文: %v", detail["text"])
	}
	transcript, ok := detail["transcript"].([]Record)
	if !ok || transcript == nil {
		t.Error("没有记录时应返回空数组而不是 null, 否则前端要额外判空")
	}
	questions, ok := detail["questions"].([]schema.Question)
	if !ok || questions == nil {
		t.Error("没有追问时应返回空数组")
	}
}

func TestStorePrunesOldestBeyondLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	cfg := &config.Config{Server: config.Server{MaxTurns: 5, PromptLimit: 20000}}
	agent := New(llm.NewRegistry(cfg), Options{
		MaxTurns: 5, SessionTTL: time.Hour, SessionPath: path, MaxSessions: 3,
	})

	for index := 0; index < 6; index++ {
		if _, err := agent.Start(Request{Text: "第 " + string(rune('A'+index)) + " 条需求"}); err != nil {
			t.Fatalf("第 %d 条失败: %v", index, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if got := len(agent.ListSessions()); got != 3 {
		t.Errorf("超出上限应裁剪到 3 条, 实际 %d", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var decoded struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(decoded.Sessions) != 3 {
		t.Errorf("落盘也只该保留 3 条, 实际 %d", len(decoded.Sessions))
	}
}

func TestStoreWithoutPathStaysInMemory(t *testing.T) {
	agent := newDemoAgent(t, 10)
	if agent.Store().Path() != "" {
		t.Errorf("未配置时不该有落盘路径: %q", agent.Store().Path())
	}
	if _, err := agent.Start(Request{Text: acceptanceText}); err != nil {
		t.Fatalf("仅内存模式也应可用: %v", err)
	}
}

// 旧会话重新打开时, 待补充的问题应按当前槽位表重新推导 ——
// 否则槽位表新增推荐值后, 那些会话永远停留在当时的形态。
func TestSessionDetailRedefinesQuestions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	agent := newAgentWithStore(t, path)

	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 模拟"旧会话": 把存下的问题清空, 只剩计划
	session, ok := agent.Store().Get(first.SessionID)
	if !ok {
		t.Fatal("未找到会话")
	}
	session.Questions = nil

	detail, ok := agent.SessionDetail(first.SessionID)
	if !ok {
		t.Fatal("应能取到详情")
	}
	questions, _ := detail["questions"].([]schema.Question)
	if len(questions) == 0 {
		t.Fatal("问题应由服务端按当前状态重新推导出来")
	}

	// 推导出来的问题要带推荐值
	withOptions := 0
	for _, question := range questions {
		if len(question.Choices)+len(question.Suggestions) > 0 {
			withOptions++
		}
	}
	if withOptions != len(questions) {
		t.Errorf("每个追问都该给出可点的推荐值, 实际 %d/%d", withOptions, len(questions))
	}
}

// 已完成的会话不该被重新推导出问题 (它的计划已经冻结)。
func TestSessionDetailKeepsReadySessionsQuiet(t *testing.T) {
	agent := newDemoAgent(t, 10)
	first, err := agent.Start(Request{Text: acceptanceText})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
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
		t.Fatalf("应已完成: %q", ready.Status)
	}

	detail, _ := agent.SessionDetail(first.SessionID)
	questions, _ := detail["questions"].([]schema.Question)
	if len(questions) != 0 {
		t.Errorf("已完成的会话不该再有问题: %v", questions)
	}
}
