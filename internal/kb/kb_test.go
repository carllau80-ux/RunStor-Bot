package kb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testBase(t *testing.T) *Base {
	t.Helper()
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	return New(options)
}

// 真实感的运维文档: 检索质量只能靠"看起来像真文档"的语料检验, 编几个短句是测不出
// 排序问题的。
// 这两份是**真实感的运维手册**: 语料长度与结构直接影响 BM25 的绝对分数, 用几句
// 话的桩文档测出来的阈值在真机上没有意义 (这正是当初把相关度阈值定错的原因)。
const zfsDoc = `# ZFS 存储池运维手册

## 日常检查

每周执行一次 zpool status 与 zpool list, 确认没有 DEGRADED/FAULTED, 且没有新增的错误计数。

## 校验失败 (checksum mismatch)

zpool status -v 出现 checksum mismatch 时, ZFS 从磁盘读出的数据与校验和不符。

影响: 修不好说明同一块数据的所有副本都已损坏, 属于静默数据损坏, 受影响文件可能已经读不出来。

处置流程:

1. 记录 zpool status -v 列出的受影响文件清单 (要写进变更工单);
2. 确认最近一次备份不在同一个池上, 并实际验证备份可读;
3. 执行 zpool scrub rpool, 让 ZFS 修复可修复的块;
4. 检查硬件: 内存用 memtest86+ 过一遍, 检查数据线、HBA/背板、电源。

## 存储池降级与换盘

1. zpool status 确认故障盘的 vdev 路径;
2. 物理更换后执行 zpool replace rpool 旧盘 新盘;
3. 等待 resilver 完成, 期间避免大批量写入。
`

const iscsiDoc = `# iSCSI 存储链路故障处置

## 登录被拒绝 / 安全协商失败

日志出现下列任一内容时, 说明目标端拒绝了发起端的登录:

- iSCSI Login negotiation failed.
- Security negotiation failed.
- is not authorized to access iSCSI target portal group

处置步骤:

1. 在 target 侧核对 ACL: 把发起端 IQN (cat /etc/iscsi/initiatorname.iscsi) 加入白名单;
2. 核对 CHAP: /etc/iscsi/iscsid.conf 里的用户名与密码必须与存储侧一致, 启用双向认证时还要填 username_in;
3. 确认网络可达: nc -zv 目标IP 3260, 并确认防火墙放行了 3260;
4. 修正配置后 systemctl restart iscsid open-iscsi, 再 iscsiadm -m session -R 重新登录。

## MODE SENSE: unimplemented page

MODE SENSE: unimplemented page/subpage 是内核在探测设备能力时得到的"不支持该可选页"回应。
它本身不代表读写失败, 常见于 iSCSI 目标、虚拟化设备与磁带机。

只有当同一设备同时出现 I/O error 或性能下降时才需要处理: 此时联系存储侧确认设备型号与固件版本。
`

const memoryDoc = `# 内存与 OOM 处置

## 进程被 OOM killer 杀掉

内核日志出现 "Out of memory: Killed process" 说明内存耗尽, 内核杀掉了占用最多的进程。

- 确认被杀进程与所属服务是否已恢复 (systemctl status);
- 查看内存大户: ps aux --sort=-%mem;
- 给关键服务设置 MemoryMax, 避免被优先杀掉;
- 长期方案是扩容内存或排查泄漏。
`

const backupDoc = `# 备份策略设计

## 全备与增量

全量备份建议每周一次, 增量每天一次。保留策略通常按"日 7 天、周 4 周、月 12 月"设置。

## 恢复演练

备份如果不演练就等于没有备份。建议每季度做一次恢复演练, 并记录恢复时间。
`

// 核心验收: 用运维人员真会问的话去检索, 期望命中文档。
//
// 这些查询刻意不是"照抄文档标题": 真实提问会说"磁盘写满了怎么办", 而文档写的是
// "空间不足处置"。中文没有分词器, 全靠二元组召回, 这组用例就是在检验它够不够用。
func TestSearchFindsRightDocument(t *testing.T) {
	base := testBase(t)
	for name, content := range map[string]string{
		"zfs-troubleshooting.md":   zfsDoc,
		"iscsi-troubleshooting.md": iscsiDoc,
		"memory-oom.md":            memoryDoc,
		"backup-strategy.md":       backupDoc,
	} {
		if _, err := base.Import(name, content); err != nil {
			t.Fatalf("导入 %s 失败: %v", name, err)
		}
	}

	cases := []struct {
		question string
		wantDoc  string
		wantWord string
	}{
		{"zpool 校验失败怎么办", "ZFS 存储池运维手册", "checksum"},
		{"checksum mismatch 怎么处理", "ZFS 存储池运维手册", "scrub"},
		{"存储池降级了, 换盘流程是什么", "ZFS 存储池运维手册", "resilver"},
		{"iSCSI 登录被拒绝", "iSCSI 存储链路故障处置", "ACL"},
		{"CHAP 认证失败怎么排查", "iSCSI 存储链路故障处置", "CHAP"},
		{"进程被 OOM killer 杀掉了", "内存与 OOM 处置", "MemoryMax"},
		{"内存耗尽怎么处理", "内存与 OOM 处置", "OOM"},
		{"全量备份和增量备份怎么安排", "备份策略设计", "增量"},
		{"恢复演练多久做一次", "备份策略设计", "演练"},
	}
	for _, entry := range cases {
		results := base.Search(entry.question, 3)
		if len(results) == 0 {
			t.Errorf("%q 没检索到任何片段", entry.question)
			continue
		}
		if results[0].Doc != entry.wantDoc {
			t.Errorf("%q 应命中《%s》, 实际命中《%s》: %s",
				entry.question, entry.wantDoc, results[0].Doc,
				describeResults(results))
			continue
		}
		if !strings.Contains(strings.ToLower(results[0].Text), strings.ToLower(entry.wantWord)) {
			t.Errorf("%q 命中的片段里没有 %q: %s", entry.question, entry.wantWord,
				Snippet(results[0].Text, 120))
		}
	}
}

// 完全不相干的提问不能硬凑出结果 —— 检索到噪声会让模型编出"看起来有依据"的答案。
func TestSearchRejectsIrrelevantQuestion(t *testing.T) {
	base := testBase(t)
	if _, err := base.Import("zfs.md", zfsDoc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	results := base.Search("今天午饭吃什么", 5)
	for _, result := range results {
		// 允许极弱的偶然命中 (单字), 但必须是低分
		if result.Score > 0.5 {
			t.Errorf("不相干的提问不该有高分结果: %.3f %s", result.Score, Snippet(result.Text, 60))
		}
	}
}

// 同名文档要替换而不是并存: 改完文档重新导入是最常见的动作, 留着旧版本会让检索
// 命中已经过时的内容。
func TestImportReplacesSameDocument(t *testing.T) {
	base := testBase(t)
	if _, err := base.Import("runbook.md", "# 旧标题\n\n旧内容: 使用 zpool scrub 修复。\n"); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if _, err := base.Import("runbook.md", "# 新标题\n\n新内容: 改为 zpool replace。\n"); err != nil {
		t.Fatalf("替换失败: %v", err)
	}
	documents, _ := base.Count()
	if documents != 1 {
		t.Fatalf("同名文档应替换, 实际 %d 份", documents)
	}
	if results := base.Search("zpool replace", 3); len(results) == 0 ||
		!strings.Contains(results[0].Text, "replace") {
		t.Errorf("应检索到新内容: %s", describeResults(results))
	}
	if results := base.Search("zpool scrub", 3); len(results) > 0 &&
		strings.Contains(results[0].Text, "scrub 修复") {
		t.Error("旧内容不该还在索引里")
	}
}

// 删除文档后索引也要干净。
func TestDeleteRemovesFromIndex(t *testing.T) {
	base := testBase(t)
	document, err := base.Import("zfs.md", zfsDoc)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(base.Search("checksum mismatch", 3)) == 0 {
		t.Fatal("删除前应能检索到")
	}
	if !base.Delete(document.ID) {
		t.Fatal("删除应成功")
	}
	if len(base.Search("checksum mismatch", 3)) != 0 {
		t.Error("删除后不该还能检索到")
	}
	if documents, chunks := base.Count(); documents != 0 || chunks != 0 {
		t.Errorf("删除后应为空: %d 份 / %d 片段", documents, chunks)
	}
	if base.Delete(document.ID) {
		t.Error("重复删除应返回 false")
	}
}

// 重启后知识库要还在 (落盘 + 重新加载 + 重建索引)。
func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	options := DefaultOptions()
	options.Path = path

	first := New(options)
	if _, err := first.Import("zfs-troubleshooting.md", zfsDoc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if _, err := first.Import("iscsi.md", iscsiDoc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	second := New(options)
	documents, chunks := second.Count()
	if documents != 2 || chunks == 0 {
		t.Fatalf("重新加载后应有 2 份文档, 实际 %d 份 / %d 片段", documents, chunks)
	}
	results := second.Search("存储池降级换盘", 3)
	if len(results) == 0 || results[0].Doc != "ZFS 存储池运维手册" {
		t.Errorf("重新加载后检索应正常: %s", describeResults(results))
	}
	// 落盘文件权限必须是 0600: 文档里可能写有内网地址、账号等信息
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("落盘文件权限应为 0600, 实际 %04o", mode)
	}
}

// 落盘文件损坏时不能连服务一起拖垮。
func TestCorruptStoreStillStarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}
	options := DefaultOptions()
	options.Path = path
	base := New(options)
	if documents, _ := base.Count(); documents != 0 {
		t.Errorf("坏文件应被当作空库, 实际 %d 份文档", documents)
	}
	if _, err := base.Import("new.md", "# 新文档\n\n内容。\n"); err != nil {
		t.Errorf("坏文件之后仍应能导入: %v", err)
	}
}

// 各种上限要给出可读的错误, 而不是静默截断。
func TestLimits(t *testing.T) {
	options := DefaultOptions()
	options.Path = filepath.Join(t.TempDir(), "kb.json")
	options.MaxDocuments = 2
	options.MaxDocumentBytes = 200
	options.MaxTotalBytes = 400
	base := New(options)

	if _, err := base.Import("big.md", "# 标题\n\n"+strings.Repeat("很长的内容", 100)); err == nil {
		t.Error("超过单份大小上限应报错")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应说明上限: %v", err)
	}
	if _, err := base.Import("empty.md", "   \n"); err == nil {
		t.Error("空文档应报错")
	}
	for index := 0; index < 2; index++ {
		if _, err := base.Import(string(rune('a'+index))+".md", "# 文档\n\n内容一二三。\n"); err != nil {
			t.Fatalf("第 %d 份导入失败: %v", index+1, err)
		}
	}
	if _, err := base.Import("third.md", "# 第三份\n\n内容。\n"); err == nil {
		t.Error("超过文档数上限应报错")
	}
}

func describeResults(results []Result) string {
	parts := make([]string, 0, len(results))
	for _, result := range results {
		parts = append(parts, result.Doc+" ("+f2s(result.Score)+"): "+Snippet(result.Text, 40))
	}
	if len(parts) == 0 {
		return "(无结果)"
	}
	return strings.Join(parts, " | ")
}

func f2s(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }

// 章节路径不该重复文档标题 (引用会变成"《X》§ X > 小节", 读着像口吃)。
func TestHeadingPathDropsDocumentTitle(t *testing.T) {
	base := testBase(t)
	document, err := base.Import("iscsi.md", iscsiDoc)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	results := base.Search("登录被拒绝 排查顺序", 3)
	if len(results) == 0 {
		t.Fatal("应检索到片段")
	}
	for _, result := range results {
		if strings.HasPrefix(result.Heading, document.Title+" > ") {
			t.Errorf("章节路径里不该重复文档标题: %q", result.Heading)
		}
	}
	if results[0].Heading == "" {
		t.Error("应该保留文档内部的层级 (登录被拒绝)")
	}
	if !strings.Contains(results[0].Heading, "登录被拒绝") {
		t.Errorf("章节路径应是文档内部层级, 实际 %q", results[0].Heading)
	}
}

// 弱命中不该被当作"相关知识"挂到巡检诊断下面: 那会把人引到无关章节。
//
// 这里用的是真实案例: 日志行 `db_root: cannot open: /etc/target` 被判为"数据库错误",
// 而手册里有"数据"二字, 于是一度挂上了《ZFS 手册》的校验失败那节。
func TestWeakMatchIsNotAttachedAsKnowledge(t *testing.T) {
	base := testBase(t)
	for name, content := range map[string]string{
		"zfs-troubleshooting.md": zfsDoc, "iscsi-troubleshooting.md": iscsiDoc,
	} {
		if _, err := base.Import(name, content); err != nil {
			t.Fatalf("导入失败: %v", err)
		}
	}

	// 弱命中: 只因为"数据"两个字撞上 ZFS 手册
	if refs := base.LookupKnowledge("数据库错误 database", 2); len(refs) != 0 {
		t.Errorf("弱命中不该被引用: %+v", refs)
	}
	// 强命中: 术语与标题都对上
	refs := base.LookupKnowledge("iSCSI 存储链路认证/授权失败 iscsi_auth", 2)
	if len(refs) == 0 {
		t.Fatal("术语命中的应给出引用")
	}
	if !strings.Contains(refs[0].Doc, "iSCSI") {
		t.Errorf("应指向 iSCSI 手册, 实际 %q", refs[0].Doc)
	}
}

// 相关度阈值的两侧都要钉住: 偶然命中要挡掉, 真命中不能误伤。
//
// 这条测试的存在是为了防止有人"顺手把阈值调高一点": 4.0 就会把 CHAP 那一节挡掉,
// 而那正是手册里写着的内容。
func TestKnowledgeScoreThresholdBothSides(t *testing.T) {
	base := testBase(t)
	for name, content := range map[string]string{
		"zfs-troubleshooting.md": zfsDoc, "iscsi-troubleshooting.md": iscsiDoc,
		"memory-oom.md": memoryDoc,
	} {
		if _, err := base.Import(name, content); err != nil {
			t.Fatalf("导入失败: %v", err)
		}
	}

	// 真命中: 必须给引用 (这些查询来自巡检里真实会产生的诊断标题)
	mustMatch := []string{
		"iSCSI 存储链路认证/授权失败 iscsi_auth",
		"内存耗尽, 进程被内核杀死 (OOM) oom",
		"SCSI 设备不支持被查询的能力 (兼容性提示) scsi_quirk",
		"CHAP 认证失败怎么排查",
	}
	for _, query := range mustMatch {
		if refs := base.LookupKnowledge(query, 2); len(refs) == 0 {
			results := base.Search(query, 2)
			t.Errorf("%q 应给出知识引用, 实际最高分 %s", query, describeResults(results))
		}
	}

	// 偶然命中: 必须不给引用
	mustNotMatch := []string{"数据库错误 database", "空间不足 no_space"}
	for _, query := range mustNotMatch {
		if refs := base.LookupKnowledge(query, 2); len(refs) > 0 {
			t.Errorf("%q 是偶然命中, 不该给引用: %f 《%s》",
				query, refs[0].Score, refs[0].Doc)
		}
	}
}

// 旧版本落盘的数据要能读出来, 但要提示重新导入 (切分规则改过, 老文档不会自动重算)。
func TestStaleVersionIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	options := DefaultOptions()
	options.Path = path

	base := New(options)
	if _, err := base.Import("old.md", zfsDoc); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if warning := base.StaleWarning(); warning != "" {
		t.Errorf("刚导入的数据不该提示过期: %q", warning)
	}

	// 把落盘文件里的版本号改成旧版本
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落盘失败: %v", err)
	}
	patched := strings.Replace(string(raw), `"version": `+itoa(kbVersion), `"version": 1`, 1)
	if patched == string(raw) {
		t.Fatal("没能改到版本号")
	}
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	reloaded := New(options)
	if documents, _ := reloaded.Count(); documents != 1 {
		t.Fatalf("旧版本数据仍应能读取, 实际 %d 份", documents)
	}
	warning := reloaded.StaleWarning()
	if warning == "" || !strings.Contains(warning, "重新导入") {
		t.Errorf("应提示重新导入: %q", warning)
	}
}

func itoa(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	out := []byte{}
	for value > 0 {
		out = append([]byte{digits[value%10]}, out...)
		value /= 10
	}
	return string(out)
}

// 引用片段是给人看的: 不该出现 markdown 的行内标记。
func TestSnippetStripsMarkdown(t *testing.T) {
	cases := map[string]string{
		"## 登录被拒绝 / 安全协商失败\n\n日志出现下列任一内容时…": "登录被拒绝",
		"# 标题":                 "标题",
		"正文里 `zpool scrub` 命令": "正文里 zpool scrub 命令",
		"### 三级标题 ###":         "三级标题",
	}
	for input, want := range cases {
		got := Snippet(input, 40)
		if strings.HasPrefix(got, "#") || strings.Contains(got, "`") {
			t.Errorf("片段里不该有 markdown 标记: %q", got)
		}
		if !strings.Contains(got, want) {
			t.Errorf("片段内容应保留: %q 里应含 %q", got, want)
		}
	}
}

// 短文档的切分: 标题与正文必须落在同一段里。
//
// 这条来自真实使用中的问题: 一份几十字的小手册写成"# 换盘流程 / ## 步骤 / 1. …",
// 切出来第一段只有标题。问"换盘流程是什么"时命中的正是那一段 —— 关键词在标题里、
// 但内容为空, 模型只能回答"资料里没有写这部分"。用户看到的是一份明明有的文档答不出来。
func TestShortDocumentKeepsTitleWithBody(t *testing.T) {
	content := "# 换盘流程\n\n## 步骤\n\n1. 确认 vdev 路径\n2. zpool replace\n"
	title, chunks := ParseDocument("换盘流程.md", content, DefaultChunkOptions())
	if title != "换盘流程" {
		t.Fatalf("标题不对: %q", title)
	}
	if len(chunks) != 1 {
		t.Fatalf("这么短的文档应合成一段, 实际 %d 段: %+v", len(chunks), chunks)
	}
	for _, want := range []string{"换盘流程", "vdev", "zpool replace"} {
		if !strings.Contains(chunks[0].Text, want) {
			t.Errorf("合并后的片段里应同时有标题与正文, 缺少 %q: %q", want, chunks[0].Text)
		}
	}

	// 检索验证: 用文档标题里的词问, 必须能召回带正文的那一段
	base := testBase(t)
	if _, err := base.Import("换盘流程.md", content); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	results := base.Search("换盘流程是什么", 2)
	if len(results) == 0 {
		t.Fatal("应检索到片段")
	}
	if !strings.Contains(results[0].Text, "zpool replace") {
		t.Errorf("召回的片段应包含正文步骤, 实际: %q", results[0].Text)
	}
}

// 小节标题不含文档关键词时, 靠"路径前缀"保证同一文档的片段都能被召回。
func TestChunksCarrySectionPath(t *testing.T) {
	content := "# ZFS 存储池运维手册\n\n## 换盘\n\n执行 zpool replace 后等待 resilver 完成。\n"
	_, chunks := ParseDocument("zfs.md", content, DefaultChunkOptions())
	if len(chunks) == 0 {
		t.Fatal("应有片段")
	}
	// 正文那一段的 heading 是"换盘", 不含"ZFS 存储池运维手册", 但正文里应带上路径
	joined := ""
	for _, chunk := range chunks {
		joined += chunk.Text + "\n"
	}
	if !strings.Contains(joined, "ZFS 存储池运维手册") {
		t.Error("片段正文里应带章节路径 (否则小节标题不含文档关键词时召回不到)")
	}
}

// 原文要落盘并在重启后还在 (界面里"打开文档"靠它)。
func TestContentPersists(t *testing.T) {
	dir := t.TempDir()
	options := DefaultOptions()
	options.Path = filepath.Join(dir, "kb.json")
	content := "# 手册\n\n## 步骤\n\n1. 第一步\n2. 第二步\n"

	base := New(options)
	document, err := base.Import("handbook.md", content)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	reloaded := New(options)
	_, got, ok := reloaded.Content(document.ID)
	if !ok {
		t.Fatal("重启后应能取到原文")
	}
	if got != content {
		t.Errorf("原文应逐字保存:\n得到 %q\n期望 %q", got, content)
	}
	// 列表里的 Document 也带原文 (界面对比 has_content 用)
	if stored, exists := reloaded.Get(document.ID); !exists || !stored.HasContent() {
		t.Error("文档应带原文标记")
	}
}

// 早先版本导入的文档没有原文: 要如实报告"没有", 而不是给一个空串让人以为文档是空的。
func TestContentMissingForLegacyDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kb.json")
	options := DefaultOptions()
	options.Path = path

	base := New(options)
	document, err := base.Import("old.md", "# 旧文档\n\n内容。\n")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	// 模拟旧版本落盘: 去掉 content 字段
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("解析落盘失败: %v", err)
	}
	for _, item := range tree["documents"].([]any) {
		delete(item.(map[string]any), "content")
	}
	patched, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("重新序列化失败: %v", err)
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	legacy := New(options)
	_, content, ok := legacy.Content(document.ID)
	if ok {
		t.Error("旧文档不该报告有原文")
	}
	if content != "" {
		t.Errorf("旧文档的原文应为空, 实际 %q", content)
	}
	// 检索仍应正常工作 (片段还在)
	if results := legacy.Search("旧文档", 2); len(results) == 0 {
		t.Error("旧文档的检索不该受影响")
	}
}

// 片段数上限只能有一个出处。
//
// 散成三份 (配置 / 请求参数 / 检索实现) 时会出现"配置写 20、接口收 50、实际用 5",
// 而症状只是"回答质量不太稳定", 没人查得出来。
func TestTopKIsBoundedEverywhere(t *testing.T) {
	if MaxTopK <= 0 || MaxTopK > 50 {
		t.Fatalf("MaxTopK 取值不合理: %d", MaxTopK)
	}

	// 配置给出的超大值要被夹住
	base := New(Options{TopK: 10_000})
	if got := base.options.TopK; got != MaxTopK {
		t.Errorf("配置里的 TopK 应被夹到 %d, 实际 %d", MaxTopK, got)
	}
	// 配置给出的 0/负数走默认值
	if got := New(Options{}).options.TopK; got != DefaultOptions().TopK {
		t.Errorf("未配置时应取默认值, 实际 %d", got)
	}
	// 单次请求的超大值同样被夹住 (否则 top_k=100000 就是一次免费 DoS)
	base = New(Options{TopK: 5})
	results, _ := base.retrieve("磁盘", AskOptions{TopK: 10_000}, nil)
	if len(results) > MaxTopK {
		t.Errorf("检索结果 %d 条, 超过上限 %d", len(results), MaxTopK)
	}
}
