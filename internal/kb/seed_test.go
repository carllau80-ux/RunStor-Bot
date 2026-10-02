package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedBase 建一个有预置目录的知识库。
func seedBase(t *testing.T, files map[string]string) (*Base, string) {
	t.Helper()
	dir := t.TempDir()
	seedDir := filepath.Join(dir, "knowledge")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("建预置目录失败: %v", err)
	}
	for name, content := range files {
		full := filepath.Join(seedDir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}
	options := DefaultOptions()
	options.Path = filepath.Join(dir, "kb.json")
	return New(options), seedDir
}

// 首次启动: 预置文档进知识库, 并且能被检索到 —— 这就是"部署完就能用"。
func TestSeedImportsBundledDocuments(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"备份策略设计.md":             "# 备份策略设计\n\n## 保留策略\n\n推荐 3-2-1: 3 份数据、2 种介质、1 份异地。\n",
		"en/backup-strategy.md": "# Backup strategy\n\nUse 3-2-1: three copies, two media, one off-site.\n",
	})

	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Added) != 2 || result.Scanned != 2 {
		t.Fatalf("应新增 2 份: %+v", result)
	}
	if summary := result.Summary(); !strings.Contains(summary, "新增 2") {
		t.Errorf("结果摘要应说明新增数量: %q", summary)
	}

	documents, _ := base.Count()
	if documents != 2 {
		t.Fatalf("知识库里应有 2 份文档, 实际 %d", documents)
	}
	// 预置文档要能被检索 (否则预置就没意义了)
	if results := base.Search("保留策略 3-2-1", 2); len(results) == 0 {
		t.Error("预置文档应能检索到")
	}
	if results := base.Search("three copies two media", 2); len(results) == 0 {
		t.Error("英文预置文档也应能检索到")
	}
	// 标记为预置 (升级时会跟着刷新)
	for _, doc := range base.List() {
		if !doc.Seeded {
			t.Errorf("预置导入的文档应带 Seeded 标记: %s", doc.Source)
		}
	}
	// 导入记录里来源是"部署预置"
	records := base.Records(0)
	if len(records) != 2 || records[0].Origin != OriginSeed {
		t.Errorf("记录来源应为 %q: %+v", OriginSeed, records)
	}
}

// 重复启动是幂等的: 不该每次都重新导入 (那会清掉引用、刷爆导入记录)。
func TestSeedIsIdempotent(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"runbook.md": "# 手册\n\n内容。\n",
	})
	if _, err := base.Seed(seedDir); err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	before := len(base.Records(0))

	second, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("再次同步失败: %v", err)
	}
	if len(second.Added) != 0 || len(second.Updated) != 0 {
		t.Errorf("第二次同步不该再导入: %+v", second)
	}
	if len(second.Skipped) != 1 {
		t.Errorf("应记为跳过: %+v", second)
	}
	if after := len(base.Records(0)); after != before {
		t.Errorf("幂等同步不该新增导入记录: %d → %d", before, after)
	}
	if documents, _ := base.Count(); documents != 1 {
		t.Errorf("文档数不该变: %d", documents)
	}
}

// 预置文档内容变了 (升级带了新版手册) → 更新。
func TestSeedUpdatesChangedBundledDocument(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"runbook.md": "# 手册\n\n第一版。\n",
	})
	if _, err := base.Seed(seedDir); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "runbook.md"), []byte("# 手册\n\n第二版。\n"), 0o644); err != nil {
		t.Fatalf("改写失败: %v", err)
	}

	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("再次同步失败: %v", err)
	}
	if len(result.Updated) != 1 {
		t.Fatalf("内容变化应触发更新: %+v", result)
	}
	if results := base.Search("第二版", 2); len(results) == 0 {
		t.Error("应检索到新内容")
	}
	if results := base.Search("第一版", 2); len(results) > 0 &&
		strings.Contains(results[0].Text, "第一版。") {
		t.Error("旧内容应已被替换")
	}
}

// 操作者自己导入的同名文档优先: 预置同步不覆盖它。
//
// 这是最重要的一条 —— "我特意换了这份手册, 部署一次又给我改回去了" 是不能接受的。
func TestSeedDoesNotOverwriteOperatorDocument(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"runbook.md": "# 手册\n\n预置版本。\n",
	})
	// 操作者先导入了自己的一份 (同名)
	if _, err := base.Import("runbook.md", "# 手册\n\n我们自己的版本, 有内网地址。\n"); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Added) != 0 || len(result.Updated) != 0 {
		t.Fatalf("不该动操作者的文档: %+v", result)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("应记为跳过: %+v", result)
	}
	document, _, _ := base.Content(base.List()[0].ID)
	if !strings.Contains(document.Content, "我们自己的版本") {
		t.Errorf("操作者的内容应原样保留, 实际: %q", document.Content)
	}
}

// 删掉的预置文档不该在下次启动时"自己长回来"。
func TestSeedDoesNotResurrectDeletedDocument(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"runbook.md": "# 手册\n\n内容。\n",
		"other.md":   "# 另一份\n\n内容。\n",
	})
	if _, err := base.Seed(seedDir); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 操作者删掉其中一份
	var targetID string
	for _, doc := range base.List() {
		if doc.Source == "runbook.md" {
			targetID = doc.ID
		}
	}
	if targetID == "" || !base.Delete(targetID) {
		t.Fatal("删除失败")
	}

	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("再次同步失败: %v", err)
	}
	if len(result.Ignored) != 1 || result.Ignored[0] != "runbook.md" {
		t.Fatalf("删过的文档应被略过: %+v", result)
	}
	for _, doc := range base.List() {
		if doc.Source == "runbook.md" {
			t.Error("删掉的预置文档不该被补回来")
		}
	}
	// 删除状态要落盘 (重启后依然不补)
	reloaded := New(Options{Path: base.Path()})
	again, err := reloaded.Seed(seedDir)
	if err != nil {
		t.Fatalf("重启后同步失败: %v", err)
	}
	if len(again.Ignored) != 1 {
		t.Errorf("重启后仍应略过: %+v", again)
	}
}

// 目录不存在 / 为空 / 未配置: 都不是错误, 只是"没有预置文档"。
func TestSeedToleratesMissingDirectory(t *testing.T) {
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	base := New(options)

	for _, dir := range []string{"", filepath.Join(t.TempDir(), "not-created")} {
		result, err := base.Seed(dir)
		if err != nil {
			t.Fatalf("目录 %q 不该报错: %v", dir, err)
		}
		if !result.EmptyDir {
			t.Errorf("应标记为空目录: %+v", result)
		}
		if !strings.Contains(result.Summary(), "跳过") {
			t.Errorf("摘要应说明跳过: %q", result.Summary())
		}
	}

	// 目录里没有可导入文件 (只有非文本) 也一样
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	result, err := base.Seed(dir)
	if err != nil || !result.EmptyDir {
		t.Errorf("没有 .md 时应视为空: %+v / %v", result, err)
	}
}

// 单份预置文档坏了 (读不了/内容为空) 不该影响其它文档。
func TestSeedReportsPerFileFailures(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"good.md":  "# 正常\n\n内容。\n",
		"empty.md": "   \n",
	})
	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Added) != 1 {
		t.Errorf("好的那份应导入: %+v", result)
	}
	if len(result.Failed) != 1 || !strings.Contains(result.Failed[0], "empty.md") {
		t.Errorf("坏的那份应逐条报出: %+v", result)
	}
}

// 随部署发布的那套文档本身要是可解析的: 标题、章节、能被检索到。
func TestBundledKnowledgeDocumentsAreUsable(t *testing.T) {
	root := filepath.Join("..", "..", "knowledge")
	if _, err := os.Stat(root); err != nil {
		t.Skip("没有随部署的 knowledge 目录")
	}
	base, err := func() (*Base, error) {
		options := DefaultOptions()
		options.Path = filepath.Join(t.TempDir(), "kb.json")
		return New(options), nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	result, err := base.Seed(root)
	if err != nil {
		t.Fatalf("同步随部署文档失败: %v", err)
	}
	if len(result.Added) < 4 {
		t.Fatalf("随部署文档太少 (至少中英各两份): %+v", result)
	}
	if len(result.Failed) > 0 {
		t.Errorf("随部署文档应全部可用: %+v", result.Failed)
	}
	// 每份都该有章节 (否则引用只能落到"全文")
	for _, doc := range base.List() {
		if doc.Chunks < 2 {
			t.Errorf("%s 只有 %d 个片段, 章节切分可能有问题", doc.Source, doc.Chunks)
		}
	}
	// 中英各能检索到
	for _, query := range []string{"保留策略 3-2-1", "checksum mismatch 怎么处理", "three copies off-site"} {
		if results := base.Search(query, 3); len(results) == 0 {
			t.Errorf("%q 应能检索到随部署的文档", query)
		}
	}
}

// 预置目录顶层的 README 是"目录说明", 不是知识文档: 不该进知识库。
//
// 真实症状: 部署后文档列表里多了一份《预置知识库文档》, 内容是"把 md 放进 knowledge/
// 目录"—— 对使用者毫无意义, 还占一个检索位置。
func TestSeedSkipsFolderReadme(t *testing.T) {
	base, seedDir := seedBase(t, map[string]string{
		"README.md":     "# 预置知识库文档\n\n把 md 放进 knowledge/ 目录即可。\n",
		"readme.txt":    "# 目录说明\n\n同上。\n",
		"zh/runbook.md": "# 手册\n\n内容。\n",
		"zh/README.md":  "# 手册的说明页\n\n这一份算内容, 不跳过。\n",
	})
	result, err := base.Seed(seedDir)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 按内容判断而不是按文件名: 顶层与子目录可能同名 (都叫 README.md),
	// 而"子目录里的说明页"是该导的。
	joined := ""
	for _, doc := range base.List() {
		joined += doc.Content + "\n"
	}
	if strings.Contains(joined, "放进 knowledge/ 目录") {
		t.Error("顶层 README 是目录说明, 不该进知识库")
	}
	if strings.Contains(joined, "同上。") {
		t.Error("顶层 readme.txt 也不该进知识库")
	}
	if !strings.Contains(joined, "手册的说明页") {
		t.Error("子目录里的说明页算内容, 应该导入")
	}
	if !strings.Contains(joined, "内容。") {
		t.Error("子目录里的手册应导入")
	}
	if len(result.Added) != 2 {
		t.Errorf("应导入 2 份 (子目录手册 + 子目录说明页), 实际 %+v", result)
	}
}
