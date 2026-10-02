package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/config"
)

// 降级链的能力缓存: 按 (base_url, model) 记住哪些端点不支持 json_object,
// 避免每轮都白试一次 400。
var jsonModeUnsupported sync.Map // map[string]bool

func capabilityKey(baseURL, model string) string {
	return baseURL + "|" + model
}

// MarkJSONModeUnsupported 标记某端点不支持结构化输出 (测试与外部探测可用)。
func MarkJSONModeUnsupported(baseURL, model string) {
	jsonModeUnsupported.Store(capabilityKey(baseURL, model), true)
}

// ResetCapabilities 清空能力缓存 (测试用)。
func ResetCapabilities() {
	jsonModeUnsupported.Range(func(key, _ any) bool {
		jsonModeUnsupported.Delete(key)
		return true
	})
}

// OpenAICompat 是任何 /chat/completions 兼容端点的客户端:
// DeepSeek / 千问 / Kimi / GLM / 豆包 / OpenAI / OpenRouter /
// Ollama / vLLM / LM Studio。
type OpenAICompat struct {
	profile    *config.Profile
	httpClient *http.Client
	label      string
}

// NewOpenAICompat 按 profile 构造客户端。
func NewOpenAICompat(profile *config.Profile, timeout time.Duration) *OpenAICompat {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &OpenAICompat{
		profile: profile,
		label:   "llm:" + profile.Name,
		httpClient: &http.Client{
			// 默认 Transport 会读 HTTP_PROXY/HTTPS_PROXY, 内网部署常需要走代理
			Timeout: timeout,
		},
	}
}

// Name 实现 Client。
func (c *OpenAICompat) Name() string { return c.label }

// SupportsJSONMode 报告该端点当前是否还会尝试 response_format。
func (c *OpenAICompat) SupportsJSONMode() bool {
	_, unsupported := jsonModeUnsupported.Load(c.capabilityKey())
	return !unsupported
}

func (c *OpenAICompat) capabilityKey() string {
	return capabilityKey(c.profile.BaseURL, c.profile.Model)
}

func (c *OpenAICompat) markJSONModeUnsupported() {
	jsonModeUnsupported.Store(c.capabilityKey(), true)
}

// Chat 实现 Client, 内含 response_format 降级链。
func (c *OpenAICompat) Chat(messages []Message) (string, error) {
	useJSONMode := c.SupportsJSONMode()

	content, err := c.request(messages, useJSONMode)
	if err == nil {
		return content, nil
	}

	// 降级: 端点不支持结构化输出 -> 记住, 去掉该参数重试一次,
	// 之后靠提示词约束 + 上层的 JSON 抢救来兜底。
	var unsupported *jsonModeUnsupportedError
	if useJSONMode && errors.As(err, &unsupported) {
		c.markJSONModeUnsupported()
		return c.request(messages, false)
	}
	return "", err
}

// Ping 发一个最小请求, 只验证 key / 端点 / 模型名三件事。
func (c *OpenAICompat) Ping() (map[string]any, error) {
	content, err := c.request([]Message{
		{Role: RoleUser, Content: "请只回复两个字: 就绪"},
	}, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":        true,
		"model":     c.profile.Model,
		"base_url":  c.profile.BaseURL,
		"reply":     truncate(strings.TrimSpace(content), 80),
		"json_mode": c.SupportsJSONMode(),
	}, nil
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

// jsonModeUnsupportedError 是内部信号: 端点不支持 response_format, 应降级重试。
type jsonModeUnsupportedError struct{ detail string }

func (e *jsonModeUnsupportedError) Error() string { return e.detail }

func (c *OpenAICompat) request(messages []Message, jsonMode bool) (string, error) {
	endpoint, err := c.endpoint()
	if err != nil {
		return "", err
	}

	payload := map[string]any{
		"model":       c.profile.Model,
		"messages":    messages,
		"temperature": 0,
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", NewError(ErrUnknown, "构造请求体失败", err.Error())
	}

	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", NewError(ErrConnection, "构造请求失败: "+endpoint, err.Error())
	}
	request.Header.Set("Content-Type", "application/json")
	if key := c.profile.APIKey; key != "" {
		if c.profile.Provider == config.ProviderAzureOpenAI {
			request.Header.Set("api-key", key)
		} else {
			request.Header.Set("Authorization", "Bearer "+key)
		}
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", c.classifyTransportError(err)
	}
	defer response.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if readErr != nil {
		return "", NewError(ErrConnection, "读取响应失败", readErr.Error())
	}

	if response.StatusCode != http.StatusOK {
		return "", c.classifyHTTPError(response.StatusCode, string(raw), jsonMode)
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", NewError(ErrFormat,
			"响应不是预期的 JSON 结构", truncate(string(raw), 400))
	}

	if decoded.Error != nil && decoded.Error.Message != "" {
		return "", NewError(ErrServer, "模型返回错误: "+decoded.Error.Message,
			truncate(string(raw), 400))
	}
	if len(decoded.Choices) == 0 {
		return "", NewError(ErrFormat,
			fmt.Sprintf("模型 %s 返回了空的 choices", c.profile.Model),
			truncate(string(raw), 400))
	}

	content := decoded.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", NewError(ErrFormat,
			fmt.Sprintf("模型 %s 返回空内容 (finish_reason=%s)",
				c.profile.Model, decoded.Choices[0].FinishReason),
			"空内容常见于模型不支持结构化输出或输出被长度上限截断; "+
				"可尝试换用更大的模型")
	}
	return content, nil
}

// endpoint 拼出 chat/completions 的完整地址。
func (c *OpenAICompat) endpoint() (string, error) {
	if c.profile.BaseURL == "" {
		return "", NewError(ErrConnection,
			fmt.Sprintf("profile %s 缺少 base_url", c.profile.Name), "")
	}

	if c.profile.Provider == config.ProviderAzureOpenAI {
		base := normalizeAzureEndpoint(c.profile.BaseURL)
		if base == "" {
			return "", NewError(ErrConnection,
				"Azure 端点为空: base_url 需形如 https://<资源名>.openai.azure.com", "")
		}
		version := c.profile.APIVersion
		if version == "" {
			version = "2024-06-01"
		}
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			base, url.PathEscape(c.profile.Model), url.QueryEscape(version)), nil
	}

	return strings.TrimRight(c.profile.BaseURL, "/") + "/chat/completions", nil
}

// normalizeAzureEndpoint 把用户可能填的完整部署 URL 规整为资源端点。
func normalizeAzureEndpoint(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if index := strings.Index(trimmed, "/openai/deployments/"); index != -1 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func (c *OpenAICompat) classifyTransportError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return NewError(ErrConnection,
			fmt.Sprintf("请求超时: %s", c.profile.BaseURL), err.Error())
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return NewError(ErrConnection,
			fmt.Sprintf("连不上端点: %s", c.profile.BaseURL), urlErr.Error())
	}
	return NewError(ErrConnection,
		fmt.Sprintf("请求失败: %s", c.profile.BaseURL), err.Error())
}

func (c *OpenAICompat) classifyHTTPError(status int, body string, jsonMode bool) error {
	detail := truncate(strings.TrimSpace(body), 600)

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		failure := NewError(ErrAuth,
			fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Name), detail)
		// 密钥来源不同, 该去哪儿修也不同: 网页填的密钥不该被指引去查环境变量
		if c.profile.Source == config.SourceRuntime {
			failure.HintOverride = "该密钥是在网页的「模型设置」里填写的, 请回到那里重新填写或更换厂商"
		}
		return failure
	case http.StatusNotFound:
		return NewError(ErrModel,
			fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Model), detail)
	case http.StatusTooManyRequests:
		return NewError(ErrRateLimit,
			fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Name), detail)
	case http.StatusBadRequest:
		// 结构化输出不被支持 —— 这是最常见的 400 原因, 走降级重试
		if jsonMode && looksLikeJSONModeProblem(body) {
			return &jsonModeUnsupportedError{detail: detail}
		}
		if looksLikeModelProblem(body) {
			return NewError(ErrModel,
				fmt.Sprintf("模型名被拒绝 (400): %s", c.profile.Model), detail)
		}
		return NewError(ErrFormat,
			fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Name), detail)
	}

	if status >= 500 {
		return NewError(ErrServer,
			fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Name), detail)
	}
	return NewError(ErrUnknown,
		fmt.Sprintf("%s: %s", describeStatus(status), c.profile.Name), detail)
}

// AsError 把任意 error 归一成 *Error, 便于接口层统一输出。
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified
	}
	return NewError(ErrUnknown, err.Error(), "")
}
