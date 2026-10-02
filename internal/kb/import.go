package kb

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"backup-restore-agent/internal/fileio"
)

// ImportRecord 是一次导入的记录。
//
// 为什么要有这个: 文档列表只反映"现在有什么", 回答不了"这份手册是什么时候、从哪儿进来的"
// 和"我上次导入的那版去哪了"。同名文档是替换语义 (改完重新导入是最常见的动作), 所以
// 没有记录的话, 被替换的旧版本就彻底查无此人了。
type ImportRecord struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	Name string    `json:"name"`
	// Origin 说明这份内容是怎么进来的: browser (浏览器选文件) / paste (粘贴) / server (服务器目录)
	Origin string `json:"origin"`
	Title  string `json:"title,omitempty"`
	Bytes  int    `json:"bytes"`
	Chunks int    `json:"chunks"`
	// Replaced 是这次导入替换掉的旧文档标题 (第一次导入时为空)。
	Replaced string `json:"replaced,omitempty"`
	DocID    string `json:"doc_id,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// 导入来源。
const (
	OriginBrowser = "browser"
	OriginPaste   = "paste"
	OriginServer  = "server"
	// OriginSeed 是随部署预置进来的文档 (从 seed 目录同步)。
	OriginSeed = "seed"
)

// 导入类型的显示名 (服务端给，避免前端各写一份)。
var originLabels = map[string]string{
	OriginBrowser: "浏览器上传",
	OriginPaste:   "粘贴文本",
	OriginServer:  "服务器目录",
	OriginSeed:    "部署预置",
}

// OriginLabel 返回导入来源的可读名称。
func OriginLabel(origin string) string {
	if label, ok := originLabels[origin]; ok {
		return label
	}
	if origin == "" {
		return "未知"
	}
	return origin
}

// ImportFrom 导入一份文档并留下记录。
//
// origin 说明内容从哪来, 会写进导入记录 (浏览器上传 / 粘贴 / 服务器目录)。
func (b *Base) ImportFrom(name, content, origin string) (Document, ImportRecord, error) {
	record := ImportRecord{
		ID:     newID("imp"),
		At:     time.Now(),
		Name:   strings.TrimSpace(name),
		Origin: origin,
		Bytes:  len(content),
	}
	if record.Name == "" {
		record.Name = "未命名.md"
	}

	document, replaced, err := b.importDocument(record.Name, content)
	if err != nil {
		record.OK = false
		record.Error = err.Error()
		b.appendRecord(record)
		return Document{}, record, err
	}

	record.OK = true
	record.Title = document.Title
	record.Chunks = document.Chunks
	record.DocID = document.ID
	if replaced != "" {
		record.Replaced = replaced
	}
	b.appendRecord(record)
	return document, record, nil
}

// Import 是 ImportFrom 的简写 (来源为浏览器上传)。
func (b *Base) Import(name, content string) (Document, error) {
	document, _, err := b.ImportFrom(name, content, OriginBrowser)
	return document, err
}

// Records 返回导入记录 (新的在前); limit <= 0 表示全部。
func (b *Base) Records(limit int) []ImportRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	records := make([]ImportRecord, len(b.imports))
	copy(records, b.imports)
	sort.SliceStable(records, func(i, j int) bool { return records[i].At.After(records[j].At) })
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records
}

// DeleteRecord 删除一条导入记录 (给"清掉某次导入的痕迹"用)。
//
// 保留单条删除而不是只提供"全部清空": 部署自检之类的操作会留下几条测试记录,
// 只为了清掉它们而把用户的完整历史一起抹掉是不合理的。
func (b *Base) DeleteRecord(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := make([]ImportRecord, 0, len(b.imports))
	found := false
	for _, record := range b.imports {
		if record.ID == id {
			found = true
			continue
		}
		kept = append(kept, record)
	}
	if !found {
		return false
	}
	b.imports = kept
	b.persistOrWarn("删除导入记录 " + id)
	return true
}

// ClearRecords 清空导入记录 (文档不受影响)。
func (b *Base) ClearRecords() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := len(b.imports)
	b.imports = []ImportRecord{}
	b.persistOrWarn("清空导入记录")
	return count
}

// appendRecord 追加记录并落盘 (调用方不得持锁)。
func (b *Base) appendRecord(record ImportRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.imports = append([]ImportRecord{record}, b.imports...)
	if len(b.imports) > b.options.MaxRecords {
		b.imports = b.imports[:b.options.MaxRecords]
	}
	b.persistOrWarn("记录一次导入")
}

// ---------------------------------------------------------------------------
// 从服务器上的文件导入
//
// 这条路很容易写成"按路径读任意文件" —— 那就是一个任意文件读取漏洞 (这个服务没有鉴权,
// 任何能连到端口的人都能把 /etc/shadow 读进知识库再检索出来)。所以路径被**限制在一个
// 指定目录内**: 只读该目录里的 .md/.markdown/.txt, 并拒绝任何逃逸出该目录的路径
// (相对路径穿越与符号链接都算)。
// ---------------------------------------------------------------------------

// InboxFile 是导入目录里的一个候选文件 (给界面预览用)。
type InboxFile struct {
	Name     string    `json:"name"`
	Bytes    int       `json:"bytes"`
	Modified time.Time `json:"modified"`
	// Imported 表示同名文档已经在知识库里了 (导入会替换它)。
	Imported bool `json:"imported"`
	// Exported 是相对导入目录的路径 (子目录里的文件也支持)。
	Path string `json:"path"`
}

// ErrNoImportDir 表示服务端导入目录未配置 (该功能关闭)。
var ErrNoImportDir = fmt.Errorf("未配置服务器导入目录")

// resolvedPathIn 把相对路径解析到导入目录内, 并确认没有逃逸出去。
//
// 三步: 拼接 → 解析软链接 (EvalSymlinks) → 确认结果仍在导入目录之下。
// 只做字符串前缀检查是不够的: 目录里的软链接可以指向 /etc。
func (b *Base) resolvedPathIn(name string) (string, error) {
	root := strings.TrimSpace(b.options.ImportDir)
	if root == "" {
		return "", ErrNoImportDir
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("导入目录无效: %w", err)
	}

	cleaned := filepath.Clean(strings.TrimSpace(name))
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("请指定文件")
	}
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("只支持导入目录内的相对路径")
	}
	target, err := filepath.Abs(filepath.Join(absoluteRoot, cleaned))
	if err != nil {
		return "", fmt.Errorf("路径无效: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("导入目录不存在: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("文件不可读: %w", err)
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("路径超出导入目录范围")
	}
	// 扩展名也要查: 只校验"在目录内"挡不住显式指名导入 script.sh / kb.json 这类文件
	if !isImportableName(resolvedTarget) {
		return "", fmt.Errorf("只支持导入 %s 文件", importableSuffixes())
	}
	return resolvedTarget, nil
}

// ListInbox 列出导入目录里可导入的文件。
func (b *Base) ListInbox() ([]InboxFile, error) {
	root := strings.TrimSpace(b.options.ImportDir)
	if root == "" {
		return nil, ErrNoImportDir
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("导入目录无效: %w", err)
	}
	if _, err := os.Stat(absoluteRoot); err != nil {
		// 目录还不存在不算错误: 告诉调用方"空"就行, 由管理员往里放文件
		return []InboxFile{}, nil
	}

	imported := map[string]bool{}
	for _, doc := range b.List() {
		imported[doc.Source] = true
	}

	files := []InboxFile{}
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件读不了不影响列出其它文件
		}
		if entry.IsDir() {
			// 隐藏目录直接跳过 (例如 .git)
			if path != absoluteRoot && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		// 隐藏文件也跳过: .hide.md、.env 这类多是编辑器/版本控制的临时文件
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		relative, err := filepath.Rel(absoluteRoot, path)
		if err != nil {
			return nil
		}
		if !isImportableName(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, InboxFile{
			Name:     filepath.Base(relative),
			Path:     relative,
			Bytes:    int(info.Size()),
			Modified: info.ModTime(),
			Imported: imported[filepath.Base(relative)],
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// ImportFromInbox 从导入目录读取若干文件并导入 (path 为空表示导入目录下全部)。
func (b *Base) ImportFromInbox(paths []string) ([]Document, []ImportRecord, error) {
	if strings.TrimSpace(b.options.ImportDir) == "" {
		return nil, nil, ErrNoImportDir
	}
	if len(paths) == 0 {
		files, err := b.ListInbox()
		if err != nil {
			return nil, nil, err
		}
		for _, file := range files {
			paths = append(paths, file.Path)
		}
	}
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("导入目录里没有可导入的文件 (只支持 %s)", importableSuffixes())
	}
	if len(paths) > 100 {
		return nil, nil, fmt.Errorf("单次最多导入 100 个文件")
	}

	var (
		documents []Document
		records   []ImportRecord
		problems  []string
	)
	for _, item := range paths {
		resolved, err := b.resolvedPathIn(item)
		if err != nil {
			// 路径不合规是**请求被拒**, 不算一次导入尝试: 不入导入记录 (否则
			// "有人试图读 /etc/passwd" 这类被挡下的请求会把用户的导入台账刷满),
			// 但它会出现在访问日志里, 需要追溯时查得到。
			problems = append(problems, filepath.Base(item)+": "+err.Error())
			continue
		}
		// 有上限地读: 先看大小再读 —— 一个 8 GB 的文件本该只是"这份超限, 跳过",
		// 而不是把服务读 OOM。
		content, err := fileio.ReadBounded(resolved, int64(b.options.MaxDocumentBytes))
		if err != nil {
			record := ImportRecord{
				ID: newID("imp"), At: time.Now(), Name: filepath.Base(resolved),
				Origin: OriginServer, OK: false, Error: "读取失败: " + err.Error(),
			}
			b.appendRecord(record)
			records = append(records, record)
			problems = append(problems, filepath.Base(resolved)+": 读取失败")
			continue
		}
		document, record, err := b.ImportFrom(filepath.Base(resolved), string(content), OriginServer)
		if err != nil {
			problems = append(problems, filepath.Base(resolved)+": "+err.Error())
			records = append(records, record)
			continue
		}
		documents = append(documents, document)
		records = append(records, record)
	}

	if len(documents) == 0 {
		if len(problems) > 0 {
			return nil, records, fmt.Errorf("%s", strings.Join(problems, "; "))
		}
		return nil, records, fmt.Errorf("没有可导入的文件")
	}
	return documents, records, nil
}

// isImportableName 判断文件名是否可导入 (只认文本类文档)。
func isImportableName(name string) bool {
	lowered := strings.ToLower(name)
	for _, suffix := range []string{".md", ".markdown", ".txt"} {
		if strings.HasSuffix(lowered, suffix) {
			return true
		}
	}
	return false
}

func importableSuffixes() string { return ".md / .markdown / .txt" }

// newID 生成带前缀的编号 (文档与导入记录共用, 便于日志里区分)。
