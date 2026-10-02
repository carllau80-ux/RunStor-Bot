// Package kb 是一个轻量级知识库: 导入 Markdown 文档, 按问题检索相关片段, 让模型
// 基于检索到的资料回答并给出引用。
//
// 为什么检索是词法 (BM25) 而不是向量:
//   - 部署目标是内网机器, 本服务的模型走 OpenAI 兼容接口, 而**很多兼容端点根本没有
//     /embeddings** (例如本项目默认预设里的 DeepSeek 就没有), 向量检索在那些环境下
//     直接不可用;
//   - 词法检索不需要任何模型、任何外部服务, 离线可用, 结果可复现、可解释 ——
//     哪几个词命中了、为什么这条排前面, 都能说清楚;
//   - 运维文档里最关键的词 (zpool、scrub、OOM、iSCSI、CHAP、checksum) 都是低频
//     专有词, 正是 BM25 最擅长区分的那一类。
//
// 中文没有分词器可用 (纯标准库), 这里用**字符二元组**做索引: "存储池降级" 索引成
// 存储/储池/池降/降级。这是检索领域对中文的经典退化方案, 不需要词典, 召回也够用。
package kb

import (
	"strings"
	"unicode"
)

// Tokenize 把一段文本切成检索用的词元。
//
// 两类处理:
//   - 拉丁字母与数字: 按连续的字母数字切词并小写 ("/dev/sda1" → dev, sda1);
//   - 中日韩字符: 切成字符二元组 ("存储池" → 存储, 储池), 单字成段时保留该字。
//
// 标点、空白全部丢弃: 它们在检索里没有区分度, 留着只会让倒排表变大。
func Tokenize(text string) []string {
	var tokens []string

	var latin strings.Builder
	var cjk []rune

	flushLatin := func() {
		if latin.Len() > 0 {
			tokens = append(tokens, latin.String())
			latin.Reset()
		}
	}
	flushCJK := func() {
		if len(cjk) == 0 {
			return
		}
		if len(cjk) == 1 {
			tokens = append(tokens, string(cjk))
			cjk = cjk[:0]
			return
		}
		for index := 0; index+1 < len(cjk); index++ {
			tokens = append(tokens, string(cjk[index:index+2]))
		}
		cjk = cjk[:0]
	}

	// 直接 range 字符串: 先转 []rune 会为整篇文档多复制一份 (大文档上很明显)
	for _, r := range strings.ToLower(text) {
		switch {
		case isCJK(r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			latin.WriteRune(r)
		default:
			// 标点与空白是词边界。下划线与连字符算连接符 (zfs_pool、web-01 保持完整),
			// 但单独的连接符不产生词元。
			if r == '_' || r == '-' {
				continue
			}
			flushLatin()
			flushCJK()
		}
	}
	flushLatin()
	flushCJK()
	return tokens
}

// isCJK 判断是否是中日韩统一表意文字 (含扩展区) 或日文假名。
//
// 这些文字没有词间空格, 必须按二元组切; 而其它语言按字母切词就够。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // 基本汉字
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	case r >= 0x20000 && r <= 0x2A6DF: // 扩展 B
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容汉字
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 韩文音节
		return true
	}
	return false
}

// Terms 把一段文本切成去重后的词元 (检索查询用)。
func Terms(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, token := range Tokenize(text) {
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}
