package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backup-restore-agent/internal/config"
)

// newTestClient 起一个假的兼容端点, 返回 (客户端, 收到的请求次数记录)。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*OpenAICompat, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	profile := &config.Profile{
		Name:        "test",
		Provider:    config.ProviderOpenAICompat,
		BaseURL:     server.URL,
		Model:       "test-model",
		APIKey:      "test-key",
		KeySource:   "inline",
		RequiresKey: true,
	}
	return NewOpenAICompat(profile, 5*time.Second), &calls
}

// writeCompletion 输出一个标准的 chat/completions 响应。
func writeCompletion(w http.ResponseWriter, content string) {
	payload := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": content},
				"finish_reason": "stop"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func requestHasJSONMode(r *http.Request) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false
	}
	_, present := decoded["response_format"]
	return present
}

func TestCapabilitiesResetBetweenTests(t *testing.T) {
	ResetCapabilities()
}

func TestChatSendsJSONModeAndAuthHeader(t *testing.T) {
	ResetCapabilities()
	var sawAuth, sawJSONMode bool

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-key" {
			sawAuth = true
		}
		sawJSONMode = requestHasJSONMode(r)
		writeCompletion(w, `{"task_name":"x"}`)
	})

	content, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if content != `{"task_name":"x"}` {
		t.Fatalf("返回内容不符: %q", content)
	}
	if !sawAuth {
		t.Error("未带上 Bearer 认证头")
	}
	if !sawJSONMode {
		t.Error("首次请求应尝试 response_format")
	}
}

// 降级链的核心: 端点不支持 response_format 时, 应去掉该参数重试一次,
// 并且把"不支持"记进能力缓存, 后续请求不再白试。
func TestJSONModeDegradationChain(t *testing.T) {
	ResetCapabilities()
	var jsonModeAttempts, plainAttempts int32

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if requestHasJSONMode(r) {
			atomic.AddInt32(&jsonModeAttempts, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(
				`{"error":{"message":"response_format is not supported by this model"}}`))
			return
		}
		atomic.AddInt32(&plainAttempts, 1)
		writeCompletion(w, `{"task_name":"ok"}`)
	})

	// 第一次调用: 先试 json_object (400) -> 降级重试成功
	content, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("首次调用应降级成功: %v", err)
	}
	if content != `{"task_name":"ok"}` {
		t.Fatalf("内容不符: %q", content)
	}
	if got := atomic.LoadInt32(&jsonModeAttempts); got != 1 {
		t.Fatalf("应尝试 1 次 json_object, 实际 %d", got)
	}
	if got := atomic.LoadInt32(&plainAttempts); got != 1 {
		t.Fatalf("应降级 1 次, 实际 %d", got)
	}
	if client.SupportsJSONMode() {
		t.Error("能力缓存应记录该端点不支持结构化输出")
	}

	// 第二次调用: 能力已缓存, 不该再试 json_object
	if _, err := client.Chat([]Message{{Role: RoleUser, Content: "again"}}); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if got := atomic.LoadInt32(&jsonModeAttempts); got != 1 {
		t.Errorf("能力缓存未生效, json_object 被尝试了 %d 次", got)
	}
	if got := atomic.LoadInt32(&plainAttempts); got != 2 {
		t.Errorf("降级调用次数应为 2, 实际 %d", got)
	}
}

// 400 但原因与 response_format 无关时, 不该盲目降级 —— 那会掩盖真实错误。
func TestBadRequestUnrelatedToJSONModeIsNotDegraded(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"messages must not be empty"}}`))
	})

	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("应返回错误")
	}
	classified := AsError(err)
	if classified.Kind != ErrFormat {
		t.Fatalf("应归类为 format, 实际 %s (%s)", classified.Kind, classified.Message)
	}
}

// 模型名被拒的 400 应归为 model, 提示用户核对模型名。
func TestModelRejectionClassifiedAsModel(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The model does not exist"}}`))
	})

	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if AsError(err).Kind != ErrModel {
		t.Fatalf("应归类为 model, 实际 %v", AsError(err).Kind)
	}
}

func TestErrorClassificationByStatus(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{http.StatusUnauthorized, ErrAuth},
		{http.StatusForbidden, ErrAuth},
		{http.StatusNotFound, ErrModel},
		{http.StatusTooManyRequests, ErrRateLimit},
		{http.StatusInternalServerError, ErrServer},
		{http.StatusBadGateway, ErrServer},
		{http.StatusTeapot, ErrUnknown},
	}
	for _, entry := range cases {
		ResetCapabilities()
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(entry.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
		})
		_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
		if err == nil {
			t.Errorf("状态 %d 应返回错误", entry.status)
			continue
		}
		if got := AsError(err).Kind; got != entry.want {
			t.Errorf("状态 %d 应归类为 %s, 实际 %s", entry.status, entry.want, got)
		}
	}
}

func TestErrorCarriesHint(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})

	classified := AsError(err)
	payload := classified.ToMap()
	hint, _ := payload["hint"].(string)
	if hint == "" {
		t.Error("错误应带处置建议")
	}
	if !strings.Contains(hint, "Key") {
		t.Errorf("认证错误的建议应提到 Key: %s", hint)
	}
}

// 空内容要报成可读错误, 而不是让下游把它当成"JSON 不合法"。
func TestEmptyContentIsReported(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeCompletion(w, "   ")
	})

	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("空内容应返回错误")
	}
	classified := AsError(err)
	if classified.Kind != ErrFormat {
		t.Fatalf("应归类为 format, 实际 %s", classified.Kind)
	}
	if !strings.Contains(classified.Message, "空内容") {
		t.Errorf("报错应说明是空内容: %s", classified.Message)
	}
}

func TestNoChoicesIsReported(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if AsError(err).Kind != ErrFormat {
		t.Fatalf("应归类为 format, 实际 %v", AsError(err))
	}
}

func TestConnectionErrorClassification(t *testing.T) {
	ResetCapabilities()
	profile := &config.Profile{
		Name: "dead", Provider: config.ProviderOpenAICompat,
		BaseURL: "http://127.0.0.1:1", Model: "m", APIKey: "k", RequiresKey: true,
	}
	client := NewOpenAICompat(profile, 2*time.Second)
	_, err := client.Chat([]Message{{Role: RoleUser, Content: "hi"}})
	if err == nil {
		t.Fatal("连不上应返回错误")
	}
	if got := AsError(err).Kind; got != ErrConnection {
		t.Fatalf("应归类为 connection, 实际 %s", got)
	}
}

func TestPingReportsCapability(t *testing.T) {
	ResetCapabilities()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeCompletion(w, "就绪")
	})
	result, err := client.Ping()
	if err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
	if result["ok"] != true {
		t.Error("Ping 应返回 ok")
	}
	if result["reply"] != "就绪" {
		t.Errorf("回复不符: %v", result["reply"])
	}
}

// Azure 的 base_url 常被填成完整部署 URL, 需要能规整。
func TestNormalizeAzureEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://res.openai.azure.com":                         "https://res.openai.azure.com",
		"https://res.openai.azure.com/openai/deployments/gpt4": "https://res.openai.azure.com",
		"https://res.openai.azure.com/":                        "https://res.openai.azure.com",
	}
	for input, want := range cases {
		if got := normalizeAzureEndpoint(input); got != want {
			t.Errorf("normalizeAzureEndpoint(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestAzureEndpointIncludesDeploymentAndVersion(t *testing.T) {
	profile := &config.Profile{
		Name: "az", Provider: config.ProviderAzureOpenAI,
		BaseURL: "https://res.openai.azure.com", Model: "my-deployment",
		APIKey: "k", RequiresKey: true, APIVersion: "2024-06-01",
	}
	client := NewOpenAICompat(profile, time.Second)
	endpoint, err := client.endpoint()
	if err != nil {
		t.Fatalf("拼装端点失败: %v", err)
	}
	for _, fragment := range []string{
		"/openai/deployments/my-deployment/chat/completions", "api-version=2024-06-01",
	} {
		if !strings.Contains(endpoint, fragment) {
			t.Errorf("端点缺少 %q: %s", fragment, endpoint)
		}
	}
}

func TestEndpointAppendsChatCompletions(t *testing.T) {
	profile := &config.Profile{
		Name: "p", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com/v1/", Model: "m", RequiresKey: true,
	}
	client := NewOpenAICompat(profile, time.Second)
	endpoint, err := client.endpoint()
	if err != nil {
		t.Fatalf("拼装端点失败: %v", err)
	}
	if endpoint != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("端点不符: %s", endpoint)
	}
}

func TestEndpointWithoutBaseURL(t *testing.T) {
	profile := &config.Profile{Name: "bad", Model: "m", RequiresKey: false}
	client := NewOpenAICompat(profile, time.Second)
	if _, err := client.endpoint(); err == nil {
		t.Fatal("缺少 base_url 应报错")
	}
}

// demo 客户端必须能完整地从消息里取出多行需求 —— 早期实现只取首行, 会静默截断。
func TestExtractUserInputKeepsAllLines(t *testing.T) {
	content := MarkRequirement + "\n第一行需求\n第二行需求\n第三行需求"
	requirement, qa := ExtractUserInput([]Message{{Role: RoleUser, Content: content}})

	if !strings.Contains(requirement, "第一行需求") ||
		!strings.Contains(requirement, "第二行需求") ||
		!strings.Contains(requirement, "第三行需求") {
		t.Fatalf("多行需求被截断了: %q", requirement)
	}
	if qa != "" {
		t.Errorf("无问答块时 qa 应为空: %q", qa)
	}
}

func TestExtractUserInputSeparatesQA(t *testing.T) {
	content := MarkRequirement + "\n备份MySQL\n\n" + MarkQA + "\n- 保留多久?: 30天"
	requirement, qa := ExtractUserInput([]Message{{Role: RoleUser, Content: content}})

	if requirement != "备份MySQL" {
		t.Errorf("需求原文不符: %q", requirement)
	}
	if !strings.Contains(qa, "30天") {
		t.Errorf("问答块不符: %q", qa)
	}
	if strings.Contains(qa, MarkFeedback) {
		t.Error("问答块不应包含后续标记")
	}
}

func TestFormatQA(t *testing.T) {
	out := FormatQA([]QAPair{
		{Slot: "a", Question: "问题一", Answer: "答案一"},
		{Slot: "b", Question: "问题二", Answer: "答案二"},
	})
	if !strings.Contains(out, "问题一: 答案一") || !strings.Contains(out, "问题二: 答案二") {
		t.Fatalf("问答渲染不符: %q", out)
	}
	if strings.HasSuffix(out, "\n") {
		t.Error("不该有多余尾换行")
	}
	if FormatQA(nil) != "" {
		t.Error("空问答应渲染成空串")
	}
}
