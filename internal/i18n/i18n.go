// Package i18n 提供中英文两套文案。
//
// 设计取舍: **用中文原文当键**。
//
//   - 代码里照旧写中文 (i18n.T(lang, "服务器巡检"))，不必先在脑子里翻译成 key，
//     也不会出现 "inspect.title" 这种和界面文案对不上的间接层;
//   - 漏译时自然退化成原文 (而不是空白或 "missing key"), 不会把界面弄坏;
//   - 翻译表就是"中文 → 英文"的对照表, 谁能看懂中文就能校对, 不需要开发参与。
//
// 代价是中文文案改了要同步改翻译表的键 —— 用测试盯着 (见 TestTranslationsCoverSources):
// 它会扫出代码里出现但表里没有的文案, 漏译不会静默存在。
package i18n

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Lang 是界面语言。
type Lang string

const (
	// ZH 中文 (默认)。
	ZH Lang = "zh"
	// EN 英文。
	EN Lang = "en"
)

// Parse 归一语言写法, 并报告是否被识别。
//
// Normalize 是宽松版 (识别不了就当中文), 适合"请求参数写错了也别把界面弄坏"的场景;
// 需要 fail-fast 的地方 (配置里 server.language 打成了 jp) 用这个 —— 否则打错的值会
// 静默退回中文, 症状是"我明明配了英文"却没人发现。
func Parse(value string) (Lang, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "en", "en-us", "en_us", "english", "英文":
		return EN, true
	case "zh", "zh-cn", "zh_cn", "cn", "chinese", "中文", "简体中文":
		return ZH, true
	default:
		return ZH, false
	}
}

// Normalize 把各种写法归一成支持的语言 (不支持的一律回中文)。
func Normalize(value string) Lang {
	lang, _ := Parse(value)
	return lang
}

// T 翻译一条文案。找不到英文时返回中文原文。
//
// 查找容忍首尾空白: 代码里为了拼接常常写成 "· {0} 字" 或 " 吗？…", 词典里存的是
// 干净的那份 —— 差一个空格就静默不翻, 这种错很难靠人发现, 所以在这里直接抹平。
func T(lang Lang, zh string) string {
	if lang != EN {
		return zh
	}
	if en, ok := lookup(english, zh); ok {
		return en
	}
	return zh
}

// lookup 先按原文查, 再按去掉首尾空白的形式查。
func lookup(table map[string]string, key string) (string, bool) {
	if en, ok := table[key]; ok {
		return en, true
	}
	trimmed := strings.TrimSpace(key)
	if trimmed == key {
		return "", false
	}
	if en, ok := table[trimmed]; ok {
		return en, true
	}
	return "", false
}

// Tf 翻译并格式化 (先查译文再 Sprintf, 这样英文可以调整语序)。
func Tf(lang Lang, format string, args ...any) string {
	translated := format
	if lang == EN {
		if en, ok := lookup(english, format); ok {
			translated = en
		}
	}
	if len(args) == 0 {
		return translated
	}
	return fmt.Sprintf(translated, args...)
}

// Ts 翻译一组文案 (列表、选项等)。
func Ts(lang Lang, items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, T(lang, item))
	}
	return out
}

// Messages 返回中文→译文的完整对照表 (给前端用)。
func Messages(lang Lang) map[string]string {
	if lang != EN {
		return map[string]string{}
	}
	out := make(map[string]string, len(english))
	for zh, en := range english {
		out[zh] = en
	}
	return out
}

// TranslatePrefix 翻译"模板 + 动态值"的文案。
//
// 例如错误信息 `会话不存在或已过期: abc123` —— 整句不可能进词典 (后面那段是动态的),
// 所以词典里存的是**前缀** `会话不存在或已过期: `, 这里按前缀匹配后只翻前缀, 动态部分
// 原样保留。前缀表按长度倒序匹配, 避免短前缀抢先命中。
func TranslatePrefix(lang Lang, text string) string {
	if lang != EN || text == "" {
		return text
	}
	if en, ok := lookup(english, text); ok {
		return en
	}
	for _, entry := range prefixKeys() {
		if strings.HasPrefix(text, entry) {
			return english[entry] + strings.TrimPrefix(text, entry)
		}
	}
	return text
}

// prefixKeys 返回词典里"以冒号加空格结尾"的键 (即模板前缀), 按长度倒序。
func prefixKeys() []string {
	prefixOnce.Do(func() {
		keys := make([]string, 0, 16)
		for key := range english {
			if strings.HasSuffix(key, ": ") {
				keys = append(keys, key)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
		prefixCache = keys
	})
	return prefixCache
}

var (
	prefixOnce  sync.Once
	prefixCache []string
)

// Translate 把一段可能来自存储/模型的文本按对照表翻译 (查不到就原样返回)。
//
// 用于"报告是上次生成的, 但用户现在切到了英文"这种情况: 结构化的文案 (检查项名称、
// 结论标签、规则库的影响与建议) 都能查到译文, 因此历史记录也能跟着切换语言;
// 模型当场生成的自由文本不在表里, 只能保持原样。
func Translate(lang Lang, text string) string {
	if lang != EN || text == "" {
		return text
	}
	return TranslatePrefix(lang, text)
}
