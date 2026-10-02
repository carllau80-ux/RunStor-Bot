// Package llm 负责模型接入: 客户端契约、OpenAI 兼容实现、demo 兜底、profile 管理。
//
// 刻意不引第三方 SDK: 目标机与开发机都不通外网, 而 /v1/chat/completions 本身
// 就是一个普通的 JSON POST。手写反而能精确控制两件关键的事 ——
// response_format 的降级链, 以及 HTTP 状态到错误类型的分类。
package llm

import (
	"fmt"
	"strings"
	"time"
)

// Message 是一条对话消息。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// 常用 role。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ErrorKind 是错误分类。分类的意义在于处置方式完全不同:
// 换 Key、换模型、查网络, 还是直接切 demo 演示。
type ErrorKind string

const (
	ErrAuth       ErrorKind = "auth"
	ErrConnection ErrorKind = "connection"
	ErrModel      ErrorKind = "model"
	ErrFormat     ErrorKind = "format"
	ErrRateLimit  ErrorKind = "rate_limit"
	ErrServer     ErrorKind = "server"
	ErrUnknown    ErrorKind = "unknown"
)

// ErrorHints 是每一类错误对应的处置建议 (直接展示给用户)。
var ErrorHints = map[ErrorKind]string{
	ErrAuth:       "API Key 无效或已过期, 请检查 api_key_env 指向的环境变量或密钥文件",
	ErrConnection: "连不上端点, 请检查 base_url、网络与端口; 本地模型请确认服务已启动",
	ErrModel:      "模型不存在或无权限, 请核对 model 名称 (Azure 填部署名, 方舟填接入点 ID)",
	ErrFormat:     "模型连通但未返回可用内容, 建议换中文理解更强的模型, 或先切 demo 模式演示",
	ErrRateLimit:  "触发限流, 请降低调用频率或稍后重试",
	ErrServer:     "模型服务端错误, 稍后重试; 若持续出现请确认 base_url 指向了正确的服务",
	ErrUnknown:    "未分类错误, 详见 detail",
}

// Error 是带分类的调用错误。
type Error struct {
	Kind    ErrorKind
	Message string
	Detail  string
	// HintOverride 非空时替代按错误类型给出的通用建议。
	//
	// 用途: 通用建议是"检查 api_key_env 指向的环境变量", 但密钥若是用户在网页上
	// 填的, 这个指引就是错的 —— 该让他回界面重填。
	HintOverride string
}

func (e *Error) Error() string { return e.Message }

// Hint 返回处置建议。
func (e *Error) Hint() string {
	if e.HintOverride != "" {
		return e.HintOverride
	}
	if hint, ok := ErrorHints[e.Kind]; ok {
		return hint
	}
	return ErrorHints[ErrUnknown]
}

// ToMap 输出给接口用的结构 (不含密钥)。
//
// 返回 map[string]any 而非 map[string]string: 同一份响应里既有字符串字段也可能有
// 结构化字段, 统一成 any 才不会让调用方在类型断言上踩坑。
func (e *Error) ToMap() map[string]any {
	return map[string]any{
		"kind":    string(e.Kind),
		"message": e.Message,
		"hint":    e.Hint(),
		"detail":  truncate(e.Detail, 600),
	}
}

// NewError 构造分类错误。
func NewError(kind ErrorKind, message, detail string) *Error {
	return &Error{Kind: kind, Message: message, Detail: detail}
}

// Client 是所有客户端实现的契约。
type Client interface {
	// Name 用于 /health、日志与回合结果, 说明这一轮实际用了哪个引擎。
	Name() string
	// Chat 发送对话并返回模型的文本输出。
	Chat(messages []Message) (string, error)
}

// truncate 按字符截断 —— 按字节会切坏中文。
func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// describeStatus 把 HTTP 状态码翻译成中文说明。
func describeStatus(status int) string {
	switch status {
	case 400:
		return "请求被拒绝 (400)"
	case 401:
		return "认证失败 (401)"
	case 403:
		return "无权限 (403)"
	case 404:
		return "接口或模型不存在 (404)"
	case 429:
		return "触发限流 (429)"
	default:
		if status >= 500 {
			return fmt.Sprintf("模型服务端错误 (%d)", status)
		}
		return fmt.Sprintf("请求失败 (%d)", status)
	}
}

// looksLikeJSONModeProblem 判断 400 响应是否由 response_format 引起。
//
// 各家兼容实现对此的报错文案不统一, 因此宁可放宽: 只要是 400 且提到
// response_format / json, 就先按"不支持结构化输出"处理, 去掉参数重试一次。
// 若实际另有原因, 重试仍会失败, 那时再把真实错误暴露出来。
func looksLikeJSONModeProblem(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "response_format") ||
		strings.Contains(lowered, "json_object") ||
		strings.Contains(lowered, "json mode") ||
		strings.Contains(lowered, "structured output")
}

// looksLikeModelProblem 判断 400 响应是否由模型名引起。
func looksLikeModelProblem(body string) bool {
	lowered := strings.ToLower(body)
	return strings.Contains(lowered, "model") &&
		(strings.Contains(lowered, "not found") ||
			strings.Contains(lowered, "does not exist") ||
			strings.Contains(lowered, "invalid") ||
			strings.Contains(lowered, "unsupported"))
}

// nowMillis 返回当前毫秒时间戳, 用于耗时统计。
func nowMillis() int64 { return time.Now().UnixMilli() }
