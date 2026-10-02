package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"backup-restore-agent/internal/agent"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/kb"
	"backup-restore-agent/internal/llm"
)

// newTestHandlerWithKB 让知识库走临时目录, 不碰真实数据。
func newTestHandlerWithKB(t *testing.T, expand bool) http.Handler {
	t.Helper()
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	server := New(orchestrator, log.New(io.Discard, "", 0), "test")
	base := kb.New(kb.Options{Path: filepath.Join(t.TempDir(), "kb.json")})
	server.SetKnowledge(base, expand)
	return server.Handler()
}

// newTestHandlerWithInbox 额外配置服务器导入目录。
func newTestHandlerWithInbox(t *testing.T, inbox string) http.Handler {
	t.Helper()
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	server := New(orchestrator, log.New(io.Discard, "", 0), "test")
	base := kb.New(kb.Options{
		Path:      filepath.Join(t.TempDir(), "kb.json"),
		ImportDir: inbox,
	})
	server.SetKnowledge(base, false)
	return server.Handler()
}

const kbDoc = `# ZFS 故障处置

## 校验失败

执行 zpool scrub 让 ZFS 修复可修复的块, 并确认备份不在同一个池上。
`

// 导入 → 列表 → 检索 → 删除 的完整链路。
func TestAPIKnowledgeBaseFlow(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)

	status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "zfs.md", "content": kbDoc,
	})
	if status != http.StatusOK {
		t.Fatalf("导入应成功: %d %v", status, payload)
	}
	imported := payload["imported"]
	if listLen(imported) != 1 {
		t.Fatalf("应导入 1 份: %v", payload)
	}
	document, _ := imported.([]any)[0].(map[string]any)
	id, _ := document["id"].(string)
	if id == "" || document["chunks"] == nil {
		t.Fatalf("返回的文档信息不完整: %v", document)
	}

	// 列表
	status, payload = doJSON(t, handler, "GET", "/api/kb", nil)
	if status != http.StatusOK {
		t.Fatalf("列表应成功: %d", status)
	}
	if listLen(payload["documents"]) != 1 {
		t.Errorf("应有 1 份文档: %v", payload["documents"])
	}
	stats, _ := payload["stats"].(map[string]any)
	if stats["chunks"] == nil {
		t.Error("概况里应有片段数")
	}

	// 只检索 (不调用模型)
	status, payload = doJSON(t, handler, "GET", "/api/kb/search?q=zpool+scrub", nil)
	if status != http.StatusOK {
		t.Fatalf("检索应成功: %d", status)
	}
	results := payload["results"]
	if listLen(results) == 0 {
		t.Fatalf("应检索到片段: %v", payload)
	}
	first, _ := results.([]any)[0].(map[string]any)
	if first["doc"] != "ZFS 故障处置" {
		t.Errorf("命中文档不对: %v", first["doc"])
	}
	if listLen(payload["terms"]) == 0 {
		t.Error("应返回实际用于检索的词元")
	}

	// 删除
	request := httptest.NewRequest("DELETE", "/api/kb/"+id, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("删除应成功: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, payload := doJSON(t, handler, "GET", "/api/kb/search?q=zpool", nil); listLen(payload["results"]) != 0 {
		t.Error("删除后不该还能检索到")
	}
}

// 没有模型时问答要退化成"只给片段", 而不是 500。
func TestAPIKnowledgeAskWithoutModel(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	if status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "zfs.md", "content": kbDoc,
	}); status != http.StatusOK {
		t.Fatalf("导入失败: %d %v", status, payload)
	}

	status, payload := doJSON(t, handler, "POST", "/api/kb/ask", map[string]any{
		"question": "zpool 校验失败怎么处理",
	})
	if status != http.StatusOK {
		t.Fatalf("问答应返回 200: %d %v", status, payload)
	}
	if payload["mode"] != "retrieval_only" {
		t.Errorf("没有模型时应为 retrieval_only, 实际 %v", payload["mode"])
	}
	if listLen(payload["citations"]) == 0 {
		t.Error("应给出检索到的片段")
	}
}

// 知识库里没有相关资料时如实说明, 而不是编一个答案。
func TestAPIKnowledgeAskNoMatch(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	if status, _ := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "zfs.md", "content": kbDoc,
	}); status != http.StatusOK {
		t.Fatal("导入失败")
	}
	_, payload := doJSON(t, handler, "POST", "/api/kb/ask", map[string]any{"question": "今天午饭吃什么"})
	if payload["mode"] != "no_match" {
		t.Errorf("应报无匹配, 实际 %v", payload["mode"])
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "没有找到") {
		t.Errorf("应说明没找到资料: %v", note)
	}
}

// 批量导入里的一份失败不该影响其它文档。
func TestAPIKnowledgeImportPartialFailure(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"documents": []map[string]string{
			{"name": "good.md", "content": kbDoc},
			{"name": "empty.md", "content": "   "},
			{"name": "good2.md", "content": "# 另一份\n\n内容。\n"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("部分成功时应返回 200: %d %v", status, payload)
	}
	if listLen(payload["imported"]) != 2 {
		t.Errorf("应导入 2 份: %v", payload["imported"])
	}
	if listLen(payload["failed"]) != 1 {
		t.Fatalf("应有 1 份失败: %v", payload["failed"])
	}
	entry, _ := payload["failed"].([]any)[0].(map[string]any)
	if entry["name"] != "empty.md" || entry["error"] == "" {
		t.Errorf("失败项要说清是哪份、为什么: %v", entry)
	}
}

// 参数错误要给出可读提示, 而不是 500。
func TestAPIKnowledgeValidation(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"导入空内容", "POST", "/api/kb/import", map[string]any{"name": "x.md"}, http.StatusBadRequest},
		{"检索缺参数", "GET", "/api/kb/search", nil, http.StatusBadRequest},
		{"提问为空", "POST", "/api/kb/ask", map[string]any{"question": "  "}, http.StatusBadRequest},
		{"删除不存在", "DELETE", "/api/kb/doc-nope", nil, http.StatusNotFound},
	}
	for _, entry := range cases {
		status, payload := doJSON(t, handler, entry.method, entry.path, entry.body)
		if status != entry.want {
			t.Errorf("%s: 期望 %d, 实际 %d (%v)", entry.name, entry.want, status, payload)
		}
	}
}

// 知识库关闭时接口要明确说"未启用", 而不是 404 或 500。
func TestAPIKnowledgeDisabled(t *testing.T) {
	handler := newTestHandler(t)
	for _, entry := range []struct{ method, path string }{
		{"GET", "/api/kb"}, {"POST", "/api/kb/import"}, {"GET", "/api/kb/search?q=x"},
		{"POST", "/api/kb/ask"},
	} {
		status, payload := doJSON(t, handler, entry.method, entry.path, map[string]any{"question": "x"})
		if status != http.StatusConflict {
			t.Errorf("%s %s 应返回 409, 实际 %d", entry.method, entry.path, status)
		}
		envelope, _ := payload["error"].(map[string]any)
		if envelope == nil || envelope["kind"] != "kb_disabled" {
			t.Errorf("%s %s 应说明未启用: %v", entry.method, entry.path, payload)
		}
	}
}

// 界面要有知识库入口。
func TestIndexRendersKnowledgeView(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`id="tabKB"`, `id="kbView"`, "/api/kb/import", "/api/kb/ask",
		`"导入文档"`, "function renderKnowledge", "function renderCitations",
		"仅检索模式",   // 没有模型时的提示
		"没找到相关资料", // 检索不到时的状态
		`"回答已引用"`, // 区分"检索到"与"真的引用了"
		`function plainText(`,
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 响应里不该把整份文档吐回去 (列表只要元信息)。
func TestAPIKnowledgeListOmitsContent(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	if status, _ := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "zfs.md", "content": kbDoc,
	}); status != http.StatusOK {
		t.Fatal("导入失败")
	}
	request := httptest.NewRequest("GET", "/api/kb", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if strings.Contains(body, "zpool scrub") {
		t.Error("列表不该包含文档正文 (只要元信息)")
	}
}

// listLen 安全地取数组长度 (JSON 里的 null 与 [] 都算 0)。
func listLen(value any) int {
	items, ok := value.([]any)
	if !ok {
		return 0
	}
	return len(items)
}

// 新接口的端到端: 记录、收件箱、按路径导入。
func TestAPIKnowledgeRecordsAndInbox(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "handbook.md"), []byte("# 运维手册\n\n## 换盘\n\n执行 zpool replace。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "notes.txt"), []byte("随手记。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	handler := newTestHandlerWithInbox(t, inbox)

	// 收件箱列表: 只列文本类
	status, payload := doJSON(t, handler, "GET", "/api/kb/inbox", nil)
	if status != http.StatusOK {
		t.Fatalf("列出导入目录应成功: %d %v", status, payload)
	}
	if listLen(payload["files"]) != 2 {
		t.Fatalf("应列出 2 个可导入文件 (排除 run.sh), 实际 %v", payload["files"])
	}
	if payload["dir"] != inbox {
		t.Errorf("应返回导入目录: %v", payload["dir"])
	}

	// 从目录导入全部
	status, payload = doJSON(t, handler, "POST", "/api/kb/import-path", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("按路径导入应成功: %d %v", status, payload)
	}
	if listLen(payload["imported"]) != 2 {
		t.Errorf("应导入 2 份: %v", payload["imported"])
	}
	records := payload["records"]
	if listLen(records) != 2 {
		t.Fatalf("应有 2 条记录: %v", records)
	}
	first, _ := records.([]any)[0].(map[string]any)
	if first["origin"] != "server" {
		t.Errorf("来源应记为服务器目录: %v", first["origin"])
	}
	if first["origin_label"] != "服务器目录" {
		t.Errorf("应带来源的中文标签: %v", first["origin_label"])
	}
	if first["ok"] != true || first["chunks"] == nil {
		t.Errorf("成功记录应带片段数: %v", first)
	}

	// 越界路径必须被拒 (这是"读服务器文件"能力的边界)
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "../../secret.md"} {
		if status, _ := doJSON(t, handler, "POST", "/api/kb/import-path",
			map[string]any{"paths": []string{bad}}); status == http.StatusOK {
			t.Errorf("%q 必须被拒绝", bad)
		}
	}
	// 非文本文件也要拒
	if status, payload := doJSON(t, handler, "POST", "/api/kb/import-path",
		map[string]any{"paths": []string{"run.sh"}}); status == http.StatusOK {
		t.Errorf("非文本文件必须被拒绝: %v", payload)
	}

	// 记录接口单独可用, 并可按时间倒序
	status, payload = doJSON(t, handler, "GET", "/api/kb/records", nil)
	if status != http.StatusOK {
		t.Fatalf("记录接口应成功: %d", status)
	}
	if listLen(payload["records"]) < 2 {
		t.Errorf("应有多条记录: %v", payload["records"])
	}

	// 清空记录不影响文档
	if status, payload := doJSON(t, handler, "DELETE", "/api/kb/records", nil); status != http.StatusOK {
		t.Fatalf("清空记录应成功: %d %v", status, payload)
	}
	_, payload = doJSON(t, handler, "GET", "/api/kb", nil)
	if listLen(payload["records"]) != 0 {
		t.Error("清空后不该还有记录")
	}
	if listLen(payload["documents"]) != 2 {
		t.Error("清空记录不该影响文档")
	}
}

// 未配置导入目录时, 相关接口要明确说"未配置", 而不是 500。
func TestAPIKnowledgeInboxDisabled(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	status, payload := doJSON(t, handler, "GET", "/api/kb/inbox", nil)
	if status != http.StatusConflict {
		t.Fatalf("未配置导入目录应返回 409, 实际 %d", status)
	}
	envelope, _ := payload["error"].(map[string]any)
	if envelope == nil || envelope["kind"] != "kb_no_inbox" {
		t.Errorf("应说明未配置导入目录: %v", payload)
	}
	if status, _ := doJSON(t, handler, "POST", "/api/kb/import-path", map[string]any{}); status == http.StatusOK {
		t.Error("未配置导入目录时按路径导入不该成功")
	}
}

// 导入记录要能区分"浏览器上传"与"粘贴": 排查"这份文档哪来的"时这是关键信息。
func TestAPIKnowledgeRecordsDistinguishOrigin(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	if status, _ := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"documents": []map[string]string{
			{"name": "uploaded.md", "content": "# 上传的\n\n内容。\n", "origin": "browser"},
			{"name": "pasted.md", "content": "# 粘贴的\n\n内容。\n", "origin": "paste"},
			{"name": "default.md", "content": "# 没写来源\n\n内容。\n"},
		},
	}); status != http.StatusOK {
		t.Fatal("导入失败")
	}
	_, payload := doJSON(t, handler, "GET", "/api/kb/records", nil)
	records, _ := payload["records"].([]any)
	got := map[string]string{}
	for _, item := range records {
		record, _ := item.(map[string]any)
		got[record["name"].(string)] = record["origin_label"].(string)
	}
	if got["uploaded.md"] != "浏览器上传" {
		t.Errorf("浏览器上传应如实记录: %v", got)
	}
	if got["pasted.md"] != "粘贴文本" {
		t.Errorf("粘贴应如实记录: %v", got)
	}
	if got["default.md"] != "浏览器上传" {
		t.Errorf("缺省来源按浏览器上传: %v", got)
	}
}

// 替换旧版本时, 记录要写明替换了谁。
func TestAPIKnowledgeRecordShowsReplacement(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	for index := 0; index < 2; index++ {
		if status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
			"name": "handbook.md", "content": "# 手册\n\n第" + string(rune('一'+index)) + "版内容。\n",
		}); status != http.StatusOK {
			t.Fatalf("第 %d 次导入失败: %v", index+1, payload)
		}
	}
	_, payload := doJSON(t, handler, "GET", "/api/kb/records", nil)
	records, _ := payload["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("应有 2 条记录, 实际 %d", len(records))
	}
	newest, _ := records[0].(map[string]any)
	if newest["replaced"] != "手册" {
		t.Errorf("最新记录应写明替换了《手册》: %v", newest)
	}
	oldest, _ := records[1].(map[string]any)
	if _, has := oldest["replaced"]; has {
		t.Errorf("首次导入不该有替换字段: %v", oldest)
	}
}

// 三个子页的关键元素都要真的进 DOM。
//
// 这条来自一个真实 bug: 记录页的两张卡片里, 记录卡建好了却从没被 appendChild ——
// 页面看着正常, 记录永远是空的。断言"函数里有这个字符串"是抓不到的, 所以这里
// 检查构建函数是否把节点交回给调用方 (返回值的形状)。
func TestKnowledgePageBuildersReturnSingleNode(t *testing.T) {
	script := inlineScript(t)
	cases := []struct {
		name string
		want []string
	}{
		{"提问页", []string{"function renderKBAskPage", "return card;"}},
		{"导入页", []string{"function renderKBImportPage", "return card;"}},
		{"文档与记录页", []string{"function renderKBDocsPage", "page.appendChild(docsCard)", "page.appendChild(recordCard)", "return page;"}},
	}
	for _, entry := range cases {
		start := strings.Index(script, entry.want[0])
		if start < 0 {
			t.Errorf("%s: 找不到 %s", entry.name, entry.want[0])
			continue
		}
		// 取到下一个顶层函数为止, 在这段里检查返回与挂载
		rest := script[start:]
		if end := strings.Index(rest[1:], "\n  function "); end > 0 {
			rest = rest[:end]
		}
		for _, fragment := range entry.want[1:] {
			if !strings.Contains(rest, fragment) {
				t.Errorf("%s: 缺少 %q (节点没进 DOM 时页面看着正常但内容永远是空的)", entry.name, fragment)
			}
		}
	}
}

// 导入页必须给"本机文件"提供真实入口: file input + 多选 + 接受文本类型。
func TestKnowledgeImportPageHasLocalFilePicker(t *testing.T) {
	script := inlineScript(t)
	for _, fragment := range []string{
		`fileInput.type = "file"`,
		`fileInput.multiple = true`,
		`fileInput.accept = ".md,.markdown,.txt`,
		`fileInput.addEventListener("change"`,
		`stageLocalFiles(`,
		// 外观是一个真按钮 (label 套隐藏的 file input), 不是一小段文字
		`el("label", "kb-pick primary")`,
		`"kb-pick-text"`,
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("本机文件导入缺少 %q", fragment)
		}
	}
}

// 粘贴 Markdown 的入口已按需求移除: 页面上不该再有它的输入框与按钮。
func TestKnowledgeImportHasNoPasteUI(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, gone := range []string{`"kbPasteText"`, `"kbPasteName"`, "粘贴 Markdown", "导入这段文本"} {
		if strings.Contains(page, gone) {
			t.Errorf("粘贴功能已取消, 页面里不该还有 %q", gone)
		}
	}
	// 服务端仍然接受该来源 (接口层不改), 但界面上不再提供
	if kb.OriginLabel(kb.OriginPaste) == "" {
		t.Error("接口层应保留来源标签 (脚本导入时仍可能用到)")
	}
}

// 已导入文档以表格呈现: 有表头、每行一份文档、行内可打开与删除。
func TestKnowledgeDocumentsUseTable(t *testing.T) {
	script := inlineScript(t)
	for _, fragment := range []string{
		`table.className = "kb-table"`,
		`table.id = "kbTable"`,
		`document.createElement("thead")`,
		`document.createElement("tbody")`,
		`el("th", column.cls, column.text)`,
		`openDocument(doc.id)`, // 行内可打开
		`"DELETE", "/api/kb/"`, // 行内可删除
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("文档表格缺少 %q", fragment)
		}
	}
	// 表头列: 文档 / 来源文件 / 片段 / 大小 / 字数 / 导入时间 / 操作
	for _, header := range []string{`"文档"`, `"来源文件"`, `"片段"`, `"大小"`, `"字数"`, `"导入时间"`, `"操作"`} {
		if !strings.Contains(script, header) {
			t.Errorf("表格缺少表头 %q", header)
		}
	}
}

// 被拒绝的路径不该进导入记录 (那是"请求被拒", 不是"一次导入尝试"),
// 否则用户的历史里会堆满别人试探性的路径。
func TestAPIKnowledgeRejectedPathLeavesNoRecord(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	handler := newTestHandlerWithInbox(t, inbox)

	for _, bad := range []string{"../../etc/passwd", "/etc/shadow", ".."} {
		if status, _ := doJSON(t, handler, "POST", "/api/kb/import-path",
			map[string]any{"paths": []string{bad}}); status == http.StatusOK {
			t.Fatalf("%q 必须被拒绝", bad)
		}
	}
	_, payload := doJSON(t, handler, "GET", "/api/kb/records", nil)
	if listLen(payload["records"]) != 0 {
		t.Errorf("被拒的路径不该留下记录: %v", payload["records"])
	}
}

// 导入接口要回带记录 (界面据此显示"替换了哪一份", 自检据此清理)。
func TestAPIImportReturnsRecords(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "handbook.md", "content": "# 手册\n\n第一版。\n",
	})
	if status != http.StatusOK {
		t.Fatalf("导入失败: %v", payload)
	}
	records := payload["records"]
	if listLen(records) != 1 {
		t.Fatalf("应回带 1 条记录: %v", records)
	}
	record, _ := records.([]any)[0].(map[string]any)
	if record["origin_label"] != "浏览器上传" {
		t.Errorf("记录要带来源标签: %v", record)
	}
	id, _ := record["id"].(string)
	if !strings.HasPrefix(id, "imp-") {
		t.Fatalf("记录编号应带 imp- 前缀 (便于清理时识别): %v", record)
	}

	// 再导一次: 记录里要写明替换了谁
	_, payload = doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "handbook.md", "content": "# 手册\n\n第二版。\n",
	})
	record, _ = payload["records"].([]any)[0].(map[string]any)
	if record["replaced"] != "手册" {
		t.Errorf("应记录替换了《手册》: %v", record)
	}
}

// 文档原文要能取回来 (界面里直接打开看的就是它)。
func TestAPIKnowledgeContent(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	content := "# 换盘流程\n\n## 步骤\n\n1. 确认 vdev 路径\n2. zpool replace\n"
	status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "swap.md", "content": content,
	})
	if status != http.StatusOK {
		t.Fatalf("导入失败: %v", payload)
	}
	document, _ := payload["imported"].([]any)[0].(map[string]any)
	id, _ := document["id"].(string)

	status, payload = doJSON(t, handler, "GET", "/api/kb/"+id+"/content", nil)
	if status != http.StatusOK {
		t.Fatalf("取原文应成功: %d %v", status, payload)
	}
	if payload["content"] != content {
		t.Errorf("原文应与导入时完全一致:\n得到 %q\n期望 %q", payload["content"], content)
	}
	if payload["has_content"] != true {
		t.Error("应标明保存了原文")
	}
	if payload["title"] != "换盘流程" || payload["source"] != "swap.md" {
		t.Errorf("元信息不对: %v", payload)
	}

	// 不存在的文档
	if status, _ := doJSON(t, handler, "GET", "/api/kb/doc-nope/content", nil); status != http.StatusNotFound {
		t.Errorf("不存在的文档应返回 404, 实际 %d", status)
	}
}

// 列表不带正文 (正文可能很大, 走单独的接口按需取)。
func TestAPIKnowledgeListHasNoContentButContentEndpointDoes(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	body := "# 大文档\n\n" + strings.Repeat("正文内容。", 200)
	if status, _ := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
		"name": "big.md", "content": body,
	}); status != http.StatusOK {
		t.Fatal("导入失败")
	}
	request := httptest.NewRequest("GET", "/api/kb", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if strings.Contains(recorder.Body.String(), "正文内容") {
		t.Error("列表不该带正文")
	}

	_, payload := doJSON(t, handler, "GET", "/api/kb", nil)
	document, _ := payload["documents"].([]any)[0].(map[string]any)
	id, _ := document["id"].(string)
	_, detail := doJSON(t, handler, "GET", "/api/kb/"+id+"/content", nil)
	if !strings.Contains(detail["content"].(string), "正文内容") {
		t.Error("原文接口应返回完整正文")
	}
}

// 重新导入后原文也要更新 (同名是替换语义)。
func TestAPIKnowledgeContentFollowsReimport(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	for _, content := range []string{"# 手册\n\n第一版。\n", "# 手册\n\n第二版。\n"} {
		if status, payload := doJSON(t, handler, "POST", "/api/kb/import", map[string]any{
			"name": "handbook.md", "content": content,
		}); status != http.StatusOK {
			t.Fatalf("导入失败: %v", payload)
		}
	}
	_, payload := doJSON(t, handler, "GET", "/api/kb", nil)
	document, _ := payload["documents"].([]any)[0].(map[string]any)
	id, _ := document["id"].(string)
	_, detail := doJSON(t, handler, "GET", "/api/kb/"+id+"/content", nil)
	if !strings.Contains(detail["content"].(string), "第二版") {
		t.Errorf("替换后原文应是新版本: %v", detail["content"])
	}
}

// 界面要有查看入口与渲染函数, 且渲染走"先转义再套标签"的安全路径。
func TestIndexHasDocumentViewer(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		`id="docDrawer"`, `id="docBody"`, `id="docTitle"`, `id="docCloseBtn"`,
		"function openDocument(", "function renderMarkdown(",
		"/content", // 取原文的接口
		// 安全: 渲染前先整体转义 (否则用户上传的文档里可以塞脚本)
		`replace(/&/g, "&amp;")`, `replace(/</g, "&lt;")`,
		".kb-doc-title", // 标题做成可点的按钮
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
	// 渲染函数里不该出现直接拼接未转义文本的写法 (粗略但有效的护栏)
	if strings.Contains(page, "container.innerHTML = text") {
		t.Error("Markdown 渲染不能直接插入原文")
	}
}

// 随部署预置的文档要在界面上有标记: 操作者得知道"这是产品带的、升级会跟着刷新"。
func TestIndexMarksSeededDocuments(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{`doc.seeded`, `i18n.t("预置")`, "随部署预置的文档"} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
}

// 预置文档的来源标签是"部署预置" (和"浏览器上传"区分开, 便于回答"这份文档哪来的")。
func TestSeededOriginLabel(t *testing.T) {
	handler := newTestHandlerWithKB(t, false)
	// 直接经由 kb 的 Seed 造一份预置文档
	base := kb.New(kb.Options{Path: filepath.Join(t.TempDir(), "seed.json")})
	seedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(seedDir, "bundled.md"), []byte("# 预置\n\n内容。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := base.Seed(seedDir); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if label := kb.OriginLabel(kb.OriginSeed); label != "部署预置" {
		t.Errorf("预置来源的标签应为\"部署预置\", 实际 %q", label)
	}
	_ = handler
}

// 同一个函数只能定义一次。
//
// 这条来自一次真实事故: 我用脚本替换整段函数时区间算错, 结果 `renderKBDocuments` 被
// 复制成两份 —— 页面照常能跑 (JS 允许重复声明, 后一个覆盖前一个), 但生效的是**旧的**
// 那份, 于是新做的表格视图怎么都不显示。只断言"脚本里有这个字符串"是抓不到的。
func TestKnowledgeFunctionsAreDefinedOnce(t *testing.T) {
	script := inlineScript(t)
	for _, name := range []string{
		"renderKBDocuments", "renderKBStats", "renderKnowledgeView", "renderKBImportPage",
		"renderKBDocsPage", "renderKBRecords", "renderCitations", "openDocument", "renderMarkdown",
		"stageLocalFiles", "applyI18n", "applyTheme", "setLang", "syncLangButtons", "renderHeader",
	} {
		definition := "function " + name + "("
		if count := strings.Count(script, definition); count != 1 {
			t.Errorf("%s 应只定义一次, 实际 %d 次 (重复声明时后一个生效, 旧版本会悄悄覆盖新的)", name, count)
		}
	}
}
