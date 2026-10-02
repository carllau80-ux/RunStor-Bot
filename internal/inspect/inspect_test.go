package inspect

import (
	"backup-restore-agent/internal/i18n"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

func TestParseLoadAverage(t *testing.T) {
	load, ok := parseLoadAverage("0.52 0.58 0.59 1/234 5678")
	if !ok {
		t.Fatal("应能解析 /proc/loadavg")
	}
	if load.Load1 != 0.52 || load.Load5 != 0.58 || load.Load15 != 0.59 {
		t.Errorf("负载解析不对: %+v", load)
	}
	if load.Running != 1 || load.Total != 234 {
		t.Errorf("进程数解析不对: %+v", load)
	}
}

func TestParseLoadAverageRejectsGarbage(t *testing.T) {
	for _, text := range []string{"", "not a load", "1.0", "abc def ghi"} {
		if _, ok := parseLoadAverage(text); ok {
			t.Errorf("%q 不该解析成功", text)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	text := `MemTotal:       16333816 kB
MemFree:         1234567 kB
MemAvailable:    4321098 kB
Buffers:          123456 kB
SwapTotal:       2097152 kB
SwapFree:        1048576 kB
`
	info, ok := parseMeminfo(text)
	if !ok {
		t.Fatal("应能解析 /proc/meminfo")
	}
	if info.TotalKB != 16333816 || info.AvailableKB != 4321098 {
		t.Errorf("解析不对: %+v", info)
	}
	if percent := info.UsedPercent(); percent < 73 || percent > 74 {
		t.Errorf("已用百分比不对: %.2f", percent)
	}
	if swap := info.SwapUsedPercent(); swap != 50 {
		t.Errorf("交换分区使用率不对: %.1f", swap)
	}
}

func TestParseMeminfoSwapDisabled(t *testing.T) {
	info, ok := parseMeminfo("MemTotal: 1000 kB\nMemAvailable: 500 kB\nSwapTotal: 0 kB\n")
	if !ok {
		t.Fatal("应能解析")
	}
	if swap := info.SwapUsedPercent(); swap >= 0 {
		t.Errorf("未启用交换时应返回负值, 实际 %.1f", swap)
	}
}

// 老内核没有 MemAvailable 时要退回 MemFree, 而不是当成"可用 0"。
func TestParseMeminfoFallsBackToMemFree(t *testing.T) {
	info, ok := parseMeminfo("MemTotal: 1000 kB\nMemFree: 400 kB\n")
	if !ok {
		t.Fatal("应能解析")
	}
	if info.AvailableKB != 400 {
		t.Errorf("应退回 MemFree: %+v", info)
	}
}

func TestParseDfFiltersPseudoFilesystems(t *testing.T) {
	text := `Filesystem     1024-blocks      Used Available Capacity Mounted on
udev               8132868         0   8132868       0% /dev
tmpfs              1633381      1234   1632147       1% /run
/dev/sda1         51471760  48123456   3348304      94% /
/dev/sdb1        103080888  12345678  90735210      12% /data
overlay           51471760  48123456   3348304      94% /var/lib/docker
tmpfs              8166900         0   8166900       0% /dev/shm
`
	mounts := parseDf(text)
	if len(mounts) != 2 {
		t.Fatalf("只应保留真实文件系统, 实际 %d 个: %+v", len(mounts), mounts)
	}
	// 按使用率倒序: / 在最前
	if mounts[0].MountPoint != "/" || mounts[0].UsePercent != 94 {
		t.Errorf("应按使用率倒序: %+v", mounts[0])
	}
	if mounts[1].MountPoint != "/data" {
		t.Errorf("第二个应是 /data: %+v", mounts[1])
	}
}

// 大磁盘的 kB 数会超过 32 位, 不能被截断。
func TestParseDfHandlesLargeDisks(t *testing.T) {
	text := `Filesystem     1024-blocks        Used   Available Capacity Mounted on
/dev/sda1      412316860416 206158430208 185406228992      50% /huge
`
	mounts := parseDf(text)
	if len(mounts) != 1 {
		t.Fatalf("应解析出 1 个挂载点: %+v", mounts)
	}
	if mounts[0].SizeKB != 412316860416 {
		t.Errorf("大容量被截断了: %d", mounts[0].SizeKB)
	}
}

func TestParseThermal(t *testing.T) {
	// sysfs 给的是毫摄氏度
	sysfs := "x86_pkg_temp=45000\nacpitz=38500\n"
	temps := parseThermal(sysfs)
	if len(temps) != 2 {
		t.Fatalf("应解析出 2 个传感器: %+v", temps)
	}
	if temps[0].Celsius != 45 || temps[1].Celsius != 38.5 {
		t.Errorf("毫摄氏度换算不对: %+v", temps)
	}

	// sensors 风格
	sensors := "Package id 0:  +45.0°C  (high = +80.0°C, crit = +100.0°C)\nCore 0:        +44.0°C\n"
	temps = parseThermal(sensors)
	if len(temps) != 2 || temps[0].Celsius != 45 {
		t.Errorf("sensors 输出解析不对: %+v", temps)
	}
}

func TestParseLogsClassifiesSeverity(t *testing.T) {
	text := `Sep 25 10:00:00 host kernel: Out of memory: Killed process 1234
Sep 25 10:01:00 host nginx: error: upstream timed out
Sep 25 10:02:00 host systemd: Started something
---DMESG---
[12345.6] critical temperature reached
Sep 25 10:03:00 host app: ERROR failed to write
`
	scan := parseLogs(text)
	if scan.CountSeverity("critical") != 2 {
		t.Errorf("严重条目数不对: %d", scan.CountSeverity("critical"))
	}
	if scan.CountSeverity("err") != 2 {
		t.Errorf("错误条目数不对: %d, %+v", scan.CountSeverity("err"), scan.Entries)
	}
}

// 这条是"诚实性"的核心: 日志读不到必须报成"无法判定", 不能显示成"没有错误"。
func TestParseLogsDetectsUnavailableSources(t *testing.T) {
	text := `Sep 25 10:00:00 host journal: journalctl: command not found
---DMESG---
dmesg: read kernel buffer failed: Operation not permitted
---SYSLOG---
`
	scan := parseLogs(text)
	if len(scan.Entries) != 0 {
		t.Fatalf("不该把报错提示当成日志条目: %+v", scan.Entries)
	}
	if len(scan.SourcesUnavailable) != 2 {
		t.Fatalf("应识别出两路日志不可用: %+v", scan.SourcesUnavailable)
	}

	finding := evaluateLogs(text, DefaultThresholds(), renderer{lang: i18n.ZH})
	if finding.Status != StatusUnknown {
		t.Fatalf("日志读不到应判为 unknown 而不是 %q (会给出虚假的安心)", finding.Status)
	}
	if !strings.Contains(finding.Summary, "未能读取") {
		t.Errorf("说明应讲清是读不到: %q", finding.Summary)
	}
}

// 同一件事被 journald 与 syslog 各记一次时不该算两条。
func TestDedupeLogs(t *testing.T) {
	text := `Sep 25 10:00:00 host nginx: error: upstream timed out
Sep 25 10:00:00 host nginx[123]: error: upstream timed out
`
	scan := dedupeLogs(parseLogs(text))
	if len(scan.Entries) != 1 {
		t.Fatalf("重复日志应合并, 实际 %d 条: %+v", len(scan.Entries), scan.Entries)
	}
}

func TestSplitSections(t *testing.T) {
	output := "__BRA_meta__\nLinux\n__BRA_cpu__\n0.5 0.4 0.3\n__BRA_end__\n"
	sections := splitSections(output)
	if sections["meta"] != "Linux" {
		t.Errorf("meta 段不对: %q", sections["meta"])
	}
	if sections["cpu"] != "0.5 0.4 0.3" {
		t.Errorf("cpu 段不对: %q", sections["cpu"])
	}
}

// ---------------------------------------------------------------------------
// 阈值判定
// ---------------------------------------------------------------------------

// fullSections 返回一份"健康"的采集输出, 各用例在其上制造问题。
func fullSections() map[string]string {
	return map[string]string{
		"meta": "BRA_OS=Debian GNU/Linux 13\nBRA_KERNEL=6.1.0-13-amd64\n" +
			"BRA_HOSTNAME=web-01\nBRA_UPTIME=up 3 days, 4 hours\nBRA_SYSTEMD=yes",
		"cpu":      "0.40 0.35 0.30 1/220 9999\n---\n8",
		"memory":   "MemTotal: 16333816 kB\nMemAvailable: 12200000 kB\nSwapTotal: 2097152 kB\nSwapFree: 2097152 kB\n",
		"disk":     "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 51471760 12345678 39126082 24% /\n---BLOCKDEV---\nsda 50G disk\n",
		"thermal":  "x86_pkg_temp=45000\n",
		"logs":     "Sep 25 10:02:00 host systemd: Started something\n",
		"services": "0 loaded units listed.\n",
	}
}

func TestEvaluateHealthyHost(t *testing.T) {
	report := Evaluate(Target{Host: "10.0.0.1", User: "root"}, fullSections(),
		time.Now(), time.Second, "SHA256:abc", DefaultThresholds(), i18n.ZH)

	if report.Status != StatusOK {
		t.Fatalf("健康主机应判为 ok, 实际 %q: %+v", report.Status, report.ProblemFindings())
	}
	if report.OS != "Debian GNU/Linux 13" || report.Kernel != "6.1.0-13-amd64" || report.HostName != "web-01" {
		t.Errorf("元信息解析不对: os=%q kernel=%q host=%q", report.OS, report.Kernel, report.HostName)
	}
	if report.HostKeyFP != "SHA256:abc" {
		t.Errorf("指纹未带上: %q", report.HostKeyFP)
	}
}

func TestEvaluateHighLoad(t *testing.T) {
	sections := fullSections()
	sections["cpu"] = "20.0 15.0 10.0 5/900 9999\n---\n8" // 每核 2.5

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "cpu")
	if finding.Status != StatusCritical {
		t.Fatalf("每核 2.5 应判为 critical: %+v", finding)
	}
	if !strings.Contains(finding.Summary, "每核 2.50") {
		t.Errorf("结论里应给出每核负载: %q", finding.Summary)
	}
}

// 拿不到核数时报 unknown: 不知道几核就无法判断负载是否偏高。
func TestEvaluateCPUWithoutCoreCount(t *testing.T) {
	sections := fullSections()
	sections["cpu"] = "1.0 1.0 1.0\n---\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "cpu")
	if finding.Status != StatusUnknown {
		t.Fatalf("没有核数应判 unknown: %+v", finding)
	}
}

func TestEvaluateLowMemory(t *testing.T) {
	sections := fullSections()
	sections["memory"] = "MemTotal: 1000000 kB\nMemAvailable: 50000 kB\nSwapTotal: 0 kB\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "memory")
	if finding.Status != StatusCritical {
		t.Fatalf("可用 5%% 应判 critical: %+v", finding)
	}
}

func TestEvaluateSwapPressure(t *testing.T) {
	sections := fullSections()
	sections["memory"] = "MemTotal: 1000000 kB\nMemAvailable: 900000 kB\nSwapTotal: 1000000 kB\nSwapFree: 200000 kB\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "memory")
	if finding.Status != StatusWarn {
		t.Fatalf("交换分区用掉 80%% 应告警: %+v", finding)
	}
}

func TestEvaluateDiskPressure(t *testing.T) {
	cases := []struct {
		capacity string
		want     Status
	}{
		{"50%", StatusOK},
		{"85%", StatusWarn},
		{"95%", StatusCritical},
	}
	for _, entry := range cases {
		sections := fullSections()
		sections["disk"] = "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
			"/dev/sda1 100000 85000 15000 " + entry.capacity + " /data\n"

		report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
		finding := findFinding(report, "disk")
		if finding.Status != entry.want {
			t.Errorf("使用率 %s 应判 %q, 实际 %q: %s", entry.capacity, entry.want, finding.Status, finding.Summary)
		}
	}
}

func TestEvaluateHighTemperature(t *testing.T) {
	sections := fullSections()
	sections["thermal"] = "x86_pkg_temp=92000\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "thermal")
	if finding.Status != StatusCritical {
		t.Fatalf("92°C 应判 critical: %+v", finding)
	}
}

// 虚拟机读不到温度是正常的, 但必须报 unknown 而不是"正常"。
func TestEvaluateNoTemperatureSensorsIsUnknown(t *testing.T) {
	sections := fullSections()
	sections["thermal"] = ""

	report := Evaluate(Target{Host: "vm"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "thermal")
	if finding.Status != StatusUnknown {
		t.Fatalf("读不到温度应判 unknown: %+v", finding)
	}
	if !strings.Contains(finding.Summary, "虚拟机") {
		t.Errorf("说明里应提示虚拟机属正常: %q", finding.Summary)
	}
	// 契约: 单项"无法判定"不让整份报告变色 (否则每台虚拟机都是非绿, 信号就废了),
	// 但结论行必须把"有项没判定成"讲出来 —— 不能说"全部正常"却漏着一项。
	if report.Status != StatusOK {
		t.Errorf("其它项都正常时整份报告仍应为 ok: %q", report.Status)
	}
	if !strings.Contains(report.SummaryLine(), "无法判定") {
		t.Errorf("结论行必须披露有项未能判定: %q", report.SummaryLine())
	}
}

func TestEvaluateCriticalLogs(t *testing.T) {
	sections := fullSections()
	sections["logs"] = "Sep 25 10:00:00 host kernel: Out of memory: Killed process 1234\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "logs")
	if finding.Status != StatusCritical {
		t.Fatalf("OOM 应判 critical: %+v", finding)
	}
}

// 无失败单元时 systemctl 输出 "0 loaded units listed.", 应判正常。
func TestEvaluateNoFailedServices(t *testing.T) {
	sections := fullSections()
	sections["services"] = "0 loaded units listed.\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "services")
	if finding.Status != StatusOK {
		t.Fatalf("没有失败单元应判 ok, 实际 %q: %s", finding.Status, finding.Summary)
	}
}

// 完全没有输出才判"无法判定" —— 那才可能是没有 systemd。
func TestEvaluateServicesMissingIsUnknown(t *testing.T) {
	sections := fullSections()
	sections["services"] = ""

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	if got := findFinding(report, "services").Status; got != StatusUnknown {
		t.Fatalf("完全没有输出应判 unknown, 实际 %q", got)
	}
}

// os-release 有多行 OS 信息时不该把后面的字段顶偏。
func TestParseMetaIsKeyBased(t *testing.T) {
	text := "BRA_OS=Ubuntu 24.04.5 LTS\nBRA_KERNEL=7.0.0-31-generic\n" +
		"BRA_HOSTNAME=web-01\nBRA_UPTIME=up 1 week\nBRA_SYSTEMD=yes"
	hostName, osName, kernel, uptime := parseMeta(text)
	if osName != "Ubuntu 24.04.5 LTS" {
		t.Errorf("OS 解析不对: %q", osName)
	}
	if kernel != "7.0.0-31-generic" {
		t.Errorf("内核解析不对: %q", kernel)
	}
	if hostName != "web-01" {
		t.Errorf("主机名解析不对: %q", hostName)
	}
	if uptime != "up 1 week" {
		t.Errorf("运行时长解析不对: %q", uptime)
	}
}

// 结论行要随报告一起返回, 否则界面上那个徽章是空的。
func TestReportCarriesSummary(t *testing.T) {
	report := Evaluate(Target{Host: "h"}, fullSections(), time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	if report.Summary == "" {
		t.Fatal("报告应带结论行")
	}
	if report.Summary != report.SummaryLine() {
		t.Errorf("结论行应与计算一致: %q vs %q", report.Summary, report.SummaryLine())
	}

	sections := fullSections()
	sections["disk"] = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100000 95000 5000 95% /\n"
	critical := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	if !strings.Contains(critical.Summary, "严重问题") {
		t.Errorf("有严重项时结论行应说明: %q", critical.Summary)
	}
}

// 连接失败时也要有结论行 (界面同样显示它)。
func TestFailedReportCarriesSummary(t *testing.T) {
	service := newTestService(t, nil, errors.New("认证失败"))
	report := service.InspectOne(Target{Host: "h", User: "root", Password: "p"})
	if report.Summary == "" {
		t.Fatal("失败报告也要有结论行")
	}
	if !strings.Contains(report.Summary, "失败") {
		t.Errorf("结论行应说明失败: %q", report.Summary)
	}
}

func TestEvaluateFailedServices(t *testing.T) {
	sections := fullSections()
	sections["services"] = "  nginx.service loaded failed failed nginx\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	finding := findFinding(report, "services")
	if finding.Status != StatusWarn {
		t.Fatalf("有失败单元应告警: %+v", finding)
	}
	if !strings.Contains(strings.Join(finding.Details, " "), "nginx") {
		t.Errorf("应列出失败的服务: %+v", finding.Details)
	}
}

// 整份报告的状态取最严重的一项。
func TestReportStatusIsWorstFinding(t *testing.T) {
	sections := fullSections()
	sections["disk"] = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 100000 95000 5000 95% /\n"

	report := Evaluate(Target{Host: "h"}, sections, time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)
	if report.Status != StatusCritical {
		t.Fatalf("应取最严重的一项: %q", report.Status)
	}
	if counts := report.Counts(); counts["critical"] != 1 {
		t.Errorf("统计不对: %+v", counts)
	}
	if line := report.SummaryLine(); !strings.Contains(line, "严重问题") {
		t.Errorf("结论行不对: %q", line)
	}
}

// ---------------------------------------------------------------------------
// 凭据安全
// ---------------------------------------------------------------------------

// 报告会落盘、会进历史、会返回给前端 —— 因此绝不能含密码或私钥。
func TestReportNeverContainsCredentials(t *testing.T) {
	const password = "sup3r-s3cret-passw0rd"
	// secret-scan:allow-test-key —— 下面是**假**密钥样本 (正文只有 AAAA / SECRETKEY / xxx
	// 之类的占位符), 只用来验证凭据处理路径会不会把密钥写进日志、argv 或报告。
	const privateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAsecret\n"

	report := Evaluate(Target{
		Host: "10.0.0.1", User: "root", Password: password, PrivateKey: privateKey,
	}, fullSections(), time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("序列化报告失败: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, password) {
		t.Fatal("报告里出现了密码")
	}
	if strings.Contains(text, "AAAAsecret") {
		t.Fatal("报告里出现了私钥")
	}
	// 认证方式要能说明 (供审计), 但不含凭据本身
	if report.AuthMode != "公钥" {
		t.Errorf("应报告认证方式: %q", report.AuthMode)
	}
	if !strings.Contains(report.Target, "10.0.0.1") {
		t.Errorf("应报告目标: %q", report.Target)
	}
}

// ---------------------------------------------------------------------------
// 编排
// ---------------------------------------------------------------------------

type fakeRunner struct {
	output string
	err    error
	script string
}

func (f *fakeRunner) Run(script string) (string, error) {
	f.script = script
	return f.output, f.err
}

func (f *fakeRunner) HostKeyFingerprint() string { return "SHA256:fake" }

// sectionsToOutput 把分段内容拼成采集脚本的输出形状。
func sectionsToOutput(sections map[string]string) string {
	var builder strings.Builder
	for _, id := range sectionIDs {
		builder.WriteString(markPrefix + id + markSuffix + "\n")
		builder.WriteString(sections[id])
		builder.WriteString("\n")
	}
	builder.WriteString(markPrefix + "end" + markSuffix + "\n")
	return builder.String()
}

func newTestService(t *testing.T, runner Runner, dialErr error) *Service {
	t.Helper()
	service := NewService(Options{
		StorePath: filepath.Join(t.TempDir(), "reports.json"),
	})
	service.SetDialer(func(Target) (Runner, func(), error) {
		if dialErr != nil {
			return nil, func() {}, dialErr
		}
		return runner, func() {}, nil
	})
	return service
}

func TestServiceInspectOne(t *testing.T) {
	service := newTestService(t, &fakeRunner{output: sectionsToOutput(fullSections())}, nil)

	report := service.InspectOne(Target{Host: "10.0.0.1", User: "root", Password: "x"})
	if report.Error != "" {
		t.Fatalf("不该失败: %s", report.Error)
	}
	if report.Status != StatusOK {
		t.Fatalf("健康主机应为 ok: %s", report.SummaryLine())
	}
	if report.ID == "" {
		t.Error("报告应有 id (历史记录要用)")
	}
	if report.HostKeyFP != "SHA256:fake" {
		t.Errorf("应带回主机指纹: %q", report.HostKeyFP)
	}
}

// 连不上时报告要说明原因, 而不是给一份"没有问题"的空报告。
func TestServiceConnectionFailureIsReported(t *testing.T) {
	service := newTestService(t, nil, errors.New("认证失败: 用户名、密码或密钥不正确"))

	report := service.InspectOne(Target{Host: "10.0.0.1", User: "root", Password: "bad"})
	if report.Error == "" {
		t.Fatal("连不上必须报出来")
	}
	if !strings.Contains(report.Error, "认证失败") {
		t.Errorf("错误原因应可读: %q", report.Error)
	}
	if len(report.Findings) != 0 {
		t.Error("失败时不该有检查项 (否则会显示成检查通过)")
	}
	if !strings.Contains(report.SummaryLine(), "失败") {
		t.Errorf("结论行应说明失败: %q", report.SummaryLine())
	}
}

// 一台失败不影响其它主机 —— 巡检最怕整批没有结果。
func TestServiceInspectMultipleHosts(t *testing.T) {
	service := NewService(Options{Concurrency: 2})
	// dialer 会被**并发**调用 (这正是巡检要的行为), 所以计数必须原子。
	// 普通 int 在这里是数据竞争: -race 能抓到, 但只有竞争真的发生时才暴露。
	var calls atomic.Int64
	service.SetDialer(func(target Target) (Runner, func(), error) {
		calls.Add(1)
		if target.Host == "bad" {
			return nil, func() {}, errors.New("网络不可达")
		}
		return &fakeRunner{output: sectionsToOutput(fullSections())}, func() {}, nil
	})

	reports := service.Inspect([]Target{
		{Host: "good-1", User: "root", Password: "p"},
		{Host: "bad", User: "root", Password: "p"},
		{Host: "good-2", User: "root", Password: "p"},
	})
	if got := calls.Load(); got != 3 {
		t.Fatalf("三台都该被巡检, 实际 %d 次", got)
	}
	if len(reports) != 3 {
		t.Fatalf("应返回三份报告: %d", len(reports))
	}
	// 顺序与输入一致, 失败的那台在中间
	if reports[0].Host != "good-1" || reports[1].Host != "bad" || reports[2].Host != "good-2" {
		t.Errorf("报告顺序应与输入一致: %v %v %v",
			reports[0].Host, reports[1].Host, reports[2].Host)
	}
	if reports[1].Error == "" {
		t.Error("失败的那台应有错误说明")
	}
	if reports[0].Error != "" || reports[2].Error != "" {
		t.Error("正常的主机不该受影响")
	}
}

// 采集脚本是固定文本, 且每项都有超时保护。
func TestRemoteScriptIsFixedAndGuarded(t *testing.T) {
	// 确定性: 生成过程里不能有 map 遍历之类的随机顺序 —— Go 每次遍历 map 的顺序都不同,
	// 所以"多调几次必须完全一致"是一条真能抓到问题的断言。
	first := remoteScript(15)
	for round := 0; round < 3; round++ {
		if again := remoteScript(15); again != first {
			t.Fatalf("采集脚本应是确定的固定文本, 第 %d 次与首次不同", round+2)
		}
	}
	for _, id := range []string{"cpu", "memory", "disk", "thermal", "logs", "services"} {
		if !strings.Contains(first, markPrefix+id+markSuffix) {
			t.Errorf("采集脚本缺少 %s 段标记", id)
		}
	}
	// 一项卡住不能拖垮整次采集
	if count := strings.Count(first, "timeout "); count < 6 {
		t.Errorf("每项检查都应套 timeout, 实际 %d 处", count)
	}
}

// 注入防线: 主机/用户名/密码只作为独立 argv 传递, 绝不拼进远端脚本。
//
// 这是本功能唯一能把用户输入带进远端执行的地方, 因此单独验一遍: 给一组带 shell
// 元字符的输入, 确认脚本参数干净、且目标恰好是它自己那一个参数。
func TestSSHArgsKeepUserInputOutOfScript(t *testing.T) {
	hostile := Target{
		Host:     "10.0.0.1; rm -rf /",
		User:     "root$(id)",
		Password: "p`id`",
	}
	runner, err := newSSHRunner(hostile, SSHOptions{})
	if err != nil {
		t.Fatalf("构造 runner 失败: %v", err)
	}
	script := remoteScript(15)
	args := runner.buildArgs(script)

	// 最后一个参数就是脚本本身, 必须与固定文本逐字节一致
	if args[len(args)-1] != script {
		t.Fatal("脚本参数被改动了")
	}
	for _, needle := range []string{"rm -rf", "$(id)", "`id`"} {
		if strings.Contains(args[len(args)-1], needle) {
			t.Errorf("脚本里出现了用户输入 %q —— 存在命令注入", needle)
		}
	}
	// 目标作为它自己那一个参数, 不会被 shell 解释 (exec.Command 不经过 shell)
	if args[len(args)-2] != "root$(id)@10.0.0.1; rm -rf /" {
		t.Errorf("目标参数不对: %q", args[len(args)-2])
	}
}

// 密码不进 argv —— ps 里能看到命令行, 而环境变量只有同用户可读。
func TestPasswordNeverEntersArgv(t *testing.T) {
	const password = "sup3r-s3cret"
	runner, err := newSSHRunner(Target{Host: "h", User: "root", Password: password}, SSHOptions{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	for _, arg := range runner.buildArgs(remoteScript(15)) {
		if strings.Contains(arg, password) {
			t.Fatalf("密码出现在 ssh 参数里 (ps 可见): %q", arg)
		}
	}
}

// 部分输出 + 非零退出: 结果可用但要提示不完整。
func TestServicePartialOutputWarns(t *testing.T) {
	service := newTestService(t, &fakeRunner{
		output: sectionsToOutput(fullSections()),
		err:    errors.New("ssh 执行失败: 连接被重置"),
	}, nil)

	report := service.InspectOne(Target{Host: "h", User: "root", Password: "p"})
	if report.Error != "" {
		t.Fatalf("有输出时不该整份失败: %s", report.Error)
	}
	finding := findFinding(report, "ssh")
	if finding.Status != StatusWarn {
		t.Fatalf("应提示采集不完整: %+v", finding)
	}
	// 追加检查项之后必须重新汇总, 否则报告停在中途的状态上
	if report.Status != StatusWarn {
		t.Errorf("状态应包含追加的告警: %q", report.Status)
	}
	if !strings.Contains(report.Summary, "告警") {
		t.Errorf("结论行应反映追加的告警: %q", report.Summary)
	}
}

// ---------------------------------------------------------------------------
// 历史留存
// ---------------------------------------------------------------------------

func TestStorePersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.json")
	service := newTestService(t, &fakeRunner{output: sectionsToOutput(fullSections())}, nil)
	service.options.StorePath = path
	service.store.path = path

	// 历史留存发生在批量入口 Inspect 上 (InspectOne 是工作单元, 不落盘)
	reports := service.Inspect([]Target{{Host: "10.0.0.1", User: "root", Password: "p"}})
	if len(reports) != 1 {
		t.Fatalf("应返回 1 份报告: %d", len(reports))
	}
	report := reports[0]
	if len(service.List()) != 1 {
		t.Fatalf("应有 1 条历史: %d", len(service.List()))
	}

	// 模拟重启
	reloaded := newStore(path, 30)
	if len(reloaded.list()) != 1 {
		t.Fatalf("重启后应保留历史: %d", len(reloaded.list()))
	}
	if reloaded.list()[0].ID != report.ID {
		t.Error("历史记录内容不符")
	}

	// 权限: 报告含主机名与日志片段, 不该对所有人可读
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("历史未落盘: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("历史文件权限应为 0600, 实际 %04o", mode)
	}
}

func TestStorePrunesOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.json")
	instance := newStore(path, 3)

	for index := 0; index < 5; index++ {
		instance.add([]Report{{
			ID:        "insp-" + string(rune('a'+index)),
			Host:      "h",
			StartedAt: time.Now().Add(time.Duration(index) * time.Second),
		}})
	}
	if len(instance.list()) != 3 {
		t.Fatalf("应裁剪到 3 条: %d", len(instance.list()))
	}
	// 最新的在最前
	if instance.list()[0].ID != "insp-e" {
		t.Errorf("应按时间倒序: %v", instance.list()[0].ID)
	}
}

func TestStoreDeleteAndGet(t *testing.T) {
	instance := newStore(filepath.Join(t.TempDir(), "r.json"), 10)
	instance.add([]Report{{ID: "insp-1", Host: "h1"}, {ID: "insp-2", Host: "h2"}})

	if _, ok := instance.get("insp-1"); !ok {
		t.Error("应能取到记录")
	}
	if instance.delete("insp-1") != true {
		t.Error("删除应成功")
	}
	if instance.delete("insp-1") != false {
		t.Error("重复删除应返回 false")
	}
	if len(instance.list()) != 1 {
		t.Errorf("删除后应剩 1 条: %d", len(instance.list()))
	}
}

func TestStoreCorruptFileDoesNotBreakStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports.json")
	if err := os.WriteFile(path, []byte("{ 不是 JSON"), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	instance := newStore(path, 10)
	if len(instance.list()) != 0 {
		t.Error("损坏的历史应被忽略而不是崩掉")
	}
	instance.add([]Report{{ID: "insp-1", Host: "h"}})
	if len(instance.list()) != 1 {
		t.Error("损坏后仍应能正常写入")
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func findFinding(report Report, id string) Finding {
	for _, finding := range report.Findings {
		if finding.ID == id {
			return finding
		}
	}
	return Finding{}
}

func TestHumanKB(t *testing.T) {
	cases := map[int64]string{
		512:             "512 KB",
		2048:            "2.0 MB",
		3 * 1024 * 1024: "3.0 GB",
	}
	for input, want := range cases {
		if got := humanKB(input); got != want {
			t.Errorf("humanKB(%d) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestParseRatio(t *testing.T) {
	left, right, ok := parseRatio("2/345")
	if !ok || left != 2 || right != 345 {
		t.Fatalf("解析不对: %d %d %v", left, right, ok)
	}
	if _, _, ok := parseRatio("nonsense"); ok {
		t.Error("非法输入不该解析成功")
	}
}

func TestEnsureTrailingNewline(t *testing.T) {
	if got := ensureTrailingNewline("key"); got != "key\n" {
		t.Errorf("应补换行: %q", got)
	}
	if got := ensureTrailingNewline("key\n"); got != "key\n" {
		t.Errorf("不该重复补换行: %q", got)
	}
}

func TestIsAskpassInvocation(t *testing.T) {
	if IsAskpassInvocation() {
		t.Error("默认不该处于 askpass 模式")
	}
	t.Setenv(askpassEnvMarker, "1")
	if !IsAskpassInvocation() {
		t.Error("设置了标记就该处于 askpass 模式")
	}
}

// OS 行要是 "BRA_OS=值", 而不是 "BRA_OS=PRETTY_NAME=值"。
func TestParseMetaStripsOSKey(t *testing.T) {
	_, osName, _, _ := parseMeta(`BRA_OS=Ubuntu 24.04.5 LTS`)
	if osName != "Ubuntu 24.04.5 LTS" {
		t.Fatalf("OS 值不该带原始键名: %q", osName)
	}
}

// 采集脚本里 meta 段必须是"替换键名"而不是"加前缀"。
func TestRemoteScriptRenamesOSKey(t *testing.T) {
	script := remoteScript(15)
	if !strings.Contains(script, "s/^PRETTY_NAME=/BRA_OS=/") {
		t.Error("应替换键名, 否则 OS 值会带上 PRETTY_NAME=")
	}
	if !strings.Contains(script, "BRA_KERNEL=") || !strings.Contains(script, "BRA_HOSTNAME=") {
		t.Error("元信息应按 BRA_* 键输出")
	}
}

// journalctl 的占位行不是日志内容, 不该算进条数。
func TestLogPlaceholdersAreSkipped(t *testing.T) {
	text := "-- No entries --\n-- Logs begin at Mon 2026-09-01, end at now. --\n" +
		"-- Reboot --\nSep 25 10:00:00 host app: real error here\n"
	scan := parseLogs(text)
	if len(scan.Entries) != 1 {
		t.Fatalf("只应保留真正的日志行, 实际 %d 条: %+v", len(scan.Entries), scan.Entries)
	}
	if !strings.Contains(scan.Entries[0].Text, "real error") {
		t.Errorf("保留的应是真实日志: %+v", scan.Entries[0])
	}
}

// dmesg 的行来自 err 级过滤, 关键字没命中也要按错误计。
func TestDmesgLinesDefaultToError(t *testing.T) {
	text := "---DMESG---\n[ 863.122265] db_root: cannot open: /etc/target\n"
	scan := parseLogs(text)
	if len(scan.Entries) != 1 {
		t.Fatalf("应有 1 条: %+v", scan.Entries)
	}
	if scan.Entries[0].Severity != "err" {
		t.Fatalf("dmesg 条目应计为 err, 实际 %q", scan.Entries[0].Severity)
	}
}

// TOFU 的意义在于"信任了什么"可见 —— 指纹必须真的读出来。
func TestHostKeyFingerprintIsReadFromKnownHosts(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("环境没有 ssh-keygen")
	}

	dir := t.TempDir()
	// 用一把真实格式的密钥生成 known_hosts 条目
	command := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", filepath.Join(dir, "k"), "-N", "", "-C", "t")
	if err := command.Run(); err != nil {
		t.Skipf("生成测试密钥失败: %v", err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "k.pub"))
	if err != nil {
		t.Fatalf("读取公钥失败: %v", err)
	}
	fields := strings.Fields(string(pub))
	knownHosts := filepath.Join(dir, "known_hosts")
	line := "10.0.0.1 " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(knownHosts, []byte(line), 0o600); err != nil {
		t.Fatalf("写 known_hosts 失败: %v", err)
	}

	runner := &sshRunner{
		target:  Target{Host: "10.0.0.1", Port: 22},
		options: SSHOptions{KnownHostsPath: knownHosts},
	}
	got := runner.hostKeyFingerprint()
	if !strings.HasPrefix(got, "SHA256:") {
		t.Fatalf("应读出 SHA256 指纹, 实际 %q", got)
	}

	// 与 ssh-keygen 直接算的结果一致
	direct := exec.Command("ssh-keygen", "-lf", filepath.Join(dir, "k.pub"))
	out, err := direct.Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf 失败: %v", err)
	}
	want := strings.Fields(string(out))[1]
	if got != want {
		t.Fatalf("指纹不符: 得到 %q, 期望 %q", got, want)
	}
}

// 非默认端口要按 "[host]:port" 匹配。
func TestHostKeyFingerprintMatchesNonDefaultPort(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("环境没有 ssh-keygen")
	}
	dir := t.TempDir()
	command := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", filepath.Join(dir, "k"), "-N", "", "-C", "t")
	if err := command.Run(); err != nil {
		t.Skipf("生成密钥失败: %v", err)
	}
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	fields := strings.Fields(string(pub))
	knownHosts := filepath.Join(dir, "known_hosts")
	// 22 端口的条目不该被 2222 端口的查询命中
	content := "10.0.0.1 " + fields[0] + " " + fields[1] + "\n" +
		"[10.0.0.2]:2222 " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(knownHosts, []byte(content), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	runner := &sshRunner{
		target:  Target{Host: "10.0.0.2", Port: 2222},
		options: SSHOptions{KnownHostsPath: knownHosts},
	}
	if got := runner.hostKeyFingerprint(); !strings.HasPrefix(got, "SHA256:") {
		t.Fatalf("非默认端口应能匹配到: %q", got)
	}

	missing := &sshRunner{
		target:  Target{Host: "10.0.0.3", Port: 22},
		options: SSHOptions{KnownHostsPath: knownHosts},
	}
	if got := missing.hostKeyFingerprint(); got != "" {
		t.Fatalf("未知主机不该给出指纹: %q", got)
	}
}

// 没配 known_hosts 时给空串, 而不是编一个指纹出来。
func TestHostKeyFingerprintEmptyWithoutKnownHosts(t *testing.T) {
	runner := &sshRunner{target: Target{Host: "10.0.0.1"}, options: SSHOptions{}}
	if got := runner.hostKeyFingerprint(); got != "" {
		t.Fatalf("没配 known_hosts 时不该有指纹: %q", got)
	}
}

// OpenSSH 默认把主机名哈希后存储, 逐行比对永远匹配不上 —— 必须用 ssh-keygen -F。
func TestHostKeyFingerprintHandlesHashedKnownHosts(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("环境没有 ssh-keygen")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k")
	if err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", keyPath, "-N", "", "-C", "t").Run(); err != nil {
		t.Skipf("生成密钥失败: %v", err)
	}
	pub, _ := os.ReadFile(keyPath + ".pub")
	fields := strings.Fields(string(pub))

	// 造一个哈希条目: 手工哈希不可行, 改为让 ssh-keygen 自己写
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatalf("建文件失败: %v", err)
	}
	// ssh-keygen -H 会把文件里的明文主机名转成哈希形式
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("10.0.0.1 "+fields[0]+" "+fields[1]+"\n"), 0o600); err != nil {
		t.Fatalf("写明文条目失败: %v", err)
	}
	if err := exec.Command("ssh-keygen", "-H", "-f", plain).Run(); err != nil {
		t.Skipf("ssh-keygen -H 不可用: %v", err)
	}
	hashed, _ := os.ReadFile(plain)
	if !strings.Contains(string(hashed), "|1|") {
		t.Skipf("该环境的 ssh-keygen 未产生哈希条目: %s", string(hashed))
	}
	if err := os.WriteFile(knownHosts, hashed, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	runner := &sshRunner{
		target:  Target{Host: "10.0.0.1", Port: 22},
		options: SSHOptions{KnownHostsPath: knownHosts},
	}
	got := runner.hostKeyFingerprint()
	if !strings.HasPrefix(got, "SHA256:") {
		t.Fatalf("哈希条目下也应读出指纹, 实际 %q (known_hosts: %s)", got, string(hashed))
	}

	// 与 ssh-keygen -lf 一致
	out, err := exec.Command("ssh-keygen", "-lf", keyPath+".pub").Output()
	if err != nil {
		t.Fatalf("ssh-keygen -lf 失败: %v", err)
	}
	if want := strings.Fields(string(out))[1]; got != want {
		t.Fatalf("指纹不符: %q vs %q", got, want)
	}
}

// 报告里必须写清每个日志来源的时间窗。
//
// dmesg 是"本次启动以来"的环形缓冲 —— 里面的事件可能发生在几周前, 而 journalctl
// 那段是近 24 小时。不写清的话, "发现 N 条错误日志"这句话就没法解释 (实测 .38 上
// 78 条 iSCSI 报错其实来自两周前的启动初期)。
func TestLogFindingStatesSourceWindows(t *testing.T) {
	output := "__BRA_logs__\nSep 25 04:12:11 host journald-line kernel: ERROR something failed\n" +
		"---DMESG---\n[ 1114.763770] MODE SENSE: unimplemented page/subpage: 0x0a/0x05\n"
	report := Evaluate(Target{Host: "h", User: "root"}, map[string]string{"logs": output},
		time.Now(), time.Second, "", DefaultThresholds(), i18n.ZH)

	var logs *Finding
	for index := range report.Findings {
		if report.Findings[index].ID == "logs" {
			logs = &report.Findings[index]
		}
	}
	if logs == nil {
		t.Fatal("缺少日志检查项")
	}
	joined := strings.Join(logs.Details, " | ")
	if !strings.Contains(joined, "近 24 小时") || !strings.Contains(joined, "本次启动以来") {
		t.Errorf("应说明各来源的时间窗, 实际: %s", joined)
	}
	if strings.Contains(logs.Summary, "近 24 小时") {
		t.Errorf("结论行不该声称固定的时间窗 (dmesg 覆盖启动以来): %q", logs.Summary)
	}
}

// 一台主机的内部错误不该带走整批结果, 更不该带走整个进程。
//
// 巡检是成批做的: 一路 panic 会让另外几十台的结果一起丢掉, 用户看到的是"服务挂了"。
// 这里让 dialer 对特定主机 panic, 断言: 服务不崩、其它主机照常出结果、出事那台如实
// 标成未完成 (绝不能被显示成"检查通过")。
func TestInspectSurvivesPanicInOneHost(t *testing.T) {
	service := NewService(Options{Concurrency: 2})
	service.SetDialer(func(target Target) (Runner, func(), error) {
		if target.Host == "boom" {
			panic("模拟巡检实现里的意外")
		}
		return &fakeRunner{output: sectionsToOutput(fullSections())}, func() {}, nil
	})

	reports := service.Inspect([]Target{
		{Host: "good-1", User: "root", Password: "p"},
		{Host: "boom", User: "root", Password: "p"},
		{Host: "good-2", User: "root", Password: "p"},
	})
	if len(reports) != 3 {
		t.Fatalf("应返回三份报告: %d", len(reports))
	}
	for _, index := range []int{0, 2} {
		if reports[index].Error != "" {
			t.Errorf("正常主机不该被牵连: %+v", reports[index])
		}
		if len(reports[index].Findings) == 0 {
			t.Errorf("正常主机应有检查项: %+v", reports[index])
		}
	}
	broken := reports[1]
	if broken.Status != StatusUnknown {
		t.Errorf("出事的主机状态应为 unknown, 实际 %q", broken.Status)
	}
	if !strings.Contains(broken.Error, "内部错误") {
		t.Errorf("应说明是内部错误: %q", broken.Error)
	}
	if len(broken.Findings) != 0 {
		t.Error("内部错误时不该有检查项 (会被显示成检查通过)")
	}
	if broken.Host != "boom" {
		t.Errorf("报告应归属出事的那台主机: %q", broken.Host)
	}
}
