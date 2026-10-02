package inspect

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sampleReport 是一份"有代表性的真实报告": 三种严重程度 + 无法判定 + 长证据 + 中英混排。
func sampleReport() Report {
	started := time.Date(2026, 9, 25, 16, 20, 3, 0, time.FixedZone("CST", 8*3600))
	return Report{
		ID:       "insp-4f2a91c7be03",
		Target:   "root@192.0.2.10:22",
		User:     "root",
		Host:     "192.0.2.10",
		AuthMode: "公钥",
		HostName: "web-01",
		OS:       "Debian GNU/Linux 13 (trixie)",
		Kernel:   "6.12.57+deb13-amd64",
		Uptime:   "up 3 weeks, 2 days, 4 hours",
		// 指纹刻意用长串, 验证元信息表格的值会折行而不是溢出
		HostKeyFP:  "SHA256:q+arOw+5RoRdRpDBwJXk9m2V1cQ8sT4uY6zA0bC3dE7f",
		StartedAt:  started,
		DurationMS: 254,
		Status:     StatusCritical,
		Summary:    "发现 2 项严重问题",
		Findings: []Finding{
			{
				ID: "cpu", Name: "CPU 负载", Status: StatusOK,
				Summary:  "1 分钟负载 0.40, 每核 0.05 (8 核)",
				Evidence: "0.40 0.35 0.30 1/720 4821\n8",
			},
			{
				ID: "memory", Name: "内存", Status: StatusWarn,
				Summary: "可用内存 18.4%, 低于告警阈值 20%",
				Details: []string{"已用 6.4 GB / 7.8 GB", "Swap 使用率 52%"},
				Evidence: "MemTotal:        8175612 kB\nMemFree:          412308 kB\nMemAvailable:    1504232 kB\n" +
					"SwapTotal:       2097148 kB\nSwapFree:        1007332 kB",
			},
			{
				ID: "disk", Name: "磁盘分区", Status: StatusCritical,
				Summary: "/data 使用率 95%, 已接近写满",
				Details: []string{"/data 剩余 3.2 GB", "inode 使用率 12%"},
				Evidence: "Filesystem     1024-blocks      Used Available Capacity Mounted on\n" +
					"/dev/sda1        103081248  92773112   3201512      95% /\n" +
					"/dev/sdb1        515924512 103184902 386565142      21% /data/backup\n" +
					"tmpfs              4087804         0   4087804       0% /dev/shm",
			},
			{
				ID: "thermal", Name: "温度", Status: StatusUnknown,
				Summary:  "无法读取温度传感器 (目标机可能是虚拟机或未暴露 hwmon)",
				Evidence: "thermal_zone0=unknown",
			},
			{
				ID: "logs", Name: "严重错误日志", Status: StatusCritical,
				Summary: "发现 2 条严重日志 (内核 panic/OOM/硬件错误等)",
				Details: []string{
					"kernel: Out of memory: Killed process 28441 (mysqld)",
					"zfs: checksum mismatch on pool rpool",
				},
				Evidence: "Sep 25 04:12:11 web-01 kernel: Out of memory: Killed process 28441 (mysqld) total-vm:4823912kB\n" +
					"Sep 25 04:12:11 web-01 kernel: oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0\n" +
					"Sep 25 05:02:44 web-01 zfs: checksum mismatch on pool rpool, dataset rpool/data, file /data/backup/dump.tar.gz\n" +
					"Sep 25 05:02:44 web-01 zfs: self-healed 3 blocks\n",
			},
			{
				ID: "services", Name: "服务状态", Status: StatusOK,
				Summary: "没有失败的服务单元",
			},
		},
	}
}

func TestRenderPDFContainsReportContent(t *testing.T) {
	data, err := RenderPDF(sampleReport(), PDFOptions{
		Now: time.Date(2026, 9, 25, 16, 30, 0, 0, time.FixedZone("CST", 8*3600)),
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(data) < 20000 {
		t.Errorf("PDF 太小, 可能没画出内容: %d 字节", len(data))
	}
	text := extractPDFText(t, data)

	// 报告里该有的东西一个都不能少
	want := []string{
		"服务器巡检报告", "web-01", "Debian GNU/Linux 13",
		"严重", "告警", "无法判定", "正常",
		"CPU 负载", "内存", "磁盘分区", "温度", "严重错误日志", "服务状态",
		"/data 使用率 95%, 已接近写满",
		"Out of memory: Killed process 28441",
		"SHA256:q+arOw+5RoRdRpDBwJXk9m2V1cQ8sT4uY6zA0bC3dE7f",
		"insp-4f2a91c7be03",
		"认证方式: 公钥",
	}
	for _, fragment := range want {
		if !strings.Contains(text, fragment) {
			t.Errorf("报告里缺少 %q", fragment)
		}
	}
}

// 每个结论都要能追溯到证据 —— 这是报告的价值所在。
func TestRenderPDFKeepsEvidence(t *testing.T) {
	data, err := RenderPDF(sampleReport(), PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	for _, fragment := range []string{
		"/dev/sda1",
		"MemAvailable:",
		"oom-kill:constraint=CONSTRAINT_NONE",
		"self-healed 3 blocks",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("证据里缺少 %q", fragment)
		}
	}
}

// 很长的证据会把报告撑到多页; 分页、页脚、总页数都要对。
func TestRenderPDFPagination(t *testing.T) {
	report := sampleReport()
	var evidence strings.Builder
	for index := 0; index < 400; index++ {
		evidence.WriteString("Sep 25 05:12:44 web-01 kernel: 这是一条用于撑开页面的日志行, 编号 ")
		evidence.WriteString(strings.Repeat("x", index%20))
		evidence.WriteString("\n")
	}
	report.Findings[4].Evidence = evidence.String()

	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	if !strings.Contains(text, "共 3 页") && !strings.Contains(text, "共 4 页") &&
		!strings.Contains(text, "共 5 页") && !strings.Contains(text, "共 6 页") {
		t.Errorf("页脚应写明总页数, 实际文本片段: %s", trimForMessage(text))
	}
	if !strings.Contains(text, "省略了") {
		t.Error("过长的证据应被截断并说明省略了多少行")
	}
	// 说明段与页脚都在, 证明末尾几页也排出来了
	if !strings.Contains(text, "本报告由 RunStor Bot 通过 SSH 登录目标机执行固定采集脚本生成") {
		t.Error("缺少说明段")
	}
}

// 巡检失败 (连不上/认证失败) 也要能出一份报告, 而且要说明白没采到东西。
func TestRenderPDFForFailedInspection(t *testing.T) {
	report := Report{
		ID:         "insp-failed",
		Target:     "root@10.0.0.9:22",
		User:       "root",
		Host:       "10.0.0.9",
		AuthMode:   "密码",
		StartedAt:  time.Date(2026, 9, 25, 16, 20, 3, 0, time.UTC),
		DurationMS: 8123,
		Status:     StatusUnknown,
		Error:      "认证失败: 用户名、密码或密钥不正确 (root@10.0.0.9: Permission denied (publickey,password).)",
	}
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	for _, fragment := range []string{
		"巡检未完成", "认证失败", "没有采集到任何检查项", "认证方式: 密码",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("失败报告里缺少 %q", fragment)
		}
	}
	if strings.Contains(text, "全部正常") {
		t.Error("失败的巡检不能显示成正常")
	}
}

// 报告里出现的每个字都要能画出来 —— 否则中文报告会满屏方框。
func TestReportLiteralsAreRenderable(t *testing.T) {
	set, err := sharedFontSet()
	if err != nil {
		t.Fatalf("加载字体失败: %v", err)
	}
	report := sampleReport()
	report.Error = "认证失败: 用户名、密码或密钥不正确"
	report.AuthMode = "密码"
	for _, status := range []Status{StatusOK, StatusWarn, StatusCritical, StatusUnknown} {
		finding := Finding{ID: "x", Name: "检查", Status: status, Summary: "结论", Evidence: "证据"}
		report.Findings = append(report.Findings, finding)
	}
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	// 用文档自己收集到的文本做检查: 只要有字画不出来, 报告里就会出现替代符号
	missing := set.MissingGlyphs(reportTexts(t, data)...)
	if len(missing) > 0 {
		t.Errorf("报告里有 %d 个字符没有字形: %v", len(missing), describeRunes(missing))
	}
	// 缺字形说明本身也不该出现
	if strings.Contains(extractPDFText(t, data), "当前字体无法显示") {
		t.Error("报告的固定文案里有字体画不出的字, 说明会被误触发")
	}
}

// 自定义字体不可用时要退回内置字体, 不能因此让导出失败。
func TestBadCustomFontFallsBack(t *testing.T) {
	data, err := RenderPDF(sampleReport(), PDFOptions{FontPath: "/nonexistent/font.ttf"})
	if err != nil {
		t.Fatalf("自定义字体不可用时仍应出报告: %v", err)
	}
	if len(data) < 10000 {
		t.Error("退回内置字体后应仍是完整报告")
	}
	if FontWarning() == "" {
		t.Error("应记录一条降级说明, 便于运维发现配置写错了")
	}
}

// extractPDFText / reportTexts 把 PDF 里的文字抠出来做断言。
//
// 这里是独立实现 (不复用 pdfdoc 的测试辅助), 因为那部分在另一个包里没导出;
// 实现方式与 pdfdoc 的测试一致: 解内容流 + 查 ToUnicode。
func extractPDFText(t *testing.T, data []byte) string {
	t.Helper()
	return strings.Join(reportTexts(t, data), "\n")
}

// trimForMessage 截断长文本, 让失败信息可读。
func trimForMessage(text string) string {
	runes := []rune(strings.ReplaceAll(text, "\n", " "))
	if len(runes) > 400 {
		runes = runes[:400]
	}
	return string(runes)
}

// 探针: 把样例报告写到文件, 供外部工具 (pdftotext / gs) 检查版面。
func TestDumpSampleReport(t *testing.T) {
	target := os.Getenv("BRA_REPORT_PROBE")
	if target == "" {
		t.Skip("未设置 BRA_REPORT_PROBE")
	}
	data, err := RenderPDF(sampleReport(), PDFOptions{
		Now: time.Date(2026, 9, 25, 16, 30, 0, 0, time.FixedZone("CST", 8*3600)),
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	t.Logf("已写出 %s (%d 字节)", target, len(data))
}

// 认证方式要如实出现在报告里, 而且不能因为映射表写错就显示成别的意思。
func TestReportShowsAuthMode(t *testing.T) {
	cases := []struct {
		authMode string
		want     string
	}{
		{"公钥", "公钥"},
		{"密码", "密码"},
		{"无凭据", "无凭据"},
		{"", "未知"},
	}
	for _, entry := range cases {
		report := sampleReport()
		report.AuthMode = entry.authMode
		data, err := RenderPDF(report, PDFOptions{})
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		text := extractPDFText(t, data)
		if !strings.Contains(text, "认证方式: "+entry.want) {
			t.Errorf("认证方式 %q 应显示为 %q", entry.authMode, entry.want)
		}
	}
}

// 跨页续画的证据块不该多占页面。
//
// 之前的做法是"整块放不下就搬到下一页", 结果一份 4 页的报告第 1 页有大半页空白
// (实测就是这样)。现在按行续画, 页数应当只由内容量决定。
func TestLongEvidenceDoesNotWastePages(t *testing.T) {
	countPages := func(report Report) int {
		t.Helper()
		data, err := RenderPDF(report, PDFOptions{})
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		text := extractPDFText(t, data)
		// 页脚写着 "第 N 页 / 共 M 页"
		const marker = "共 "
		index := strings.LastIndex(text, marker)
		if index < 0 {
			t.Fatalf("页脚没有总页数: %s", trimForMessage(text))
		}
		rest := text[index+len(marker):]
		digits := ""
		for _, r := range rest {
			if r < '0' || r > '9' {
				break
			}
			digits += string(r)
		}
		if digits == "" {
			t.Fatalf("页脚总页数解析失败: %q", rest)
		}
		value, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("页脚总页数不是数字: %q", digits)
		}
		return value
	}

	// 一份内容极少的报告: 应当只有 1 页
	small := Report{ID: "insp-small", Target: "root@h:22", User: "root", Status: StatusOK,
		Findings: []Finding{{ID: "a", Name: "检查", Status: StatusOK, Summary: "正常"}}}
	if pages := countPages(small); pages != 1 {
		t.Errorf("内容很少时应只有 1 页, 实际 %d 页", pages)
	}

	// 一份超长证据的报告
	big := sampleReport()
	var evidence strings.Builder
	for index := 0; index < 900; index++ {
		fmt.Fprintf(&evidence, "Sep 25 05:12:44 web-01 kernel: 日志行 %d\n", index)
	}
	big.Findings = []Finding{{ID: "logs", Name: "严重错误日志", Status: StatusCritical,
		Summary: "发现多条严重日志", Evidence: evidence.String()}}
	// 证据被截断到 200 行 (约 3 页), 加上标题与说明, 不该超过 5 页
	if pages := countPages(big); pages > 5 {
		t.Errorf("超长证据的报告页数过多 (%d 页), 可能没有跨页续画", pages)
	}
	if pages := countPages(big); pages < 3 {
		t.Errorf("200 行证据至少应排出 3 页, 实际 %d 页", pages)
	}
}

// 影响与处置建议必须出现在 PDF 里, 而且要标出结论来源。
//
// 这是报告的"行动部分" —— 只有条数没有建议的报告, 运维拿到手还得自己去查该做什么。
func TestRenderPDFIncludesImpactAndActions(t *testing.T) {
	report := sampleReport()
	report.Findings = []Finding{{
		ID: "logs", Name: "严重错误日志", Status: StatusCritical,
		Summary: "发现 2 条严重日志（内存耗尽, 进程被内核杀死 (OOM)）",
		Diagnoses: []Diagnosis{
			{Origin: OriginRule, RuleID: "oom", Title: "内存耗尽, 进程被内核杀死 (OOM)",
				Impact:   "OOM killer 直接杀掉了占用内存最多的进程, 服务会突然中断。",
				Actions:  []string{"确认被杀的是哪个服务", "查内存大户与 cgroup 用量"},
				Severity: "critical", Count: 2, Samples: []string{"kernel: Out of memory: Killed process 28441 (mysqld)"}},
			{Origin: OriginModel, RuleID: "model", Title: "自定义组件 widgetd 反复重平衡",
				Impact:  "重平衡期间该组件的请求会变慢。",
				Actions: []string{"查看 widgetd 的完整日志"}, Severity: "err", Count: 1},
		},
	}}
	report.Analysis = "影响分析与处置建议: 内置规则库 + 模型 (含模型给出的结论, 已在建议处标注)"

	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	for _, fragment := range []string{
		"影响与处置建议",
		"内存耗尽, 进程被内核杀死 (OOM)",
		"影响: OOM killer 直接杀掉了占用内存最多的进程",
		"处置:",
		"1) 确认被杀的是哪个服务",
		"2) 查内存大户与 cgroup 用量",
		"自定义组件 widgetd 反复重平衡",
		"模型分析", // 结论来源要标出来
		"规则库",
		"请结合现场判断", // 提醒模型结论需要人判断
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("报告里缺少 %q", fragment)
		}
	}
	// 原始日志样本要能对上, 便于人工核对结论
	if !strings.Contains(text, "Killed process 28441") {
		t.Error("诊断里应带原始日志样本")
	}
}

// 没有诊断时不该出现空的"影响与处置建议"标题。
func TestRenderPDFOmitsEmptyDiagnoses(t *testing.T) {
	report := sampleReport()
	report.Findings = []Finding{{ID: "cpu", Name: "CPU 负载", Status: StatusOK, Summary: "正常"}}
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if strings.Contains(extractPDFText(t, data), "影响与处置建议") {
		t.Error("没有诊断时不该出现该小节")
	}
}

// 模型给的文本可能带 markdown 强调, 报告里要清掉, 不能出现成对星号。
func TestDiagnosisTextStripsMarkdown(t *testing.T) {
	report := sampleReport()
	report.Findings = []Finding{{
		ID: "logs", Name: "严重错误日志", Status: StatusCritical, Summary: "发现 1 条",
		Diagnoses: []Diagnosis{{
			Origin: OriginModel, Title: "**组件** rebalance 延迟", Impact: "这**不代表**读写失败",
			Actions: []string{"检查 `dmesg` 输出"}, Severity: "err", Count: 1,
		}},
	}}
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	if strings.Contains(text, "**") {
		t.Error("报告里不该出现 markdown 强调标记")
	}
	if !strings.Contains(text, "这不代表读写失败") {
		t.Errorf("强调标记去掉后文字应保持完整: %s", trimForMessage(text))
	}
}

// 诊断里的知识引用 (运维手册章节) 要出现在报告里。
func TestRenderPDFIncludesKnowledgeRefs(t *testing.T) {
	report := sampleReport()
	report.Findings = []Finding{{
		ID: "logs", Name: "严重错误日志", Status: StatusCritical, Summary: "发现 1 条",
		Diagnoses: []Diagnosis{{
			Origin: OriginRule, RuleID: "iscsi_auth", Title: "iSCSI 存储链路认证/授权失败",
			Impact: "存储卷挂不上。", Actions: []string{"核对 ACL"}, Severity: "err", Count: 5,
			Knowledge: []KnowledgeRef{{
				DocID: "doc-1", Doc: "iSCSI 链路排查", Heading: "登录被拒绝",
				Snippet: "在 target 侧确认发起端 IQN 是否在 ACL 白名单里。", Score: 12.5,
			}},
		}},
	}}
	data, err := RenderPDF(report, PDFOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	text := extractPDFText(t, data)
	// 引用可能被折行, 所以分段断言而不是整句
	for _, fragment := range []string{
		"相关知识: 《iSCSI 链路排查》",
		"登录被拒绝",
		"在 target 侧确认发起端 IQN",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("报告里缺少 %q", fragment)
		}
	}
}
