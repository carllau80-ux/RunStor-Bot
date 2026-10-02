package llm

import "strings"

// 提示词分块标记。
//
// 既是给人看的小标题, 也是客户端定位信息的锚点: demo 客户端要能确定地从消息里
// 取出"需求原文 + 已答问答", 而不必理解整段提示词。
//
// 注意: 需求原文必须完整保留全部行。早期实现只取第一行, 多行输入会被静默截断,
// 表现为"用户写了两行的需求, 第二行被完全忽略", 极难排查。
const (
	MarkKind        = "【期望的计划类型】"
	MarkRequirement = "【备份/恢复需求描述】"
	MarkQA          = "【用户对追问的回答】"
	MarkDraft       = "【当前计划草稿】"
	MarkFeedback    = "【上一轮输出不合格, 请修正后重新输出完整 JSON】"
)

var allMarks = []string{MarkKind, MarkRequirement, MarkQA, MarkDraft, MarkFeedback}

// ExtractKind 读出本轮期望的计划类型 (backup / restore)。
//
// 为什么需要它: 调用方可以用 kind 参数强制指定意图 (例如文本里看不出是恢复,
// 但接口明确要求恢复计划)。真实模型靠 system prompt 得知该填哪种 schema, 而
// demo 客户端必须有一个同样明确的信号 —— 否则它会自行猜测, 产出与目标 schema
// 不匹配的 JSON, 表现为一连串莫名其妙的解析失败。
func ExtractKind(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	index := strings.Index(messages[len(messages)-1].Content, MarkKind)
	if index == -1 {
		return ""
	}
	rest := messages[len(messages)-1].Content[index+len(MarkKind):]
	lowered := strings.ToLower(rest)
	backupAt := strings.Index(lowered, "backup")
	restoreAt := strings.Index(lowered, "restore")

	switch {
	case backupAt == -1 && restoreAt == -1:
		return ""
	case restoreAt == -1:
		return "backup"
	case backupAt == -1:
		return "restore"
	case restoreAt < backupAt:
		return "restore"
	default:
		return "backup"
	}
}

// ExtractUserInput 从最后一条消息里取出 (需求原文, 问答文本)。
//
// 找不到标记时退化为整段内容, 保证任何调用方式下都能拿到可用的文本。
func ExtractUserInput(messages []Message) (requirement, qa string) {
	if len(messages) == 0 {
		return "", ""
	}
	content := messages[len(messages)-1].Content

	requirement = section(content, MarkRequirement, "")
	if requirement == "" {
		requirement = strings.TrimSpace(content)
	}
	qa = section(content, MarkQA, MarkRequirement)
	return requirement, qa
}

// section 截取 mark 到下一个标记之间的内容。
func section(content, mark, skip string) string {
	index := strings.Index(content, mark)
	if index == -1 {
		return ""
	}
	rest := content[index+len(mark):]

	cut := len(rest)
	for _, other := range allMarks {
		if other == mark || other == skip {
			continue
		}
		if position := strings.Index(rest, other); position != -1 && position < cut {
			cut = position
		}
	}
	return strings.TrimSpace(rest[:cut])
}

// FormatQA 把"槽位 -> 回答"渲染成提示词里的问答块。
func FormatQA(pairs []QAPair) string {
	if len(pairs) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, pair := range pairs {
		builder.WriteString("- ")
		builder.WriteString(pair.Question)
		builder.WriteString(": ")
		builder.WriteString(pair.Answer)
		builder.WriteString("\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// QAPair 是一条"问题 -> 回答"。
type QAPair struct {
	Slot     string
	Question string
	Answer   string
}
