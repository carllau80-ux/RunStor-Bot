package web

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"backup-restore-agent/internal/agent"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/kb"
	"backup-restore-agent/internal/llm"
)

var cjkPattern = regexp.MustCompile(`[\x{4e00}-\x{9fff}]`)

// 英文模式下, 响应里不该再出现中文。
//
// 这条守卫是这套 i18n 的"账本": 它把漏译的地方直接指出来 (哪个接口、哪段文案),
// 所以覆盖面是机械保证的, 不靠人记得。
func TestEnglishResponsesHaveNoChinese(t *testing.T) {
	handler := newTestHandlerI18n(t)

	// 先造点数据出来: 一次巡检 + 一次知识库导入 + 一次会话
	if status, payload := doJSON(t, handler, "POST", "/api/inspect?lang=en", map[string]any{
		"hosts": []string{"10.0.0.9"}, "user": "root", "password": "pw",
	}); status != http.StatusOK {
		t.Fatalf("巡检应成功: %d %v", status, payload)
	}
	if status, payload := doJSON(t, handler, "POST", "/api/kb/import?lang=en", map[string]any{
		"name": "runbook.md", "content": "# Runbook\n\n## Steps\n\n1. zpool scrub\n",
	}); status != http.StatusOK {
		t.Fatalf("导入应成功: %d %v", status, payload)
	}
	if status, payload := doJSON(t, handler, "POST", "/api/parse?lang=en", map[string]any{
		"text": "backup oracle every sunday",
	}); status != http.StatusOK {
		t.Fatalf("解析应成功: %d %v", status, payload)
	}

	endpoints := []struct {
		method string
		path   string
		body   any
	}{
		{"GET", "/health?lang=en", nil},
		{"GET", "/api/drivers?lang=en", nil},
		{"GET", "/api/catalog?lang=en", nil},
		{"GET", "/api/llm/profiles?lang=en", nil},
		{"GET", "/api/inspect?lang=en", nil},
		{"GET", "/api/kb?lang=en", nil},
		{"GET", "/api/kb/records?lang=en", nil},
		{"GET", "/api/sessions?lang=en", nil},
		// 错误路径也要翻: 参数校验是最常被看到的中文
		{"POST", "/api/parse?lang=en", map[string]any{"text": ""}},
		{"POST", "/api/inspect?lang=en", map[string]any{"hosts": []string{}}},
		{"POST", "/api/kb/ask?lang=en", map[string]any{"question": ""}},
		{"POST", "/api/clarify?lang=en", map[string]any{"session_id": "nope", "answers": map[string]string{"a": "b"}}},
		{"DELETE", "/api/sessions/nope?lang=en", nil},
		{"GET", "/api/inspect/nope?lang=en", nil},
	}

	for _, entry := range endpoints {
		status, payload := doJSON(t, handler, entry.method, entry.path, entry.body)
		// 数据里可能有中文 (例如导入的文档名), 所以只检查"我们自己的文案"所在的字段
		for _, field := range []string{"message", "hint", "detail", "warning", "warnings", "summary",
			"name", "question", "title", "note", "reason", "error",
			// 规则库的影响与处置建议: 报告里最长的文案块, 漏译了必须被这条抓住
			"impact", "actions", "samples", "heading", "origin_label"} {
			for _, text := range collectStrings(payload, field) {
				if cjkPattern.MatchString(text) {
					t.Errorf("%s %s → 字段 %s 仍是中文: %q (状态 %d)", entry.method, entry.path, field, text, status)
				}
			}
		}
	}
}

// newTestHandlerI18n 建一个"什么都能用"的服务: 巡检 + 知识库 + 可改模型配置。
func newTestHandlerI18n(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{Server: config.Server{MaxTurns: 10, PromptLimit: 20000}}
	store, err := config.OpenRuntime(filepath.Join(t.TempDir(), "runtime.json"))
	if err != nil {
		t.Fatalf("打开运行态存储失败: %v", err)
	}
	cfg.Runtime = store
	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{MaxTurns: 10, SessionTTL: time.Minute})
	server := New(orchestrator, log.New(io.Discard, "", 0), "test")
	service := inspect.NewService(inspect.Options{StorePath: filepath.Join(t.TempDir(), "inspections.json")})
	service.SetDialer(func(inspect.Target) (inspect.Runner, func(), error) {
		return fakeInspectRunner{output: inspectOutput}, func() {}, nil
	})
	server.SetInspector(service, true)
	base := kb.New(kb.Options{Path: filepath.Join(t.TempDir(), "kb.json")})
	server.SetKnowledge(base, false)
	return server.Handler()
}

// collectStrings 取出 JSON 树里某个字段的所有字符串值。
func collectStrings(value any, field string) []string {
	var out []string
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == field {
				if text, ok := item.(string); ok {
					out = append(out, text)
				}
			}
			out = append(out, collectStrings(item, field)...)
		}
	case []any:
		for _, item := range typed {
			out = append(out, collectStrings(item, field)...)
		}
	}
	return out
}

// 界面词典必须覆盖页面里出现的每一句中文文案。
//
// 反过来说: 页面新增文案时如果忘了配译文, 这条测试会点名指出来 —— 否则英文界面里
// 会静静混着中文, 只能靠人肉发现。
func TestUIMessagesCoverAllLiterals(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	messages := i18nMessagesForTest(t, handler)

	// 页面里含中文的字符串字面量与静态文本
	candidates := map[string]bool{}
	for _, literal := range chineseLiterals(page) {
		candidates[literal] = true
	}
	missing := []string{}
	for literal := range candidates {
		if _, ok := messages[literal]; !ok {
			// 词典里可能存的是带前后空白的那份, 两边都试
			if _, ok := messages[" "+literal]; !ok {
				if _, ok := messages[literal+" "]; !ok {
					missing = append(missing, literal)
				}
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("有 %d 条界面文案没有英文译文 (英文界面下会显示中文):", len(missing))
		for _, item := range missing {
			t.Errorf("    %q", item)
		}
	}
}

// chineseLiterals 粗略提取页面里"像给人看的"中文片段。
//
// 先剔掉注释与样式: 那里的中文是写给维护者看的, 不会出现在界面上 (CSS 注释、HTML 注释)。
func chineseLiterals(page string) []string {
	if start := strings.Index(page, "<style>"); start >= 0 {
		if end := strings.Index(page, "</style>"); end > start {
			page = page[:start] + page[end:]
		}
	}
	for {
		start := strings.Index(page, "<!--")
		if start < 0 {
			break
		}
		end := strings.Index(page[start:], "-->")
		if end < 0 {
			break
		}
		page = page[:start] + page[start+end+3:]
	}
	out := []string{}
	// 静态文本: >中文<
	for _, match := range regexp.MustCompile(`>([^<>{}]*[\x{4e00}-\x{9fff}][^<>{}]*)<`).FindAllStringSubmatch(page, -1) {
		out = append(out, strings.TrimSpace(match[1]))
	}
	// 属性里的文案
	for _, match := range regexp.MustCompile(`(?:placeholder|title)="([^"]*[\x{4e00}-\x{9fff}][^"]*)"`).FindAllStringSubmatch(page, -1) {
		out = append(out, strings.TrimSpace(match[1]))
	}
	// JS 字符串字面量 (排除注释行)
	for _, line := range strings.Split(page, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		for _, match := range regexp.MustCompile(`"([^"\\]*[\x{4e00}-\x{9fff}][^"\\]*)"`).FindAllStringSubmatch(line, -1) {
			out = append(out, strings.TrimSpace(match[1]))
		}
	}
	filtered := out[:0]
	for _, item := range out {
		if len([]rune(item)) < 2 {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// i18nMessagesForTest 取服务端词典 (就是界面词典的来源)。
func i18nMessagesForTest(t *testing.T, handler http.Handler) map[string]string {
	t.Helper()
	status, payload := doJSON(t, handler, "GET", "/api/i18n?lang=en", nil)
	if status != http.StatusOK {
		t.Fatalf("词典接口应返回 200: %d", status)
	}
	raw, _ := payload["messages"].(map[string]any)
	out := map[string]string{}
	for key, value := range raw {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}
	if len(out) < 100 {
		t.Fatalf("词典条目太少 (%d), 接口可能没在工作", len(out))
	}
	return out
}

// 主题: 两套调色板都要定义同一批语义变量。
//
// 只写 var(--xxx) 才能跟着主题走; 少定义一个变量, 那个地方在另一套主题下就会显示成
// 继承来的颜色 (常常是"看不见的黑字"), 而这类问题只有真的切到浅色才看得出来。
func TestThemesDefineSameVariables(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	style := ""
	if start := strings.Index(page, "<style>"); start >= 0 {
		if end := strings.Index(page, "</style>"); end > start {
			style = page[start:end]
		}
	}
	if style == "" {
		t.Fatal("页面里没有样式块")
	}

	parseBlock := func(marker string) map[string]string {
		vars := map[string]string{}
		index := strings.Index(style, marker)
		if index < 0 {
			return vars
		}
		block := style[index:]
		if end := strings.Index(block, "\n  }"); end > 0 {
			block = block[:end]
		}
		for _, match := range regexp.MustCompile(`(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(block, -1) {
			vars[match[1]] = ""
		}
		return vars
	}

	light := parseBlock(`:root, :root[data-theme="light"]`)
	dark := parseBlock(`:root[data-theme="dark"]`)
	if len(light) < 15 || len(dark) < 15 {
		t.Fatalf("两套调色板变量太少: light=%d dark=%d", len(light), len(dark))
	}
	for name := range light {
		if _, ok := dark[name]; !ok {
			t.Errorf("深色主题缺少变量 %s (浅色下有定义)", name)
		}
	}
	for name := range dark {
		if _, ok := light[name]; !ok {
			t.Errorf("浅色主题缺少变量 %s (深色下有定义)", name)
		}
	}
}

// 界面要有主题与语言切换, 并且首屏就要定好主题 (否则会闪一下另一套配色)。
func TestIndexHasThemeAndLanguageControls(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	for _, fragment := range []string{
		// 主题三个图标按钮 + 语言两个按钮 (图标化: 不再用下拉)
		`id="themeAuto"`, `id="themeLight"`, `id="themeDark"`, `data-theme-choice="auto"`,
		`data-theme-choice="light"`, `data-theme-choice="dark"`,
		`id="langZh"`, `id="langEn"`, `data-lang-choice="zh"`, `data-lang-choice="en"`,
		"runstor-theme", "runstor-lang",
		"function applyTheme(", "function setTheme(", "function setLang(", "function applyI18n(",
		"function syncLangButtons(", "prefers-color-scheme", // 跟随系统
		`class="seg-btn"`, // 分段按钮样式
	} {
		if !strings.Contains(page, fragment) {
			t.Errorf("页面缺少 %q", fragment)
		}
	}
	// 首屏定主题的脚本必须在 <style> 之前 (否则先渲染一帧再换色)
	themeScript := strings.Index(page, "runstor-theme")
	styleStart := strings.Index(page, "<style>")
	if themeScript < 0 || styleStart < 0 || themeScript > styleStart {
		t.Error("首屏定主题的脚本应在样式之前")
	}
}

// 模型 / 主题 / 语言这三样必须放在顶栏里。
//
// 它们原来在左侧栏的顶部 —— 窄屏 (<=720px) 时侧栏变成抽屉滑出屏幕, 于是"想切个语言"
// 得先打开汉堡菜单, 够不着。顶栏在所有视图、所有宽度下都在。
func TestTopBarHoldsGlobalControls(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest("GET", "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	page := recorder.Body.String()

	topbarStart := strings.Index(page, `class="topbar"`)
	if topbarStart < 0 {
		t.Fatal("页面里没有顶栏")
	}
	// 顶栏的结束位置: 下一个顶层结构 (app-body) 之前
	topbarEnd := strings.Index(page[topbarStart:], `class="app-body"`)
	if topbarEnd < 0 {
		t.Fatal("顶栏之后应有 app-body")
	}
	topbar := page[topbarStart : topbarStart+topbarEnd]

	for _, control := range []string{`id="settingsBtn"`, `id="themeAuto"`, `id="themeLight"`,
		`id="themeDark"`, `id="langZh"`, `id="langEn"`, `id="modelPillName"`} {
		if !strings.Contains(topbar, control) {
			t.Errorf("顶栏里缺少 %q (放到侧栏的话窄屏就够不着了)", control)
		}
	}
	// 侧栏里不该再留一份, 否则 `$()` 可能取到被抽屉挡住的那个
	sidebarStart := strings.Index(page, `id="sidebar"`)
	sidebarEnd := strings.Index(page[sidebarStart:], "</aside>")
	sidebar := page[sidebarStart : sidebarStart+sidebarEnd]
	for _, control := range []string{`id="settingsBtn"`, `id="themeAuto"`, `id="langZh"`} {
		if strings.Contains(sidebar, control) {
			t.Errorf("侧栏里不该再有 %q (会与顶栏重复)", control)
		}
	}
}
