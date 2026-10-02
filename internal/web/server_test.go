package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"backup-restore-agent/internal/agent"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/llm"
)

const acceptanceText = "周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4"

func newTestHandler(t *testing.T, profiles ...*config.Profile) http.Handler {
	t.Helper()
	return newTestHandlerWithRuntime(t, true, profiles...)
}

// newTestHandlerWithRuntime 可控制是否开放"网页改配置"。
func newTestHandlerWithRuntime(t *testing.T, uiConfig bool, profiles ...*config.Profile) http.Handler {
	t.Helper()
	cfg := &config.Config{
		Server:         config.Server{MaxTurns: 10, PromptLimit: 20000, SessionTTLMin: 120},
		Profiles:       profiles,
		DefaultProfile: "demo",
	}
	if uiConfig {
		store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
		if err != nil {
			t.Fatalf("打开运行态存储失败: %v", err)
		}
		cfg.Runtime = store
	}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	logger := log.New(io.Discard, "", 0)
	return New(orchestrator, logger, "test").Handler()
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求失败: %v", err)
		}
		reader = strings.NewReader(string(raw))
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	payload := map[string]any{}
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("响应不是 JSON (%d): %s", recorder.Code, recorder.Body.String())
		}
	}
	return recorder.Code, payload
}

// ---------------------------------------------------------------------------
// 验收 A (经 HTTP 完整走一遍)
// ---------------------------------------------------------------------------

func TestAPIHealth(t *testing.T) {
	handler := newTestHandler(t)
	status, payload := doJSON(t, handler, "GET", "/health", nil)

	if status != http.StatusOK {
		t.Fatalf("状态码应为 200, 实际 %d", status)
	}
	if payload["status"] != "ok" {
		t.Errorf("status 应为 ok: %v", payload["status"])
	}
	if payload["mode"] != "demo" {
		t.Errorf("无 profile 时应为 demo 模式: %v", payload["mode"])
	}
	drivers, ok := payload["drivers"].([]any)
	if !ok || len(drivers) < 5 {
		t.Errorf("应列出已注册引擎: %v", payload["drivers"])
	}
}

func TestAPIFullClarificationFlow(t *testing.T) {
	handler := newTestHandler(t)

	// 第一轮: 解析
	status, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	if status != http.StatusOK {
		t.Fatalf("解析状态码应为 200, 实际 %d: %v", status, first)
	}
	if first["status"] != "needs_clarification" {
		t.Fatalf("信息不完整应为 needs_clarification: %v", first["status"])
	}
	sessionID, _ := first["session_id"].(string)
	if sessionID == "" {
		t.Fatal("应返回 session_id")
	}

	questions, _ := first["questions"].([]any)
	if len(questions) != 4 {
		t.Fatalf("应恰好追问 4 项, 实际 %d: %v", len(questions), questions)
	}
	for _, raw := range questions {
		question, _ := raw.(map[string]any)
		for _, field := range []string{"slot", "question", "kind"} {
			if question[field] == nil || question[field] == "" {
				t.Errorf("追问缺少字段 %s: %v", field, question)
			}
		}
	}

	// 第二轮: 回答
	status, second := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})
	if status != http.StatusOK {
		t.Fatalf("续答状态码应为 200, 实际 %d: %v", status, second)
	}
	if second["status"] != "ready" {
		t.Fatalf("补齐后应为 ready, 实际 %v, 剩余追问 %v",
			second["status"], second["questions"])
	}
	if second["kind"] != "backup" {
		t.Errorf("意图应为 backup: %v", second["kind"])
	}

	plan, _ := second["plan"].(map[string]any)
	if plan == nil {
		t.Fatal("ready 时应返回计划")
	}
	if plan["task_name"] == nil || plan["task_name"] == "" {
		t.Error("计划应有 task_name")
	}

	items := plan["source"].(map[string]any)["items"].([]any)
	options := items[0].(map[string]any)["options"].(map[string]any)
	if options["auth_mode"] != "os_auth" {
		t.Errorf("auth_mode 应为 os_auth: %v", options["auth_mode"])
	}
	if options["scope"] != "whole_instance" {
		t.Errorf("scope 应为 whole_instance: %v", options["scope"])
	}
	if options["channels"] != float64(4) {
		t.Errorf("channels 应为 4: %v", options["channels"])
	}

	schedule := plan["schedule"].(map[string]any)
	full := schedule["full"].(map[string]any)
	if full["frequency"] != "weekly" || full["weekday"] != float64(7) || full["time"] != "01:00" {
		t.Errorf("全量调度不符: %v", full)
	}
	if schedule["incremental"].(map[string]any)["frequency"] != "daily" {
		t.Errorf("增量调度不符: %v", schedule["incremental"])
	}

	retention := plan["retention"].(map[string]any)
	fullRetention := retention["full"].(map[string]any)
	if fullRetention["value"] != float64(14) || fullRetention["unit"] != "days" {
		t.Errorf("保留期不符: %v", fullRetention)
	}

	destinations := plan["destinations"].([]any)
	if len(destinations) != 1 {
		t.Fatalf("应有 1 个目的地: %v", destinations)
	}
	destination := destinations[0].(map[string]any)
	if destination["type"] != "local" || destination["path"] != "/u01/backup/orcl" {
		t.Errorf("目的地不符: %v", destination)
	}

	if second["confidence"] == float64(0) {
		t.Error("ready 时置信度不该为 0")
	}
}

func TestAPIRestoreFlow(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/parse", map[string]any{
		"text": "Oracle库出故障了，从生产Oracle实例周全备恢复到2026-09-25 10:30:00",
	})
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if payload["kind"] != "restore" {
		t.Fatalf("意图应为 restore: %v", payload["kind"])
	}

	plan, _ := payload["plan"].(map[string]any)
	point, _ := plan["point"].(map[string]any)
	if point["type"] != "timestamp" || point["value"] != "2026-09-25 10:30:00" {
		t.Errorf("恢复时间点不符: %v", point)
	}
	// 恢复计划不该带备份侧字段
	for _, forbidden := range []string{"schedule", "retention", "destinations"} {
		if _, exists := plan[forbidden]; exists {
			t.Errorf("恢复计划不该包含 %s", forbidden)
		}
	}
}

func TestAPIKindOverride(t *testing.T) {
	handler := newTestHandler(t)

	_, payload := doJSON(t, handler, "POST", "/api/parse", map[string]any{
		"text": "Oracle 生产库", "kind": "restore",
	})
	if payload["kind"] != "restore" {
		t.Fatalf("显式 kind 应生效: %v", payload["kind"])
	}
}

func TestAPISessionReset(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID := first["session_id"].(string)

	// 答一项, 让回合数增加
	_, second := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers":    map[string]string{"schedule.full.time": "凌晨1点"},
	})
	if second["turns"] != float64(2) {
		t.Fatalf("回合数应为 2: %v", second["turns"])
	}

	status, reset := doJSON(t, handler, "POST", "/api/sessions/"+sessionID+"/reset", nil)
	if status != http.StatusOK {
		t.Fatalf("重置状态码应为 200: %d", status)
	}
	if reset["turns"] != float64(1) {
		t.Errorf("重置后回合数应回到 1: %v", reset["turns"])
	}
	if reset["status"] != "needs_clarification" {
		t.Errorf("重置后应重新开始追问: %v", reset["status"])
	}
}

func TestAPIGetSession(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID := first["session_id"].(string)

	status, session := doJSON(t, handler, "GET", "/api/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if session["text"] != acceptanceText {
		t.Errorf("应回显需求原文: %v", session["text"])
	}
	if session["kind"] != "backup" {
		t.Errorf("kind 不符: %v", session["kind"])
	}
}

// ---------------------------------------------------------------------------
// 错误处理
// ---------------------------------------------------------------------------

func TestAPIParseValidationErrors(t *testing.T) {
	handler := newTestHandler(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"空文本", map[string]any{"text": ""}},
		{"只有空白", map[string]any{"text": "   "}},
		{"缺少 text", map[string]any{}},
		{"非法 kind", map[string]any{"text": "备份", "kind": "explode"}},
	}
	for _, entry := range cases {
		status, payload := doJSON(t, handler, "POST", "/api/parse", entry.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: 状态码应为 400, 实际 %d", entry.name, status)
			continue
		}
		if payload["error"] == nil {
			t.Errorf("%s: 应返回 error 字段", entry.name)
		}
	}
}

func TestAPIClarifyValidationErrors(t *testing.T) {
	handler := newTestHandler(t)

	status, _ := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": "s-does-not-exist",
		"answers":    map[string]string{"a": "b"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("未知会话应返回 400, 实际 %d", status)
	}

	status, _ = doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": "s-x", "answers": map[string]string{},
	})
	if status != http.StatusBadRequest {
		t.Errorf("空 answers 应返回 400, 实际 %d", status)
	}

	status, _ = doJSON(t, handler, "POST", "/api/clarify", map[string]any{"answers": map[string]string{"a": "b"}})
	if status != http.StatusBadRequest {
		t.Errorf("缺 session_id 应返回 400, 实际 %d", status)
	}
}

func TestAPIRejectsUnknownField(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "POST", "/api/parse", map[string]any{
		"text": "备份MySQL", "unknown_field": "x",
	})
	if status != http.StatusBadRequest {
		t.Errorf("未知字段应返回 400 (避免拼错被静默忽略), 实际 %d", status)
	}
}

func TestAPIUnknownSessionReturns404(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "GET", "/api/sessions/s-nope", nil)
	if status != http.StatusNotFound {
		t.Errorf("状态码应为 404, 实际 %d", status)
	}
}

// ServeMux 的 "GET /" 会兜住所有路径, 必须显式回 404 而不是返回首页。
func TestAPIUnknownPathReturns404(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "GET", "/api/no-such-endpoint", nil)
	if status != http.StatusNotFound {
		t.Errorf("未知路径应返回 404, 实际 %d", status)
	}
}

// ---------------------------------------------------------------------------
// 模型设置接口
// ---------------------------------------------------------------------------

func TestAPIProfilesNeverLeakKeys(t *testing.T) {
	secret := "sk-leak-canary"
	handler := newTestHandler(t, &config.Profile{
		Name: "leaky", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		APIKey: secret, KeySource: "env:BRA_X", RequiresKey: true,
	})

	status, payload := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("响应里泄露了 API Key")
	}

	profiles, _ := payload["profiles"].([]any)
	if len(profiles) != 2 { // 配置的 + demo
		t.Fatalf("应有 2 个 profile: %v", profiles)
	}
	var sawHasKey bool
	for _, item := range profiles {
		profile, _ := item.(map[string]any)
		if profile["name"] == "leaky" && profile["has_key"] == true {
			sawHasKey = true
		}
	}
	if !sawHasKey {
		t.Error("应报告已配置 Key (只报告有无, 不报告内容)")
	}
}

func TestAPIUnavailableProfileIsListedWithReason(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "no-key", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		RequiresKey: true, // 没有 key
	})

	_, payload := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	profiles, _ := payload["profiles"].([]any)
	for _, item := range profiles {
		profile, _ := item.(map[string]any)
		if profile["name"] != "no-key" {
			continue
		}
		if profile["available"] != false {
			t.Error("缺 Key 的 profile 应标记为不可用")
		}
		if reason, _ := profile["unavailable_reason"].(string); reason == "" {
			t.Error("应说明不可用原因")
		}
		return
	}
	t.Fatal("未找到 no-key profile")
}

func TestAPISetActiveRejectsUnknownProfile(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "PUT", "/api/llm/active", map[string]any{"profile": "ghost"})
	if status != http.StatusBadRequest {
		t.Fatalf("未知 profile 应返回 400, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if errorInfo == nil {
		t.Fatal("应返回 error")
	}
	if !strings.Contains(errorInfo["message"].(string), "ghost") {
		t.Errorf("报错应指明 profile 名: %v", errorInfo["message"])
	}
	if errorInfo["hint"] == "" {
		t.Error("应给出可用值提示")
	}
}

func TestAPISetActiveRejectsUnavailableProfile(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "no-key", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m", RequiresKey: true,
	})

	status, payload := doJSON(t, handler, "PUT", "/api/llm/active", map[string]any{"profile": "no-key"})
	if status != http.StatusBadRequest {
		t.Fatalf("不可用 profile 应返回 400, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if !strings.Contains(errorInfo["message"].(string), "不可用") {
		t.Errorf("报错应说明不可用: %v", errorInfo["message"])
	}
}

func TestAPISwitchToDemoAlwaysWorks(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "PUT", "/api/llm/active", map[string]any{"profile": "demo"})
	if status != http.StatusOK {
		t.Fatalf("切到 demo 应成功: %d %v", status, payload)
	}
	if payload["mode"] != "demo" {
		t.Errorf("mode 应为 demo: %v", payload["mode"])
	}
}

func TestAPITestLLMForDemo(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/test", map[string]any{"profile": "demo"})
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if payload["ok"] != true {
		t.Errorf("demo 客户端应始终可用: %v", payload)
	}
}

// 指向不存在的模型时要返回可读错误, 而不是 500。
func TestAPITestLLMWithUnknownProfile(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/test", map[string]any{"profile": "ghost"})
	if status != http.StatusOK {
		t.Fatalf("测试接口本身应 200, 错误放在 body 里: %d", status)
	}
	if payload["ok"] != false {
		t.Fatal("应报告失败")
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if errorInfo == nil || errorInfo["message"] == "" {
		t.Fatalf("应给出可读错误: %v", payload)
	}
}

// 不可达端点要归类为 connection, 而不是笼统的 500。
func TestAPITestLLMClassifiesUnreachableEndpoint(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "dead", Provider: config.ProviderOpenAICompat,
		BaseURL: "http://127.0.0.1:1", Model: "m",
		APIKey: "k", KeySource: "inline", RequiresKey: true,
	})

	status, payload := doJSON(t, handler, "POST", "/api/llm/test", map[string]any{"profile": "dead"})
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if payload["ok"] != false {
		t.Fatal("不可达端点应报告失败")
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if errorInfo["kind"] != "connection" {
		t.Errorf("应归类为 connection: %v", errorInfo)
	}
	if errorInfo["hint"] == "" {
		t.Error("应给出处置建议")
	}
}

func TestAPIDriversListing(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "GET", "/api/drivers", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	drivers, _ := payload["drivers"].([]any)
	if len(drivers) < 5 {
		t.Fatalf("应列出全部驱动: %v", drivers)
	}

	var oracle map[string]any
	for _, item := range drivers {
		driver, _ := item.(map[string]any)
		if driver["kind"] == "oracle" {
			oracle = driver
		}
	}
	if oracle == nil {
		t.Fatal("应包含 oracle 驱动")
	}
	slots, _ := oracle["slots"].([]any)
	if len(slots) == 0 {
		t.Fatal("oracle 驱动应声明槽位")
	}
	for _, item := range slots {
		slot, _ := item.(map[string]any)
		if slot["field"] == "auth_mode" {
			if slot["required"] != true {
				t.Error("auth_mode 应为必填")
			}
			choices, _ := slot["choices"].([]any)
			if len(choices) == 0 {
				t.Error("枚举槽位应给出候选值")
			}
			return
		}
	}
	t.Fatal("oracle 驱动应包含 auth_mode 槽位")
}

// ---------------------------------------------------------------------------
// 首页
// ---------------------------------------------------------------------------

func TestIndexServesPage(t *testing.T) {
	handler := newTestHandler(t)

	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200, 实际 %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Errorf("Content-Type 应为 HTML: %q", contentType)
	}
	body := recorder.Body.String()
	for _, fragment := range []string{"RunStor Bot", "/api/parse", "/api/clarify"} {
		if !strings.Contains(body, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 页面不引用任何外部资源 —— 否则内网部署会加载失败。
func TestIndexHasNoExternalAssets(t *testing.T) {
	handler := newTestHandler(t)

	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()

	for _, pattern := range []string{"src=\"http://", "src=\"https://", "href=\"http://", "href=\"https://"} {
		if strings.Contains(body, pattern) {
			t.Errorf("页面引用了外部资源: %s", pattern)
		}
	}
}

// ---------------------------------------------------------------------------
// 网页改配置: 选厂商 + 填 Key
// ---------------------------------------------------------------------------

func TestAPISaveProfileCreatesAndActivates(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset":   "deepseek",
		"api_key":  "sk-pasted-in-browser",
		"activate": true, // 界面上的"保存并启用"
	})
	if status != http.StatusOK {
		t.Fatalf("保存应成功, 状态 %d: %v", status, payload)
	}
	if payload["saved"] != "deepseek" {
		t.Errorf("应按 preset 自动命名: %v", payload["saved"])
	}
	if payload["active"] != "deepseek" {
		t.Errorf("保存后应自动设为当前模型: %v", payload["active"])
	}
	if payload["mode"] != "llm" {
		t.Errorf("应切换到真实模型模式: %v", payload["mode"])
	}

	// 健康检查也应反映出来
	_, health := doJSON(t, handler, "GET", "/health", nil)
	if health["mode"] != "llm" || health["active_profile"] != "deepseek" {
		t.Errorf("健康检查未反映切换: %v", health)
	}
}

func TestAPISavedKeyNeverReturned(t *testing.T) {
	secret := "sk-canary-2"
	handler := newTestHandler(t)

	if status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": secret,
	}); status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, payload)
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	raw, err := json.Marshal(profiles)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("GET /api/llm/profiles 泄露了刚保存的密钥")
	}
	// 但必须能看出"已配置密钥", 否则界面无法提示
	items, _ := profiles["profiles"].([]any)
	found := false
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] == "deepseek" && profile["has_key"] == true {
			found = true
			if profile["source"] != "runtime" {
				t.Errorf("来源应标为 runtime: %v", profile["source"])
			}
			if profile["editable"] != true {
				t.Errorf("网页创建的配置应可编辑: %v", profile)
			}
		}
	}
	if !found {
		t.Fatal("未在列表中找到刚保存的配置, 或未报告 has_key")
	}
}

func TestAPISaveProfileCustomEndpoint(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name":     "intra-gateway",
		"base_url": "https://llm.intra/v1",
		"model":    "qwen-max",
		"api_key":  "sk-intra",
	})
	if status != http.StatusOK {
		t.Fatalf("自定义端点应可保存: %d %v", status, payload)
	}
	if payload["saved"] != "intra-gateway" {
		t.Errorf("配置名不符: %v", payload["saved"])
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] != "intra-gateway" {
			continue
		}
		if profile["base_url"] != "https://llm.intra/v1" || profile["model"] != "qwen-max" {
			t.Fatalf("自定义配置不符: %v", profile)
		}
		if profile["available"] != true {
			t.Errorf("带 Key 的自定义配置应可用: %v", profile)
		}
		return
	}
	t.Fatal("未找到自定义配置")
}

func TestAPISaveProfileLocalModelNeedsNoKey(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "ollama", "model": "qwen2.5:14b", "activate": true,
	})
	if status != http.StatusOK {
		t.Fatalf("本地模型应可无 Key 保存: %d %v", status, payload)
	}
	if payload["mode"] != "llm" {
		t.Errorf("本地模型应可用: %v", payload)
	}
}

// 改接入点后旧密钥不沿用, 并且要在响应里说清楚 —— 否则用户以为"只改了个地址",
// 直到调用模型才发现没 key。
//
// 这条同时守住一条凭据外发路径: 只要"留着旧密钥 + 允许改 base_url", 把地址改成
// 自己的机器再点一次测试连接, 就能收到那把密钥。
func TestAPISaveProfileChangingEndpointDropsKey(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "target", "base_url": "https://api.deepseek.com", "model": "deepseek-chat",
		"api_key": "sk-real-key",
	})
	if status != http.StatusOK {
		t.Fatalf("首次保存应成功: %d %v", status, payload)
	}

	// 只改地址, 不带密钥
	status, payload = doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "target", "base_url": "https://elsewhere.example.com/v1", "model": "deepseek-chat",
	})
	if status != http.StatusOK {
		t.Fatalf("更新应成功: %d %v", status, payload)
	}
	if payload["key_cleared"] != true {
		t.Errorf("应明确报告密钥未沿用: %v", payload)
	}
	if notice, _ := payload["notice"].(string); notice == "" {
		t.Error("应给出可读的提示文案")
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] != "target" {
			continue
		}
		if profile["has_key"] == true {
			t.Fatal("换了接入点仍保留着旧密钥 —— 这把密钥会被发往新地址")
		}
		if profile["base_url"] != "https://elsewhere.example.com/v1" {
			t.Errorf("新地址应已生效: %v", profile["base_url"])
		}
		return
	}
	t.Fatal("未找到 target 配置")
}

func TestAPISaveProfileValidationErrors(t *testing.T) {
	handler := newTestHandler(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"未知 preset", map[string]any{"preset": "no-such", "api_key": "k"}},
		{"非法配置名", map[string]any{"name": "有中文", "preset": "ollama"}},
		{"未知 provider", map[string]any{"name": "p", "provider": "bedrock"}},
	}
	for _, entry := range cases {
		status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", entry.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: 应返回 400, 实际 %d", entry.name, status)
			continue
		}
		if payload["error"] == nil {
			t.Errorf("%s: 应返回 error 字段", entry.name)
		}
	}
}

// 缺 Key 的远程配置可以保存, 但必须被标为不可用 —— 界面据此提示用户补 Key。
func TestAPISaveProfileWithoutKeyIsListedUnavailable(t *testing.T) {
	handler := newTestHandler(t)

	if status, _ := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek",
	}); status != http.StatusOK {
		t.Fatalf("保存应成功")
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] != "deepseek" {
			continue
		}
		if profile["available"] != false {
			t.Error("缺 Key 时应标记为不可用")
		}
		if reason, _ := profile["unavailable_reason"].(string); reason == "" {
			t.Error("应说明原因")
		}
		return
	}
	t.Fatal("未找到配置")
}

func TestAPIDeleteRuntimeProfile(t *testing.T) {
	handler := newTestHandler(t)

	doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-x",
	})

	status, payload := doJSON(t, handler, "DELETE", "/api/llm/profiles/deepseek", nil)
	if status != http.StatusOK {
		t.Fatalf("删除应成功: %d %v", status, payload)
	}
	if payload["deleted"] != "deepseek" {
		t.Errorf("删除结果不符: %v", payload)
	}
	// 删掉当前模型后应回退
	if payload["active"] == "deepseek" {
		t.Errorf("删掉当前模型后不该仍指向它: %v", payload["active"])
	}
}

func TestAPIDeleteConfigProfileIsRefused(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "managed", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://api.example.com", Model: "m",
		APIKey: "k", KeySource: "inline", RequiresKey: true, Source: config.SourceConfig,
	})

	status, payload := doJSON(t, handler, "DELETE", "/api/llm/profiles/managed", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("配置文件声明的项不该能在网页上删除: %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if !strings.Contains(errorInfo["message"].(string), "配置文件") {
		t.Errorf("报错应说明来源: %v", errorInfo["message"])
	}
}

func TestAPIDeleteUnknownProfile(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "DELETE", "/api/llm/profiles/ghost", nil)
	if status != http.StatusBadRequest {
		t.Errorf("删除不存在的项应返回 400, 实际 %d", status)
	}
}

// 关闭网页改配置时, 保存接口必须明确拒绝, 而不是静默丢弃。
func TestAPISaveProfileRefusedWhenUIConfigDisabled(t *testing.T) {
	handler := newTestHandlerWithRuntime(t, false)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-x",
	})
	if status != http.StatusConflict {
		t.Fatalf("应返回 409, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if errorInfo["kind"] != "ui_config_disabled" {
		t.Errorf("错误类型不符: %v", errorInfo)
	}
	if errorInfo["hint"] == "" {
		t.Error("应告诉用户如何开启")
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	if profiles["runtime_enabled"] != false {
		t.Error("列表应报告网页改配置已关闭")
	}
}

// 同名时网页配置覆盖配置文件, 且界面能看出这一点。
func TestAPIRuntimeProfileOverridesConfigProfile(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "deepseek", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://from-config.example.com", Model: "config-model",
		APIKey: "config-key", KeySource: "env:X", RequiresKey: true, Source: config.SourceConfig,
	})

	// 显式指定同名, 才会走"覆盖配置文件"这条路
	status, saved := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "deepseek", "preset": "deepseek", "api_key": "sk-from-ui",
	})
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, saved)
	}
	if saved["saved"] != "deepseek" {
		t.Fatalf("显式指定名字时应沿用: %v", saved["saved"])
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	count := 0
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] != "deepseek" {
			continue
		}
		count++
		if profile["source"] != "runtime" {
			t.Errorf("同名应显示为网页配置: %v", profile["source"])
		}
		if profile["overrides_config"] != true {
			t.Error("应提示覆盖了配置文件里的同名项")
		}
		if profile["base_url"] != "https://api.deepseek.com" {
			t.Errorf("生效的应是网页配置: %v", profile["base_url"])
		}
	}
	if count != 1 {
		t.Fatalf("同名项不该出现两次: %d", count)
	}
}

func TestAPISaveProfileKeepsKeyWhenBlank(t *testing.T) {
	handler := newTestHandler(t)

	doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-first",
	})
	// 只改模型名, 不带 Key
	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "deepseek", "preset": "deepseek", "model": "deepseek-reasoner",
	})
	if status != http.StatusOK {
		t.Fatalf("更新应成功: %d %v", status, payload)
	}

	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	for _, item := range items {
		profile, _ := item.(map[string]any)
		if profile["name"] != "deepseek" {
			continue
		}
		if profile["model"] != "deepseek-reasoner" {
			t.Errorf("模型名应被更新: %v", profile["model"])
		}
		if profile["has_key"] != true {
			t.Error("留空时密钥应被保留")
		}
		return
	}
	t.Fatal("未找到配置")
}

// 省略配置名时自动避让同名 —— 这是安全默认: 不覆盖已有的配置文件项。
func TestAPISaveProfileAutoRenamesOnCollision(t *testing.T) {
	handler := newTestHandler(t, &config.Profile{
		Name: "deepseek", Provider: config.ProviderOpenAICompat,
		BaseURL: "https://from-config.example.com", Model: "config-model",
		APIKey: "config-key", KeySource: "env:X", RequiresKey: true, Source: config.SourceConfig,
	})

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-from-ui",
	})
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, payload)
	}
	if payload["saved"] == "deepseek" {
		t.Fatal("省略名字时不该占用已存在的名字, 否则会静默覆盖配置文件的声明")
	}
	if payload["saved"] != "deepseek-2" {
		t.Errorf("应按序号避让, 实际 %v", payload["saved"])
	}

	// 原有配置文件项应完好无损
	_, profiles := doJSON(t, handler, "GET", "/api/llm/profiles", nil)
	items, _ := profiles["profiles"].([]any)
	var sawConfig, sawRuntime bool
	for _, item := range items {
		profile, _ := item.(map[string]any)
		switch profile["name"] {
		case "deepseek":
			sawConfig = true
			if profile["source"] != "config" || profile["base_url"] != "https://from-config.example.com" {
				t.Errorf("配置文件项被改动了: %v", profile)
			}
		case "deepseek-2":
			sawRuntime = true
			if profile["source"] != "runtime" {
				t.Errorf("新配置来源应标为 runtime: %v", profile["source"])
			}
		}
	}
	if !sawConfig || !sawRuntime {
		t.Fatalf("应同时存在两个配置: config=%v runtime=%v", sawConfig, sawRuntime)
	}
}

// ---------------------------------------------------------------------------
// 对话式界面: 历史列表 / 详情 / 删除 / 自由文本补充
// ---------------------------------------------------------------------------

func TestAPIListSessions(t *testing.T) {
	handler := newTestHandler(t)

	// 初始为空
	status, payload := doJSON(t, handler, "GET", "/api/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if total, _ := payload["total"].(float64); total != 0 {
		t.Errorf("初始应为 0 条: %v", payload["total"])
	}

	// 建两条
	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	_, second := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": "把Oracle数据库恢复一下"})

	status, payload = doJSON(t, handler, "GET", "/api/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	sessions, _ := payload["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("应有 2 条历史, 实际 %d", len(sessions))
	}

	// 最近活动的排最前, 且摘要字段齐全
	head, _ := sessions[0].(map[string]any)
	if head["id"] != second["session_id"] {
		t.Errorf("最近活动的应排最前: %v", head["id"])
	}
	for _, field := range []string{"id", "title", "kind", "status", "turns", "pending", "updated_at"} {
		if _, ok := head[field]; !ok {
			t.Errorf("摘要缺少字段 %s", field)
		}
	}
	if head["kind"] != "restore" {
		t.Errorf("第二条意图应为 restore: %v", head["kind"])
	}
	if title, _ := head["title"].(string); title == "" {
		t.Error("标题不该为空")
	}
	_ = first
}

func TestAPISessionDetailRebuildsConversation(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)

	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers":    map[string]string{"schedule.full.time": "凌晨1点"},
	})

	status, detail := doJSON(t, handler, "GET", "/api/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if detail["text"] != acceptanceText {
		t.Errorf("应回显需求原文: %v", detail["text"])
	}
	if detail["turns"] != float64(2) {
		t.Errorf("回合数应为 2: %v", detail["turns"])
	}

	transcript, _ := detail["transcript"].([]any)
	if len(transcript) < 3 {
		t.Fatalf("对话记录应至少 3 条, 实际 %d", len(transcript))
	}
	head, _ := transcript[0].(map[string]any)
	if head["role"] != "user" {
		t.Errorf("第一条应是用户发言: %v", head["role"])
	}
	last, _ := transcript[len(transcript)-1].(map[string]any)
	if last["role"] != "agent" {
		t.Errorf("最后一条应是 agent: %v", last["role"])
	}
	// 待补充的问题要带出来, 界面才能继续作答
	questions, _ := detail["questions"].([]any)
	if len(questions) == 0 {
		t.Error("详情应带出待补充的问题")
	}
}

func TestAPIDeleteSession(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)

	status, payload := doJSON(t, handler, "DELETE", "/api/sessions/"+sessionID, nil)
	if status != http.StatusOK {
		t.Fatalf("删除应成功: %d %v", status, payload)
	}
	if payload["deleted"] != sessionID {
		t.Errorf("返回的会话标识不符: %v", payload["deleted"])
	}

	// 删掉后详情应 404
	status, _ = doJSON(t, handler, "GET", "/api/sessions/"+sessionID, nil)
	if status != http.StatusNotFound {
		t.Errorf("删除后应 404, 实际 %d", status)
	}

	// 重复删除也应 404
	status, _ = doJSON(t, handler, "DELETE", "/api/sessions/"+sessionID, nil)
	if status != http.StatusNotFound {
		t.Errorf("重复删除应 404, 实际 %d", status)
	}
}

// 对话式界面的核心: 用户可以用一句话补充, 而不是必须逐格填空。
func TestAPIClarifyAcceptsFreeText(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)
	if first["status"] != "needs_clarification" {
		t.Fatalf("应先进入追问: %v", first["status"])
	}

	status, second := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"text":       "都是凌晨1点执行，保留14天，备份到本地 /u01/backup/orcl",
	})
	if status != http.StatusOK {
		t.Fatalf("自由补充应被接受: %d %v", status, second)
	}
	if second["session_id"] != sessionID {
		t.Errorf("应续在同一会话里: %v", second["session_id"])
	}
	// 起点信息不能在补充过程中丢失
	plan, _ := second["plan"].(map[string]any)
	options := plan["source"].(map[string]any)["items"].([]any)[0].(map[string]any)["options"].(map[string]any)
	if options["auth_mode"] != "os_auth" || options["channels"] != float64(4) {
		t.Errorf("原有信息丢失: %v", options)
	}
}

func TestAPIClarifyRequiresAnswersOrText(t *testing.T) {
	handler := newTestHandler(t)
	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)

	status, payload := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("既无 answers 也无 text 应返回 400, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if !strings.Contains(errorInfo["message"].(string), "text") {
		t.Errorf("报错应说明需要什么: %v", errorInfo["message"])
	}
}

func TestAPIClarifyFreeTextOnUnknownSession(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": "s-nope", "text": "补充内容",
	})
	if status != http.StatusBadRequest {
		t.Errorf("未知会话应返回 400, 实际 %d", status)
	}
}

// 已完成后再补充应能在此基础上修改, 而不是报错。
func TestAPIClarifyAfterReady(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)
	_, ready := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})
	if ready["status"] != "ready" {
		t.Fatalf("应已完成: %v", ready["status"])
	}

	status, modified := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"text":       "保留期改成30天",
	})
	if status != http.StatusOK {
		t.Fatalf("完成后补充应被接受: %d %v", status, modified)
	}
	if modified["turns"] != float64(3) {
		t.Errorf("应计入新的一轮: %v", modified["turns"])
	}
}

// 侧栏要按 recent 排序, 因此重新打开旧会话会把它顶上来。
func TestAPISessionListReordersOnActivity(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	firstID, _ := first["session_id"].(string)
	time.Sleep(5 * time.Millisecond)
	doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": "把Oracle数据库恢复一下"})

	// 回到第一个会话并补充一句, 它应该排到最前
	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": firstID, "text": "保留7天",
	})

	_, payload := doJSON(t, handler, "GET", "/api/sessions", nil)
	sessions, _ := payload["sessions"].([]any)
	head, _ := sessions[0].(map[string]any)
	if head["id"] != firstID {
		t.Errorf("最近活动的会话应排最前: %v", head["id"])
	}
}

// 保存并启用必须把"当前模型"记住, 否则重启后会悄悄回到旧模型。
func TestAPISaveProfilePersistsActivation(t *testing.T) {
	store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
	if err != nil {
		t.Fatalf("打开运行态失败: %v", err)
	}
	cfg := &config.Config{
		Server:  config.Server{MaxTurns: 10, PromptLimit: 20000},
		Runtime: store, DefaultProfile: "demo",
	}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	handler := New(orchestrator, log.New(io.Discard, "", 0), "test").Handler()

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-test", "activate": true,
	})
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, payload)
	}
	if payload["activate_error"] != nil {
		t.Fatalf("启用不应失败: %v", payload["activate_error"])
	}
	if payload["active"] != "deepseek" || payload["mode"] != "llm" {
		t.Fatalf("保存后应立即生效: %v", payload)
	}
	// 关键: 选择要落盘, 重启后才不会退回旧模型
	if store.Active() != "deepseek" {
		t.Fatalf("当前模型选择未持久化: %q", store.Active())
	}
}

// activate=false 时只保存不切换。
func TestAPISaveProfileWithoutActivate(t *testing.T) {
	handler := newTestHandler(t)

	status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset": "deepseek", "api_key": "sk-test", "activate": false,
	})
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, payload)
	}
	if payload["saved"] != "deepseek" {
		t.Errorf("应保存成功: %v", payload["saved"])
	}
	if payload["mode"] != "demo" {
		t.Errorf("未要求启用时不该切换模型: %v", payload)
	}
}

// 回归: 保存临时配置再删除, 不能扰动用户选定并已持久化的模型。
//
// 这正是部署自检踩过的坑 —— 自检默认 activate=true 抢走当前模型, 删除时又因
// "删的正是当前模型"把选择清空, 于是每次部署后用户的模型都退回 demo。
func TestAPISmokeCycleDoesNotDisturbActiveModel(t *testing.T) {
	store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
	if err != nil {
		t.Fatalf("打开运行态失败: %v", err)
	}
	cfg := &config.Config{
		Server:  config.Server{MaxTurns: 10, PromptLimit: 20000},
		Runtime: store, DefaultProfile: "demo",
	}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	handler := New(orchestrator, log.New(io.Discard, "", 0), "test").Handler()

	// 用户配置并启用了自己的模型
	doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "mine", "preset": "deepseek", "api_key": "sk-user", "activate": true,
	})
	if store.Active() != "mine" {
		t.Fatalf("用户模型应为当前: %q", store.Active())
	}

	// 自检: 保存一个临时配置再不激活, 然后删除
	doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"name": "smoke-local", "preset": "ollama", "model": "qwen2.5:14b", "activate": false,
	})
	doJSON(t, handler, "DELETE", "/api/llm/profiles/smoke-local", nil)

	if store.Active() != "mine" {
		t.Fatalf("自检后用户的模型被改动了: %q", store.Active())
	}
	_, health := doJSON(t, handler, "GET", "/health", nil)
	if health["active_profile"] != "mine" || health["mode"] != "llm" {
		t.Fatalf("健康检查应仍指向用户的模型: %v", health)
	}
}

// 输入框高度是需求方明确提过的要求, 加一条断言免得日后被改回去。
//
// 只断言下限而不锁死具体数值: 微调高度是正常的, 但退回"只露两行"就不该通过。
func TestComposerTextareaIsTallEnough(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	// 默认行数
	if !strings.Contains(page, `rows="4"`) {
		t.Error("输入框默认行数应至少 4 行")
	}

	// CSS 里的最小高度
	match := regexp.MustCompile(`min-height:\s*(\d+)px`).FindStringSubmatch(page)
	if match == nil {
		t.Fatal("未找到输入框的 min-height 设置")
	}
	minHeight, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("min-height 不是整数: %v", match[1])
	}
	if minHeight < 100 {
		t.Errorf("输入框最小高度应 >= 100px (约 4 行), 实际 %dpx —— 需求方明确要求加高", minHeight)
	}

	// 自动增长的上界必须取自 CSS, 不能在这里另写一个数字
	if !strings.Contains(page, "getComputedStyle") {
		t.Error("自动增长应读取 CSS 的 min/max, 避免两处数字漂移")
	}

	// 上限要随视口自适应, 免得在矮屏上把对话区挤没
	if !strings.Contains(page, "max-height") {
		t.Error("应设置最大高度")
	}
}

// 追问要带上可点的推荐值 —— 需求方明确要求"推荐选择 + 自定义回答"。
func TestAPIQuestionsCarryRecommendations(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	questions, _ := first["questions"].([]any)
	if len(questions) == 0 {
		t.Fatal("应产生追问")
	}

	for _, raw := range questions {
		question, _ := raw.(map[string]any)
		slot, _ := question["slot"].(string)
		choices, _ := question["choices"].([]any)
		suggestions, _ := question["suggestions"].([]any)

		// 断言字段存在 (即使是空数组), 前端不必额外判空
		if question["choices"] == nil || question["suggestions"] == nil {
			t.Errorf("%s: choices/suggestions 字段不该缺失", slot)
		}
		if len(choices)+len(suggestions) == 0 {
			t.Errorf("%s: 既无候选值也无推荐值, 用户只能手打", slot)
		}
	}
}

// 候选值与推荐值必须合起来覆盖用户能点到的所有答案, 且不重复。
func TestAPIQuestionsHaveNoDuplicateOptions(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	questions, _ := first["questions"].([]any)

	for _, raw := range questions {
		question, _ := raw.(map[string]any)
		slot, _ := question["slot"].(string)
		seen := map[string]bool{}
		for _, field := range []string{"choices", "suggestions"} {
			items, _ := question[field].([]any)
			for _, item := range items {
				text, _ := item.(string)
				if seen[text] {
					t.Errorf("%s: 选项 %q 重复出现, 界面上会看到两个一样的按钮", slot, text)
				}
				seen[text] = true
			}
		}
	}
}

// 通用枚举槽位过去只登记了 ChoicesMap, 界面读 Choices 时是空的 —— 于是
// "备份存储到哪里?" 从来没有可点的候选值。这条断言守住那个漏洞。
func TestAPIEnumQuestionsExposeChoices(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	questions, _ := first["questions"].([]any)

	var destinations map[string]any
	for _, raw := range questions {
		question, _ := raw.(map[string]any)
		if question["slot"] == "destinations" {
			destinations = question
		}
	}
	if destinations == nil {
		t.Fatal("应有 destinations 追问")
	}
	choices, _ := destinations["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("destinations 应给出可选的存储类型: %v", destinations)
	}

	// 界面渲染时点一下就提交, 所以候选值本身必须能被解析
	handler2 := newTestHandler(t)
	_, second := doJSON(t, handler2, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := second["session_id"].(string)
	if status, payload := doJSON(t, handler2, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers":    map[string]string{"destinations": choices[0].(string)},
	}); status != http.StatusOK {
		t.Fatalf("点候选值应能提交成功: %d %v", status, payload)
	}
}

// 页面上要能看到推荐选择的入口。
func TestIndexRendersRecommendationOptions(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		"选择一项（也可以自己填写）",        // 封闭候选
		"常用值（也可以自己填写）",         // 开放推荐
		"或自己填写",                // 自定义输入
		"question.suggestions", // 必须读后端的推荐值, 而不是前端硬编码一份
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 内联脚本没有任何编译期检查, 所以"调用了一个不存在的函数"要等用户点下去才暴露。
//
// 这不是假设: 对话式界面改写时把 sendAnswers 错写成 submitAnswers, 结果问题卡片上的
// "提交"与推荐值按钮一直是死的 —— 因为只验证过底部输入框那条路径, 一直没被发现。
// node --check 只能查语法, 抓不到这类错误, 因此这里自己查一遍。
// methodDefinitionPattern 匹配对象字面量里的方法定义 (t(text) { ... })。
//
// 它们与调用长得一样, 不认出来就会把 i18n.t / i18n.tf / i18n.tr 误报成未定义函数。
var methodDefinitionPattern = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{`)

func TestInlineScriptCallsOnlyDefinedFunctions(t *testing.T) {
	// 用共用的提取器逐段扫, 不要自己写贪婪正则: `(?s)<script>(.*)</script>` 会从第一个
	// <script> 一直吃到最后一个 </script>, 把中间的 <style> 也算进来 —— 于是 CSS 里的
	// rgba( / rect( / translateX( 全被当成"未定义的函数调用"。
	scripts := inlineScripts(t)
	script := scripts[len(scripts)-1]

	// 本页定义的函数与变量 (各段脚本合并统计: 前一段定义的函数后一段也能用)
	defined := map[string]bool{}
	all := strings.Join(scripts, "\n")
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`),
		regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=`),
		regexp.MustCompile(`function\s*\(\s*([A-Za-z_$][\w$]*)`),
		methodDefinitionPattern,
	} {
		for _, match := range pattern.FindAllStringSubmatch(all, -1) {
			defined[match[1]] = true
		}
	}

	// 浏览器全局与语言关键字 (后者会被 "ident(" 这个形状误判)
	allowed := map[string]bool{
		// 浏览器 API
		"alert": true, "confirm": true, "prompt": true, "fetch": true,
		"encodeURIComponent": true, "decodeURIComponent": true,
		"setTimeout": true, "clearTimeout": true, "setInterval": true, "clearInterval": true,
		"getComputedStyle": true, "parseFloat": true, "parseInt": true,
		"isNaN": true, "isFinite": true, "encodeURI": true, "decodeURI": true,
		"Blob": true, "URL": true, "FileReader": true, "TextDecoder": true, "TextEncoder": true,
		"requestAnimationFrame": true, "structuredClone": true, "queueMicrotask": true,
		// 关键字: 会以 "if (" / "catch (" 被正则命中, 也可能出现在字符串里
		// (如 CSS 的 "var(--x)")。它们都是保留字, 不可能是真函数名, 放行不损失检测能力。
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"function": true, "async": true, "return": true, "typeof": true, "await": true,
		"new": true, "delete": true, "void": true, "else": true, "do": true, "in": true,
		"var": true, "let": true, "const": true, "case": true, "of": true,
		"instanceof": true, "yield": true, "throw": true, "default": true,
	}

	// 调用点: 标识符后紧跟左括号。Go 的正则 (RE2) 不支持后向断言, 因此取匹配位置后
	// 手工看前一个字符是不是 . 或词字符 —— 那表示这是方法调用, 不是本页函数。
	callPattern := regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)
	unknown := map[string]int{}
	for _, location := range callPattern.FindAllStringSubmatchIndex(script, -1) {
		start, nameStart, nameEnd := location[0], location[2], location[3]
		if start > 0 {
			previous := script[start-1]
			isWord := previous == '_' || previous == '$' ||
				(previous >= 'a' && previous <= 'z') ||
				(previous >= 'A' && previous <= 'Z') ||
				(previous >= '0' && previous <= '9')
			if previous == '.' || isWord {
				continue
			}
		}

		name := script[nameStart:nameEnd]
		// 大写开头的是构造器与内建对象 (Blob / URL / Array ...), 不在本测试范围
		if name[0] < 'a' || name[0] > 'z' {
			continue
		}
		if defined[name] || allowed[name] {
			continue
		}
		unknown[name]++
	}

	if len(unknown) > 0 {
		names := make([]string, 0, len(unknown))
		for name, count := range unknown {
			names = append(names, fmt.Sprintf("%s(%d 次)", name, count))
		}
		sort.Strings(names)
		t.Fatalf("页面调用了未定义的函数 (点下去会抛 ReferenceError): %s", strings.Join(names, ", "))
	}
}

// 追问应是"左右分步"而不是上下长列表 —— 需求方明确要求逐题、可退回。
func TestIndexRendersQuestionStepper(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		"function renderQuestionStepper", // 分步容器
		"← 上一题",                          // 可退回
		"下一题 →",                          // 向前
		"step-dots",                      // 步骤指示
		`i18n.tf("第 {0} 题 / 共 {1}"`,      // 进度显示 (整句翻译, 英文语序不同)
		"function focusQuestions",        // 答完滚到题卡而不是最底部
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}

	// 只读的历史轮次仍应走紧凑列表, 不给作答控件
	if !strings.Contains(page, "function renderQuestionList") {
		t.Error("历史轮次应使用只读的紧凑列表")
	}
}

// 答完一题后不该滚到整段对话的最底部 —— 那会让人找不回下一题。
func TestIndexDoesNotJumpToBottomWhenQuestionsRemain(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	if !strings.Contains(page, `if (result.status === "ready") { scrollToBottom(); } else { focusQuestions(); }`) {
		t.Error("应只在信息完整时滚到底部, 否则聚焦到待作答的问题")
	}
}

// ---------------------------------------------------------------------------
// 总结与提交
// ---------------------------------------------------------------------------

// 完成后的响应要带中文总结, 界面才不必自己去解析枚举代码值。
func TestAPIReadyResponseCarriesSummary(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)
	_, ready := doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})
	if ready["status"] != "ready" {
		t.Fatalf("应已完成: %v", ready["status"])
	}

	summary, _ := ready["summary"].(map[string]any)
	if summary == nil {
		t.Fatal("完成时应带总结")
	}
	if summary["title"] == nil || summary["title"] == "" {
		t.Errorf("总结应有标题: %v", summary)
	}
	rows, _ := summary["rows"].([]any)
	if len(rows) < 5 {
		t.Fatalf("总结行太少: %v", rows)
	}

	// 值必须是中文而不是代码 (os_auth / whole_instance 这类不能直接给用户看)
	raw, _ := json.Marshal(summary)
	for _, code := range []string{"os_auth", "whole_instance", `"local"`} {
		if strings.Contains(string(raw), code) {
			t.Errorf("总结里出现了代码值 %q: %s", code, string(raw))
		}
	}
	for _, want := range []string{"认证方式", "全量备份", "保留策略", "备份目的地", "01:00"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("总结里缺少 %q", want)
		}
	}

	// 未完成时不给总结
	_, partial := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	if partial["summary"] != nil {
		t.Error("信息未完整时不该给总结")
	}
}

// 会话详情也要带总结, 否则重新打开一个已完成的会话就看不到清单。
func TestAPISessionDetailCarriesSummary(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)
	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})

	_, detail := doJSON(t, handler, "GET", "/api/sessions/"+sessionID, nil)
	if detail["summary"] == nil {
		t.Fatal("详情应带总结")
	}
}

func TestAPISubmitSession(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)

	// 未完成时不能提交
	status, payload := doJSON(t, handler, "POST", "/api/sessions/"+sessionID+"/submit", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("未完成时提交应返回 400, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if !strings.Contains(errorInfo["message"].(string), "待补充") {
		t.Errorf("报错应说明还差什么: %v", errorInfo["message"])
	}

	// 补齐后提交
	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})
	status, submitted := doJSON(t, handler, "POST", "/api/sessions/"+sessionID+"/submit", nil)
	if status != http.StatusOK {
		t.Fatalf("提交应成功: %d %v", status, submitted)
	}
	if submitted["plan"] == nil || submitted["summary"] == nil {
		t.Error("提交结果应带计划与总结 (界面要显示已定稿的清单)")
	}

	// 侧栏要能看出已提交
	_, list := doJSON(t, handler, "GET", "/api/sessions", nil)
	sessions, _ := list["sessions"].([]any)
	for _, raw := range sessions {
		item, _ := raw.(map[string]any)
		if item["id"] != sessionID {
			continue
		}
		if item["submitted"] != true {
			t.Errorf("列表应标记已提交: %v", item)
		}
		return
	}
	t.Fatal("未找到会话")
}

func TestAPISubmitUnknownSession(t *testing.T) {
	handler := newTestHandler(t)
	status, _ := doJSON(t, handler, "POST", "/api/sessions/s-nope/submit", nil)
	if status != http.StatusBadRequest {
		t.Errorf("未知会话应返回 400, 实际 %d", status)
	}
}

// 定稿后再改动会让"已提交"失效 —— 交付物已经变了。
func TestAPISubmitInvalidatedByModification(t *testing.T) {
	handler := newTestHandler(t)

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	sessionID, _ := first["session_id"].(string)
	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID,
		"answers": map[string]string{
			"schedule.full.time":        "凌晨1点",
			"schedule.incremental.time": "凌晨1点",
			"retention":                 "保留14天",
			"destinations":              "本地磁盘 /u01/backup/orcl",
		},
	})
	doJSON(t, handler, "POST", "/api/sessions/"+sessionID+"/submit", nil)

	doJSON(t, handler, "POST", "/api/clarify", map[string]any{
		"session_id": sessionID, "text": "保留期改成90天",
	})

	_, list := doJSON(t, handler, "GET", "/api/sessions", nil)
	sessions, _ := list["sessions"].([]any)
	for _, raw := range sessions {
		item, _ := raw.(map[string]any)
		if item["id"] != sessionID {
			continue
		}
		if item["submitted"] == true {
			t.Error("改动之后不该仍标记已提交")
		}
		return
	}
	t.Fatal("未找到会话")
}

// 总结卡与就地修改必须渲染出来。
func TestIndexRendersSummaryCard(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		"function renderSummary(", // 总结卡
		"计划待确认",                   // 未提交时的标题
		"计划已定稿",                   // 提交后的标题
		"function submitPlan(",    // 提交
		"/submit",                 // 提交接口
		"function openRowEditor(", // 单行修改
		"查看 / 复制计划 JSON",          // 原始 JSON 收进折叠区
		"提交计划",                    // 按钮文案
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 新的总结卡要取代旧的: 否则改一次就多一张可编辑清单, 用户分不清该改哪张。
func TestIndexStalesSupersededSummaryCards(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`old.classList.add("stale")`,                   // 标记旧卡
		`old.querySelectorAll(".summary-fix").forEach`, // 撤掉旧的修改按钮
		`old.querySelector(".summary-foot")`,           // 撤掉旧的提交按钮
		"（已被后续修改取代）",                                   // 界面上说明这是留档
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 定稿状态要在界面上有明确体现: 侧栏标记 + 按钮不可再点。
func TestIndexShowsSubmittedState(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`item.submitted`, // 侧栏读已提交标记
		"· 已提交",          // 侧栏文案
		`settings.submitted ? "已定稿" : "提交计划"`,    // 定稿后按钮改文案
		"submit.disabled = !!settings.submitted", // 且不可再点
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// ---------------------------------------------------------------------------
// 运行时清单 (能备份哪些应用 / 能备份到哪些介质来自接口)
// ---------------------------------------------------------------------------

// newTestHandlerWithCatalog 让清单来自一个 mock 接口。
func newTestHandlerWithCatalog(t *testing.T, applications, mediaServers []map[string]any) http.Handler {
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

	enabled := true
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{
		MaxTurns: 10, SessionTTL: time.Minute,
		Catalog: config.CatalogConfig{Enabled: &enabled, URL: server.URL, TTLSeconds: 60},
	})
	return New(orchestrator, log.New(io.Discard, "", 0), "test").Handler()
}

func TestAPICatalogFromAPI(t *testing.T) {
	handler := newTestHandlerWithCatalog(t,
		[]map[string]any{{"kind": "dameng", "display": "达梦 DM8", "source_type": "database"}},
		[]map[string]any{{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"}})

	status, payload := doJSON(t, handler, "GET", "/api/catalog", nil)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200: %d", status)
	}
	if payload["source"] != "api" || payload["from_api"] != true {
		t.Fatalf("清单应来自接口: %v", payload)
	}

	applications, _ := payload["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("应用数量不符: %v", applications)
	}
	app, _ := applications[0].(map[string]any)
	if app["kind"] != "dameng" || app["display"] != "达梦 DM8" {
		t.Errorf("应用内容不符: %v", app)
	}

	servers, _ := payload["media_servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("介质服务器数量不符: %v", servers)
	}
	server, _ := servers[0].(map[string]any)
	if server["display"] != "本地磁盘池A /u01/backup" {
		t.Errorf("展示名应含名称与落点: %v", server["display"])
	}
}

// 健康检查要能看出清单从哪来 —— 否则用户不知道候选值是环境里的还是内置的。
func TestAPIHealthReportsCatalogSource(t *testing.T) {
	handler := newTestHandlerWithCatalog(t, []map[string]any{{"kind": "dameng"}}, nil)

	_, payload := doJSON(t, handler, "GET", "/health", nil)
	catalogInfo, _ := payload["catalog"].(map[string]any)
	if catalogInfo == nil {
		t.Fatal("健康检查应报告清单信息")
	}
	if catalogInfo["source"] != "api" {
		t.Errorf("清单来源应为 api: %v", catalogInfo)
	}
	if catalogInfo["applications"] != float64(1) {
		t.Errorf("应用数量不符: %v", catalogInfo)
	}
}

// 清单接口不可用时健康检查要说明退化, 而不是静默用内置清单。
func TestAPIHealthReportsCatalogFallback(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)

	enabled := true
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	orchestrator := agent.New(llm.NewRegistry(cfg), agent.Options{
		MaxTurns: 10, SessionTTL: time.Minute,
		Catalog: config.CatalogConfig{Enabled: &enabled, URL: dead.URL, TTLSeconds: 60},
	})
	handler := New(orchestrator, log.New(io.Discard, "", 0), "test").Handler()

	_, payload := doJSON(t, handler, "GET", "/health", nil)
	catalogInfo, _ := payload["catalog"].(map[string]any)
	if catalogInfo["source"] != "builtin-fallback" {
		t.Errorf("应报告退回内置清单: %v", catalogInfo)
	}
	if warning, _ := catalogInfo["warning"].(string); warning == "" {
		t.Error("应说明退化原因")
	}
}

// 目的地追问的候选项要来自清单接口。
func TestAPIDestinationChoicesComeFromCatalog(t *testing.T) {
	handler := newTestHandlerWithCatalog(t, nil, []map[string]any{
		{"id": "pool-a", "name": "本地磁盘池A", "type": "local", "path": "/u01/backup"},
		{"id": "tape-1", "name": "磁带库1", "type": "tape"},
	})

	_, first := doJSON(t, handler, "POST", "/api/parse", map[string]any{"text": acceptanceText})
	questions, _ := first["questions"].([]any)
	for _, raw := range questions {
		question, _ := raw.(map[string]any)
		if question["slot"] != "destinations" {
			continue
		}
		choices, _ := question["choices"].([]any)
		joined := ""
		for _, choice := range choices {
			joined += choice.(string) + "|"
		}
		if !strings.Contains(joined, "本地磁盘池A") || !strings.Contains(joined, "磁带库1") {
			t.Fatalf("候选项应来自接口清单: %v", choices)
		}
		return
	}
	t.Fatalf("应有目的地追问: %v", questions)
}

// 应用与介质分开的两个接口也要支持。
func TestAPICatalogRefresh(t *testing.T) {
	handler := newTestHandlerWithCatalog(t, []map[string]any{{"kind": "dameng"}}, nil)

	status, payload := doJSON(t, handler, "POST", "/api/catalog/refresh", nil)
	if status != http.StatusOK {
		t.Fatalf("刷新应成功: %d", status)
	}
	if payload["applications"] != float64(1) {
		t.Errorf("刷新结果不符: %v", payload)
	}
}

// 驱动列表以清单为准, 同时不隐藏内置驱动。
func TestAPIDriversFollowCatalog(t *testing.T) {
	handler := newTestHandlerWithCatalog(t,
		[]map[string]any{{"kind": "dameng", "display": "达梦 DM8"}}, nil)

	_, payload := doJSON(t, handler, "GET", "/api/drivers", nil)
	if payload["catalog_source"] != "api" {
		t.Errorf("应说明清单来源: %v", payload["catalog_source"])
	}

	drivers, _ := payload["drivers"].([]any)
	var sawCatalog, sawBuiltinOnly bool
	for _, raw := range drivers {
		item, _ := raw.(map[string]any)
		if item["kind"] == "dameng" && item["in_catalog"] == true {
			sawCatalog = true
		}
		// oracle 不在接口清单里, 但它已注册, 仍应列出并标注
		if item["kind"] == "oracle" && item["in_catalog"] == false && item["registered"] == true {
			sawBuiltinOnly = true
		}
	}
	if !sawCatalog {
		t.Error("清单里的应用应被列出")
	}
	if !sawBuiltinOnly {
		t.Error("内置驱动也该列出, 否则用户以为它们消失了")
	}
}

// 清单来源必须让用户看得见: 否则他不知道候选值是环境里的还是内置的。
func TestIndexShowsCatalogSource(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		"catalogBadge",         // 头部徽章
		"catalogBox",           // 抽屉里的清单区块
		"应用与介质清单",              // 区块标题
		"/api/catalog/refresh", // 刷新接口
		"清单：运行时接口",             // 来自接口时的文案
		"清单：内置",                // 内置兜底时的文案
		"能备份哪些应用、能备份到哪些介质服务器，由运行时接口给出", // 说明文字
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// ---------------------------------------------------------------------------
// 服务器巡检
// ---------------------------------------------------------------------------

// newTestHandlerWithInspector 让巡检走一个假连接器, 不真的连服务器。
func newTestHandlerWithInspector(t *testing.T, enabled bool, output string, dialErr error) http.Handler {
	t.Helper()
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	orchestrator := agent.New(llm.NewRegistry(cfg), agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	server := New(orchestrator, log.New(io.Discard, "", 0), "test")

	service := inspect.NewService(inspect.Options{
		StorePath: filepath.Join(t.TempDir(), "inspections.json"),
	})
	service.SetDialer(func(inspect.Target) (inspect.Runner, func(), error) {
		if dialErr != nil {
			return nil, func() {}, dialErr
		}
		return fakeInspectRunner{output: output}, func() {}, nil
	})
	server.SetInspector(service, enabled)
	return server.Handler()
}

type fakeInspectRunner struct{ output string }

func (f fakeInspectRunner) Run(string) (string, error) { return f.output, nil }
func (f fakeInspectRunner) HostKeyFingerprint() string { return "SHA256:test" }

// 假采集输出: 一台有问题的机器。
const inspectOutput = "__BRA_meta__\nPRETTY_NAME=\"Debian GNU/Linux 13\"\n6.1.0-13-amd64\nweb-01\nup 3 days\n" +
	"__BRA_cpu__\n18.0 12.0 8.0 3/500 9999\n---\n4\n" +
	"__BRA_memory__\nMemTotal: 1000000 kB\nMemAvailable: 40000 kB\nSwapTotal: 0 kB\n" +
	"__BRA_disk__\nFilesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100000 95000 5000 95% /\n---BLOCKDEV---\nsda 50G disk\n" +
	"__BRA_thermal__\nx86_pkg_temp=95000\n" +
	"__BRA_logs__\nSep 25 10:00:00 host kernel: Out of memory: Killed process 4321\n" +
	"__BRA_services__\n0 loaded units listed.\n" +
	"__BRA_end__\n"

func TestAPIInspectRunsChecks(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1"}, "user": "root", "password": "secret",
	})
	if status != http.StatusOK {
		t.Fatalf("巡检应成功: %d %v", status, payload)
	}
	if payload["total"] != float64(1) || payload["failed"] != float64(0) {
		t.Fatalf("统计不对: %v", payload)
	}

	reports, _ := payload["reports"].([]any)
	report, _ := reports[0].(map[string]any)
	if report["status"] != "critical" {
		t.Errorf("有 OOM 与 95%% 磁盘占用应判 critical: %v", report["status"])
	}
	if report["host_key_fingerprint"] != "SHA256:test" {
		t.Errorf("应带回主机指纹: %v", report["host_key_fingerprint"])
	}

	findings, _ := report["findings"].([]any)
	if len(findings) < 6 {
		t.Fatalf("应有 6 项检查: %d", len(findings))
	}
	byID := map[string]map[string]any{}
	for _, raw := range findings {
		item, _ := raw.(map[string]any)
		byID[item["id"].(string)] = item
	}
	for id, want := range map[string]string{
		"cpu": "critical", "memory": "critical", "disk": "critical",
		"thermal": "critical", "logs": "critical", "services": "ok",
	} {
		if got := byID[id]["status"]; got != want {
			t.Errorf("%s 应判 %q, 实际 %v (%v)", id, want, got, byID[id]["summary"])
		}
	}
}

// 巡检响应里绝不能出现密码。
func TestAPIInspectNeverEchoesPassword(t *testing.T) {
	const password = "sup3r-s3cret-passw0rd"
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	_, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1"}, "user": "root", "password": password,
	})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatal("响应里出现了密码")
	}

	// 历史里也不能有
	_, history := doJSON(t, handler, "GET", "/api/inspect", nil)
	raw, _ = json.Marshal(history)
	if strings.Contains(string(raw), password) {
		t.Fatal("巡检历史里出现了密码")
	}
}

func TestAPIInspectValidatesInput(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺主机", map[string]any{"user": "root", "password": "p"}},
		{"缺用户", map[string]any{"hosts": []string{"1.1.1.1"}, "password": "p"}},
		{"缺凭据", map[string]any{"hosts": []string{"1.1.1.1"}, "user": "root"}},
	}
	for _, entry := range cases {
		status, payload := doJSON(t, handler, "POST", "/api/inspect", entry.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: 应返回 400, 实际 %d", entry.name, status)
			continue
		}
		if payload["error"] == nil {
			t.Errorf("%s: 应返回 error", entry.name)
		}
	}
}

// 部分地址非法时不该静默丢弃: 合法的照常巡检, 被拒的要回报出来 ——
// 静默忽略会让用户以为"这些也巡检过了"。
func TestAPIInspectReportsRejectedHosts(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1", "bad$(id)"}, "user": "root", "password": "p",
	})
	if status != http.StatusOK {
		t.Fatalf("合法地址应照常巡检: %d %v", status, payload)
	}
	if payload["total"] != float64(1) {
		t.Errorf("只该巡检合法的那个: %v", payload["total"])
	}
	rejected, _ := payload["rejected"].([]any)
	if len(rejected) != 1 || rejected[0] != "bad$(id)" {
		t.Fatalf("被丢弃的地址必须回报: %v", payload["rejected"])
	}
}

// 非法主机被过滤后若一台不剩, 要明确报错而不是静默返回空结果。
func TestAPIInspectRejectsAllIllegalHosts(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)
	// 注意不要用 "a b" 之类: 按空格拆分后 "a"、"b" 本身是合法主机名,
	// 那样测的就不是"全部非法"了。
	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"a$(id)", "b`id`"}, "user": "root", "password": "p",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("全部非法时应返回 400, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if !strings.Contains(errorInfo["message"].(string), "不合法") {
		t.Errorf("报错应说明地址不合法: %v", errorInfo["message"])
	}
}

// 多台主机: 失败的不影响成功的, 且都进历史。
func TestAPIInspectMultipleHosts(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1,10.0.0.2\n10.0.0.3"}, "user": "root", "password": "p",
	})
	if status != http.StatusOK {
		t.Fatalf("应成功: %d", status)
	}
	if payload["total"] != float64(3) {
		t.Fatalf("三台都该被巡检: %v", payload["total"])
	}

	_, history := doJSON(t, handler, "GET", "/api/inspect", nil)
	if history["total"] != float64(3) {
		t.Errorf("三台都该进历史: %v", history["total"])
	}
	// 列表只给摘要, 不带原始输出 (列表可能很长)
	items, _ := history["reports"].([]any)
	item, _ := items[0].(map[string]any)
	if _, exists := item["findings"]; exists {
		t.Error("历史列表不该带完整 findings, 应只在详情里给")
	}
	if item["target"] == nil || item["summary"] == nil {
		t.Errorf("列表项缺少必要字段: %v", item)
	}
}

// 连接失败要如实报告, 而不是给一份"没有问题"的空报告。
func TestAPIInspectReportsConnectionFailure(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, "", errors.New("认证失败: 用户名、密码或密钥不正确"))

	_, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1"}, "user": "root", "password": "bad",
	})
	if payload["failed"] != float64(1) {
		t.Fatalf("应报告 1 台失败: %v", payload)
	}
	reports, _ := payload["reports"].([]any)
	report, _ := reports[0].(map[string]any)
	if report["error"] == nil || report["error"] == "" {
		t.Fatal("必须说明失败原因")
	}
	if report["status"] != "unknown" {
		t.Errorf("失败时状态应为 unknown: %v", report["status"])
	}
}

func TestAPIInspectDetailAndDelete(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1"}, "user": "root", "password": "p",
	})
	_, history := doJSON(t, handler, "GET", "/api/inspect", nil)
	items, _ := history["reports"].([]any)
	item, _ := items[0].(map[string]any)
	id := item["id"].(string)

	// 详情要带完整 findings
	status, detail := doJSON(t, handler, "GET", "/api/inspect/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("详情应 200: %d", status)
	}
	if findings, _ := detail["findings"].([]any); len(findings) == 0 {
		t.Error("详情应带完整检查项")
	}

	status, _ = doJSON(t, handler, "DELETE", "/api/inspect/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("删除应成功: %d", status)
	}
	status, _ = doJSON(t, handler, "GET", "/api/inspect/"+id, nil)
	if status != http.StatusNotFound {
		t.Errorf("删除后应 404: %d", status)
	}
}

// 关闭巡检时接口必须明确拒绝, 而不是静默不做事。
func TestAPIInspectDisabled(t *testing.T) {
	handler := newTestHandlerWithInspector(t, false, inspectOutput, nil)

	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.1"}, "user": "root", "password": "p",
	})
	if status != http.StatusConflict {
		t.Fatalf("应返回 409, 实际 %d", status)
	}
	errorInfo, _ := payload["error"].(map[string]any)
	if errorInfo["kind"] != "inspect_disabled" {
		t.Errorf("错误类型不符: %v", errorInfo)
	}
	if errorInfo["hint"] == "" {
		t.Error("应告诉用户如何开启")
	}
}

func TestIndexRendersInspectView(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`id="tabInspect"`,  // 视图切换
		`id="inspectView"`, // 巡检主区
		// 这三个输入控件由 JS 创建, 因此断言 id 字符串本身而不是 HTML 属性
		`"inspectHosts"`,         // 主机输入
		`"inspectPassword"`,      // 密码输入
		`"inspectKey"`,           // 私钥输入 (密码之外的更安全选项)
		"function renderReport(", // 报告渲染
		"密码只用于本次连接",              // 安全说明
		"主机指纹",                   // 指纹可见
		"/api/inspect",           // 接口
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 模型配好之后, /health 不该还在提示"将使用 demo 客户端"。
//
// 真实症状: 配置加载时确实没有可用 profile (key 还没填), 于是这条警告被记进
// 配置快照; 用户在网页里填好 key 之后, 快照依然被原样返回 —— 于是 /health
// 同时出现 mode=llm 与"将使用 demo 客户端"。用户没法判断到底在用哪个模型,
// 也就不会再信任模型设置面板。
func TestHealthWarningReflectsCurrentModelNotLoadTimeSnapshot(t *testing.T) {
	cfg := &config.Config{
		Server:         config.Server{MaxTurns: 10, PromptLimit: 20000, SessionTTLMin: 120},
		DefaultProfile: "demo",
		// 模拟"加载时没有任何可用 profile"的那份快照
		Warnings: []string{config.NoUsableProfileWarning},
	}
	store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
	if err != nil {
		t.Fatalf("打开运行态存储失败: %v", err)
	}
	cfg.Runtime = store
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	handler := New(orchestrator, log.New(io.Discard, "", 0), "test").Handler()

	// 还没配模型: 提示必须照旧出现 (这是真的)
	_, health := doJSON(t, handler, "GET", "/health", nil)
	if health["mode"] != "demo" {
		t.Fatalf("未配模型时应为 demo 模式: %v", health)
	}
	if !warningsContain(t, health, config.NoUsableProfileWarning) {
		t.Errorf("未配模型时应提示将退回 demo: %v", health["warnings"])
	}

	// 用户在网页里填好 key
	if status, payload := doJSON(t, handler, "POST", "/api/llm/profiles", map[string]any{
		"preset":   "deepseek",
		"api_key":  "sk-pasted-in-browser",
		"activate": true,
	}); status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, payload)
	}

	_, health = doJSON(t, handler, "GET", "/health", nil)
	if health["mode"] != "llm" {
		t.Fatalf("应切到真实模型: %v", health)
	}
	// 关键断言: 不能一边说在用真实模型, 一边说将退回 demo
	if warningsContain(t, health, config.NoUsableProfileWarning) {
		t.Errorf("模型已配好, 不该再提示将退回 demo: %v", health["warnings"])
	}
}

// warningsContain 判断 /health 响应的 warnings 里是否含某条。
func warningsContain(t *testing.T, health map[string]any, want string) bool {
	t.Helper()
	raw, ok := health["warnings"].([]any)
	if !ok {
		return false
	}
	for _, item := range raw {
		if text, ok := item.(string); ok && text == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 巡检报告导出成 PDF
// ---------------------------------------------------------------------------

// 巡检记录要能导出成 PDF, 而且导出的是**真的 PDF** (不是把 JSON 换个后缀)。
func TestAPIInspectionPDFExport(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)

	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.9"}, "user": "root", "password": "pw",
	})
	if status != http.StatusOK {
		t.Fatalf("巡检应成功: %d %v", status, payload)
	}
	reports, _ := payload["reports"].([]any)
	if len(reports) != 1 {
		t.Fatalf("应返回 1 份报告: %v", payload)
	}
	id, _ := reports[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("报告缺少 id: %v", reports[0])
	}

	request := httptest.NewRequest("GET", "/api/inspect/"+id+"/report.pdf", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("导出应返回 200, 实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/pdf" {
		t.Errorf("Content-Type 应为 application/pdf, 实际 %q", contentType)
	}
	body := recorder.Body.Bytes()
	if len(body) < 10000 {
		t.Fatalf("PDF 太小, 可能没画出内容: %d 字节", len(body))
	}
	if !strings.HasPrefix(string(body), "%PDF-1.4") {
		t.Errorf("不是 PDF 文件: %q", body[:min(20, len(body))])
	}
	if !strings.Contains(string(body), "%%EOF") {
		t.Error("PDF 没有正常收尾")
	}

	// 中文文件名要走 RFC 5987, 否则下载下来是乱码; 同时给 ASCII 兜底
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, "attachment;") {
		t.Errorf("应作为附件下载: %q", disposition)
	}
	if !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Errorf("中文文件名应使用 filename* 编码: %q", disposition)
	}
	if !strings.Contains(disposition, ".pdf") {
		t.Errorf("下载文件名应带 .pdf: %q", disposition)
	}
}

// 记录不存在时给 404, 而不是 500 或者一个空文件。
func TestAPIInspectionPDFUnknownID(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, nil)
	request := httptest.NewRequest("GET", "/api/inspect/insp-nope/report.pdf", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("不存在的记录应返回 404, 实际 %d", recorder.Code)
	}
}

// 巡检失败的报告也要能导出 (它同样是要归档的记录)。
func TestAPIInspectionPDFForFailedReport(t *testing.T) {
	handler := newTestHandlerWithInspector(t, true, inspectOutput, errors.New("认证失败: 用户名、密码或密钥不正确"))
	status, payload := doJSON(t, handler, "POST", "/api/inspect", map[string]any{
		"hosts": []string{"10.0.0.9"}, "user": "root", "password": "wrong",
	})
	if status != http.StatusOK {
		t.Fatalf("失败的巡检也应返回 200: %d %v", status, payload)
	}
	reports, _ := payload["reports"].([]any)
	report, _ := reports[0].(map[string]any)
	id, _ := report["id"].(string)
	if report["error"] == nil {
		t.Fatalf("应带失败原因: %v", report)
	}

	request := httptest.NewRequest("GET", "/api/inspect/"+id+"/report.pdf", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("失败报告也应能导出, 实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.HasPrefix(recorder.Body.String(), "%PDF-1.4") {
		t.Error("导出的不是 PDF")
	}
}

// 页面上要真的有导出入口, 并且指向正确的接口。
func TestIndexHasPDFExportEntry(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`function pdfLink(`,
		`/report.pdf`,
		`"导出 PDF"`,
		`a.pdf-link`,
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 界面要把影响与处置建议显示出来, 并标出结论来源。
func TestIndexRendersDiagnoses(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		"function renderDiagnoses(",
		`"影响与处置建议"`,
		`"影响：{0}"`, // 拼接改成模板 (英文语序不同)
		`"处置："`,
		`"规则库"`,           // 结论来源
		`"模型分析"`,          // 结论来源
		`.diagnoses`,      // 样式
		`report.analysis`, // 报告级说明
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 巡检表单要有"用模型分析日志"开关, 并把选择传给后端。
func TestIndexHasLogAnalysisToggle(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`"inspectAnalyze"`, // 开关
		`"用模型分析日志"`,        // 界面文案
		`analyze_logs: `,   // 传给后端
		`.check-inline`,    // 样式
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 模型输出可能带 markdown, 界面显示前要清掉 (网页不渲染它们, 会原样露出星号)。
func TestIndexStripsMarkdown(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if page := recorder.Body.String(); !strings.Contains(page, "function plainText(") {
		t.Error("界面缺少清理 markdown 的处理")
	}
}

// ---------------------------------------------------------------------------
// 内联脚本的语法体检
// ---------------------------------------------------------------------------

// 页面的内联脚本必须"至少在语法上成立"。
//
// 这条测试来自一次真实事故: 新增的知识库文案里有一个没转义的直引号
// (`会说"没有写"`), 于是整段脚本**一行都执行不了** —— 三个标签全部点不动,
// 而所有 Go 测试都是绿的: 断言的是"页面里有这个字符串", 字符串当然在。
// Go 里没有 JS 解析器, 所以这里只做能可靠判定、又几乎没有误报的检查:
// 字符串/模板/注释必须闭合, 且闭合的字符串后面不能紧跟标识符字符。
func TestInlineScriptIsSyntacticallySane(t *testing.T) {
	for index, script := range inlineScripts(t) {
		checkScriptSyntax(t, index, script)
	}
}

func checkScriptSyntax(t *testing.T, scriptIndex int, script string) {
	t.Helper()

	type state int
	const (
		normal state = iota
		inSingle
		inDouble
		inTemplate
		inLineComment
		inBlockComment
		inRegex
	)
	current := normal
	escaped := false
	inClass := false
	// lastSignificant 是最近一个"有意义的字符", 用来判断斜杠是正则开头还是除号
	lastSignificant := rune(0)

	runes := []rune(script)
	for index := 0; index < len(runes); index++ {
		r := runes[index]
		next := rune(0)
		if index+1 < len(runes) {
			next = runes[index+1]
		}

		switch current {
		case normal:
			switch {
			case r == '/' && next == '/':
				current = inLineComment
				index++
			case r == '/' && next == '*':
				current = inBlockComment
				index++
			case r == '/' && regexCanStartAfter(lastSignificant):
				// 正则字面量: 里面可能有引号 (如 /["<>]+/), 不识别就会误判成字符串
				current = inRegex
				inClass = false
			case r == '\'':
				current = inSingle
			case r == '"':
				current = inDouble
			case r == '`':
				current = inTemplate
			}
			if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
				lastSignificant = r
			}
		case inLineComment:
			if r == '\n' {
				current = normal
			}
		case inBlockComment:
			if r == '*' && next == '/' {
				current = normal
				index++
			}
		case inRegex:
			if escaped {
				escaped = false
				continue
			}
			switch {
			case r == '\\':
				escaped = true
			case r == '[':
				inClass = true
			case r == ']':
				inClass = false
			case r == '/' && !inClass:
				current = normal
			case r == '\n':
				// 正则不跨行: 走到这里说明判断错了, 当作除号继续
				current = normal
			}
		case inSingle, inDouble, inTemplate:
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if (current == inSingle && r == '\'') ||
				(current == inDouble && r == '"') ||
				(current == inTemplate && r == '`') {
				// 闭合后紧跟标识符字符 = 字符串里的引号没转义 (本次事故的形态)
				if canStartIdentifier(next) {
					line := lineOfRunes(runes[:index])
					t.Fatalf("第 %d 段脚本第 %d 行附近: 字符串结束后紧跟标识符字符 %q —— 多半是字符串里有个没转义的引号\n%s",
						scriptIndex+1, line, string(next), lineTextAt(script, line))
				}
				current = normal
			}
		}
	}
	switch current {
	case inSingle, inDouble, inTemplate:
		t.Fatalf("第 %d 段脚本里有没闭合的字符串 (整段脚本都会执行不了)", scriptIndex+1)
	case inBlockComment:
		t.Fatalf("第 %d 段脚本里有没闭合的块注释", scriptIndex+1)
	}
}

// regexCanStartAfter 判断斜杠是否可能是正则字面量的开头。
//
// 标准做法看前一个有意义字符: 运算符/左括号/逗号/冒号之后是正则, 标识符或右括号
// 之后是除号。判错的方向是"漏报"(把正则当除号), 不会造成误报。
func regexCanStartAfter(previous rune) bool {
	switch previous {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	return false
}

func canStartIdentifier(r rune) bool {
	if r == 0 {
		return false
	}
	if r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
		return true
	}
	// 中文/日文等非 ASCII 字母在 JS 里也是合法的标识符字符
	return r > 0x7F && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// lineOfRunes 数出前面已经过了多少行 (按 rune 切片, 不能用字节偏移)。
func lineOfRunes(before []rune) int {
	return strings.Count(string(before), "\n") + 1
}

func lineTextAt(text string, line int) string {
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[line-1])
}

// inlineScripts 取出页面里的每一段内联脚本 (页面有两段: <head> 里"首屏定主题"的小脚本
// 与应用主脚本)。
func inlineScripts(t *testing.T) []string {
	t.Helper()
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	var scripts []string
	rest := page
	for {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			break
		}
		rest = rest[start+len("<script>"):]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			break
		}
		scripts = append(scripts, rest[:end])
		rest = rest[end+len("</script>"):]
	}
	if len(scripts) == 0 {
		t.Fatal("页面里没有内联脚本")
	}
	return scripts
}

// inlineScript 返回应用主脚本 (最后一段), 供"只看业务代码"的断言使用。
func inlineScript(t *testing.T) string {
	t.Helper()
	scripts := inlineScripts(t)
	return scripts[len(scripts)-1]
}
