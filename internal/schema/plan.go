package schema

import (
	"bytes"
	"encoding/json"
)

// BackupPlan 是一份完整的备份计划。
//
// 刻意不包含 questions 字段: 追问由确定性缺口分析产出, 只存在于 TurnResult。
// 计划被冻结 (status=ready) 时必然没有任何疑问, 放在计划里只会是噪音。
type BackupPlan struct {
	SchemaVersion string        `json:"schema_version"`
	Kind          string        `json:"kind"`
	TaskName      string        `json:"task_name"`
	Source        BackupSource  `json:"source"`
	Schedule      Schedule      `json:"schedule"`
	Retention     Retention     `json:"retention"`
	Destinations  []Destination `json:"destinations"`
	SLA           SLA           `json:"sla"`
	Verification  Verification  `json:"verification"`
	Notification  Notification  `json:"notification"`
	Confidence    float64       `json:"confidence"`
	Warnings      []string      `json:"warnings"`
}

// NewBackupPlan 返回带默认值的空计划。
func NewBackupPlan() *BackupPlan {
	return &BackupPlan{
		SchemaVersion: SchemaVersion,
		Kind:          KindBackup,
		Source:        BackupSource{Type: "mixed", Items: []SourceItem{}},
		Retention:     Retention{MinCopies: 1},
		Destinations:  []Destination{},
		Notification:  Notification{Channels: []string{}, Recipients: []string{}},
		Warnings:      []string{},
	}
}

// RestorePoint 描述恢复到哪个时间点。
type RestorePoint struct {
	Type     string `json:"type"`
	Value    string `json:"value,omitempty"`
	Timezone string `json:"timezone"`
}

// RestoreTarget 描述恢复到哪台机/哪个实例。
type RestoreTarget struct {
	Mode          string `json:"mode"`
	Host          string `json:"host,omitempty"`
	Instance      string `json:"instance,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

// RestoreScope 描述恢复范围。
type RestoreScope struct {
	Level string   `json:"level"`
	Items []string `json:"items"`
}

// AlternateLocation 描述换路径/换实例名/仅校验。
type AlternateLocation struct {
	Enabled      bool   `json:"enabled"`
	DataDir      string `json:"data_dir,omitempty"`
	InstanceName string `json:"instance_name,omitempty"`
	ValidateOnly bool   `json:"validate_only"`
}

// RestoreVerification 描述恢复后的校验动作。
type RestoreVerification struct {
	ValidateBackup   bool `json:"validate_backup"`
	OpenDatabase     bool `json:"open_database"`
	CheckConsistency bool `json:"check_consistency"`
}

// RestorePlan 是一份完整的恢复计划。
type RestorePlan struct {
	SchemaVersion   string              `json:"schema_version"`
	Kind            string              `json:"kind"`
	TaskName        string              `json:"task_name"`
	Source          BackupSource        `json:"source"`
	SourceBackupRef string              `json:"source_backup_ref,omitempty"`
	Target          RestoreTarget       `json:"target"`
	Point           RestorePoint        `json:"point"`
	Scope           RestoreScope        `json:"scope"`
	Alternate       AlternateLocation   `json:"alternate"`
	Verification    RestoreVerification `json:"verification"`
	SLA             SLA                 `json:"sla"`
	Notification    Notification        `json:"notification"`
	Confidence      float64             `json:"confidence"`
	Warnings        []string            `json:"warnings"`
}

// NewRestorePlan 返回带默认值的空恢复计划。
func NewRestorePlan() *RestorePlan {
	return &RestorePlan{
		SchemaVersion: SchemaVersion,
		Kind:          KindRestore,
		Source:        BackupSource{Type: "mixed", Items: []SourceItem{}},
		Target:        RestoreTarget{Mode: "in_place"},
		Point:         RestorePoint{Type: "latest", Timezone: "Asia/Shanghai"},
		Scope:         RestoreScope{Level: "whole", Items: []string{}},
		Verification:  RestoreVerification{OpenDatabase: true},
		Notification:  Notification{Channels: []string{}, Recipients: []string{}},
		Warnings:      []string{},
	}
}

// NewPlanForKind 按意图返回对应的空计划 (typed)。
func NewPlanForKind(kind string) any {
	if kind == KindRestore {
		return NewRestorePlan()
	}
	return NewBackupPlan()
}

// Question 是一条待回答的追问。
//
// Slot 是这套设计的关键: 前端按 slot 路径回传答案, 而不是按问题原文。
// 问题文案可以随轮次调整、可以改写, 答案的归属始终稳定。
type Question struct {
	Slot     string   `json:"slot"`
	Question string   `json:"question"`
	Kind     string   `json:"kind"`
	Required bool     `json:"required"`
	Example  string   `json:"example,omitempty"`
	Choices  []string `json:"choices"`
	// Suggestions 是推荐值: 点它即可提交, 也可以自己填别的。
	// 与 Choices 的区别是它不属于封闭集合 —— 时刻、保留期这类参数没有固定候选,
	// 但绝大多数人只会从几个常见值里选。
	Suggestions []string `json:"suggestions"`
	Scope       string   `json:"scope,omitempty"`
	Hint        string   `json:"hint,omitempty"`
}

// SummaryRow 是总结里的一行。
//
// Slot 非空表示这一项可以"修改": 界面拿它当槽位路径提交新答案, 复用追问那套机制。
type SummaryRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Slot  string `json:"slot,omitempty"`
	// Choices / Suggestions 让界面上的"修改这一项"直接给出可选项,
	// 不必再回一次接口去问这个槽位有哪些取值。
	Choices     []string `json:"choices,omitempty"`
	Suggestions []string `json:"suggestions,omitempty"`
}

// Summary 是给人看的计划总结 (交付前让用户过一眼, 而不是直接扔一段 JSON)。
type Summary struct {
	Title      string       `json:"title"`
	Rows       []SummaryRow `json:"rows"`
	Warnings   []string     `json:"warnings"`
	Confidence float64      `json:"confidence"`
}

// TurnResult 是一次对话回合的完整结果。
type TurnResult struct {
	SessionID  string     `json:"session_id"`
	Status     string     `json:"status"`
	Kind       string     `json:"kind"`
	Plan       any        `json:"plan"`
	Questions  []Question `json:"questions"`
	Warnings   []string   `json:"warnings"`
	Confidence float64    `json:"confidence"`
	Turns      int        `json:"turns"`
	LLM        string     `json:"llm"`
	Notices    []string   `json:"notices"`
	Errors     []string   `json:"errors,omitempty"`
	// Summary 只在信息完整 (ready) 时给出: 那时计划才是可交付的。
	Summary *Summary `json:"summary,omitempty"`
}

// MarshalPlan 把计划序列化为 JSON 字节 (给会话存档与接口输出复用)。
func MarshalPlan(plan any) ([]byte, error) {
	return json.Marshal(plan)
}

// PlanToMap 把计划转成 map, 供槽位路径读写使用。
//
// 用 JSON 往返而不是反射, 是因为槽位机制本来就以 JSON 路径为接口
// (如 "source.items[0].options.auth_mode"), map 是这套机制最自然的载体。
func PlanToMap(plan any) (map[string]any, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// PlanFromMap 把 map 还原成对应类型的计划, 并拒绝未知字段。
func PlanFromMap(kind string, data map[string]any) (any, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	target := NewPlanForKind(kind)
	if err := UnmarshalStrict(raw, target); err != nil {
		return nil, err
	}
	return target, nil
}
