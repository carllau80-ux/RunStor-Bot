package kb

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/llm"
)

// Citation 是一条引用: 回答里 [n] 指向的片段。
//
// 引用必须能落到"哪份文档的哪一节", 并且带上原文片段 —— 用户是照着它去操作的,
// 引用不到原文的答案等于没有依据。
type Citation struct {
	Index   int     `json:"index"`
	DocID   string  `json:"doc_id"`
	Doc     string  `json:"doc"`
	Heading string  `json:"heading,omitempty"`
	Snippet string  `json:"snippet"`
	Text    string  `json:"text,omitempty"`
	Score   float64 `json:"score"`
	// Cited 表示回答里真的引用了它 (模型可能检索到 5 条只用 2 条)。
	Cited bool `json:"cited"`
}

// Answer 是一次问答的结果。
type Answer struct {
	Question  string     `json:"question"`
	Answer    string     `json:"answer"`
	Citations []Citation `json:"citations"`
	// Mode 说明这次是怎么回答的:
	//   model          —— 模型基于检索到的资料生成
	//   retrieval_only —— 没有可用模型 (或模型失败), 只给检索结果
	//   no_match       —— 知识库里没有相关片段
	Mode string `json:"mode"`
	// Model 是本次使用的模型名 (没有则为空)。
	Model string `json:"model,omitempty"`
	// Note 是给用户看的说明 (例如"未配置模型""引用了不存在的编号")。
	Note string `json:"note,omitempty"`
	// Terms 是实际用于检索的词元, 便于用户理解"为什么没检索到"。
	Terms []string `json:"terms,omitempty"`
	// Expanded 是模型给出的扩展检索词 (如果有)。
	Expanded  []string `json:"expanded,omitempty"`
	ElapsedMS int64    `json:"elapsed_ms"`
}

// AskOptions 是一次问答的设置。
type AskOptions struct {
	Question string
	TopK     int
	// Expand 是否让模型把问题改写成若干检索词以提升召回。
	Expand bool
	// Lang 是回答语言 (零值按中文)。
	//
	// 提示词决定了模型用什么语言回答: 界面切成英文却让模型答中文, 是最容易漏掉的一环。
	Lang i18n.Lang
}

// Ask 用知识库回答一个问题。
//
// 三种结果对应三种诚实程度不同的情况, 界面上要能一眼区分:
//   - 检索不到资料 → 直接说没有, 不调用模型 (省 token, 也避免模型凭常识瞎答);
//   - 没有可用模型 → 只给检索到的片段, 让用户自己读 (而不是假装回答不了);
//   - 有模型 → 基于资料回答并要求标注引用。
func (b *Base) Ask(options AskOptions, client llm.Client, modelName string) Answer {
	started := time.Now()
	question := strings.TrimSpace(options.Question)
	result := Answer{Question: question, Citations: []Citation{}}

	if question == "" {
		result.Mode = "no_match"
		result.Note = "请输入问题"
		return result
	}

	results, expanded := b.retrieve(question, options, client)
	result.Expanded = expanded
	result.Terms = Terms(question)
	result.ElapsedMS = time.Since(started).Milliseconds()

	if len(results) == 0 {
		result.Mode = "no_match"
		result.Note = "知识库里没有找到与这个问题相关的片段。可以换个说法 (用文档里的术语), " +
			"或者先把相关资料导入知识库。"
		return result
	}
	for index, item := range results {
		result.Citations = append(result.Citations, Citation{
			Index: index + 1, DocID: item.DocID, Doc: item.Doc, Heading: item.Heading,
			Snippet: Snippet(item.Text, 240), Text: item.Text, Score: item.Score,
		})
	}

	if client == nil || client.Name() == llm.DemoName {
		result.Mode = "retrieval_only"
		result.Note = "未配置可用的模型, 以下是知识库里与问题最相关的片段, 请自行阅读判断。"
		return result
	}

	answer, err := client.Chat([]llm.Message{
		{Role: "system", Content: askSystemPromptFor(options.Lang)},
		{Role: "user", Content: buildAskPrompt(question, results)},
	})
	if err != nil {
		// 模型失败不影响检索结果的价值: 退化成"只给片段", 并说明原因
		result.Mode = "retrieval_only"
		result.Note = "模型调用失败, 以下是检索到的片段: " + err.Error()
		return result
	}

	result.Mode = "model"
	result.Model = modelName
	result.Answer = strings.TrimSpace(answer)
	markCited(&result)
	result.ElapsedMS = time.Since(started).Milliseconds()
	return result
}

// retrieve 检索片段; 需要时先让模型改写检索词。
func (b *Base) retrieve(question string, options AskOptions, client llm.Client) ([]Result, []string) {
	topK := options.TopK
	if topK <= 0 {
		topK = b.options.TopK
	}
	// 单次请求也能指定片段数, 同样要有上限: 否则一个 top_k=100000 的请求
	// 就是一次"把检索结果与提示词一起撑爆"的免费 DoS。
	if topK > MaxTopK {
		topK = MaxTopK
	}
	if !options.Expand || client == nil || client.Name() == llm.DemoName {
		return b.Search(question, topK), nil
	}

	expanded := b.expandTerms(question, client, options)
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.searchWeighted(question, topK, expanded), expanded
}

// expandTerms 让模型给出若干检索词。
//
// 目的是补上"同义不同词"的缺口: 用户问"磁盘写满了", 文档写的是"空间不足",
// 字面二元组对不上, 但模型知道该去找哪些词。失败就当作没有扩展 —— 它只是锦上添花。
func (b *Base) expandTerms(question string, client llm.Client, options AskOptions) []string {
	content, err := client.Chat([]llm.Message{
		{Role: "system", Content: expandSystemPromptFor(options.Lang)},
		{Role: "user", Content: question},
	})
	if err != nil {
		return nil
	}
	var terms []string
	for _, line := range strings.Split(content, "\n") {
		candidate := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*0123456789.、)）"))
		candidate = strings.Trim(candidate, "\"'“”")
		if candidate == "" || len([]rune(candidate)) > 24 {
			continue
		}
		// 只保留看起来像检索词的行 (模型可能夹带解释)
		if strings.ContainsAny(candidate, "。！？；") {
			continue
		}
		terms = append(terms, candidate)
		if len(terms) >= 8 {
			break
		}
	}
	return terms
}

// searchWeighted 用加权词元检索 (原始问题权重 1.0, 扩展词 0.4)。
//
// 扩展词权重更低是刻意的: 模型给的词可能跑偏, 不能让它盖过用户原话的命中。
func (b *Base) searchWeighted(question string, topK int, expanded []string) []Result {
	weighted := []termWeight{}
	for _, term := range Terms(question) {
		weighted = append(weighted, termWeight{term: term, weight: 1.0})
	}
	for _, extra := range expanded {
		for _, term := range Terms(extra) {
			weighted = append(weighted, termWeight{term: term, weight: 0.4})
		}
	}
	if len(weighted) == 0 {
		return nil
	}
	scores := b.index.scoreWeighted(weighted)
	all := make([]string, 0, len(weighted))
	for _, item := range weighted {
		all = append(all, item.term)
	}
	results := b.index.rank(scores, dedupe(all), b.chunks, topK)
	for index := range results {
		docID := b.chunkDoc[results[index].ChunkID]
		results[index].DocID = docID
		for _, doc := range b.docs {
			if doc.ID == docID {
				results[index].Doc = doc.Title
				break
			}
		}
	}
	return results
}

// lookupLimit 是巡检报告里每个问题附加的知识引用条数上限。
const lookupLimit = 2

// minKnowledgeScore 是给巡检报告附加引用所需的最低相关度。
//
// 用来挡掉"只靠一个常见词偶然命中"的引用: 例如日志行 `db_root: cannot open: /etc/target`
// 被判为"数据库错误", 因为手册里有"数据"二字而匹配到 ZFS 校验那节 —— 把这种引用挂在
// 诊断下面会把人引到完全无关的章节, 不如不给。
//
// 值取自实测的两个区间 (真实感的运维手册 + 巡检里真实会产生的诊断标题):
//   - 偶然命中: 2.4~2.8 ("数据库错误"、"服务状态")
//   - 真正命中: 3.6~19.1 (CHAP 认证失败 3.6、iSCSI 认证失败 6.5、OOM 19.1)
//
// 取在两者之间。
//
// **注意**: BM25 的绝对分数会随切分规则变化 (片段越长, 长度归一化越重), 所以改动
// internal/kb/chunk.go 之后这个门槛要重新测 —— TestKnowledgeScoreThresholdBothSides
// 会把两侧都验一遍, 它红了就说明该重新量一次区间, 而不是把测试改松。
const minKnowledgeScore = 3.2

// LookupKnowledge 按一段文字找相关知识 (给巡检报告引用手册用)。
//
// 实现 inspect 那边的 KnowledgeLookup 接口: 巡检发现问题时, 报告里直接指向运维手册
// 的对应章节, 比只给一堆处置建议更省事。
func (b *Base) LookupKnowledge(query string, limit int) []KnowledgeRef {
	if limit <= 0 {
		limit = lookupLimit
	}
	results := b.Search(query, limit)
	out := make([]KnowledgeRef, 0, len(results))
	for _, item := range results {
		if item.Score < minKnowledgeScore {
			continue
		}
		out = append(out, KnowledgeRef{
			DocID:   item.DocID,
			Doc:     item.Doc,
			Heading: item.Heading,
			Snippet: Snippet(item.Text, 200),
			Score:   item.Score,
		})
	}
	return out
}

// KnowledgeRef 是巡检报告里的一条知识引用 (字段与 inspect.KnowledgeRef 对应)。
type KnowledgeRef struct {
	DocID   string  `json:"doc_id"`
	Doc     string  `json:"doc"`
	Heading string  `json:"heading,omitempty"`
	Snippet string  `json:"snippet,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// termWeight 是带权重的检索词。
type termWeight struct {
	term   string
	weight float64
}

// citePattern 匹配回答里的引用标记 [1] [2]。
var citePattern = regexp.MustCompile(`\[(\d{1,2})\]`)

// markCited 标出回答真正引用了哪几条, 并处理"引用了不存在的编号"。
//
// 这是能自动做的接地检查: 模型写了 [7] 但我们只给了 5 条资料, 说明它在编引用。
// 这种情况必须告诉用户, 否则引用看起来更"有据", 反而更危险。
func markCited(answer *Answer) {
	used := map[int]bool{}
	invalid := []int{}
	for _, match := range citePattern.FindAllStringSubmatch(answer.Answer, -1) {
		index, err := strconv.Atoi(match[1])
		if err != nil || index < 1 {
			continue
		}
		if index > len(answer.Citations) {
			invalid = append(invalid, index)
			continue
		}
		used[index] = true
	}
	for position := range answer.Citations {
		answer.Citations[position].Cited = used[answer.Citations[position].Index]
	}

	notices := []string{}
	if len(invalid) > 0 {
		sort.Ints(invalid)
		notices = append(notices, fmt.Sprintf(
			"注意: 回答里引用了不存在的资料编号 %s, 这部分内容没有依据, 请以引用为准",
			formatIndices(invalid)))
	}
	if len(used) == 0 {
		notices = append(notices, "回答里没有标注引用编号, 下面是检索到的片段, 请自行核对")
	}
	answer.Note = strings.Join(notices, "; ")
}

func formatIndices(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("[%d]", value))
	}
	return strings.Join(parts, " ")
}

// askSystemPrompt 是问答的系统提示词。
//
// 约束的重点是"不许超出资料范围": 运维场景里一条编造出来的命令可能直接造成故障,
// 所以宁可回答"资料里没有"。
const askSystemPrompt = `你是运维知识库助手。用户会问一个运维问题, 你会拿到从知识库里检索到的资料片段。

回答要求:
1. 只根据给出的资料回答。资料里没有的内容一律不要写, 更不要凭常识补充命令、参数或数值。
2. 每个结论后面用 [编号] 标注它来自哪段资料, 例如 "先用 zpool scrub 修复 [2]"。
3. 如果资料不足以回答问题, 直接说"资料里没有写这部分", 并说明还缺什么信息。
4. 如果资料之间有冲突, 指出冲突, 不要擅自选一个。
5. 用中文回答, 步骤多时用有序列表, 命令用行内代码格式 (反引号)。
6. 不要重复整段资料, 用你自己的话组织成可执行的步骤。`

// askSystemPromptEN 是英文版问答提示词 (产出自然也是英文)。
const askSystemPromptEN = `You are an operations knowledge-base assistant. The user asks an operations
question and you receive passages retrieved from the knowledge base.

Answering rules:
1. Answer only from the given passages. Never add commands, parameters or numbers that are not
   in them, and never fill gaps from general knowledge.
2. Cite the source of every statement with its number, e.g. "run zpool scrub to repair [2]".
3. If the passages are insufficient, say plainly "the documents do not cover this" and state
   what information is missing.
4. If the passages contradict each other, point out the conflict instead of picking one.
5. Prefer a numbered list when there are steps; write commands in inline code (backticks).
6. Do not repeat whole passages — organise them into actionable steps in your own words.`

// expandSystemPromptEN 是英文版检索词改写提示词。
const expandSystemPromptEN = `Rewrite the user's question into 3-6 keywords for document retrieval,
one per line, no numbering, no explanation.
Cover synonyms (e.g. for "disk full" also give "out of space"), likely technical terms, and the
components involved. Output only the keywords.`

// askSystemPromptFor 按语言选问答提示词。
func askSystemPromptFor(lang i18n.Lang) string {
	if lang == i18n.EN {
		return askSystemPromptEN
	}
	return askSystemPrompt
}

// expandSystemPromptFor 按语言选检索词提示词。
func expandSystemPromptFor(lang i18n.Lang) string {
	if lang == i18n.EN {
		return expandSystemPromptEN
	}
	return expandSystemPrompt
}

// expandSystemPrompt 是检索词改写的系统提示词。
const expandSystemPrompt = `把用户的问题改写成 3~6 个用于文档检索的关键词, 每行一个, 不要编号, 不要解释。
关键词要覆盖: 同义词 (例如"磁盘写满"也要给出"空间不足")、可能的专业术语、以及问题涉及的组件名。
只输出关键词本身。`

// buildAskPrompt 拼装提问内容: 编号的资料 + 问题。
func buildAskPrompt(question string, results []Result) string {
	const maxChars = 12000
	var builder strings.Builder
	builder.WriteString("以下是知识库里检索到的资料片段:\n")
	used := 0
	for index, item := range results {
		piece := item.Text
		if used+len([]rune(piece)) > maxChars {
			// 资料太长时截断而不是整条丢掉: 后半段往往还有关键步骤
			piece = Truncate(piece, maxChars-used)
		}
		fmt.Fprintf(&builder, "\n--- 资料 [%d] %s", index+1, DescribeChunk(item.Doc, item.Heading))
		builder.WriteString("\n")
		builder.WriteString(piece)
		builder.WriteString("\n")
		used += len([]rune(piece))
		if used >= maxChars {
			break
		}
	}
	fmt.Fprintf(&builder, "\n问题: %s\n", question)
	return builder.String()
}
