// Package web 提供 HTTP 接口与内置网页。
//
// 零依赖: 路由用 Go 1.22 的 ServeMux 模式匹配 (支持 "POST /api/parse" 这种写法),
// 界面用 //go:embed 打进二进制, 因此交付物就是单个可执行文件。
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backup-restore-agent/internal/agent"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/kb"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

//go:embed index.html
var staticFS embed.FS

const maxBodyBytes = 1 << 20 // 1 MiB

// Server 是 HTTP 服务。
type Server struct {
	agent   *agent.Agent
	logger  *log.Logger
	version string

	inspector      *inspect.Service
	inspectEnabled bool
	// pdfFontPath 是自定义的拉丁字体 (空则用内置字体)。
	pdfFontPath string
	// knowledge 是知识库; nil 表示未启用。
	knowledge *kb.Base
	// kbExpand 是默认是否用模型扩展检索词。
	kbExpand bool
	// defaultLang 是没有 ?lang= 参数时的默认语言 (来自 config.toml)。
	defaultLang i18n.Lang
}

// SetInspector 注入巡检服务并决定是否开放该能力。
func (s *Server) SetInspector(service *inspect.Service, enabled bool) {
	s.inspector = service
	s.inspectEnabled = enabled
}

// SetPDFFont 设置导出 PDF 用的自定义字体 (空表示用内置字体)。
func (s *Server) SetPDFFont(path string) { s.pdfFontPath = path }

// SetDefaultLang 设置没有请求参数时的默认语言。
func (s *Server) SetDefaultLang(value i18n.Lang) { s.defaultLang = value }

// SetKnowledge 注入知识库; nil 表示不启用。
func (s *Server) SetKnowledge(base *kb.Base, expand bool) {
	s.knowledge = base
	s.kbExpand = expand
}

// New 构造服务。
func New(ag *agent.Agent, logger *log.Logger, version string) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{agent: ag, logger: logger, version: version, defaultLang: i18n.ZH}
}

// Handler 返回装配好中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/i18n", s.handleI18n)
	mux.HandleFunc("GET /api/drivers", s.handleDrivers)
	mux.HandleFunc("GET /api/catalog", s.handleCatalog)
	mux.HandleFunc("GET /api/inspect", s.handleListInspections)
	mux.HandleFunc("POST /api/inspect", s.handleInspect)
	mux.HandleFunc("GET /api/inspect/{id}", s.handleGetInspection)
	mux.HandleFunc("DELETE /api/inspect/{id}", s.handleDeleteInspection)
	// 报告导出成 PDF: 路径带 .pdf 后缀, 便于浏览器与下载工具识别
	mux.HandleFunc("GET /api/inspect/{id}/report.pdf", s.handleInspectionPDF)
	mux.HandleFunc("POST /api/catalog/refresh", s.handleRefreshCatalog)

	mux.HandleFunc("GET /api/llm/profiles", s.handleListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.handleSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{name}", s.handleDeleteProfile)
	mux.HandleFunc("PUT /api/llm/active", s.handleSetActive)
	mux.HandleFunc("POST /api/llm/test", s.handleTestLLM)

	mux.HandleFunc("POST /api/parse", s.handleParse)
	mux.HandleFunc("POST /api/clarify", s.handleClarify)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/reset", s.handleResetSession)
	mux.HandleFunc("POST /api/sessions/{id}/submit", s.handleSubmitSession)

	mux.HandleFunc("GET /api/kb", s.handleKBList)
	mux.HandleFunc("POST /api/kb/import", s.handleKBImport)
	mux.HandleFunc("DELETE /api/kb/{id}", s.handleKBDelete)
	mux.HandleFunc("GET /api/kb/{id}/content", s.handleKBContent)
	mux.HandleFunc("GET /api/kb/search", s.handleKBSearch)
	mux.HandleFunc("POST /api/kb/ask", s.handleKBAsk)
	mux.HandleFunc("GET /api/kb/records", s.handleKBRecords)
	mux.HandleFunc("DELETE /api/kb/records", s.handleKBRecordsClear)
	mux.HandleFunc("DELETE /api/kb/records/{id}", s.handleKBRecordDelete)
	mux.HandleFunc("GET /api/kb/inbox", s.handleKBInbox)
	mux.HandleFunc("POST /api/kb/import-path", s.handleKBImportPath)

	mux.HandleFunc("GET /", s.handleIndex)

	return s.recover(s.logRequests(mux))
}

// ---------------------------------------------------------------------------
// 业务接口
// ---------------------------------------------------------------------------

type parseRequest struct {
	Text    string `json:"text"`
	Kind    string `json:"kind"`
	Profile string `json:"profile"`
}

type clarifyRequest struct {
	SessionID string            `json:"session_id"`
	Answers   map[string]string `json:"answers"`
	// Text 是用户在对话里补充的自由文本 (与 Answers 二选一)。
	// 对话式界面主输入框走这条路: 用户往往是一句话补充多个信息,
	// 让他逐格填空既别扭也不现实。
	Text    string `json:"text"`
	Profile string `json:"profile"`
}

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) {
	var request parseRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "text 不能为空"), "", "")
		return
	}
	if request.Kind != "" && agent.NormalizeKind(request.Kind) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			`kind 只能是 "backup" 或 "restore"`, "", "")
		return
	}

	result, err := s.agent.Start(agent.Request{
		Text: request.Text, Kind: request.Kind, Profile: request.Profile,
	})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleClarify(w http.ResponseWriter, r *http.Request) {
	var request clarifyRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.SessionID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "session_id 不能为空"), "", "")
		return
	}
	hasAnswers := len(request.Answers) > 0
	hasText := strings.TrimSpace(request.Text) != ""
	if !hasAnswers && !hasText {
		writeError(w, http.StatusBadRequest, "invalid_request",
			s.t(r, "需要提供 answers (逐项回答) 或 text (补充说明)"), "", "")
		return
	}

	var result *schema.TurnResult
	var err error
	switch {
	case hasAnswers:
		result, err = s.agent.Answer(request.SessionID, request.Answers, request.Profile)
	case hasText:
		result, err = s.agent.Continue(request.SessionID, request.Text)
	}
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, ok := s.agent.SessionDetail(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", s.tf(r, "会话不存在或已过期: {0}", id), "", "")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleListSessions 返回侧栏用的历史会话列表。
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.agent.ListSessions(),
		"total":    s.agent.Store().Count(),
	})
}

// handleSubmitSession 把会话的计划定稿。
//
// MVP 不执行备份, 所以"提交"= 用户确认这份 JSON 就是最终要用的那份。
func (s *Server) handleSubmitSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	result, err := s.agent.Submit(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "not_ready", s.errText(r, err),
			s.t(r, "信息完整后才能提交"), "")
		return
	}
	s.logger.Printf("会话 %s 计划已定稿", id)
	writeJSON(w, http.StatusOK, result)
}

// handleDeleteSession 删除一条历史会话。
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.agent.DeleteSession(id) {
		writeError(w, http.StatusNotFound, "not_found", s.tf(r, "会话不存在: {0}", id), "", "")
		return
	}
	s.logger.Printf("删除会话 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	result, err := s.agent.Reset(r.PathValue("id"))
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// 模型设置
// ---------------------------------------------------------------------------

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	registry := s.agent.Registry()
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), map[string]any{
		"active":          registry.ActiveName(),
		"mode":            registry.Mode(),
		"profiles":        registry.ListProfiles(),
		"presets":         presetSummary(),
		"runtime_enabled": registry.RuntimeEnabled(),
		"runtime_path":    registry.RuntimePath(),
	}))
}

type setActiveRequest struct {
	Profile string `json:"profile"`
}

func (s *Server) handleSetActive(w http.ResponseWriter, r *http.Request) {
	var request setActiveRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Profile) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "profile 不能为空"), "", "")
		return
	}
	registry := s.agent.Registry()
	warning, err := registry.SetActive(request.Profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", s.errText(r, err),
			"可用 profile 见 GET /api/llm/profiles", "")
		return
	}
	s.logger.Printf("切换模型 profile -> %s", registry.ActiveName())
	writeJSON(w, http.StatusOK, map[string]any{
		"active":  registry.ActiveName(),
		"mode":    registry.Mode(),
		"warning": warning,
	})
}

// saveProfileRequest 是网页上"选厂商 + 填 Key"表单的提交体。
//
// APIKey 留空表示保留原有密钥 —— 界面不回显已保存的密钥, 因此编辑其它字段时
// 不该被迫重新粘贴一遍。
type saveProfileRequest struct {
	Name       string `json:"name"`
	Preset     string `json:"preset"`
	Provider   string `json:"provider"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIVersion string `json:"api_version"`
	Note       string `json:"note"`
	APIKey     string `json:"api_key"`
	Activate   *bool  `json:"activate"`
}

func (s *Server) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	var request saveProfileRequest
	if !decodeBody(w, r, &request) {
		return
	}
	registry := s.agent.Registry()
	if !registry.RuntimeEnabled() {
		writeError(w, http.StatusConflict, "ui_config_disabled",
			s.t(r, "网页改配置已关闭"), "请在 config.toml 里设置 server.allow_ui_config=true 后重启", "")
		return
	}

	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = suggestProfileName(registry, request.Preset)
	}

	entry := config.RuntimeProfile{
		Preset:     request.Preset,
		Provider:   request.Provider,
		BaseURL:    request.BaseURL,
		Model:      request.Model,
		APIVersion: request.APIVersion,
		Note:       request.Note,
		APIKey:     request.APIKey,
	}
	keyCleared, err := registry.AddProfile(name, entry)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", s.errText(r, err), "", "")
		return
	}

	// 保存后默认直接启用 —— 用户填 Key 的目的就是用起来
	result := map[string]any{"saved": name, "active": registry.ActiveName()}
	// 接入点变了就不会沿用旧密钥 (见 config.RuntimeStore.Put): 必须说清楚,
	// 否则用户会以为"改个地址而已", 直到调用模型才发现没 key。
	if keyCleared {
		result["key_cleared"] = true
		result["notice"] = s.t(r, "接入点已改变, 原密钥不再沿用, 请重新填写密钥")
	}
	// 默认不切换当前模型, 必须显式要求。
	//
	// 方向很重要: 若默认切换, 任何脚本化的保存 (包括部署自检) 都会把用户选定的
	// 模型抢走; 而删除那个临时配置时代码又会认为"删的是当前模型"从而清空选择,
	// 最终表现为用户的模型在每次部署后悄悄退回 demo。
	activate := request.Activate != nil && *request.Activate
	if activate {
		if warning, err := registry.SetActive(name); err != nil {
			result["activate_error"] = err.Error()
			// 保存成功但启用失败时必须留下痕迹: 否则用户只看到"已保存",
			// 以为配置生效了, 而实际仍跑在原来的模型上。
			s.logger.Printf("保存后启用 %s 失败: %v", name, err)
		} else {
			if warning != "" {
				result["warning"] = warning
			}
			s.logger.Printf("切换模型 profile -> %s (保存时自动启用)", name)
		}
	}
	result["active"] = registry.ActiveName()
	result["mode"] = registry.Mode()
	s.logger.Printf("保存模型配置 %s (preset=%s model=%s, key=%s)",
		name, entry.Preset, entry.Model, maskKey(entry.APIKey))
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	registry := s.agent.Registry()
	if err := registry.DeleteProfile(name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", s.errText(r, err), "", "")
		return
	}
	s.logger.Printf("删除模型配置 %s", name)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": name,
		"active":  registry.ActiveName(),
		"mode":    registry.Mode(),
	})
}

// suggestProfileName 依据 preset 生成一个不冲突的名字。
func suggestProfileName(registry *llm.Registry, preset string) string {
	base := strings.TrimSpace(preset)
	if base == "" {
		base = "custom"
	}
	base = strings.Map(func(char rune) rune {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '_', char == '-', char == '.':
			return char
		default:
			return '-'
		}
	}, base)

	existing := map[string]bool{}
	for _, item := range registry.ListProfiles() {
		if text, ok := item["name"].(string); ok {
			existing[text] = true
		}
	}
	if !existing[base] {
		return base
	}
	for index := 2; index < 100; index++ {
		candidate := fmt.Sprintf("%s-%d", base, index)
		if !existing[candidate] {
			return candidate
		}
	}
	return base
}

// maskKey 只保留长度信息, 任何情况下都不记录密钥内容。
func maskKey(key string) string {
	if key == "" {
		return "(未设置)"
	}
	return fmt.Sprintf("已设置(%d 字符)", len([]rune(key)))
}

type testLLMRequest struct {
	Profile string `json:"profile"`
	Deep    bool   `json:"deep"`
}

func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	// 同时支持 query 与 body, 便于 curl 直接调用
	request := testLLMRequest{
		Profile: r.URL.Query().Get("profile"),
		Deep:    parseBoolQuery(r.URL.Query().Get("deep")),
	}
	if r.Body != nil && r.ContentLength != 0 {
		var body testLLMRequest
		if !decodeBody(w, r, &body) {
			return
		}
		if body.Profile != "" {
			request.Profile = body.Profile
		}
		request.Deep = request.Deep || body.Deep
	}

	registry := s.agent.Registry()
	result := registry.Ping(request.Profile)
	if request.Deep {
		// 抽取冒烟: 真跑一次抽取, 区分"key 错"与"模型能力不足"
		result["smoke"] = agent.SmokeTest(registry, request.Profile, s.agent.Catalog())
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDrivers(w http.ResponseWriter, r *http.Request) {
	// 清单是"能备份哪些应用"的权威来源, 因此以它为准; 内置驱动作为补充列出,
	// 免得用户以为那些引擎消失了。
	cat := s.agent.Catalog()
	items := make([]map[string]any, 0, len(cat.Applications)+len(driver.All()))

	for _, app := range cat.Applications {
		slots := make([]map[string]any, 0, len(app.Options))
		for _, option := range app.Options {
			slots = append(slots, map[string]any{
				"field":    option.Field,
				"question": option.Label,
				"kind":     option.Kind,
				"required": option.IsRequired(),
				"choices":  option.Choices,
			})
		}
		items = append(items, map[string]any{
			"kind":        app.Kind,
			"display":     app.Display,
			"source_type": app.SourceType,
			"aliases":     app.Aliases,
			"note":        app.Note,
			"in_catalog":  true,
			"registered":  driver.Registered(app.Kind),
			"slots":       slots,
		})
	}

	for _, engine := range driver.All() {
		if _, known := cat.ApplicationFor(engine.Kind); known {
			continue
		}
		slots := make([]map[string]any, 0, len(engine.Slots))
		for _, slot := range engine.Slots {
			slots = append(slots, map[string]any{
				"field":    slot.Field,
				"question": slot.Question,
				"kind":     string(slot.Kind),
				"required": !slot.Optional,
				"choices":  slot.ChoiceLabels(),
			})
		}
		items = append(items, map[string]any{
			"kind":        engine.Kind,
			"display":     engine.Display,
			"source_type": engine.SourceType,
			"aliases":     engine.Aliases,
			"in_catalog":  false,
			"registered":  true,
			"slots":       slots,
		})
	}

	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), map[string]any{
		"drivers":        items,
		"catalog_source": cat.Source,
	}))
}

// handleCatalog 返回当前的运行时清单 (能备份哪些应用 / 能备份到哪些介质)。
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	cat := s.agent.Catalog()

	applications := make([]map[string]any, 0, len(cat.Applications))
	for _, app := range cat.Applications {
		fields := make([]string, 0, len(app.Options))
		for _, option := range app.Options {
			fields = append(fields, option.Field)
		}
		applications = append(applications, map[string]any{
			"kind":        app.Kind,
			"display":     app.Display,
			"source_type": app.SourceType,
			"aliases":     app.Aliases,
			"note":        app.Note,
			"options":     fields,
		})
	}

	mediaServers := make([]map[string]any, 0, len(cat.MediaServers))
	for _, server := range cat.MediaServers {
		mediaServers = append(mediaServers, map[string]any{
			"id":       server.ID,
			"name":     server.Name,
			"type":     server.Type,
			"path":     server.Path,
			"bucket":   server.Bucket,
			"region":   server.Region,
			"endpoint": server.Endpoint,
			"note":     server.Note,
			"display":  server.Display(),
		})
	}

	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), map[string]any{
		"source":        cat.Source,
		"from_api":      cat.FromAPI(),
		"warning":       cat.Warning,
		"fetched_at":    cat.FetchedAt.Format(time.RFC3339),
		"configured":    s.agent.CatalogProvider().Configured(),
		"applications":  applications,
		"media_servers": mediaServers,
	}))
}

// handleRefreshCatalog 忽略缓存强制重新查询清单接口。
func (s *Server) handleRefreshCatalog(w http.ResponseWriter, r *http.Request) {
	cat := s.agent.CatalogProvider().Refresh()
	s.logger.Printf("刷新运行时清单: 来源=%s 应用=%d 介质=%d",
		cat.Source, len(cat.Applications), len(cat.MediaServers))
	writeJSON(w, http.StatusOK, map[string]any{
		"source":        cat.Source,
		"from_api":      cat.FromAPI(),
		"warning":       cat.Warning,
		"applications":  len(cat.Applications),
		"media_servers": len(cat.MediaServers),
	})
}

// ---------------------------------------------------------------------------
// 健康检查与首页
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	registry := s.agent.Registry()
	cat := s.agent.Catalog()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"catalog": map[string]any{
			"source":        cat.Source,
			"from_api":      cat.FromAPI(),
			"applications":  len(cat.Applications),
			"media_servers": len(cat.MediaServers),
			"warning":       cat.Warning,
		},
		"version":        s.version,
		"mode":           registry.Mode(),
		"active_profile": registry.ActiveName(),
		"profiles":       len(registry.ListProfiles()),
		"drivers":        driver.Kinds(),
		"max_turns":      s.agent.MaxTurns(),
		"sessions":       s.agent.Store().Count(),
		"config_path":    registry.Config().Path,
		// 用 registry.Warnings() 而不是配置里那份快照: 网页上配好模型之后,
		// 加载时那条"将使用 demo 客户端"就不再成立了, 照抄会出现
		// "mode=llm 同时提示将退回 demo" 这种自相矛盾的响应
		"warnings": healthWarnings(registry),
	})
}

// healthWarnings 汇总"当前仍然成立"的配置类提示。
func healthWarnings(registry *llm.Registry) []string {
	warnings := registry.Warnings()
	if warning := inspect.FontWarning(); warning != "" {
		warnings = append(warnings, warning)
	}
	return warnings
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// ServeMux 的 "GET /" 会兜住所有未匹配路径, 这里明确回 404
		writeError(w, http.StatusNotFound, "not_found", "路径不存在: "+r.URL.Path, "", "")
		return
	}
	page, err := staticFS.ReadFile("index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "内置页面缺失", "", "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// ---------------------------------------------------------------------------
// 中间件与工具
// ---------------------------------------------------------------------------

func (s *Server) writeAgentError(w http.ResponseWriter, r *http.Request, err error) {
	lang := s.langOf(r)
	var extraction *agent.ExtractionError
	if errors.As(err, &extraction) {
		// 422: 请求本身没问题, 是模型输出不可用
		writeError(w, http.StatusUnprocessableEntity, "extraction_failed",
			extraction.Error(), i18n.T(lang, "可换用能力更强的模型, 或先切 demo 模式"), "")
		return
	}

	var llmErr *llm.Error
	if errors.As(err, &llmErr) {
		writeError(w, http.StatusBadGateway, string(llmErr.Kind),
			i18n.TranslatePrefix(lang, llmErr.Error()),
			i18n.TranslatePrefix(lang, llmErr.Hint()), llmErr.Detail)
		return
	}

	// 409 而不是 400: 这不是"请求写错了", 而是同一会话上一轮还没结束的时间冲突
	// (双击提交、两个标签页)。界面据此提示稍后再试, 而不是让用户以为自己填错了。
	if errors.Is(err, agent.ErrSessionBusy) {
		writeError(w, http.StatusConflict, "session_busy",
			i18n.TranslatePrefix(lang, err.Error()),
			i18n.T(lang, "等上一轮结束后再继续; 刷新页面可看到最新进展"), "")
		return
	}

	// 其余 (会话不存在、文本为空等) 归为请求问题
	writeError(w, http.StatusBadRequest, "invalid_request", i18n.TranslatePrefix(lang, err.Error()), "", "")
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		// 首页与健康检查太频繁, 不记日志
		if r.URL.Path == "/" || r.URL.Path == "/health" {
			return
		}
		s.logger.Printf("%s %s -> %d (%dms)", r.Method, r.URL.Path,
			recorder.status, time.Since(started).Milliseconds())
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Printf("panic 处理 %s %s: %v", r.Method, r.URL.Path, recovered)
				writeError(w, http.StatusInternalServerError, "internal",
					"服务内部错误", "请查看服务日志", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// decodeBody 解析请求体; 失败时已写好响应, 返回 false。
func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "缺少请求体", "", "")
		return false
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		message := i18n.TranslatePrefix(i18n.Normalize(r.URL.Query().Get("lang")), "请求体格式错误: "+err.Error())
		if errors.Is(err, io.EOF) {
			message = "请求体为空"
		}
		writeError(w, http.StatusBadRequest, "invalid_request", message, "", "")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := schema.MarshalIndentNoEscape(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"响应序列化失败"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, kind, message, hint, detail string) {
	payload := map[string]any{
		"error": map[string]string{
			"kind":    kind,
			"message": message,
			"hint":    hint,
			"detail":  detail,
		},
	}
	writeJSON(w, status, payload)
}

func parseBoolQuery(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return true // 带了参数就按 true 处理, "?deep" 也应生效
	}
	return parsed
}

// presetSummary 列出内置厂商预设, 供界面提示可选的 base_url / model 组合。
func presetSummary() []map[string]string {
	out := []map[string]string{}
	for _, name := range config.PresetNames() {
		preset, ok := config.FindPreset(name)
		if !ok {
			continue
		}
		out = append(out, map[string]string{
			"name":     preset.Name,
			"display":  preset.Display,
			"base_url": preset.BaseURL,
			"model":    preset.Model,
			"note":     preset.Note,
		})
	}
	return out
}
