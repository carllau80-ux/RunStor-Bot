package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 服务器目录导入是"按路径读文件"的能力 —— 写松了就是任意文件读取漏洞
// (这个服务没有鉴权, 任何能连到端口的人都能把 /etc/shadow 读进知识库再检索出来)。
// 这一组测试全是围绕"越界必须被拒绝"。
func TestServerImportSandboxRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// 导入目录外面放一个"机密文件"与一个"机密目录"
	secret := filepath.Join(root, "secret.md")
	if err := os.WriteFile(secret, []byte("# 机密\n\n不该被读到。\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "leak.md"), []byte("# 泄漏\n\n内容。\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	// 目录内的软链接指向外部文件 (只做字符串前缀检查会漏掉这种)
	if err := os.Symlink(secret, filepath.Join(inbox, "link.md")); err != nil {
		t.Fatalf("建软链失败: %v", err)
	}
	// 合法的文件
	if err := os.WriteFile(filepath.Join(inbox, "good.md"), []byte("# 正常\n\n合法内容。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	options := DefaultOptions()
	options.Path = filepath.Join(root, "kb.json")
	options.ImportDir = inbox
	base := New(options)

	cases := []struct {
		name string
		path string
	}{
		{"相对路径穿越", "../secret.md"},
		{"多级穿越", "../../etc/passwd"},
		{"指向外部的软链接", "link.md"},
		{"穿越到外部目录", "../outside/leak.md"},
		{"绝对路径", "/etc/passwd"},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			if _, _, err := base.ImportFromInbox([]string{entry.path}); err == nil {
				t.Fatalf("%q 必须被拒绝", entry.path)
			}
			// 不管报什么错, 关键是内容没进知识库
			for _, doc := range base.List() {
				if strings.Contains(doc.Source, "secret") || strings.Contains(doc.Source, "leak") {
					t.Fatalf("越界文件被导入了: %s", doc.Source)
				}
			}
			// 被拒的请求不算"一次导入尝试", 不该进导入台账 (会刷满用户的历史);
			// 它出现在访问日志里, 需要追溯时查得到。
			for _, record := range base.Records(10) {
				if strings.Contains(record.Name, "secret") || strings.Contains(record.Name, "passwd") {
					t.Errorf("被拒的路径不该留下导入记录: %+v", record)
				}
			}
		})
	}

	// 目录内的正常文件必须能导入
	documents, records, err := base.ImportFromInbox([]string{"good.md"})
	if err != nil {
		t.Fatalf("合法文件应能导入: %v", err)
	}
	if len(documents) != 1 || documents[0].Title != "正常" {
		t.Fatalf("导入结果不对: %+v", documents)
	}
	if len(records) != 1 || records[0].Origin != OriginServer || !records[0].OK {
		t.Errorf("应留下一条来源为服务器目录的成功记录: %+v", records)
	}
}

// 只认文本类文档: 别让 .sh / .key / .env 这类文件被顺手导进知识库。
func TestServerImportOnlyTextDocuments(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"doc.md":       "# 文档\n\n内容。\n",
		"doc.markdown": "# 文档\n\n内容。\n",
		"notes.txt":    "内容。\n",
		"script.sh":    "#!/bin/sh\necho hi\n",
		// secret-scan:allow-test-key —— "key.pem" 是**假**样本 (正文只有一个 xxx),
		// 用于验证导入器会拒绝非文档后缀的文件。
		"key.pem":       "-----BEGIN PRIVATE KEY-----\nxxx\n",
		".env":          "SECRET=1\n",
		".hide.md":      "# 隐藏\n\n内容。\n",
		"sub/nested.md": "# 子目录\n\n内容。\n",
	}
	for name, content := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}

	options := DefaultOptions()
	options.Path = filepath.Join(root, "kb.json")
	options.ImportDir = root
	base := New(options)

	listed, err := base.ListInbox()
	if err != nil {
		t.Fatalf("列出失败: %v", err)
	}
	names := map[string]bool{}
	for _, file := range listed {
		names[file.Path] = true
	}
	for _, want := range []string{"doc.md", "doc.markdown", "notes.txt", filepath.Join("sub", "nested.md")} {
		if !names[want] {
			t.Errorf("应列出 %s, 实际 %v", want, names)
		}
	}
	for _, banned := range []string{"script.sh", "key.pem", ".env"} {
		if names[banned] {
			t.Errorf("不该列出 %s", banned)
		}
	}
	// 隐藏文件 (.hide.md) 也不列: 多半是编辑器/版本控制的临时文件
	if names[".hide.md"] {
		t.Error("不该列出隐藏文件")
	}

	// 显式指名导入非文本文件也要被拒
	if _, _, err := base.ImportFromInbox([]string{"script.sh"}); err == nil {
		t.Error("非文本文件必须被拒绝")
	}
}

// 目录不存在 / 未配置时要给出可读的错误, 而不是崩或静默成功。
func TestServerImportWithoutDirectory(t *testing.T) {
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	base := New(options) // 未配置 ImportDir

	if _, err := base.ListInbox(); err == nil {
		t.Error("未配置导入目录时应报错")
	}
	if _, _, err := base.ImportFromInbox(nil); err == nil {
		t.Error("未配置导入目录时导入应报错")
	}

	// 配置了但目录不存在: 列出时当作空, 不报错 (管理员还没放文件而已)
	options.ImportDir = filepath.Join(t.TempDir(), "not-created")
	second := New(options)
	files, err := second.ListInbox()
	if err != nil {
		t.Errorf("目录不存在不该报错: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("应列为空, 实际 %d 个", len(files))
	}
	if _, _, err := second.ImportFromInbox(nil); err == nil {
		t.Error("目录里没有文件时应报错说明")
	}
}

// 整目录导入: 一次把导入目录里的文档都收进来, 并逐条留记录。
func TestServerImportWholeDirectory(t *testing.T) {
	root := t.TempDir()
	for index, name := range []string{"a.md", "b.md", "c.md"} {
		content := "# 文档" + name + "\n\n这是第 " + string(rune('一'+index)) + " 份。\n"
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}
	// 一份空文件: 应当逐条失败, 不影响其它文件
	if err := os.WriteFile(filepath.Join(root, "empty.md"), []byte("   \n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	options := DefaultOptions()
	options.Path = filepath.Join(root, "kb.json")
	options.ImportDir = root
	base := New(options)

	documents, records, err := base.ImportFromInbox(nil)
	if err != nil {
		t.Fatalf("整目录导入应成功: %v", err)
	}
	if len(documents) != 3 {
		t.Errorf("应导入 3 份, 实际 %d", len(documents))
	}
	if len(records) != 4 {
		t.Errorf("每份文件都该有记录 (含失败的那份), 实际 %d 条", len(records))
	}
	var failures int
	for _, record := range records {
		if !record.OK {
			failures++
			if record.Error == "" {
				t.Error("失败记录必须带原因")
			}
		}
	}
	if failures != 1 {
		t.Errorf("应有 1 条失败记录, 实际 %d", failures)
	}
}

// 导入记录要落盘并在重启后还在。
func TestImportRecordsPersist(t *testing.T) {
	dir := t.TempDir()
	options := DefaultOptions()
	options.Path = filepath.Join(dir, "kb.json")
	base := New(options)

	if _, _, err := base.ImportFrom("手册.md", "# 手册\n\n内容。\n", OriginBrowser); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 同名再导一次: 应记下"替换了谁"
	if _, record, err := base.ImportFrom("手册.md", "# 手册\n\n改过的内容。\n", OriginPaste); err != nil {
		t.Fatalf("重新导入失败: %v", err)
	} else if record.Replaced != "手册" {
		t.Errorf("记录里应写明替换了哪份文档, 实际 %q", record.Replaced)
	}
	// 一次失败也要留记录
	if _, _, err := base.ImportFrom("空.md", "   ", OriginBrowser); err == nil {
		t.Fatal("空文档应报错")
	}

	reloaded := New(options)
	records := reloaded.Records(0)
	if len(records) != 3 {
		t.Fatalf("重启后应有 3 条记录, 实际 %d", len(records))
	}
	// 新的在前
	if !records[0].At.After(records[len(records)-1].At) && !records[0].At.Equal(records[len(records)-1].At) {
		t.Error("记录应按时间倒序")
	}
	origins := map[string]int{}
	for _, record := range records {
		origins[record.Origin]++
	}
	if origins[OriginBrowser] != 2 || origins[OriginPaste] != 1 {
		t.Errorf("来源应被如实记录: %v", origins)
	}
	if len(reloaded.List()) != 1 {
		t.Error("替换语义下应只有 1 份文档")
	}

	// 清空记录不影响文档
	if count := reloaded.ClearRecords(); count != 3 {
		t.Errorf("应清掉 3 条, 实际 %d", count)
	}
	if len(New(options).Records(0)) != 0 {
		t.Error("清空后不该还有记录")
	}
	if len(reloaded.List()) != 1 {
		t.Error("清空记录不该影响文档")
	}
}

// 记录条数有上限, 不能让落盘文件无限长。
func TestImportRecordsAreBounded(t *testing.T) {
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	options.MaxRecords = 5
	base := New(options)

	for index := 0; index < 12; index++ {
		if _, _, err := base.ImportFrom("doc.md", "# 标题\n\n内容"+string(rune('a'+index))+"。\n", OriginBrowser); err != nil {
			t.Fatalf("第 %d 次导入失败: %v", index, err)
		}
	}
	if records := base.Records(0); len(records) != 5 {
		t.Errorf("记录应被限制在 5 条, 实际 %d", len(records))
	}
}

// InboxFile 要标出"这个文件在知识库里已经有了" (导入会替换它)。
func TestInboxMarksAlreadyImported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "手册.md"), []byte("# 手册\n\n内容。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "新手册.md"), []byte("# 新手册\n\n内容。\n"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	options := DefaultOptions()
	options.Path = filepath.Join(root, "kb.json")
	options.ImportDir = root
	base := New(options)
	if _, _, err := base.ImportFromInbox([]string{"手册.md"}); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	files, err := base.ListInbox()
	if err != nil {
		t.Fatalf("列出失败: %v", err)
	}
	state := map[string]bool{}
	for _, file := range files {
		state[file.Name] = file.Imported
	}
	if !state["手册.md"] {
		t.Error("已导入的文件应标记出来")
	}
	if state["新手册.md"] {
		t.Error("没导入过的文件不该标记")
	}
}

// 单条记录删除: 部署自检留下的几条测试记录要能单独清掉, 而不是只能全清。
func TestDeleteSingleRecord(t *testing.T) {
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	base := New(options)

	var ids []string
	for index := 0; index < 3; index++ {
		_, record, err := base.ImportFrom("doc.md", "# 文档\n\n内容"+string(rune('a'+index))+"。\n", OriginBrowser)
		if err != nil {
			t.Fatalf("导入失败: %v", err)
		}
		ids = append(ids, record.ID)
	}
	if !base.DeleteRecord(ids[1]) {
		t.Fatal("删除记录应成功")
	}
	if base.DeleteRecord(ids[1]) {
		t.Error("重复删除应返回 false")
	}
	if base.DeleteRecord("imp-nope") {
		t.Error("不存在的编号应返回 false")
	}
	records := base.Records(0)
	if len(records) != 2 {
		t.Fatalf("应剩 2 条记录, 实际 %d", len(records))
	}
	for _, record := range records {
		if record.ID == ids[1] {
			t.Error("被删的记录不该还在")
		}
	}
	// 落盘后也要保持
	if len(New(options).Records(0)) != 2 {
		t.Error("重启后应仍是 2 条")
	}
	if len(base.List()) != 1 {
		t.Error("删记录不该影响文档")
	}
}
