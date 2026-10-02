package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/schema"
)

// 语言处理。
//
// 语言是**每台浏览器**的偏好 (和主题一样), 所以真正的选择存在前端 localStorage 里,
// 每个 API 请求带 ?lang= 过来; config.toml 里的 server.language 只是"没有请求参数时
// 的默认值"。这样一个人切英文不会影响同事, 也不需要重启服务。

// langOf 取本次请求的语言: 请求参数优先, 否则用服务默认。
func (s *Server) langOf(r *http.Request) i18n.Lang {
	if r != nil {
		if value := strings.TrimSpace(r.URL.Query().Get("lang")); value != "" {
			return i18n.Normalize(value)
		}
	}
	return s.defaultLang
}

// t 按本次请求的语言翻译一条文案。
func (s *Server) t(r *http.Request, zh string) string {
	return i18n.T(s.langOf(r), zh)
}

// tf 按本次请求的语言翻译模板并格式化。
func (s *Server) tf(r *http.Request, format string, args ...any) string {
	return i18n.Tf(s.langOf(r), format, args...)
}

// errText 把服务层返回的错误翻译成当前语言。
//
// 服务层 (agent / inspect / kb) 的错误多为"模板 + 动态值" (例如"会话不存在或已过期: xxx"),
// 所以走前缀翻译: 词典里存模板, 动态部分原样保留。
func (s *Server) errText(r *http.Request, err error) string {
	if err == nil {
		return ""
	}
	return i18n.TranslatePrefix(s.langOf(r), err.Error())
}

// handleI18n 返回界面文案的对照表 (中文原文 → 译文)。
//
// 词典放在服务端而不是前端文件里: 同一批文案服务端也在用 (报错、状态标签、报告),
// 一份词典两边共享, 不会出现"界面上是英文、报错还是中文"的错位。
func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	lang := i18n.Normalize(r.URL.Query().Get("lang"))
	if lang == "" {
		lang = s.langOf(r)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lang":     string(lang),
		"messages": i18n.Messages(lang),
	})
}

// translatePayload 按当前语言重新呈现一个响应结构里的文案 (只翻词典里有的整句)。
//
// 走"序列化 → 遍历 JSON 树 → 写回"而不是反射结构体: 反射遍历 struct 会碰到
// time.Time 这种"字段全私有"的类型, 一不小心就把时间戳变成零值。JSON 树里只有
// map/slice/string/数字/布尔, 遍历是安全的。
//
// 用途是"用另一种语言重新呈现已存在的数据": 巡检报告是之前生成并存下来的, 检查项
// 名称、状态标签、规则库结论这些结构化文案都在词典里, 所以切换语言后老记录也能跟着变;
// 当时由模型生成的自由文本与日志原文不在词典里, 保持原样 —— 这是刻意的, 不能瞎改。
func translatePayload(lang i18n.Lang, payload any) any {
	if lang != i18n.EN {
		return payload
	}
	raw, err := schema.MarshalIndentNoEscape(payload)
	if err != nil {
		return payload
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return payload
	}
	return translateTree(lang, tree)
}

// translateTree 递归翻译 JSON 树里的字符串。
func translateTree(lang i18n.Lang, value any) any {
	switch typed := value.(type) {
	case string:
		return i18n.Translate(lang, typed)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = translateTree(lang, item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = translateTree(lang, item)
		}
		return out
	default:
		return value
	}
}
