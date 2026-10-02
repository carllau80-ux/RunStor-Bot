package kb

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/fileio"
)

// Document 是一份已导入的文档。
type Document struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Source  string    `json:"source"` // 原始文件名
	AddedAt time.Time `json:"added_at"`
	Bytes   int       `json:"bytes"`
	Chunks  int       `json:"chunks"`
	// Runes 是去空白后的字符数, 用于给人看的"文档多长"。
	Runes int `json:"runes"`
	// Seeded 标记这份文档是从部署预置目录同步来的。
	//
	// 只有预置文档会在升级时被刷新; 操作者自己导入的同名文档一律不动。
	Seeded bool `json:"seeded,omitempty"`
	// Content 是导入时的原文。
	//
	// 本来只存切好的片段就够了 (检索与引用用的都是片段), 但知识库界面要能**直接把
	// 文档打开来看** —— 从片段拼回原文是不行的: 片段之间有重叠, 拼起来会重复。
	// 早先导入的文档没有这个字段, 界面上会提示重新导入。
	Content string `json:"content,omitempty"`
}

// HasContent 报告这份文档是否保存了原文 (早先版本导入的没有)。
func (d Document) HasContent() bool { return strings.TrimSpace(d.Content) != "" }

// MaxTopK 是一次问答能取用的片段数上限。
//
// 放在这里当唯一出处: 上限散在配置、请求参数、检索实现三处时, 很容易出现
// "配置里写 20、接口收 50、实际用 5" 这种三方不一致, 而它只会表现为
// "回答的质量不太稳定", 没人查得出来。
const MaxTopK = 20

// Options 是知识库的参数。
type Options struct {
	// Path 是落盘位置; 空表示仅内存 (重启即丢)。
	Path string
	// MaxDocuments 限制文档数。
	MaxDocuments int
	// MaxDocumentBytes 限制单份文档大小。
	MaxDocumentBytes int
	// MaxTotalBytes 限制全部文档的总大小 (落盘文件别无限长)。
	MaxTotalBytes int
	// TopK 是默认返回的片段数 (上限 MaxTopK)。
	TopK int
	// Chunk 是切分参数。
	Chunk ChunkOptions
	// ImportDir 是"从服务器目录导入"的受限目录; 空表示该功能关闭。
	//
	// 只读这个目录里的文档, 且拒绝逃逸出该目录的路径 —— 否则就成了任意文件读取。
	ImportDir string
	// MaxRecords 限制保留的导入记录条数。
	MaxRecords int
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		MaxDocuments:     200,
		MaxDocumentBytes: 2 << 20,  // 2 MB
		MaxTotalBytes:    16 << 20, // 16 MB
		TopK:             5,
		Chunk:            DefaultChunkOptions(),
		MaxRecords:       200,
	}
}

func (o Options) withDefaults() Options {
	defaults := DefaultOptions()
	if o.MaxDocuments <= 0 {
		o.MaxDocuments = defaults.MaxDocuments
	}
	if o.MaxDocumentBytes <= 0 {
		o.MaxDocumentBytes = defaults.MaxDocumentBytes
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = defaults.MaxTotalBytes
	}
	if o.TopK <= 0 {
		o.TopK = defaults.TopK
	}
	if o.TopK > MaxTopK {
		o.TopK = MaxTopK
	}
	if o.Chunk.MaxRunes <= 0 {
		o.Chunk = defaults.Chunk
	}
	if o.MaxRecords <= 0 {
		o.MaxRecords = defaults.MaxRecords
	}
	return o
}

// Base 是知识库本体。
//
// 全部数据放在内存里 (文档 + 片段 + 倒排索引), 落盘只为了重启后还在。对这个体量
// (默认上限 16 MB 文档) 来说, 内存索引比任何外部检索引擎都简单也够快 ——
// 这就是"轻量级"的取舍: 不引向量库、不引搜索引擎、不引分词器。
type Base struct {
	mu      sync.RWMutex
	options Options
	docs    []Document
	// chunks 按文档 ID 分组, 与 index 里的 chunkID 一一对应 (顺序即 chunkID)。
	chunks   []Chunk
	chunkDoc []string // chunkID → 文档 ID
	// imports 是导入记录 (新的在前)。
	imports []ImportRecord
	// seedIgnored 记录被操作者删除的预置文档名: 删了就不再补回来。
	seedIgnored []string
	index       *index
	loadedAt    time.Time
	// loadedVersion 是落盘数据写出的版本 (0 表示空库或没有版本号)。
	loadedVersion int
}

// New 构造知识库并加载已有数据。
func New(options Options) *Base {
	options = options.withDefaults()
	base := &Base{options: options, index: newIndex(), loadedAt: time.Now(), imports: []ImportRecord{}}
	base.load()
	return base
}

// Path 返回落盘位置。
func (b *Base) Path() string { return b.options.Path }

// Count 返回文档数与片段数。
func (b *Base) Count() (int, int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.docs), len(b.chunks)
}

// List 返回全部文档 (按导入时间倒序)。
func (b *Base) List() []Document {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Document, len(b.docs))
	copy(out, b.docs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.After(out[j].AddedAt) })
	return out
}

// Get 返回单份文档。
func (b *Base) Get(id string) (Document, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, doc := range b.docs {
		if doc.ID == id {
			return doc, true
		}
	}
	return Document{}, false
}

// importDocument 是导入的实际实现 (记录由调用方负责)。
//
// 返回被替换的旧文档标题 (没有替换则为空): 同名替换是常见动作, 记录里要能看出来
// "这一版覆盖了哪一版"。
func (b *Base) importDocument(name, content string) (Document, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名.md"
	}
	if strings.TrimSpace(content) == "" {
		return Document{}, "", fmt.Errorf("文档内容为空")
	}
	if len(content) > b.options.MaxDocumentBytes {
		return Document{}, "", fmt.Errorf("文档超过单份上限 %s (实际 %s)",
			humanBytes(b.options.MaxDocumentBytes), humanBytes(len(content)))
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 替换同名文档
	replaced := ""
	for _, doc := range b.docs {
		if doc.Source == name || doc.Title == titleFromName(name) {
			replaced = doc.Title
			b.removeLocked(doc.ID)
			break
		}
	}

	if len(b.docs) >= b.options.MaxDocuments {
		return Document{}, replaced, fmt.Errorf("文档数已达上限 %d, 请先删除一些", b.options.MaxDocuments)
	}
	if used := b.totalBytesLocked(); used+len(content) > b.options.MaxTotalBytes {
		return Document{}, replaced, fmt.Errorf("知识库总大小将超过上限 %s (当前 %s), 请先删除一些文档",
			humanBytes(b.options.MaxTotalBytes), humanBytes(used))
	}

	title, chunks := ParseDocument(name, content, b.options.Chunk)
	if len(chunks) == 0 {
		return Document{}, replaced, fmt.Errorf("文档里没有可索引的内容")
	}

	document := Document{
		ID:      newID("doc"),
		Title:   title,
		Source:  name,
		AddedAt: time.Now(),
		Bytes:   len(content),
		Chunks:  len(chunks),
		Runes:   runeLen(content),
		Content: content,
	}
	b.docs = append(b.docs, document)
	for _, chunk := range chunks {
		b.chunks = append(b.chunks, chunk)
		b.chunkDoc = append(b.chunkDoc, document.ID)
		b.index.add(len(b.chunks)-1, chunk.Heading, chunk.Text)
	}
	b.index.finish()
	b.persistOrWarn("导入文档 " + document.Source)
	return document, replaced, nil
}

// Content 返回文档原文。
//
// 第二个返回值报告"这份文档有没有存原文": 早先版本导入的没有, 界面要据此提示
// 用户重新导入, 而不是显示一片空白。
func (b *Base) Content(id string) (Document, string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, doc := range b.docs {
		if doc.ID == id {
			return doc, doc.Content, doc.HasContent()
		}
	}
	return Document{}, "", false
}

// Delete 删除一份文档。
func (b *Base) Delete(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.removeLocked(id) {
		return false
	}
	b.persistOrWarn("删除文档 " + id)
	return true
}

// removeLocked 删除文档并重建索引 (调用方必须已持锁)。
//
// 预置文档被删除时会记下名字: 启动时的同步不再把它补回来 —— "删了又自己长回来"
// 是最让人恼火的行为。想让它回来, 重新导入即可 (那一份就属于操作者自己了)。
//
// 重建而不是"从倒排表里摘除": 摘除要遍历每个词元的 posting 列表, 代码更绕且更容易
// 出错; 重建是 O(全部片段), 对 16 MB 上限的体量只是一瞬间的事。
func (b *Base) removeLocked(id string) bool {
	found := false
	docs := make([]Document, 0, len(b.docs))
	for _, doc := range b.docs {
		if doc.ID == id {
			found = true
			if doc.Seeded && !containsString(b.seedIgnored, doc.Source) {
				b.seedIgnored = append(b.seedIgnored, doc.Source)
			}
			continue
		}
		docs = append(docs, doc)
	}
	if !found {
		return false
	}
	b.docs = docs
	b.rebuildLocked()
	return true
}

// rebuildLocked 重建全部索引。
func (b *Base) rebuildLocked() {
	// 片段本身没有变化, 只是被删除文档的那些要丢掉
	keptChunks := make([]Chunk, 0, len(b.chunks))
	keptDoc := make([]string, 0, len(b.chunkDoc))
	alive := map[string]bool{}
	for _, doc := range b.docs {
		alive[doc.ID] = true
	}
	for index, chunk := range b.chunks {
		if !alive[b.chunkDoc[index]] {
			continue
		}
		keptChunks = append(keptChunks, chunk)
		keptDoc = append(keptDoc, b.chunkDoc[index])
	}
	b.chunks, b.chunkDoc = keptChunks, keptDoc

	b.index = newIndex()
	for chunkID, chunk := range b.chunks {
		b.index.add(chunkID, chunk.Heading, chunk.Text)
	}
	b.index.finish()
}

// Search 检索相关片段。
func (b *Base) Search(query string, topK int) []Result {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.searchLocked(query, topK, nil)
}

// searchLocked 是检索的内部实现 (调用方持锁)。
func (b *Base) searchLocked(query string, topK int, extraTerms []string) []Result {
	if topK <= 0 {
		topK = b.options.TopK
	}
	terms := Terms(query)
	// 额外词元 (模型给的检索词) 只用于**扩展召回**: 它们不能改变原始查询的排序权重,
	// 否则模型一旦给了不相关的词, 检索结果就会被带偏。
	for _, term := range extraTerms {
		terms = append(terms, Terms(term)...)
	}
	terms = dedupe(terms)
	if len(terms) == 0 {
		return nil
	}

	scores := b.index.score(terms)
	results := b.index.rank(scores, terms, b.chunks, topK)
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

// Stats 返回知识库概况 (给界面顶部显示)。
func (b *Base) Stats() map[string]any {
	documents, chunks := b.Count()
	b.mu.RLock()
	records := len(b.imports)
	importDir := b.options.ImportDir
	b.mu.RUnlock()
	return map[string]any{
		"documents":          documents,
		"chunks":             chunks,
		"path":               b.options.Path,
		"bytes":              b.totalBytes(),
		"records":            records,
		"import_dir":         importDir,
		"max_documents":      b.options.MaxDocuments,
		"max_document_bytes": b.options.MaxDocumentBytes,
		"max_total_bytes":    b.options.MaxTotalBytes,
	}
}

func (b *Base) totalBytes() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.totalBytesLocked()
}

func (b *Base) totalBytesLocked() int {
	total := 0
	for _, doc := range b.docs {
		total += doc.Bytes
	}
	return total
}

// ---------------------------------------------------------------------------
// 落盘
// ---------------------------------------------------------------------------

// persisted 是落盘的结构。
//
// 片段与原文都存: 片段用于检索与引用 (编号必须稳定), 原文用于"打开文档"查看。
// 版本 3 起带上原文; 早先导入的文档没有原文, 界面上会提示重新导入一次。
type persisted struct {
	Version int            `json:"version"`
	Docs    []Document     `json:"documents"`
	Chunks  []Chunk        `json:"chunks"`
	DocOf   []string       `json:"chunk_doc"`
	Imports []ImportRecord `json:"imports,omitempty"`
	// SeedIgnored 是被删除的预置文档名 (不再补回)。
	SeedIgnored []string `json:"seed_ignored,omitempty"`
}

// kbVersion 是落盘格式与**切分规则**的版本号。
//
// 片段与章节标题是导入时算好的, 之后代码改了切分/去重规则, 老文档不会自动跟着变
// (落盘里存的是片段, 没有原文可重算)。这不是数据损坏, 只是老文档的章节路径可能按旧
// 规则; 版本不一致时启动会提示重新导入, 而不是悄悄留着不一致的状态。
const kbVersion = 3

// StaleWarning 在落盘数据由旧版本写出时返回一句提示 (供启动日志展示)。
func (b *Base) StaleWarning() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.loadedVersion == 0 || b.loadedVersion == kbVersion {
		return ""
	}
	return fmt.Sprintf("知识库里有按旧规则 (%d) 切分的文档 (当前规则 %d), 建议重新导入这些文档以应用新规则",
		b.loadedVersion, kbVersion)
}

func (b *Base) load() {
	if b.options.Path == "" {
		return
	}
	// 自己的落盘文件也要有上限: 文件损坏或被塞了东西时, 不该在读的时候把内存吃光。
	// 正常内容由 MaxTotalBytes 约束, 这里给足余量 (片段与记录会带来额外开销)。
	limit := int64(b.options.MaxTotalBytes)*4 + (1 << 20)
	raw, err := fileio.ReadBounded(b.options.Path, limit)
	if err != nil {
		return
	}
	var decoded persisted
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// 文件坏了就当空库启动, 而不是让整个服务起不来
		return
	}
	if len(decoded.Docs) != countDocs(decoded.DocOf) && len(decoded.DocOf) != len(decoded.Chunks) {
		return
	}
	b.docs = decoded.Docs
	b.chunks = decoded.Chunks
	b.chunkDoc = decoded.DocOf
	b.seedIgnored = decoded.SeedIgnored
	b.imports = decoded.Imports
	if b.imports == nil {
		b.imports = []ImportRecord{}
	}
	b.loadedVersion = decoded.Version
	b.index = newIndex()
	for chunkID, chunk := range b.chunks {
		b.index.add(chunkID, chunk.Heading, chunk.Text)
	}
	b.index.finish()
}

func countDocs(docOf []string) int {
	seen := map[string]bool{}
	for _, id := range docOf {
		seen[id] = true
	}
	return len(seen)
}

// persistLocked 原子写入落盘文件 (调用方持锁)。
//
// 返回错误而不是静默吞掉: 写失败意味着"内存里有、盘上没有" —— 服务照常回答, 但重启后
// 刚导入的文档就没了。这种事必须让调用方知道并说出来。
func (b *Base) persistLocked() error {
	if b.options.Path == "" {
		return nil
	}
	payload, err := json.MarshalIndent(persisted{
		Version:     kbVersion,
		Docs:        b.docs,
		Chunks:      b.chunks,
		DocOf:       b.chunkDoc,
		Imports:     b.imports,
		SeedIgnored: b.seedIgnored,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化知识库失败: %w", err)
	}
	if err := fileio.WriteAtomic(b.options.Path, payload, 0o600); err != nil {
		return fmt.Errorf("知识库落盘失败 (%s): %w", b.options.Path, err)
	}
	return nil
}

// persistOrWarn 用于"内存已经改完、签名里没有错误可返回"的场景 (删除之类)。
//
// 提示里必须写清后果: 只说"落盘失败"运维不知道要不要处理, 说清"重启后这次改动会丢失"
// 才知道该做什么。
func (b *Base) persistOrWarn(action string) {
	if err := b.persistLocked(); err != nil {
		log.Printf("警告: %s 已生效但未能落盘: %v; 重启后这次改动会丢失", action, err)
	}
}

func newID(prefix string) string {
	return prefix + "-" + time.Now().Format("20060102150405") + "-" + randomSuffix()
}

func randomSuffix() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	seed := time.Now().UnixNano()
	out := make([]byte, 6)
	for index := range out {
		out[index] = letters[seed%int64(len(letters))]
		seed /= int64(len(letters))
	}
	return string(out)
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	out := items[:0]
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func humanBytes(size int) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// containsString 报告切片里是否含某个字符串。
func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
