package inspect

import (
	"fmt"

	"backup-restore-agent/internal/i18n"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Thresholds 是巡检的判定阈值。
//
// 全部集中在这里而不是散在判定逻辑里: 不同环境的容忍度差别很大 (压测机常年高负载,
// 归档机的磁盘常年 85%), 集中放置才好在将来做成可配置。
type Thresholds struct {
	LoadPerCoreWarn     float64
	LoadPerCoreCritical float64
	MemoryAvailWarn     float64 // 可用内存低于该百分比 -> 告警
	MemoryAvailCritical float64
	SwapUsedWarnPercent float64
	DiskUseWarnPercent  int
	DiskUseCritical     int
	TempWarnCelsius     float64
	TempCriticalCelsius float64
	// LogErrorWarnCount: 错误日志达到多少条开始告警 (0 表示只要有一条就报)
	LogErrorWarnCount int
}

// DefaultThresholds 返回默认阈值。
func DefaultThresholds() Thresholds {
	return Thresholds{
		LoadPerCoreWarn:     1.0,
		LoadPerCoreCritical: 2.0,
		MemoryAvailWarn:     20,
		MemoryAvailCritical: 10,
		SwapUsedWarnPercent: 50,
		DiskUseWarnPercent:  80,
		DiskUseCritical:     90,
		TempWarnCelsius:     80,
		TempCriticalCelsius: 90,
		LogErrorWarnCount:   0,
	}
}

// Evaluate 把采集到的各段输出判定成一份报告 (纯函数, 便于测试)。
func Evaluate(target Target, sections map[string]string, startedAt time.Time, duration time.Duration, hostKeyFP string, th Thresholds, lang i18n.Lang) Report {
	r := renderer{lang: lang}
	report := Report{
		Target:     target.Address(),
		User:       target.User,
		Host:       target.Host,
		AuthMode:   target.AuthMode(),
		StartedAt:  startedAt,
		DurationMS: duration.Milliseconds(),
		HostKeyFP:  hostKeyFP,
		Findings:   []Finding{},
	}

	report.HostName, report.OS, report.Kernel, report.Uptime = parseMeta(sections["meta"])

	report.Findings = append(report.Findings,
		evaluateCPU(sections["cpu"], th, r),
		evaluateMemory(sections["memory"], th, r),
		evaluateDisk(sections["disk"], th, r),
		evaluateThermal(sections["thermal"], th, r),
		evaluateLogs(sections["logs"], th, r),
		evaluateServices(sections["services"], r),
	)

	report.finalize(r.lang)
	return report
}

// finalize 汇总状态与结论行。
//
// 单独成函数是因为调用方可能随后追加检查项 (例如"采集完整性"), 那之后必须重新汇总,
// 否则报告会停在中途的状态上。
func (r *Report) finalize(lang i18n.Lang) {
	r.Status = StatusOK
	for _, finding := range r.Findings {
		r.Status = Worse(r.Status, finding.Status)
	}
	r.Summary = r.SummaryLineLang(lang)
}

// parseMeta 解析元信息段 (采集脚本按 BRA_* 键输出)。
//
// 按键取值而不是按行序: os-release 里 PRETTY_NAME 与 VERSION 可能各占一行,
// 行序一变就会把主机名解析成内核版本。
func parseMeta(text string) (hostName, osName, kernel, uptime string) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "BRA_OS="):
			osName = strings.Trim(strings.TrimPrefix(trimmed, "BRA_OS="), `"`)
		case strings.HasPrefix(trimmed, "BRA_KERNEL="):
			kernel = strings.TrimSpace(strings.TrimPrefix(trimmed, "BRA_KERNEL="))
		case strings.HasPrefix(trimmed, "BRA_HOSTNAME="):
			hostName = strings.TrimSpace(strings.TrimPrefix(trimmed, "BRA_HOSTNAME="))
		case strings.HasPrefix(trimmed, "BRA_UPTIME="):
			uptime = strings.TrimSpace(strings.TrimPrefix(trimmed, "BRA_UPTIME="))
		}
	}
	return hostName, osName, kernel, uptime
}

func evaluateCPU(section string, th Thresholds, r renderer) Finding {
	finding := Finding{ID: "cpu", Name: r.t("CPU 负载")}

	parts := strings.SplitN(section, "---", 2)
	load, ok := parseLoadAverage(parts[0])
	if !ok {
		finding.Status = StatusUnknown
		finding.Summary = r.t("未能读取 /proc/loadavg")
		finding.Error = strings.TrimSpace(section)
		return finding
	}

	cores := 0
	if len(parts) > 1 {
		// 顺序要紧: 分段以换行开头, 必须先 TrimSpace 再取首行, 否则永远取到空串 ——
		// 表现为"核数拿不到、CPU 负载永远无法判定"。
		if value, err := parseInt(firstLine(strings.TrimSpace(parts[1]))); err == nil {
			cores = value
		}
	}

	perCore := 0.0
	if cores > 0 {
		perCore = load.Load1 / float64(cores)
	}

	switch {
	case cores == 0:
		// 拿不到核数就只能报原始负载, 并说明无法判定
		finding.Status = StatusUnknown
		finding.Summary = r.tf("1 分钟负载 %.2f（未能取得核数, 无法判定是否偏高）", load.Load1)
	case perCore >= th.LoadPerCoreCritical:
		finding.Status = StatusCritical
		finding.Summary = r.tf("1 分钟负载 %.2f, 每核 %.2f（%d 核）, 明显偏高", load.Load1, perCore, cores)
	case perCore >= th.LoadPerCoreWarn:
		finding.Status = StatusWarn
		finding.Summary = r.tf("1 分钟负载 %.2f, 每核 %.2f（%d 核）, 偏高", load.Load1, perCore, cores)
	default:
		finding.Status = StatusOK
		finding.Summary = r.tf("1 分钟负载 %.2f, 每核 %.2f（%d 核）", load.Load1, perCore, cores)
	}

	finding.Details = append(finding.Details,
		r.tf("负载(1/5/15 分钟): %.2f / %.2f / %.2f", load.Load1, load.Load5, load.Load15))
	if cores > 0 {
		finding.Details = append(finding.Details, r.tf("CPU 核数: %d", cores))
	}
	if load.Total > 0 {
		finding.Details = append(finding.Details,
			r.tf("运行/总进程: %d / %d", load.Running, load.Total))
	}
	finding.Evidence = truncateText(section, 800)
	return finding
}

func evaluateMemory(section string, th Thresholds, r renderer) Finding {
	finding := Finding{ID: "memory", Name: r.t("内存")}

	info, ok := parseMeminfo(section)
	if !ok {
		finding.Status = StatusUnknown
		finding.Summary = r.t("未能读取 /proc/meminfo")
		finding.Error = strings.TrimSpace(section)
		return finding
	}

	availablePercent := float64(info.AvailableKB) / float64(info.TotalKB) * 100

	switch {
	case availablePercent < th.MemoryAvailCritical:
		finding.Status = StatusCritical
		finding.Summary = r.tf("可用内存仅 %.1f%%（%s / %s）",
			availablePercent, humanKB(info.AvailableKB), humanKB(info.TotalKB))
	case availablePercent < th.MemoryAvailWarn:
		finding.Status = StatusWarn
		finding.Summary = r.tf("可用内存偏低 %.1f%%（%s / %s）",
			availablePercent, humanKB(info.AvailableKB), humanKB(info.TotalKB))
	default:
		finding.Status = StatusOK
		finding.Summary = r.tf("已用 %.1f%%, 可用 %s / %s",
			info.UsedPercent(), humanKB(info.AvailableKB), humanKB(info.TotalKB))
	}

	finding.Details = append(finding.Details,
		r.tf("总计 %s, 已用 %s, 可用 %s",
			humanKB(info.TotalKB), humanKB(info.TotalKB-info.AvailableKB), humanKB(info.AvailableKB)))

	if swap := info.SwapUsedPercent(); swap < 0 {
		finding.Details = append(finding.Details, r.t("未启用交换分区"))
	} else {
		swapLine := r.tf("交换分区已用 %.1f%%（%s / %s）",
			swap, humanKB(info.SwapTotalKB-info.SwapFreeKB), humanKB(info.SwapTotalKB))
		finding.Details = append(finding.Details, swapLine)
		if swap >= th.SwapUsedWarnPercent && finding.Status == StatusOK {
			// 交换分区被大量占用通常意味着内存压力已经发生过
			finding.Status = StatusWarn
			finding.Summary += "；交换分区使用率偏高"
		}
	}

	finding.Evidence = truncateText(section, 800)
	return finding
}

func evaluateDisk(section string, th Thresholds, r renderer) Finding {
	finding := Finding{ID: "disk", Name: r.t("磁盘分区")}

	dfPart := strings.SplitN(section, "---BLOCKDEV---", 2)[0]
	mounts := parseDf(dfPart)
	if len(mounts) == 0 {
		finding.Status = StatusUnknown
		finding.Summary = r.t("未能读取磁盘分区信息")
		finding.Error = strings.TrimSpace(section)
		return finding
	}

	worst := mounts[0] // parseDf 已按使用率倒序
	switch {
	case worst.UsePercent >= th.DiskUseCritical:
		finding.Status = StatusCritical
		finding.Summary = r.tf("%s 使用率 %d%%, 已接近写满", worst.MountPoint, worst.UsePercent)
	case worst.UsePercent >= th.DiskUseWarnPercent:
		finding.Status = StatusWarn
		finding.Summary = r.tf("%s 使用率 %d%%, 建议关注", worst.MountPoint, worst.UsePercent)
	default:
		finding.Status = StatusOK
		finding.Summary = r.tf("%d 个挂载点, 最高使用率 %d%% (%s)",
			len(mounts), worst.UsePercent, worst.MountPoint)
	}

	for _, mount := range mounts {
		finding.Details = append(finding.Details, r.tf("%s  %d%%  已用 %s / 可用 %s",
			mount.MountPoint, mount.UsePercent, humanKB(mount.UsedKB), humanKB(mount.AvailKB)))
	}

	// 块设备拓扑能帮运维定位"哪个物理盘"（虚拟机上通常也有）
	if len(section) > 0 {
		if blockdev := strings.SplitN(section, "---BLOCKDEV---", 2); len(blockdev) == 2 {
			if topology := strings.TrimSpace(blockdev[1]); topology != "" {
				finding.Evidence = truncateText("df:\n"+dfPart+"\n块设备:\n"+topology, 1200)
				return finding
			}
		}
	}
	finding.Evidence = truncateText(dfPart, 1200)
	return finding
}

func evaluateThermal(section string, th Thresholds, r renderer) Finding {
	finding := Finding{ID: "thermal", Name: r.t("温度")}

	temperatures := parseThermal(section)
	if len(temperatures) == 0 {
		// 虚拟机、云主机通常不暴露温度传感器 —— 这是"读不到", 不是"温度正常"
		finding.Status = StatusUnknown
		finding.Summary = "未能读取温度（虚拟机或未暴露温度传感器属正常情况）"
		finding.Evidence = truncateText(section, 400)
		return finding
	}

	sort.SliceStable(temperatures, func(i, j int) bool {
		return temperatures[i].Celsius > temperatures[j].Celsius
	})
	highest := temperatures[0]

	switch {
	case highest.Celsius >= th.TempCriticalCelsius:
		finding.Status = StatusCritical
		finding.Summary = r.tf("%s 达 %.1f°C, 过热", highest.Sensor, highest.Celsius)
	case highest.Celsius >= th.TempWarnCelsius:
		finding.Status = StatusWarn
		finding.Summary = r.tf("%s 达 %.1f°C, 偏高", highest.Sensor, highest.Celsius)
	default:
		finding.Status = StatusOK
		finding.Summary = r.tf("最高 %.1f°C (%s)", highest.Celsius, highest.Sensor)
	}

	for _, temperature := range temperatures {
		finding.Details = append(finding.Details,
			fmt.Sprintf("%s: %.1f°C", temperature.Sensor, temperature.Celsius))
	}
	finding.Evidence = truncateText(section, 600)
	return finding
}

func evaluateLogs(section string, th Thresholds, r renderer) Finding {
	finding := Finding{ID: "logs", Name: r.t("严重错误日志")}

	scan := dedupeLogs(parseLogs(section))
	// 先用规则库抬升等级再统计。
	//
	// 通用关键字判定认不出所有严重模式: "zfs: checksum mismatch" 里没有 error/failed 字样,
	// 会被当成待确认条目, 于是整份报告状态仍是"正常" —— 但数据可能已经坏了。
	// 规则表知道得更多, 以它为准。
	for index := range scan.Entries {
		if rule := matchRule(scan.Entries[index].Text); rule != nil && rule.Severity != "" {
			scan.Entries[index].Severity = worseSeverityLabel(scan.Entries[index].Severity, rule.Severity)
		}
	}
	criticalCount := scan.CountSeverity("critical")
	errorCount := scan.CountSeverity("err")

	// 影响分析与处置建议: 只对真正需要处理的条目做 (critical/err),
	// 把"待确认"的条目也一起分析 —— 它们往往正是"看起来不对但没写 error 字样"的那种。
	var actionable []LogEntry
	for _, entry := range scan.Entries {
		if entry.Severity == "critical" || entry.Severity == "err" {
			actionable = append(actionable, entry)
		}
	}
	if len(actionable) > 0 {
		finding.Diagnoses = DiagnoseLogs(actionable)
	}

	switch {
	case criticalCount > 0:
		finding.Status = StatusCritical
		finding.Summary = r.tf("发现 %d 条严重日志%s", criticalCount, diagnosisHint(r, finding.Diagnoses))
	case errorCount > th.LogErrorWarnCount:
		finding.Status = StatusWarn
		// 不写"近 24 小时": 日志来自多个来源, 时间窗各不相同 (见下面的来源说明),
		// 把 since boot 的 dmesg 行说成"近 24 小时"是错的 —— 那些行可能已经躺了两周。
		finding.Summary = r.tf("发现 %d 条错误日志%s", errorCount, diagnosisHint(r, finding.Diagnoses))
	case len(scan.Entries) == 0 && len(scan.SourcesUnavailable) > 0:
		// 读不到日志 ≠ 没有错误: 必须如实说明, 否则会给出虚假的安心
		finding.Status = StatusUnknown
		finding.Summary = r.t("未能读取系统日志（") +
			strings.Join(scan.SourcesUnavailable, "；") + "）"
	case len(scan.Entries) == 0:
		finding.Status = StatusOK
		finding.Summary = r.t("近 24 小时未发现严重错误日志")
	default:
		finding.Status = StatusOK
		finding.Summary = r.tf("未发现严重错误（有 %d 条待确认条目）", len(scan.Entries))
	}

	if window := sourceWindowNote(scan.Entries, r); window != "" {
		finding.Details = append(finding.Details, window)
	}
	if len(scan.SourcesUnavailable) > 0 && finding.Status != StatusUnknown {
		finding.Details = append(finding.Details, r.t("部分日志源不可用: ")+
			strings.Join(scan.SourcesUnavailable, "；"))
	}

	sort.SliceStable(scan.Entries, func(i, j int) bool {
		left, right := scan.Entries[i], scan.Entries[j]
		if left.Severity != right.Severity {
			return left.Severity == "critical"
		}
		return false
	})
	for index, entry := range scan.Entries {
		if index >= 10 {
			finding.Details = append(finding.Details,
				r.tf("… 另有 %d 条", len(scan.Entries)-index))
			break
		}
		finding.Details = append(finding.Details,
			fmt.Sprintf("[%s/%s] %s", entry.Source, entry.Severity, truncateText(entry.Text, 160)))
	}
	finding.Evidence = truncateText(section, 1500)
	return finding
}

func evaluateServices(section string, r renderer) Finding {
	finding := Finding{ID: "services", Name: r.t("服务状态")}

	text := strings.TrimSpace(section)
	if text == "" {
		// 采集脚本已带 --no-legend 之外的完整输出, 空结果只可能是没有 systemd;
		// 有 systemd 且无失败单元时输出 "0 loaded units listed."
		finding.Status = StatusUnknown
		finding.Summary = r.t("未取得服务状态（目标机可能不是 systemd 系统）")
		return finding
	}
	if reason, unavailable := unavailableReason(text); unavailable {
		finding.Status = StatusUnknown
		finding.Summary = r.t("未能读取服务状态: ") + reason
		return finding
	}

	failed := []string{}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// systemctl --failed 无失败单元时输出 "0 loaded units listed."
		if strings.HasPrefix(trimmed, "0 loaded units") {
			continue
		}
		failed = append(failed, trimmed)
	}

	if len(failed) == 0 {
		finding.Status = StatusOK
		finding.Summary = r.t("没有失败的服务单元")
		return finding
	}

	finding.Status = StatusWarn
	finding.Summary = r.tf("%d 个服务单元处于失败状态", len(failed))
	for _, line := range failed {
		finding.Details = append(finding.Details, truncateText(line, 160))
	}
	finding.Evidence = truncateText(text, 800)
	return finding
}

// dedupeLogs 去掉重复条目。
//
// journalctl 与传统日志文件常常记录同一件事 (journald 也可能把 syslog 转发出去),
// 不去重会把条数翻倍, 让"错误数"这个指标失去意义。
func dedupeLogs(scan LogScan) LogScan {
	seen := map[string]bool{}
	out := LogScan{SourcesUnavailable: scan.SourcesUnavailable, Entries: []LogEntry{}}
	for _, entry := range scan.Entries {
		key := logKey(entry.Text)
		if seen[key] {
			continue
		}
		seen[key] = true
		out.Entries = append(out.Entries, entry)
	}
	return out
}

// unitPIDPattern 匹配 "nginx[123]" 里的进程号。
//
// 同一件事被 journald 与 syslog 各记一次时, 单元后面的 pid 常常不同
// (nginx 与 nginx[123]), 不归一就会被算成两条, 让"错误条数"这个指标失真。
var unitPIDPattern = regexp.MustCompile(`\[\d+\]`)

// logKey 去掉日志行的时间戳与进程号, 让同一件事在不同来源下得到同一个键。
func logKey(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 4 && isMonth(fields[0]) && isClockField(fields[2]) {
		return unitPIDPattern.ReplaceAllString(strings.Join(fields[3:], " "), "")
	}
	// ISO 风格: 2026-09-25T10:00:00+0800 host unit: msg
	if len(fields) >= 3 && strings.Contains(fields[0], "T") && strings.Contains(fields[0], ":") {
		return strings.Join(fields[2:], " ")
	}
	return line
}

func isMonth(value string) bool {
	switch value {
	case "Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec":
		return true
	}
	return false
}

func isClockField(value string) bool {
	return len(value) == 8 && value[2] == ':' && value[5] == ':'
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// humanKB 把 kB 渲染成人读的大小。
func humanKB(kb int64) string {
	if kb < 0 {
		kb = 0
	}
	const (
		gb = 1024 * 1024
		mb = 1024
	)
	switch {
	case kb >= gb:
		return fmt.Sprintf("%.1f GB", float64(kb)/gb)
	case kb >= mb:
		return fmt.Sprintf("%.1f MB", float64(kb)/mb)
	default:
		return fmt.Sprintf("%d KB", kb)
	}
}

func parseFloat(value string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(value), 64)
}

func parseInt(value string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(value))
}

// parseKB 解析 /proc/meminfo 的数值 (有些内核会带 "kB" 后缀)。
func parseKB(value string) (int64, bool) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimSuffix(trimmed, "kB")
	trimmed = strings.TrimSpace(trimmed)
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// parseRatio 解析 "1/234"。
func parseRatio(value string) (int, int, bool) {
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	left, err1 := parseInt(parts[0])
	right, err2 := parseInt(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return left, right, true
}

// firstCelsius 从 "+45.0°C  (high = +80.0°C)" 里取第一个温度。
func firstCelsius(text string) (float64, bool) {
	for _, field := range strings.Fields(text) {
		cleaned := strings.Trim(field, "+°C ")
		cleaned = strings.TrimSuffix(cleaned, "°C")
		if value, err := parseFloat(cleaned); err == nil {
			return value, true
		}
	}
	return 0, false
}

// diagnosisHint 把识别出的问题类别放进结论行, 让人一眼看到"是什么问题"而不是只有条数。
func diagnosisHint(r renderer, items []Diagnosis) string {
	if len(items) == 0 {
		return ""
	}
	titles := make([]string, 0, 2)
	for _, item := range items {
		if item.Origin == OriginUnknown {
			continue
		}
		titles = append(titles, r.t(item.Title))
		if len(titles) == 2 {
			break
		}
	}
	if len(titles) == 0 {
		return r.t("（均为未识别条目, 需人工确认）")
	}
	hint := r.t("（") + strings.Join(titles, "、")
	if len(items) > len(titles) {
		hint += r.tf(" 等 %d 类", len(items))
	}
	return hint + "）"
}

// logWindowBySource 说明每个日志来源覆盖的时间范围。
//
// 这不是细节: dmesg 是"本次启动以来"的环形缓冲, 里面的事件可能发生在几周前, 而
// journalctl 那段是近 24 小时 —— 报告里若不写清, "有多少条错误"这句话就没法解释。
var logWindowBySource = map[string]string{
	"journalctl": "journalctl -p err 近 24 小时",
	"dmesg":      "dmesg 内核缓冲 (本次启动以来)",
	"syslog":     "/var/log 尾部",
}

// sourceWindowNote 生成一行来源与时间窗说明 (只有一种来源时也写, 免得读者猜)。
func sourceWindowNote(entries []LogEntry, r renderer) string {
	if len(entries) == 0 {
		return ""
	}
	seen := []string{}
	for _, entry := range entries {
		if !containsString(seen, entry.Source) {
			seen = append(seen, entry.Source)
		}
	}
	sort.Strings(seen)
	parts := make([]string, 0, len(seen))
	for _, source := range seen {
		if window, ok := logWindowBySource[source]; ok {
			parts = append(parts, r.t(window))
			continue
		}
		parts = append(parts, source)
	}
	return r.t("日志来源: ") + strings.Join(parts, "; ")
}
