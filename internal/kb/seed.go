package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"backup-restore-agent/internal/fileio"
)

// 预置文档 (seed): 随部署一起装上来的 Markdown, 启动时同步进知识库。
//
// 为什么要有它: 一套装完就能用的知识库比"空库 + 一句请自行导入"有用得多 ——
// 尤其是刚部署、还没人写过手册的时候。做法是把文档放进一个目录随包发出去
// (deploy 里装到 /usr/share/<name>/knowledge), 服务启动时"缺什么补什么"。
//
// 同步规则 (刻意保守, 不跟操作者抢控制权):
//   - 名字在库里没有 → 导入, 标记为预置;
//   - 库里有同名文档, 且它也是预置的 → 内容不同就更新 (升级时刷新手册);
//   - 库里有同名文档, 但是操作者自己导入的 → **不动它** (他的版本优先), 只记一条日志;
//   - 操作者删掉过某个预置文档 → 记在 SeedIgnored 里, 以后不再补回来
//     (删了又自己长回来是最让人恼火的行为)。

// SeedResult 是一次同步的结果 (给启动日志用)。
type SeedResult struct {
	Added    []string
	Updated  []string
	Skipped  []string // 已存在同名文档, 未改动
	Ignored  []string // 操作者删过, 不再补
	Failed   []string
	Scanned  int
	EmptyDir bool
}

// Summary 生成一行可读的同步结果。
func (r SeedResult) Summary() string {
	if r.EmptyDir {
		return "预置目录为空或不存在, 跳过"
	}
	parts := []string{fmt.Sprintf("扫描 %d 份", r.Scanned)}
	if len(r.Added) > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d", len(r.Added)))
	}
	if len(r.Updated) > 0 {
		parts = append(parts, fmt.Sprintf("更新 %d", len(r.Updated)))
	}
	if len(r.Skipped) > 0 {
		parts = append(parts, fmt.Sprintf("跳过 %d (已有同名文档)", len(r.Skipped)))
	}
	if len(r.Ignored) > 0 {
		parts = append(parts, fmt.Sprintf("略过 %d (曾被删除)", len(r.Ignored)))
	}
	if len(r.Failed) > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d: %s", len(r.Failed), strings.Join(r.Failed, "; ")))
	}
	return strings.Join(parts, ", ")
}

// Seed 把预置目录里的文档同步进知识库。
//
// 目录不存在不算错误 (可能这个部署没带预置文档), 由调用方按 Summary 判断。
func (b *Base) Seed(dir string) (SeedResult, error) {
	result := SeedResult{}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		result.EmptyDir = true
		return result, nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return result, fmt.Errorf("预置目录无效: %w", err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		result.EmptyDir = true
		return result, nil
	}

	files := []string{}
	walkErr := filepath.WalkDir(absolute, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != absolute && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		if isImportableName(path) && !isSeedFolderDoc(absolute, path) {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		return result, walkErr
	}
	sort.Strings(files)
	result.Scanned = len(files)
	if len(files) == 0 {
		result.EmptyDir = true
		return result, nil
	}

	for _, path := range files {
		content, err := fileio.ReadBounded(path, int64(b.options.MaxDocumentBytes))
		if err != nil {
			result.Failed = append(result.Failed, filepath.Base(path)+": 读取失败")
			continue
		}
		name := filepath.Base(path)
		outcome, err := b.syncSeeded(name, string(content))
		if err != nil {
			result.Failed = append(result.Failed, name+": "+err.Error())
			continue
		}
		switch outcome {
		case seedAdded:
			result.Added = append(result.Added, name)
		case seedUpdated:
			result.Updated = append(result.Updated, name)
		case seedIgnored:
			result.Ignored = append(result.Ignored, name)
		default:
			result.Skipped = append(result.Skipped, name)
		}
	}
	return result, nil
}

// isSeedFolderDoc 判断这是不是"写给维护者的目录说明", 而不是知识文档。
//
// 预置目录顶层通常有个 README.md 说明怎么往里加文档 —— 那是**关于这个目录**的说明,
// 不是运维知识, 导进知识库只会在文档列表里多一份没用的东西 (而且它的内容还是
// "把 md 放进 knowledge/ 目录"这种对用户没意义的话)。子目录里的 README 不跳过:
// 那种通常是某本手册自己的说明页, 算内容。
func isSeedFolderDoc(seedRoot, path string) bool {
	relative, err := filepath.Rel(seedRoot, path)
	if err != nil {
		return false
	}
	if strings.Contains(relative, string(filepath.Separator)) {
		return false
	}
	name := strings.ToLower(filepath.Base(path))
	return name == "readme.md" || name == "readme.markdown" || name == "readme.txt"
}

type seedOutcome int

const (
	seedSkipped seedOutcome = iota
	seedAdded
	seedUpdated
	seedIgnored
)

// syncSeeded 按上面的规则同步一份文件。
func (b *Base) syncSeeded(name, content string) (seedOutcome, error) {
	b.mu.RLock()
	var existing *Document
	for index := range b.docs {
		if b.docs[index].Source == name {
			existing = &b.docs[index]
			break
		}
	}
	ignored := false
	for _, item := range b.seedIgnored {
		if item == name {
			ignored = true
			break
		}
	}
	b.mu.RUnlock()

	if existing == nil {
		if ignored {
			return seedIgnored, nil
		}
		if _, _, err := b.ImportFrom(name, content, OriginSeed); err != nil {
			return seedSkipped, err
		}
		b.markSeeded(name, true)
		return seedAdded, nil
	}

	// 已存在: 只有"也是预置的"才更新, 操作者自己的版本一律不动
	if !existing.Seeded {
		return seedSkipped, nil
	}
	if strings.TrimSpace(existing.Content) == strings.TrimSpace(content) {
		return seedSkipped, nil
	}
	if _, _, err := b.ImportFrom(name, content, OriginSeed); err != nil {
		return seedSkipped, err
	}
	b.markSeeded(name, true)
	return seedUpdated, nil
}

// markSeeded 给同名文档打上"预置"标记 (导入会重建文档, 所以要按名字重新打)。
func (b *Base) markSeeded(name string, seeded bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for index := range b.docs {
		if b.docs[index].Source == name {
			b.docs[index].Seeded = seeded
		}
	}
	b.persistOrWarn("更新预置文档标记")
}

// SeedIgnoredNames 返回被操作者删除、不再补回的预置文档名。
func (b *Base) SeedIgnoredNames() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.seedIgnored))
	copy(out, b.seedIgnored)
	return out
}
