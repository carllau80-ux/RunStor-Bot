package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"backup-restore-agent/internal/catalog"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// Options 是 Agent 的可调参数。
type Options struct {
	MaxTurns    int
	PromptLimit int
	SessionTTL  time.Duration
	// SessionPath 是会话历史的落盘位置; 空表示仅内存 (重启即丢)。
	SessionPath string
	// MaxSessions 限制保留的历史条数, 让落盘文件大小有界。
	MaxSessions int
	// Catalog 是"能备份哪些应用 / 能备份到哪些介质服务器"的查询配置。
	Catalog config.CatalogConfig
}

// Agent 编排一次对话回合: 抽取 -> 槽位回填 -> 缺口分析 -> 追问或冻结。
type Agent struct {
	registry    *llm.Registry
	store       *Store
	catalog     *catalog.Provider
	maxTurns    int
	promptLimit int
}

// New 构造 Agent。
func New(registry *llm.Registry, opts Options) *Agent {
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 10
	}
	if opts.PromptLimit <= 0 {
		opts.PromptLimit = 20000
	}
	return &Agent{
		registry: registry,
		store: NewStore(StoreOptions{
			TTL: opts.SessionTTL, Path: opts.SessionPath, MaxSessions: opts.MaxSessions,
		}),
		catalog:     catalog.NewProvider(opts.Catalog),
		maxTurns:    opts.MaxTurns,
		promptLimit: opts.PromptLimit,
	}
}

// Store 暴露会话存储 (接口层需要查会话与重置)。
func (a *Agent) Store() *Store { return a.store }

// Registry 暴露模型注册表。
func (a *Agent) Registry() *llm.Registry { return a.registry }

// Catalog 返回当前清单 (带缓存)。
func (a *Agent) Catalog() catalog.Catalog { return a.catalog.Get() }

// CatalogProvider 暴露清单提供者 (供"刷新清单"等操作)。
func (a *Agent) CatalogProvider() *catalog.Provider { return a.catalog }

// MaxTurns 返回追问熔断轮次。
func (a *Agent) MaxTurns() int { return a.maxTurns }

// PromptLimit 返回需求文本长度上限。
func (a *Agent) PromptLimit() int { return a.promptLimit }

// Request 是一次新需求的入参。
type Request struct {
	Text string
	// Kind 可显式指定 backup/restore; 留空则自动判定
	Kind string
	// Profile 是请求级模型覆盖, 优先级最高; 留空用当前生效的 profile
	Profile string
}

// Start 处理一条新需求。
func (a *Agent) Start(req Request) (*schema.TurnResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return nil, fmt.Errorf("需求文本不能为空")
	}
	if len([]rune(text)) > a.promptLimit {
		return nil, fmt.Errorf("需求文本过长 (%d 字), 上限 %d 字",
			len([]rune(text)), a.promptLimit)
	}

	kind := NormalizeKind(req.Kind)
	if kind == "" {
		kind = DetectIntent(text)
	}

	session := a.store.Create(kind, text)
	// Create 已经把会话发布进 Store 了, 所以从这里开始也只能在副本上改
	// (已发布的对象对外只读, 见 Session.Clone)。
	next := session.Clone()
	next.Append(Record{Role: RoleUser, Text: text})
	return a.run(next, req.Profile)
}

// ErrSessionBusy 表示同一个会话上一轮还在处理中。
//
// 单独一个哨兵错误, 是为了让上层能映射成 409 (而不是 500): 这是正常的并发输入
// (双击提交、两个标签页), 不是服务出错。
var ErrSessionBusy = errors.New("该会话上一轮还在处理中, 请等它结束后再继续")

// beginTurn 取会话的独占权, 返回当前快照 (只读) 与解锁函数。
//
// 一轮问答里要调模型 (秒级), 所以这里不做阻塞排队: 并发打到同一会话就直接回
// ErrSessionBusy, 由界面提示用户稍等 —— 比让请求默默排几秒要好。
func (a *Agent) beginTurn(sessionID string) (*Session, func(), error) {
	release, exists, busy := a.store.TryLockSession(sessionID)
	if busy {
		return nil, nil, ErrSessionBusy
	}
	if !exists {
		return nil, nil, fmt.Errorf("会话不存在或已过期: %s", sessionID)
	}
	session, ok := a.store.Get(sessionID)
	if !ok {
		release()
		return nil, nil, fmt.Errorf("会话不存在或已过期: %s", sessionID)
	}
	return session, release, nil
}

// Answer 处理用户对追问的回答。
func (a *Agent) Answer(sessionID string, answers map[string]string, profile string) (*schema.TurnResult, error) {
	session, release, err := a.beginTurn(sessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	if len(answers) == 0 {
		return nil, fmt.Errorf("请至少回答一个待补充的问题")
	}

	next := session.Clone()
	for slot, answer := range answers {
		trimmed := strings.TrimSpace(answer)
		if trimmed == "" {
			continue
		}
		next.Answers[slot] = trimmed
		question := next.Asked[slot]
		if question == "" {
			question = slot
		}
		next.QAPairs = append(next.QAPairs, llm.QAPair{
			Slot: slot, Question: question, Answer: trimmed,
		})
	}
	next.Append(Record{
		Role:    RoleUser,
		Text:    formatAnswers(answers),
		Answers: answers,
	})
	return a.run(next, profile)
}

// Reset 重置会话, 相当于用同一份需求重新开始。
func (a *Agent) Reset(sessionID string) (*schema.TurnResult, error) {
	release, exists, busy := a.store.TryLockSession(sessionID)
	if busy {
		return nil, ErrSessionBusy
	}
	if !exists {
		return nil, fmt.Errorf("会话不存在或已过期: %s", sessionID)
	}
	defer release()

	session, ok := a.store.Reset(sessionID)
	if !ok {
		return nil, fmt.Errorf("会话不存在或已过期: %s", sessionID)
	}
	// Reset 造出的是一份全新的会话 (新 ID), 同样按"副本上改"的规矩走
	return a.run(session.Clone(), "")
}

// run 是一个完整回合。
//
// session 必须是**尚未发布的副本** (调用方用 Session.Clone 造出来): 这里从头到尾就地改它,
// 结束时一次性 Publish —— 期间读接口看到的一直是变更前的完整状态, 不会读到写了一半的会话。
func (a *Agent) run(session *Session, profile string) (*schema.TurnResult, error) {
	session.Turns++
	client := a.registry.Client(profile)

	// 本轮取一次清单快照: 提示词、校验、推荐值、总结都用同一份,
	// 免得清单接口在两次调用之间变了导致前后不一致。
	cat := a.catalog.Get()

	qa := llm.FormatQA(session.QAPairs)
	result, err := Extract(client, ExtractRequest{
		Kind:        session.Kind,
		Requirement: session.Requirement(),
		QA:          qa,
		Draft:       session.Plan,
		Catalog:     cat,
	})
	if err != nil {
		return nil, err
	}

	notices := append([]string{}, result.Notices...)
	plan := result.Plan

	// 已答槽位确定性回填。
	//
	// 注意这是在第 6 步 (模型抽取) 之后: 两处都做是有意的 ——
	// 模型路径能处理规则解析不了的说法, 确定性路径保证能解析的说法一定落进计划。
	// 用户明确回答过的值优先于模型的理解。
	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		return nil, fmt.Errorf("计划序列化失败: %w", err)
	}
	if len(session.Answers) > 0 {
		patched, applied, problems := ApplyAnswers(
			session.Kind, planMap, session.Answers, cat,
			func(path string) (SlotMeta, bool) { return LookupSlotMeta(planMap, path, cat) },
		)
		if patched != nil {
			plan = patched
			if planMap, err = schema.PlanToMap(plan); err != nil {
				return nil, fmt.Errorf("计划序列化失败: %w", err)
			}
			if len(applied) > 0 {
				notices = append(notices, fmt.Sprintf("已按回答回填 %d 个字段", len(applied)))
			}
		}
		if len(problems) > 0 {
			notices = append(notices, "部分回答未能自动识别, 已交由模型理解: "+
				strings.Join(problems, "; "))
		}
	}

	report := ValidatePlan(plan, cat)
	gaps := AnalyzeGaps(session.Kind, plan, session.Answers, cat)

	// 汇总警告: 模型的观察 + 校验建议 + 业务规则 + 追问过程的提示
	warnings := mergeWarnings(planMap, report, gaps, session)
	if len(warnings) > 0 {
		_ = SetPath(planMap, "warnings", warnings)
		if rebuilt, err := schema.PlanFromMap(session.Kind, planMap); err == nil {
			plan = rebuilt
		}
	}

	residual := residualErrors(report)
	status := schema.StatusReady
	if len(gaps.Questions) > 0 || len(residual) > 0 {
		status = schema.StatusNeedsClarification
	}

	// 熔断: 追问轮次用尽则交付当前最佳结果, 绝不无限追问
	if status == schema.StatusNeedsClarification && session.Turns >= a.maxTurns {
		status = schema.StatusReady
		notices = append(notices, fmt.Sprintf(
			"已达最大追问轮次 (%d), 交付当前结果; 未补充的字段请人工确认", a.maxTurns))
		warnings = append(warnings, fmt.Sprintf(
			"追问达到 %d 轮上限, 以下信息仍未补充: %s",
			a.maxTurns, joinQuestionTopics(gaps.Questions)))
		_ = SetPath(planMap, "warnings", warnings)
		if rebuilt, err := schema.PlanFromMap(session.Kind, planMap); err == nil {
			plan = rebuilt
		}
	}

	// 记录本轮问过的问题, 供下一轮在问题文案里说明"上轮没听懂"
	refs := make([]QuestionRef, 0, len(gaps.Questions))
	for _, question := range gaps.Questions {
		refs = append(refs, QuestionRef{Slot: question.Slot, Text: question.Question})
	}
	session.Ask(refs)

	// 存草稿: 下一轮把它作为"当前计划"送回提示词, 让模型在已有基础上改而不是重猜
	if raw, err := schema.MarshalIndentNoEscape(plan); err == nil {
		session.Plan = string(raw)
	}

	turn := &schema.TurnResult{
		SessionID:  session.ID,
		Status:     status,
		Kind:       session.Kind,
		Plan:       plan,
		Questions:  gaps.Questions,
		Warnings:   warnings,
		Confidence: confidenceOf(planMap),
		Turns:      session.Turns,
		LLM:        client.Name(),
		Notices:    notices,
		Errors:     issueMessages(residual),
	}
	// 只有信息完整时才给总结 —— 那时计划才是可交付的, 用户才需要"过一眼再提交"
	if status == schema.StatusReady {
		turn.Summary = summarize(session.Kind, plan, cat)
	}

	// 计划一旦重新生成, 之前的定稿确认就失效了 —— 交付物已经变了, 不能让一条
	// 过期的"已提交"继续挂着。
	session.SubmittedAt = time.Time{}

	// 会话状态与对话记录: 界面据此重建历史, 也决定侧栏里显示"待补充/已完成"
	session.Status = status
	session.Questions = gaps.Questions
	session.Append(Record{
		Role:       RoleAgent,
		Status:     status,
		Kind:       session.Kind,
		Questions:  gaps.Questions,
		Warnings:   warnings,
		Notices:    notices,
		Errors:     turn.Errors,
		Confidence: turn.Confidence,
		Turns:      session.Turns,
		LLM:        client.Name(),
	})
	// 整体发布: 会话被删除/回收时返回 false, 这轮结果就没人要了 (不该把它变回存在)。
	// 发布同时刷新活跃时间并落盘。
	a.store.Publish(session)

	return turn, nil
}

// Continue 处理用户在对话里补充的自由文本。
//
// 与 Answer 的区别: Answer 收的是"槽位 -> 精确回答", 走确定性回填;
// Continue 收的是一段自由补充 (如"再改成保留30天"), 没有槽位归属,
// 只作为追加的需求文本交给抽取流程理解。对话式界面两者都要有 ——
// 只留 Answer 就退化成逐格填空, 只留 Continue 就丢掉了确定性回填的可靠。
func (a *Agent) Continue(sessionID string, text string) (*schema.TurnResult, error) {
	session, release, err := a.beginTurn(sessionID)
	if err != nil {
		return nil, err
	}
	defer release()

	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("补充内容不能为空")
	}
	if len([]rune(trimmed)) > a.promptLimit {
		return nil, fmt.Errorf("补充内容过长 (%d 字), 上限 %d 字",
			len([]rune(trimmed)), a.promptLimit)
	}

	next := session.Clone()
	next.FollowUps = append(next.FollowUps, trimmed)
	next.Append(Record{Role: RoleUser, Text: trimmed})
	return a.run(next, "")
}

// Submit 把会话的计划定稿。
//
// MVP 不执行备份, 所以"提交"的含义是: 用户确认这份 JSON 就是最终要用的那份。
// 之后任何一轮修改都会让它失效 (见 run)。
func (a *Agent) Submit(sessionID string) (*schema.TurnResult, error) {
	session, release, err := a.beginTurn(sessionID)
	if err != nil {
		return nil, err
	}
	defer release()

	if session.Status != schema.StatusReady {
		return nil, fmt.Errorf("信息尚未完整, 不能提交; 还有 %d 项待补充", len(session.Questions))
	}
	if strings.TrimSpace(session.Plan) == "" {
		return nil, fmt.Errorf("会话里没有可提交的计划")
	}

	plan, err := decodePlanJSON(session.Kind, session.Plan)
	if err != nil {
		return nil, fmt.Errorf("会话里的计划无法解析: %w", err)
	}

	next := session.Clone()
	next.SubmittedAt = time.Now()
	a.store.Publish(next)

	return &schema.TurnResult{
		SessionID:  next.ID,
		Status:     schema.StatusReady,
		Kind:       next.Kind,
		Plan:       plan,
		Questions:  []schema.Question{},
		Warnings:   warningsFromPlan(next.Plan),
		Confidence: confidenceFromPlanJSON(next.Plan),
		Turns:      next.Turns,
		Notices:    []string{"计划已定稿"},
		Summary:    summarize(next.Kind, plan, a.catalog.Get()),
	}, nil
}

// ListSessions 返回历史会话摘要 (按最近活动倒序)。
func (a *Agent) ListSessions() []map[string]any {
	sessions := a.store.List()
	out := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		out = append(out, session.Summary())
	}
	return out
}

// SessionDetail 返回会话详情, 供界面重建整段对话。
//
// 待补充的问题按当前状态重新推导, 而不是回放当时存下的那一份。
// 缺口分析是确定性的、不花模型调用, 所以这么做没有成本; 好处是槽位表新增推荐值或
// 新增追问项之后, 旧会话重新打开也能立刻跟上, 而不是永远停留在当时的形态。
func (a *Agent) SessionDetail(sessionID string) (map[string]any, bool) {
	session, ok := a.store.Get(sessionID)
	if !ok {
		return nil, false
	}
	detail := session.Detail()
	cat := a.catalog.Get()
	if fresh, ok := a.freshQuestions(session); ok {
		detail["questions"] = fresh
	}
	// 信息完整时把总结一并给出 (界面直接渲染成"待确认"的清单)
	if session.Status == schema.StatusReady {
		if plan, err := decodePlanJSON(session.Kind, session.Plan); err == nil {
			detail["summary"] = summarize(session.Kind, plan, cat)
		}
	}
	return detail, true
}

// decodePlanJSON 把存下的计划 JSON 还原成计划对象。
func decodePlanJSON(kind, planJSON string) (any, error) {
	if strings.TrimSpace(planJSON) == "" {
		return nil, fmt.Errorf("计划为空")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(planJSON), &raw); err != nil {
		return nil, err
	}
	return schema.PlanFromMap(kind, raw)
}

// freshQuestions 重新推导待补充的问题; 推不出来时返回 false 由调用方沿用原值。
func (a *Agent) freshQuestions(session *Session) ([]schema.Question, bool) {
	if session.Status == schema.StatusReady || strings.TrimSpace(session.Plan) == "" {
		return nil, false
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(session.Plan), &raw); err != nil {
		return nil, false
	}
	plan, err := schema.PlanFromMap(session.Kind, raw)
	if err != nil {
		return nil, false
	}
	gaps := AnalyzeGaps(session.Kind, plan, session.Answers, a.catalog.Get())
	return gaps.Questions, true
}

// DeleteSession 删除一条历史会话。
func (a *Agent) DeleteSession(sessionID string) bool {
	return a.store.Delete(sessionID)
}

// residualErrors 返回用户无法补充、只能由模型修正的残留错误。
func residualErrors(report schema.Report) []schema.Issue {
	out := []schema.Issue{}
	for _, issue := range report.Errors() {
		if issue.Slot == "" {
			out = append(out, issue)
		}
	}
	return out
}

func issueMessages(issues []schema.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Path != "" {
			out = append(out, issue.Path+": "+issue.Message)
			continue
		}
		out = append(out, issue.Message)
	}
	return out
}

// mergeWarnings 汇总各来源的警告并去重。
func mergeWarnings(planMap map[string]any, report schema.Report, gaps gapResult,
	session *Session) []string {

	out := []string{}
	seen := map[string]bool{}      // 完整文案
	seenTopic := map[string]bool{} // 主题
	add := func(items []string) {
		for _, item := range items {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			// 同一件事只留一条。
			//
			// 模型与引擎会各自发现同一个风险, 措辞却不完全相同 ——
			// 例如模型说"已配置增量备份, 但未单独指定增量保留策略, 暂按全量保留 14 天处理",
			// 引擎说"已配置增量备份, 但未单独指定增量保留策略"。整句比对去不掉,
			// 因此按第一个逗号前的"主题"再判一次。主题太短时不参与判重, 免得误合并。
			topic := warningTopic(trimmed)
			if len([]rune(topic)) >= 6 && seenTopic[topic] {
				continue
			}
			seen[trimmed] = true
			if len([]rune(topic)) >= 6 {
				seenTopic[topic] = true
			}
			out = append(out, trimmed)
		}
	}

	// 模型自己写进计划的观察
	if raw, ok := GetPath(planMap, "warnings"); ok {
		if list, ok := raw.([]any); ok {
			modelWarnings := make([]string, 0, len(list))
			for _, item := range list {
				if text, ok := item.(string); ok {
					modelWarnings = append(modelWarnings, text)
				}
			}
			add(modelWarnings)
		}
	}

	add(report.Messages(schema.LevelWarning))
	add(gaps.Warnings)
	add(businessWarnings(session.Kind, planMap))
	return out
}

func joinQuestionTopics(questions []schema.Question) string {
	if len(questions) == 0 {
		return "(无)"
	}
	topics := make([]string, 0, len(questions))
	for _, question := range questions {
		topics = append(topics, question.Question)
	}
	return strings.Join(topics, " / ")
}

// confidenceOf 从计划里读置信度。
func confidenceOf(planMap map[string]any) float64 {
	value, ok := GetPath(planMap, "confidence")
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case float64:
		return clamp01(typed)
	case int:
		return clamp01(float64(typed))
	case json.Number:
		// PlanToMap 用 UseNumber 保留整数精度, 因此这里的数字可能是 json.Number
		parsed, err := typed.Float64()
		if err != nil {
			return 0
		}
		return clamp01(parsed)
	default:
		return 0
	}
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// formatAnswers 把槽位回答渲染成对话记录里可读的一行行文本。
//
// 对话界面要显示"用户说了什么", 而 Answers 是 slot -> 值的映射; 直接展示键名
// 对用户没意义, 因此取问题原文作为标签。键名兜底, 保证任何情况下都有内容可显示。
func formatAnswers(answers map[string]string) string {
	slots := make([]string, 0, len(answers))
	for slot := range answers {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	lines := make([]string, 0, len(slots))
	for _, slot := range slots {
		lines = append(lines, slot+" = "+answers[slot])
	}
	return strings.Join(lines, "\n")
}

// warningTopic 取警告的"主题": 第一个逗号/分号/冒号之前的部分。
func warningTopic(text string) string {
	cut := len(text)
	for _, separator := range []string{"，", ",", "；", ";", "：", ":"} {
		if index := strings.Index(text, separator); index != -1 && index < cut {
			cut = index
		}
	}
	return strings.TrimSpace(text[:cut])
}
