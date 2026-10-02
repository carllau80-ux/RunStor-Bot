package kb

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Chunk 是文档切出来的一段, 带它在文档里的位置与所属章节标题。
//
// 带章节标题不是装饰: 检索结果要能定位到"《ZFS 故障处置》> 校验失败"这样一处,
// 用户才敢照着做; 而且章节标题参与打分, 让整节相关的片段更容易被召回。
type Chunk struct {
	Index   int    `json:"index"`
	Heading string `json:"heading"` // 章节路径, 如 "一、ZFS > 校验失败"
	Text    string `json:"text"`
}

// ChunkOptions 是切分参数。
type ChunkOptions struct {
	// MaxRunes 是单个片段的字符数上限。
	MaxRunes int
	// OverlapRunes 是相邻片段的重叠字符数。
	//
	// 必须有重叠: 一句话被切断时, 前后两个片段各自都不完整, 重叠能让至少一个片段
	// 包含完整语义, 否则检索到的片段读起来像半句话。
	OverlapRunes int
}

// DefaultChunkOptions 返回默认切分参数。
//
// 400 字左右是中文技术文档"一节内容"的常见长度, 也正好是一段能被模型完整消化的
// 上下文; 80 字重叠约等于两句话。
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxRunes: 400, OverlapRunes: 80}
}

// ParseDocument 解析 Markdown 文档: 返回标题与切好的片段。
//
// 按标题切分 (而不是按固定字数硬切) 是关键: Markdown 的结构就是作者的思路, 按章节
// 切出来的片段天然语义完整, 引用时也说得清"在讲哪一节"。
func ParseDocument(name, content string, options ChunkOptions) (string, []Chunk) {
	if options.MaxRunes <= 0 {
		options = DefaultChunkOptions()
	}
	content = normalizeNewlines(content)
	content = stripFrontMatter(content)

	title := titleFromName(name)
	var (
		chunks    []Chunk
		heading   string   // 当前章节路径
		stack     []string // 标题层级栈, 用来拼 "一、ZFS > 校验失败"
		buffer    []string // 当前章节累积的行
		inFence   bool     // 是否在 ``` 代码块里
		fenceMark string
	)

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(buffer, "\n"))
		buffer = buffer[:0]
		if text == "" {
			return
		}
		for _, piece := range splitLong(text, options) {
			chunks = append(chunks, Chunk{Index: len(chunks), Heading: heading, Text: piece})
		}
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// 代码块内部的行不做标题识别: ``` 里的 "# 注释" 不是标题
		if isFence(trimmed) {
			if !inFence {
				inFence = true
				fenceMark = fenceOf(trimmed)
			} else if strings.HasPrefix(trimmed, fenceMark) {
				inFence = false
			}
			buffer = append(buffer, line)
			continue
		}
		if !inFence {
			if level, text, ok := parseHeading(trimmed); ok {
				flush()
				if level <= len(stack) {
					stack = stack[:level-1]
				}
				stack = append(stack, text)
				heading = strings.Join(stack, " > ")
				if level == 1 && title == "" {
					title = text
				}
				// 标题本身也作为片段的一部分放进去: 检索"校验失败"时,
				// 该节的所有片段都能因为标题命中而受益
				buffer = append(buffer, trimmed)
				continue
			}
		}
		buffer = append(buffer, line)
	}
	flush()

	// 第一个一级标题就是文档标题 (比文件名更贴近内容)
	if first := firstHeading(content); first != "" {
		title = first
	}
	if title == "" {
		title = name
	}

	// 片段正文里带上完整章节路径。
	//
	// 这一步是为了**检索**: 小节标题往往不含文档的关键词 (例如文档叫《换盘流程》,
	// 小节叫《步骤》), 只按小节文本索引的话, 问"换盘流程是什么"就匹配不上那一段。
	// 带上路径后, 同一文档的所有片段都能被文档关键词召回。
	//
	// 同时它也改善引用: 片段开头就写着"这段出自哪一节", 模型判断更准, 用户也看得明白。
	for index := range chunks {
		chunks[index].Text = withPathPrefix(chunks[index].Text, chunks[index].Heading)
		// 展示用的路径去掉与文档标题重复的那一级: 引用写成
		// "《iSCSI 故障处置》§ iSCSI 故障处置 > 登录被拒绝" 是把文档名说了两遍。
		chunks[index].Heading = trimTitleFromHeading(chunks[index].Heading, title)
	}
	return title, mergeStubs(chunks)
}

// withPathPrefix 在片段正文前加上章节路径 (已经带了就不重复加)。
func withPathPrefix(text, path string) string {
	path = strings.TrimSpace(path)
	text = strings.TrimSpace(text)
	if path == "" {
		return text
	}
	// 片段本身以标题行开头时 (比如这一节只有标题), 不必再加前缀
	trimmed := strings.TrimLeft(text, "#")
	if strings.HasPrefix(strings.TrimSpace(trimmed), strings.TrimPrefix(path, "#")) {
		return text
	}
	return path + "\n\n" + text
}

// stubBodyRunes 是"这段内容太短"的判定门槛。
//
// 短到这种程度的片段几乎只有一个标题 (例如 "# 手册" 后面紧跟着下一个标题),
// 单独成段检索不到、引用出去也没内容 —— 它应该跟后面的正文合在一起。
const stubBodyRunes = 60

// mergeStubs 把"只有标题没什么内容"的片段并入下一个片段。
//
// 真实场景: 一份几十字的小手册写成
//
//	# 换盘流程
//	## 步骤
//	1. 确认 vdev 路径
//	2. zpool replace
//
// 切出来是两段, 第一段只有"# 换盘流程"。问"换盘流程是什么"时, 命中第一段 (因为
// 关键词在标题里), 但那一段没有任何实质内容 —— 模型只好回答"资料里没有写这部分"。
// 合并之后, 标题与正文在同一段里, 召回与回答就都对上了。
func mergeStubs(chunks []Chunk) []Chunk {
	if len(chunks) < 2 {
		return chunks
	}
	out := make([]Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if len(out) > 0 && bodyRunes(out[len(out)-1].Text) < stubBodyRunes {
			previous := &out[len(out)-1]
			previous.Text = strings.TrimSpace(previous.Text) + "\n\n" + strings.TrimSpace(chunk.Text)
			if previous.Heading == "" {
				previous.Heading = chunk.Heading
			} else if chunk.Heading != "" && !strings.HasPrefix(chunk.Heading, previous.Heading) {
				previous.Heading = previous.Heading + " > " + chunk.Heading
			}
			continue
		}
		out = append(out, chunk)
	}
	// 最后一段仍然过短的话, 并回上一段 (没有下一段可并了)
	if len(out) >= 2 && bodyRunes(out[len(out)-1].Text) < stubBodyRunes {
		last := out[len(out)-1]
		out = out[:len(out)-1]
		out[len(out)-1].Text = strings.TrimSpace(out[len(out)-1].Text) + "\n\n" + strings.TrimSpace(last.Text)
	}
	for index := range out {
		out[index].Index = index
	}
	return out
}

// bodyRunes 返回片段里"去掉标题行之后"的字符数。
func bodyRunes(text string) int {
	total := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if trimmed == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		total += len([]rune(trimmed))
	}
	return total
}

// trimTitleFromHeading 去掉章节路径开头与文档标题重复的部分。
func trimTitleFromHeading(heading, title string) string {
	heading = strings.TrimSpace(heading)
	title = strings.TrimSpace(title)
	if title == "" {
		return heading
	}
	if heading == title {
		return ""
	}
	if rest, ok := strings.CutPrefix(heading, title+" > "); ok {
		return strings.TrimSpace(rest)
	}
	return heading
}

// splitLong 把过长的文本切成带重叠的片段。
//
// 优先在空行 (段落边界) 处切: 一句话被截成两半的片段, 检索出来读着别扭, 模型也容易
// 理解错。
func splitLong(text string, options ChunkOptions) []string {
	runes := []rune(text)
	if len(runes) <= options.MaxRunes {
		return []string{text}
	}

	var pieces []string
	start := 0
	for start < len(runes) {
		end := start + options.MaxRunes
		if end >= len(runes) {
			pieces = append(pieces, strings.TrimSpace(string(runes[start:])))
			break
		}
		// 往回找一个段落边界 (连续换行), 找不到就找一个句子结束符, 再找不到才硬切
		cut := end
		if index := lastParagraphBreak(runes, start+options.MaxRunes/2, end); index > 0 {
			cut = index
		} else if index := lastSentenceEnd(runes, start+options.MaxRunes/2, end); index > 0 {
			cut = index
		}
		pieces = append(pieces, strings.TrimSpace(string(runes[start:cut])))
		next := cut - options.OverlapRunes
		if next <= start {
			next = cut
		}
		start = next
	}

	out := pieces[:0]
	for _, piece := range pieces {
		if strings.TrimSpace(piece) != "" {
			out = append(out, piece)
		}
	}
	return out
}

// lastParagraphBreak 在 [from, to) 里找最后一个段落边界 (空行) 之后的第一个字符位置。
func lastParagraphBreak(runes []rune, from, to int) int {
	for index := to - 1; index > from; index-- {
		if runes[index] == '\n' && runes[index-1] == '\n' {
			return index + 1
		}
	}
	return -1
}

// lastSentenceEnd 在 [from, to) 里找最后一个句末标点之后的位置。
func lastSentenceEnd(runes []rune, from, to int) int {
	for index := to - 1; index > from; index-- {
		switch runes[index] {
		case '。', '！', '？', '.', '!', '?', ';', '；', '\n':
			return index + 1
		}
	}
	return -1
}

// parseHeading 识别 ATX 标题 (# 开头)。
func parseHeading(line string) (int, string, bool) {
	if !strings.HasPrefix(line, "#") {
		return 0, "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level > 6 {
		return 0, "", false
	}
	text := strings.TrimSpace(line[level:])
	if text == "" {
		return 0, "", false
	}
	// 去掉尾部用来闭合的 #
	text = strings.TrimSpace(strings.TrimRight(text, "#"))
	return level, text, text != ""
}

// isFence 判断是否是代码块围栏行。
func isFence(line string) bool {
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

func fenceOf(line string) string {
	if strings.HasPrefix(line, "~~~") {
		return "~~~"
	}
	return "```"
}

// stripFrontMatter 去掉开头的 YAML front matter (--- ... ---)。
//
// 那些是元数据 (title/date/tags), 放进去只会污染检索。
func stripFrontMatter(content string) string {
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return content
	}
	lines := strings.Split(content, "\n")
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.Join(lines[index+1:], "\n")
		}
	}
	return content
}

// firstHeading 返回第一个一级标题 (用作文档标题)。
func firstHeading(content string) string {
	inFence := false
	var fenceMark string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if isFence(trimmed) {
			if !inFence {
				inFence, fenceMark = true, fenceOf(trimmed)
			} else if strings.HasPrefix(trimmed, fenceMark) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		if level, text, ok := parseHeading(trimmed); ok && level == 1 {
			return text
		}
	}
	return ""
}

// titleFromName 从文件名推一个标题 (去掉扩展名)。
func titleFromName(name string) string {
	base := name
	if index := strings.LastIndexAny(name, "/\\"); index >= 0 {
		base = name[index+1:]
	}
	for _, suffix := range []string{".markdown", ".md", ".txt"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			base = base[:len(base)-len(suffix)]
			break
		}
	}
	return strings.TrimSpace(base)
}

func normalizeNewlines(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

// runeLen 返回字符数 (用于大小限制与统计)。
func runeLen(text string) int { return utf8.RuneCountInString(text) }

// Truncate 按字符数截断, 用于片段预览。
func Truncate(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

// Snippet 生成引用片段: 压缩空白、去掉 markdown 标记并截断, 让引用读得下去。
//
// 片段本身保留 markdown (标题层级对模型理解结构有用), 但**展示**时要去掉: 报告与
// 界面里出现"## 登录被拒绝"这种行内井号只是噪音。
func Snippet(text string, limit int) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	collapsed = strings.TrimSpace(strings.TrimLeft(collapsed, "#"))
	collapsed = strings.ReplaceAll(collapsed, "`", "")
	return Truncate(collapsed, limit)
}

// DescribeChunk 生成"文档 § 章节"形式的位置说明 (给引用用)。
func DescribeChunk(docTitle, heading string) string {
	if strings.TrimSpace(heading) == "" {
		return docTitle
	}
	return fmt.Sprintf("%s § %s", docTitle, heading)
}
