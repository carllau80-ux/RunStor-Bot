package web

import (
	"errors"
	"net/http"
	"strings"

	"backup-restore-agent/internal/kb"
)

// 知识库的接口。
//
// 设计上刻意把"检索"与"问答"分开: /api/kb/search 只做检索, /api/kb/ask 才调用模型。
// 用户问不到东西时, 第一件事是看检索命中了什么 —— 能直接看到词元与命中片段, 才知道
// 该换个说法还是该补文档。

// kbImportRequest 是导入请求。
type kbImportRequest struct {
	// Documents 支持一次导入多份 (浏览器里多选文件)。
	Documents []kbDocument `json:"documents"`
	// Name/Content 也支持单份的简写形式。
	Name    string `json:"name"`
	Content string `json:"content"`
}

type kbDocument struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	// Origin 区分"浏览器选文件"与"粘贴文本"; 缺省按浏览器上传记。
	Origin string `json:"origin"`
}

func (d kbDocument) origin() string {
	if d.Origin == kb.OriginPaste || d.Origin == kb.OriginBrowser || d.Origin == kb.OriginServer {
		return d.Origin
	}
	return kb.OriginBrowser
}

// handleKBList 返回知识库概况与文档列表。
func (s *Server) handleKBList(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"),
			"请在 config.toml 里设置 server.kb.enabled=true 后重启", "")
		return
	}
	// 列表只给元信息: 正文可能很大 (单份上限 2 MB), 塞进列表会让界面每次刷新都很重。
	// 要正文走 GET /api/kb/{id}/content。
	documents := s.knowledge.List()
	for index := range documents {
		documents[index].Content = ""
	}
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), map[string]any{
		"stats":     s.knowledge.Stats(),
		"documents": documents,
		"records":   kbRecordViews(s.knowledge.Records(0)),
	}))
}

// handleKBImport 导入 Markdown 文档。
func (s *Server) handleKBImport(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	var request kbImportRequest
	if !decodeBody(w, r, &request) {
		return
	}

	documents := request.Documents
	if len(documents) == 0 && strings.TrimSpace(request.Content) != "" {
		documents = []kbDocument{{Name: request.Name, Content: request.Content}}
	}
	if len(documents) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "没有可导入的内容"),
			"请提供 documents 数组或 name+content", "")
		return
	}
	if len(documents) > 50 {
		writeError(w, http.StatusBadRequest, "invalid_request", "单次最多导入 50 份文档", "", "")
		return
	}

	imported := make([]kb.Document, 0, len(documents))
	failed := make([]map[string]string, 0)
	created := make([]kb.ImportRecord, 0, len(documents))
	for _, document := range documents {
		saved, record, err := s.knowledge.ImportFrom(document.Name, document.Content, document.origin())
		created = append(created, record)
		if err != nil {
			// 逐份报告成败: 一次导入十份时, 不能因为一份超限就整批失败
			failed = append(failed, map[string]string{"name": document.Name, "error": err.Error()})
			continue
		}
		imported = append(imported, saved)
	}

	status := http.StatusOK
	if len(imported) == 0 && len(failed) > 0 {
		status = http.StatusBadRequest
	}
	documentsCount, chunks := s.knowledge.Count()
	writeJSON(w, status, map[string]any{
		"imported": imported,
		"failed":   failed,
		// 记录也回给调用方: 界面要显示"替换了哪一份", 部署自检要能清掉自己留下的记录
		"records":   kbRecordViews(created),
		"documents": documentsCount,
		"chunks":    chunks,
	})
}

// handleKBDelete 删除一份文档。
func (s *Server) handleKBDelete(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	id := r.PathValue("id")
	if !s.knowledge.Delete(id) {
		writeError(w, http.StatusNotFound, "not_found", "文档不存在: "+id, "", "")
		return
	}
	documentsCount, chunks := s.knowledge.Count()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "documents": documentsCount, "chunks": chunks})
}

// handleKBSearch 只做检索 (不调用模型), 便于排查"为什么没搜到"。
func (s *Server) handleKBSearch(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	question := strings.TrimSpace(r.URL.Query().Get("q"))
	if question == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "请提供查询词 q"), "", "")
		return
	}
	topK := parseTopK(r.URL.Query().Get("top_k"))
	results := s.knowledge.Search(question, topK)
	if results == nil {
		// 空结果给 [] 而不是 null: 客户端不必为"没有结果"单独写分支
		results = []kb.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"question": question,
		"terms":    kb.Terms(question),
		"results":  results,
	})
}

// handleKBAsk 用知识库回答问题 (检索 + 模型生成 + 引用)。
func (s *Server) handleKBAsk(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	var request struct {
		Question string `json:"question"`
		TopK     int    `json:"top_k"`
		// Expand 覆盖服务默认的检索词扩展开关。
		Expand *bool `json:"expand"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Question) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "请输入问题"), "", "")
		return
	}

	expand := s.kbExpand
	if request.Expand != nil {
		expand = *request.Expand
	}
	registry := s.agent.Registry()
	answer := s.knowledge.Ask(kb.AskOptions{
		Question: request.Question,
		TopK:     request.TopK,
		Expand:   expand,
		Lang:     s.langOf(r),
	}, registry.ActiveClient(), registry.ActiveName())

	writeJSON(w, http.StatusOK, answer)
}

// handleKBContent 返回文档原文, 供界面里直接打开查看。
func (s *Server) handleKBContent(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	id := r.PathValue("id")
	document, content, ok := s.knowledge.Content(id)
	if document.ID == "" {
		writeError(w, http.StatusNotFound, "not_found", s.tf(r, "文档不存在: {0}", id), "", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       document.ID,
		"title":    document.Title,
		"source":   document.Source,
		"added_at": document.AddedAt,
		"bytes":    document.Bytes,
		"chunks":   document.Chunks,
		"format":   "markdown",
		// has_content=false 表示这份文档是早先版本导入的, 没保存原文
		"has_content": ok,
		"content":     content,
	})
}

// handleKBRecords 只返回导入记录 (界面里的"记录"页单独拉)。
func (s *Server) handleKBRecords(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r),
		map[string]any{"records": kbRecordViews(s.knowledge.Records(0))}))
}

// handleKBRecordsClear 清空导入记录 (文档不受影响)。
func (s *Server) handleKBRecordsClear(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	removed := s.knowledge.ClearRecords()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": removed})
}

// handleKBRecordDelete 删除一条导入记录。
func (s *Server) handleKBRecordDelete(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	id := r.PathValue("id")
	if !s.knowledge.DeleteRecord(id) {
		writeError(w, http.StatusNotFound, "not_found", "记录不存在: "+id, "", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleKBInbox 列出服务器导入目录里的候选文件 (导入前先看一眼)。
func (s *Server) handleKBInbox(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	files, err := s.knowledge.ListInbox()
	if err != nil {
		if errors.Is(err, kb.ErrNoImportDir) {
			writeError(w, http.StatusConflict, "kb_no_inbox",
				s.t(r, "未配置服务器导入目录"), s.t(r, "在 config.toml 的 server.kb.import_dir 指定目录后重启"), "")
			return
		}
		writeError(w, http.StatusInternalServerError, "kb_inbox_failed", err.Error(), "", "")
		return
	}
	stats := s.knowledge.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":      stats["import_dir"],
		"files":    files,
		"suffixes": ".md / .markdown / .txt",
	})
}

// handleKBImportPath 从服务器目录导入文件。
//
// 路径只能是导入目录内的相对路径 (见 kb.resolvedPathIn): 这个接口是"读服务器上的文件",
// 放开路径就等于给出任意文件读取。
func (s *Server) handleKBImportPath(w http.ResponseWriter, r *http.Request) {
	if s.knowledge == nil {
		writeError(w, http.StatusConflict, "kb_disabled", s.t(r, "知识库未启用"), "", "")
		return
	}
	var request struct {
		// Paths 是导入目录内的相对路径; 为空表示导入目录下全部可导入文件。
		Paths []string `json:"paths"`
	}
	if !decodeBody(w, r, &request) {
		return
	}

	documents, records, err := s.knowledge.ImportFromInbox(request.Paths)
	if err != nil {
		// 一份都没成功: 把逐条原因带回去, 而不是只说"失败"
		writeError(w, http.StatusBadRequest, "kb_import_failed", err.Error(),
			"只支持导入目录内的 .md / .markdown / .txt 文件", "")
		return
	}
	counted, chunks := s.knowledge.Count()
	writeJSON(w, http.StatusOK, map[string]any{
		"imported":  documents,
		"records":   kbRecordViews(records),
		"documents": counted,
		"chunks":    chunks,
	})
}

// kbRecordView 是给界面的导入记录: 在原记录上加一个来源的可读名称。
//
// 标签在服务端拼 (kb.OriginLabel), 不写进落盘: 落盘里存的是 origin 代码, 显示文案
// 改了不必迁移数据。
type kbRecordView struct {
	kb.ImportRecord
	OriginLabel string `json:"origin_label"`
}

func kbRecordViews(records []kb.ImportRecord) []kbRecordView {
	out := make([]kbRecordView, 0, len(records))
	for _, record := range records {
		out = append(out, kbRecordView{ImportRecord: record, OriginLabel: kb.OriginLabel(record.Origin)})
	}
	return out
}

func parseTopK(raw string) int {
	value := 0
	for _, r := range strings.TrimSpace(raw) {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
		// 上限以 kb.MaxTopK 为唯一出处 (配置与检索实现也用同一个值)
		if value > kb.MaxTopK {
			return kb.MaxTopK
		}
	}
	return value
}
