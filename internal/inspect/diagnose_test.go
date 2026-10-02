package inspect

import (
	"backup-restore-agent/internal/i18n"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// 用**真实**的日志行做测试: 规则库的价值全在"能不能认出真机上真的会出现的那些行",
// 自己编的样例只能证明关键字写对了。
//
// 下面这些行来自 192.0.2.10 的巡检输出 (journald 与 dmesg)。
func TestDiagnoseRealLogLines(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		title string
	}{
		{
			"OOM 杀进程",
			"Sep 25 04:12:11 web-01 kernel: Out of memory: Killed process 28441 (mysqld) total-vm:4823912kB",
			"内存耗尽, 进程被内核杀死 (OOM)",
		},
		{
			"oom-kill 行",
			"Sep 25 04:12:11 web-01 kernel: oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0",
			"内存耗尽, 进程被内核杀死 (OOM)",
		},
		{
			"ZFS 校验失败",
			"Sep 25 05:02:44 web-01 zfs: checksum mismatch on pool rpool, dataset rpool/data, file /data/backup/dump.tar.gz",
			"ZFS 数据校验失败 (checksum mismatch)",
		},
		{
			"iSCSI 登录协商失败",
			"[dmesg/err] [ 1114.763770] iSCSI Initiator Node: iqn.1993-08.org.debian:01:5d424f68f4f7 is not authorized to access iSCSI target portal group: 1.",
			"iSCSI 存储链路认证/授权失败",
		},
		{
			"iSCSI 协商失败 (另一种措辞)",
			"[dmesg/err] [ 1114.764413] iSCSI Login negotiation failed.",
			"iSCSI 存储链路认证/授权失败",
		},
		{
			"域认证失败",
			"[journald/err] Sep 25 04:16:00 node-01 sshd-session[678035]: pam_winbind(sshd:auth): request wbcLogonUser failed: WBC_ERR_AUTH_ERROR, PAM error: PAM_USER_UNKNOWN (10), NTSTATUS: NT_STATUS_NO_SUCH_USER",
			"认证失败 (登录被拒)",
		},
		{
			"target 配置读不到",
			"[dmesg/err] [ 863.122265] db_root: cannot open: /etc/target",
			"数据库错误",
		},
		{
			"磁盘 I/O 错误",
			"Sep 25 06:01:02 node1 kernel: blk_update_request: I/O error, dev sda, sector 123456 op WRITE",
			"磁盘 I/O 错误",
		},
		{
			"文件系统转只读",
			"Sep 25 06:01:02 node1 kernel: EXT4-fs (sda1): Remounting filesystem read-only",
			"文件系统检测到不一致",
		},
		{
			"内核 oops",
			"Sep 25 07:00:00 node1 kernel: BUG: unable to handle kernel NULL pointer dereference at 0000000000000010",
			"内核 panic / oops",
		},
		{
			"段错误",
			"Sep 25 07:10:00 node1 kernel: mysqld[1234]: segfault at 0 ip 00007f sp 00007ff error 4",
			"进程异常退出 (段错误/崩溃)",
		},
		{
			"服务启动失败",
			"Sep 25 07:20:00 node1 systemd[1]: Failed to start Backup Service.",
			"系统服务启动或运行失败",
		},
		{
			"空间不足",
			"Sep 25 07:30:00 node1 postgres[900]: ERROR: could not write to file: No space left on device",
			"空间不足",
		},
		{
			"ZFS 池降级",
			"Sep 25 07:40:00 node1 zfs: pool rpool has encountered an uncorrectable I/O failure and has been suspended",
			"ZFS 存储池降级或故障",
		},
		{
			"链接断开",
			"Sep 25 07:50:00 node1 kernel: e1000e 0000:00:1f.6 enp0s31f6: NIC Link is Down",
			"网络链路中断或超时",
		},
	}

	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			diagnoses := DiagnoseLogs([]LogEntry{{Source: "dmesg", Severity: "critical", Text: entry.line}})
			if len(diagnoses) != 1 {
				t.Fatalf("应归成 1 类问题, 实际 %d 类", len(diagnoses))
			}
			got := diagnoses[0]
			if got.Title != entry.title {
				t.Errorf("识别为 %q, 期望 %q", got.Title, entry.title)
			}
			if got.Origin != OriginRule {
				t.Errorf("应来自规则库, 实际 %q", got.Origin)
			}
			if got.Impact == "" || len(got.Actions) == 0 {
				t.Error("影响与处置建议都必须有内容")
			}
			if len(got.Samples) == 0 {
				t.Error("应保留原始日志样本, 结论要能追溯")
			}
		})
	}
}

// 同一类问题出现 100 次只该出一条结论 (带次数), 而不是刷满报告。
func TestDiagnoseGroupsSameIssue(t *testing.T) {
	entries := []LogEntry{}
	for index := 0; index < 100; index++ {
		entries = append(entries, LogEntry{
			Source: "dmesg", Severity: "critical",
			Text: "kernel: Out of memory: Killed process 28441 (mysqld) total-vm:4823912kB",
		})
	}
	entries = append(entries, LogEntry{Source: "journald", Severity: "critical",
		Text: "zfs: checksum mismatch on pool rpool, file /data/backup/x.gz"})
	entries = append(entries, LogEntry{Source: "dmesg", Severity: "critical",
		Text: "iSCSI Login negotiation failed."})

	diagnoses := DiagnoseLogs(entries)
	if len(diagnoses) != 3 {
		t.Fatalf("应归成 3 类问题, 实际 %d 类", len(diagnoses))
	}
	if diagnoses[0].Count != 100 {
		t.Errorf("OOM 应有 100 条, 实际 %d", diagnoses[0].Count)
	}
	// 样本最多留两条, 否则一条结论拖着一堆重复行
	for _, item := range diagnoses {
		if len(item.Samples) > 2 {
			t.Errorf("%s 的样本应限量, 实际 %d 条", item.Title, len(item.Samples))
		}
	}
}

// 认不出来的日志必须说"需人工确认", 不能编一个看起来合理的结论。
func TestDiagnoseUnknownLogDoesNotInvent(t *testing.T) {
	diagnoses := DiagnoseLogs([]LogEntry{{
		Source: "journald", Severity: "err",
		Text: "Sep 25 08:00:00 node1 whateverd[42]: widget rebalance deferred (code 17)",
	}})
	if len(diagnoses) != 1 {
		t.Fatalf("应有 1 条结论, 实际 %d", len(diagnoses))
	}
	got := diagnoses[0]
	if got.Origin != OriginUnknown {
		t.Errorf("未识别的日志应标为 unknown, 实际 %q", got.Origin)
	}
	if !strings.Contains(got.Impact, "无法凭它判断影响") {
		t.Errorf("未识别时不该给出具体影响, 实际 %q", got.Impact)
	}
	// 不能混进任何"规则库才知道的具体措施" —— 那些话只有识别出故障类型才能说
	specific := []string{"zpool", "smartctl", "fsck", "MemoryMax", "fail2ban", "memtest",
		"xfs_repair", "更换磁盘", "扩容", "resilver", "CHAP"}
	for _, action := range got.Actions {
		for _, keyword := range specific {
			if strings.Contains(action, keyword) {
				t.Errorf("未识别时不该给出具体处置动作 (%q): %q", keyword, action)
			}
		}
	}
	if len(got.Samples) == 0 {
		t.Error("未识别时更要保留原始行, 方便人工查")
	}
}

// 结论排序: 严重的、量大的在前, 未识别的最后 (行动价值最低)。
func TestDiagnoseOrdering(t *testing.T) {
	entries := []LogEntry{
		{Source: "journald", Severity: "err", Text: "whateverd[42]: widget rebalance deferred (code 17)"},
		{Source: "dmesg", Severity: "critical", Text: "kernel: Out of memory: Killed process 1 (x)"},
	}
	diagnoses := DiagnoseLogs(entries)
	if len(diagnoses) != 2 {
		t.Fatalf("应有 2 条, 实际 %d", len(diagnoses))
	}
	if diagnoses[0].Origin == OriginUnknown {
		t.Error("未识别的应排在最后")
	}
}

// 规则库本身要自洽: 每条规则都得有话可说, 关键字不能为空。
func TestRemedyRulesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range remedyRules {
		if rule.ID == "" || rule.Title == "" || rule.Impact == "" {
			t.Errorf("规则 %q 缺少必填内容", rule.ID)
		}
		if len(rule.Actions) == 0 {
			t.Errorf("规则 %q 没有处置建议", rule.ID)
		}
		if len(rule.Any) == 0 && len(rule.All) == 0 {
			t.Errorf("规则 %q 没有任何匹配条件 (会命中所有日志)", rule.ID)
		}
		if seen[rule.ID] {
			t.Errorf("规则 ID 重复: %q", rule.ID)
		}
		seen[rule.ID] = true
		for _, action := range rule.Actions {
			if strings.TrimSpace(action) == "" {
				t.Errorf("规则 %q 有空的处置建议", rule.ID)
			}
		}
	}
}

// 合并模型结论: 规则能识别的以规则为准, 规则没识别的由模型补上并标明来源。
func TestMergeModelDiagnoses(t *testing.T) {
	ruled := []Diagnosis{
		{RuleID: "oom", Origin: OriginRule, Title: "内存耗尽, 进程被内核杀死 (OOM)",
			Impact: "影响", Actions: []string{"动作"}, Severity: "critical", Count: 3},
		{RuleID: OriginUnknown, Origin: OriginUnknown, Title: "未识别的严重日志",
			Impact: "影响", Actions: []string{"动作"}, Severity: "err", Count: 1,
			Sources: []string{"journald"}, Samples: []string{"whateverd[42]: widget rebalance deferred"}},
	}
	modeled := []Diagnosis{
		{Title: "自定义组件 widgetd 的再平衡失败", Impact: "模型给出的影响",
			Actions: []string{"模型给出的动作"}, Severity: "err", Count: 1, Samples: []string{"原始行"}},
	}
	merged := MergeModelDiagnoses(ruled, modeled)

	if len(merged) != 2 {
		t.Fatalf("未识别的条目应被模型结论取代, 实际 %d 条", len(merged))
	}
	found := false
	for _, item := range merged {
		if item.Title == "自定义组件 widgetd 的再平衡失败" {
			found = true
			if item.Origin != OriginModel {
				t.Errorf("模型结论应标明来源, 实际 %q", item.Origin)
			}
			// 原始日志与条数必须保留 (模型只负责填结论, 不能把证据带走)
			if len(item.Samples) == 0 || item.Count != 1 || len(item.Sources) == 0 {
				t.Errorf("模型结论必须保留原有的样本/条数/来源: %+v", item)
			}
		}
		if strings.Contains(item.Title, "未识别") {
			t.Error("有模型结论时不该再留一条'未识别'")
		}
	}
	if !found {
		t.Error("模型给出的结论没有被并入")
	}

	// 模型没有结论时, 保留"未识别"并说明尝试过
	kept := MergeModelDiagnoses(ruled, nil)
	if len(kept) != 2 {
		t.Fatalf("应保留两条, 实际 %d", len(kept))
	}
	unrecognized := false
	for _, item := range kept {
		if item.Origin == OriginUnknown {
			unrecognized = true
			if !strings.Contains(item.Note, "需人工确认") {
				t.Errorf("未识别条目应明确要求人工确认, 实际 %q", item.Note)
			}
		}
	}
	if !unrecognized {
		t.Error("模型没结论时应保留未识别条目")
	}
}

// fakeAnalyzer 模拟模型分析器。
type fakeAnalyzer struct {
	available bool
	reply     []Diagnosis
	err       error
	seen      []Diagnosis
}

func (f *fakeAnalyzer) Available() bool { return f.available }
func (f *fakeAnalyzer) AnalyzeUnknown(lang i18n.Lang, groups []Diagnosis) ([]Diagnosis, error) {
	f.seen = groups
	return f.reply, f.err
}

// 端到端: 采集到严重日志 → 报告里带影响分析与处置建议, 并说明分析来源。
func TestInspectionReportCarriesDiagnoses(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"Debian GNU/Linux 13\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\nSep 25 04:12:11 web-01 kernel: Out of memory: Killed process 28441 (mysqld)\n" +
		"Sep 25 04:12:11 web-01 kernel: oom-kill:constraint=CONSTRAINT_NONE\n" +
		"Sep 25 05:02:44 web-01 zfs: checksum mismatch on pool rpool, file /data/x.gz\n" +
		"Sep 25 06:00:00 web-01 whateverd[42]: ERROR widget rebalance deferred (code 17)\n"
	service := NewService(Options{
		StorePath: filepath.Join(t.TempDir(), "inspections.json"),
		LogAnalyzer: &fakeAnalyzer{available: true, reply: []Diagnosis{{
			Title: "组件重平衡失败", Impact: "重平衡期间请求变慢", Actions: []string{"查完整日志"},
			Severity: "err",
		}}},
		AnalyzeLogs: true,
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})

	report := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	if report.Status != StatusCritical {
		t.Fatalf("有严重日志时状态应为 critical, 实际 %s", report.Status)
	}

	var logs *Finding
	for index := range report.Findings {
		if report.Findings[index].ID == "logs" {
			logs = &report.Findings[index]
		}
	}
	if logs == nil {
		t.Fatal("缺少日志检查项")
	}
	if len(logs.Diagnoses) != 3 {
		t.Fatalf("应识别出 3 类问题 (OOM/ZFS 校验/模型补充), 实际 %d 类: %+v",
			len(logs.Diagnoses), logs.Diagnoses)
	}
	// 结论行要说清是什么问题, 不能只有条数
	if !strings.Contains(logs.Summary, "内存耗尽") {
		t.Errorf("结论行应带上问题类别, 实际 %q", logs.Summary)
	}
	// 模型的结论要标出来源
	origin := map[string]bool{}
	for _, item := range logs.Diagnoses {
		origin[item.Origin] = true
		if len(item.Actions) == 0 {
			t.Errorf("%s 缺少处置建议", item.Title)
		}
		if item.Impact == "" {
			t.Errorf("%s 缺少影响分析", item.Title)
		}
	}
	if !origin[OriginRule] || !origin[OriginModel] {
		t.Errorf("应同时含规则结论与模型结论, 实际 %v", origin)
	}
	if !strings.Contains(report.Analysis, "内置规则库") || !strings.Contains(report.Analysis, "模型") {
		t.Errorf("报告应说明分析来源, 实际 %q", report.Analysis)
	}
	// PDF 里也要有
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成 PDF 失败: %v", err)
	}
	text := extractPDFText(t, data)
	for _, fragment := range []string{"影响与处置建议", "内存耗尽", "处置:", "组件重平衡失败"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("PDF 里缺少 %q", fragment)
		}
	}
}

// 模型调用失败的原因只属于**那一次**巡检, 不该粘在后来的报告上。
//
// 从前这个原因存在包级变量里, 有两个后果:
//   - 它从不被清空 —— 一次失败之后, 后面每一份报告都挂着"模型调用失败", 哪怕那次
//     调用其实成功了。运维据此怀疑模型配置, 白查半天。
//   - 并发巡检时跨请求串台, 别人的失败原因出现在自己的报告里。
func TestModelFailureDoesNotStickToLaterReports(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"Debian GNU/Linux 13\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\nSep 25 06:00:00 web-01 whateverd[42]: ERROR widget rebalance deferred (code 17)\n"
	analyzer := &fakeAnalyzer{
		available: true,
		reply:     []Diagnosis{{Title: "模型结论", Impact: "影响", Actions: []string{"动作"}, Severity: "err"}},
		err:       errors.New("连接模型超时"),
	}
	service := NewService(Options{
		StorePath:   filepath.Join(t.TempDir(), "inspections.json"),
		LogAnalyzer: analyzer,
		AnalyzeLogs: true,
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})

	first := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	if !strings.Contains(first.Analysis, "模型调用失败") {
		t.Fatalf("模型失败时应在报告里说明, 实际 %q", first.Analysis)
	}
	if !strings.Contains(first.Analysis, "连接模型超时") {
		t.Errorf("失败原因应写进报告, 便于排查: %q", first.Analysis)
	}

	// 模型恢复正常: 这一份报告不该再提上一次的失败
	analyzer.err = nil
	second := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	if strings.Contains(second.Analysis, "模型调用失败") {
		t.Errorf("上一次的失败粘到了这一份报告上: %q", second.Analysis)
	}
	if !strings.Contains(second.Analysis, "模型") {
		t.Errorf("这一份应说明模型结论已并入: %q", second.Analysis)
	}
}

// 关掉模型分析时只给规则结论, 并且报告里如实说明。
func TestInspectionWithoutModelAnalysis(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"Debian GNU/Linux 13\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\nSep 25 04:12:11 web-01 kernel: Out of memory: Killed process 28441 (mysqld)\n"
	analyzer := &fakeAnalyzer{available: true, reply: []Diagnosis{{Title: "不该被用到"}}}
	service := NewService(Options{
		StorePath:   filepath.Join(t.TempDir(), "inspections.json"),
		LogAnalyzer: analyzer,
		AnalyzeLogs: false, // 默认关闭
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})

	report := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	if analyzer.seen != nil {
		t.Error("关闭时不该调用模型")
	}
	if !strings.Contains(report.Analysis, "未启用模型分析") {
		t.Errorf("应说明本次没做模型分析, 实际 %q", report.Analysis)
	}
	for _, finding := range report.Findings {
		for _, item := range finding.Diagnoses {
			if item.Origin == OriginModel {
				t.Error("关闭时不该有模型结论")
			}
		}
	}
}

// 单次巡检可以覆盖服务的默认开关 (界面上的勾选框)。
func TestRunOptionsOverride(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"x\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\nSep 25 06:00:00 web-01 whateverd[42]: ERROR widget rebalance deferred\n"
	analyzer := &fakeAnalyzer{available: true, reply: []Diagnosis{{
		Title: "模型结论", Impact: "影响", Actions: []string{"动作"}, Severity: "err",
	}}}
	service := NewService(Options{
		StorePath:   filepath.Join(t.TempDir(), "inspections.json"),
		LogAnalyzer: analyzer,
		AnalyzeLogs: false,
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})

	enabled := true
	report := service.InspectOneWith(Target{Host: "web-01", User: "root", Password: "p"},
		RunOptions{AnalyzeLogs: &enabled})
	if analyzer.seen == nil {
		t.Fatal("显式打开时应调用模型")
	}
	found := false
	for _, finding := range report.Findings {
		for _, item := range finding.Diagnoses {
			if item.Title == "模型结论" {
				found = true
			}
		}
	}
	if !found {
		t.Error("模型结论应出现在报告里")
	}
}

// 真机上最容易刷屏的两类行也要能识别 —— 它们正是"看起来严重但分不清要不要处理"的典型。
func TestDiagnoseNoisyRealLines(t *testing.T) {
	cases := []struct{ line, title string }{
		{"[dmesg/err] [ 900.1] Security negotiation failed.", "iSCSI 存储链路认证/授权失败"},
		{"[dmesg/err] [ 901.2] MODE SENSE: unimplemented page/subpage: 0x0a/0x05",
			"SCSI 设备不支持被查询的能力 (兼容性提示)"},
		{"[dmesg/err] [ 902.3] Initiator is requesting CSG: 1, has not been successfully authenticated, and the Target is enforcing iSCSI Authentication, login failed.",
			"iSCSI 存储链路认证/授权失败"},
	}
	for _, entry := range cases {
		diagnoses := DiagnoseLogs([]LogEntry{{Source: "dmesg", Severity: "err", Text: entry.line}})
		if len(diagnoses) != 1 || diagnoses[0].Title != entry.title {
			t.Errorf("%q 应识别为 %q, 实际 %+v", entry.line, entry.title, diagnoses)
		}
	}
	// SCSI 兼容性提示的影响里必须说清"不一定影响读写", 否则会把人引向错误的紧急程度
	quirk := DiagnoseLogs([]LogEntry{{Text: "MODE SENSE: unimplemented page/subpage: 0x0a/0x05"}})
	if len(quirk) != 1 || !strings.Contains(quirk[0].Impact, "不代表读写失败") {
		t.Errorf("兼容性提示要说清严重程度: %+v", quirk)
	}
}

// 规则文案里不该出现 markdown 标记: PDF 不渲染 markdown, 星号会原样出现在报告里。
func TestRuleTextHasNoMarkdown(t *testing.T) {
	for _, rule := range remedyRules {
		texts := append([]string{rule.Title, rule.Impact}, rule.Actions...)
		for _, text := range texts {
			for _, marker := range []string{"**", "__", "`"} {
				if strings.Contains(text, marker) {
					t.Errorf("规则 %s 的文案里有 markdown 标记 %q: %s", rule.ID, marker, text)
				}
			}
		}
	}
}

// fakeKnowledge 模拟知识库。
type fakeKnowledge struct {
	refs  []KnowledgeRef
	query []string
}

func (f *fakeKnowledge) LookupKnowledge(query string, limit int) []KnowledgeRef {
	f.query = append(f.query, query)
	if len(f.refs) > limit {
		return f.refs[:limit]
	}
	return f.refs
}

// 巡检报告要附上知识库里的相关章节; 未识别的条目不去查 (连"是什么问题"都没定,
// 拿它的标题去检索只会撞出一堆无关章节)。
func TestInspectionAttachesKnowledge(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"Debian\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\n[ 100.1] iSCSI Login negotiation failed.\n" +
		"[ 101.2] whateverd[42]: ERROR widget rebalance deferred\n"
	knowledge := &fakeKnowledge{refs: []KnowledgeRef{{
		DocID: "doc-1", Doc: "iSCSI 链路排查", Heading: "登录被拒绝", Snippet: "核对 ACL 白名单。",
	}}}
	service := NewService(Options{
		StorePath: filepath.Join(t.TempDir(), "inspections.json"),
		Knowledge: knowledge,
		// 不启用模型分析: 让未识别的条目留在报告里, 才能验证它没去查知识库
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})

	report := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	attached, unrecognized := 0, 0
	for _, finding := range report.Findings {
		for _, item := range finding.Diagnoses {
			switch {
			case item.Origin == OriginUnknown:
				unrecognized++
				if len(item.Knowledge) != 0 {
					t.Error("未识别的条目不该带知识引用")
				}
			case len(item.Knowledge) > 0:
				attached++
			}
		}
	}
	if attached == 0 {
		t.Fatal("已识别的问题应带上知识引用")
	}
	if unrecognized == 0 {
		t.Fatal("测试数据里应有一条未识别的日志")
	}
	for _, query := range knowledge.query {
		if strings.Contains(query, "未识别") {
			t.Errorf("未识别的条目不该去查知识库: %q", query)
		}
	}
}

// 没有知识库 (或知识库为空) 时, 巡检照常完成, 只是不带引用。
func TestInspectionWithoutKnowledgeBase(t *testing.T) {
	output := "__BRA_meta__\nPRETTY_NAME=\"Debian\"\n6.1.0\nweb-01\nup 1 day\n" +
		"__BRA_logs__\n[ 100.1] iSCSI Login negotiation failed.\n"
	service := NewService(Options{StorePath: filepath.Join(t.TempDir(), "inspections.json")})
	service.SetDialer(func(Target) (Runner, func(), error) {
		return &fakeRunner{output: output}, func() {}, nil
	})
	report := service.InspectOne(Target{Host: "web-01", User: "root", Password: "p"})
	for _, finding := range report.Findings {
		for _, item := range finding.Diagnoses {
			if len(item.Knowledge) != 0 {
				t.Error("没有知识库时不该有引用")
			}
		}
	}
}
