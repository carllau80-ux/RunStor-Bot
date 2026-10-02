// Package inspect 做服务器自动化巡检: 连上目标机, 采集 CPU/内存/磁盘/温度与严重
// 错误日志, 给出带阈值判定的结构化报告。
//
// 与项目其它部分的区别: 这是**真的连服务器执行命令**, 而不是产出计划。因此安全上
// 有三条硬约束:
//  1. 密码/私钥只在内存里用于本次连接 —— 不记录日志、不落盘、不回显。
//  2. 远端脚本是**固定文本**, 用户输入(主机/用户/密码)只作为 ssh 的参数与环境变量,
//     绝不拼进脚本 —— 从根上杜绝命令注入。
//  3. 主机指纹采用 TOFU: 已知主机按 known_hosts 严格校验; 未知主机接受但把指纹写进
//     报告并提示, 让"信任了什么"是可见的。
package inspect

import (
	"fmt"

	"backup-restore-agent/internal/i18n"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status 是单项检查与整份报告的结论。
type Status string

const (
	StatusOK       Status = "ok"
	StatusWarn     Status = "warn"
	StatusCritical Status = "critical"
	StatusUnknown  Status = "unknown"
)

// severity 用于汇总: 数值越大越严重。
func (s Status) severity() int {
	switch s {
	case StatusCritical:
		return 3
	case StatusWarn:
		return 2
	case StatusOK:
		return 1
	default:
		return 0
	}
}

// Worse 返回两者中更严重的那个。
func Worse(a, b Status) Status {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// Target 是一台待巡检的服务器。
//
// 三个凭据字段只在内存中存在: 报告与历史记录都只保存主机与用户。
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
	User string `json:"user"`

	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

// Address 返回用于连接与展示的 user@host:port。
func (t Target) Address() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	user := t.User
	if user == "" {
		user = "root"
	}
	return fmt.Sprintf("%s@%s:%d", user, t.Host, port)
}

// AuthMode 说明本次用了哪种认证方式 (只用于展示, 不含任何凭据)。
func (t Target) AuthMode() string {
	if strings.TrimSpace(t.PrivateKey) != "" {
		return "公钥"
	}
	if strings.TrimSpace(t.Password) != "" {
		return "密码"
	}
	return "无凭据"
}

// Finding 是一项检查的结果。
type Finding struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Status  Status   `json:"status"`
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
	// Evidence 是原始输出, 截断保留 —— 报结论要能追溯到证据。
	Evidence string `json:"evidence,omitempty"`
	// Error 非空表示这项检查没能正常完成 (与"检查通过"是两件事)。
	Error string `json:"error,omitempty"`
	// Diagnoses 是影响分析与处置建议 (目前用于严重错误日志)。
	//
	// 放在检查项里而不是单独一节: 建议是针对**这一项**的结论给出的, 读者顺着读下来
	// 就能看到"发现了什么 → 意味着什么 → 该做什么"。
	Diagnoses []Diagnosis `json:"diagnoses,omitempty"`
}

// Report 是一次巡检的完整结果。
type Report struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	User     string `json:"user"`
	Host     string `json:"host"`
	AuthMode string `json:"auth_mode"`

	HostName  string `json:"host_name,omitempty"`
	OS        string `json:"os,omitempty"`
	Kernel    string `json:"kernel,omitempty"`
	Uptime    string `json:"uptime,omitempty"`
	HostKeyFP string `json:"host_key_fingerprint,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`

	Findings []Finding `json:"findings"`
	Status   Status    `json:"status"`
	// Summary 是结论行, 随报告一起返回 —— 界面直接显示它, 不必自己拼。
	Summary string `json:"summary"`
	// Analysis 说明这份报告的影响分析是怎么来的 (规则库 / 规则库+模型)。
	//
	// 规则结论与模型结论的可信度不同, 报告里要写清楚, 读者才知道自己在看哪一种。
	Analysis string `json:"analysis,omitempty"`
	// Error 非空表示整次巡检失败 (连不上/认证失败), 此时 Findings 为空。
	Error string `json:"error,omitempty"`
}

// Counts 统计各状态的项数 (给界面显示概览)。
func (r Report) Counts() map[string]int {
	out := map[string]int{"ok": 0, "warn": 0, "critical": 0, "unknown": 0}
	for _, finding := range r.Findings {
		out[string(finding.Status)]++
	}
	return out
}

// ProblemFindings 返回需要关注的项 (warn / critical)。
func (r Report) ProblemFindings() []Finding {
	out := []Finding{}
	for _, finding := range r.Findings {
		if finding.Status == StatusWarn || finding.Status == StatusCritical {
			out = append(out, finding)
		}
	}
	return out
}

// SummaryLine 渲染成一行结论 (给侧栏/列表用, 中文)。
func (r Report) SummaryLine() string { return r.SummaryLineLang(i18n.ZH) }

// SummaryLineLang 按语言渲染一行结论。
//
// 结论行里带计数, 事后逐词替换翻不出正确语序, 所以生成时就要定语言。
func (r Report) SummaryLineLang(lang i18n.Lang) string {
	tr := renderer{lang: lang}
	if r.Error != "" {
		return tr.t("巡检失败: ") + r.Error
	}
	counts := r.Counts()
	if counts["critical"] > 0 {
		return tr.tf("发现 %d 项严重问题", counts["critical"])
	}
	if counts["warn"] > 0 {
		return tr.tf("发现 %d 项告警", counts["warn"])
	}
	if counts["unknown"] > 0 {
		return tr.tf("正常 (%d 项无法判定)", counts["unknown"])
	}
	return tr.t("全部正常")
}

// ---------------------------------------------------------------------------
// 采集脚本
// ---------------------------------------------------------------------------

// 标记: 用于把一次 ssh 输出切成若干块。
//
// 把全部检查放进**一次** ssh 调用: 只握手一次 (并行也好、串行也好, 总延迟约等于最慢
// 的那一项), 而且每项都套 timeout, 一项卡住不影响其它项。
const (
	markPrefix = "__BRA_"
	markSuffix = "__"
)

// sectionIDs 是采集脚本里的分段标识, 顺序即输出顺序。
var sectionIDs = []string{"meta", "cpu", "memory", "disk", "thermal", "logs", "services"}

// remoteScript 返回固定不变的采集脚本。
//
// 这里绝不能出现任何用户输入 —— 主机、用户、密码都只作为 ssh 的参数传递。
// 每项都套 `timeout` 并 `|| true`: 单项失败不该让整次采集崩掉, 缺哪项就在哪项上
// 标记为"不可用", 而不是整份报告失败。
func remoteScript(commandTimeout int) string {
	if commandTimeout <= 0 {
		commandTimeout = 15
	}
	limit := fmt.Sprintf("timeout %d", commandTimeout)

	var builder strings.Builder
	builder.WriteString("set +e\n")

	// ---- 元信息 ----
	builder.WriteString("echo " + markPrefix + "meta" + markSuffix + "\n")
	// 带键输出而不是按行序取值: os-release 可能有 PRETTY_NAME 与 VERSION 两行,
	// 顺序一变后面全部错位 (曾把主机名显示成内核版本)。
	builder.WriteString(limit + " sh -c '" +
		"grep -E \"^PRETTY_NAME=\" /etc/os-release 2>/dev/null | sed \"s/^PRETTY_NAME=/BRA_OS=/\"; " +
		"echo \"BRA_KERNEL=$(uname -r 2>/dev/null)\"; " +
		"echo \"BRA_HOSTNAME=$(uname -n 2>/dev/null)\"; " +
		"echo \"BRA_UPTIME=$(uptime -p 2>/dev/null || uptime 2>/dev/null)\"; " +
		"echo \"BRA_SYSTEMD=$(command -v systemctl >/dev/null 2>&1 && echo yes || echo no)\"' 2>&1 || true\n")

	// ---- CPU: 负载与核数 ----
	builder.WriteString("echo " + markPrefix + "cpu" + markSuffix + "\n")
	builder.WriteString(limit + " sh -c 'cat /proc/loadavg 2>/dev/null; echo ---; " +
		"nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null' 2>&1 || true\n")

	// ---- 内存 ----
	builder.WriteString("echo " + markPrefix + "memory" + markSuffix + "\n")
	builder.WriteString(limit + " cat /proc/meminfo 2>&1 || true\n")

	// ---- 磁盘 ----
	builder.WriteString("echo " + markPrefix + "disk" + markSuffix + "\n")
	builder.WriteString(limit + " sh -c 'df -Pk 2>/dev/null; echo ---BLOCKDEV---; " +
		"lsblk -P -o NAME,SIZE,TYPE,MOUNTPOINT 2>/dev/null' 2>&1 || true\n")

	// ---- 温度: 先 sysfs 热区, 再 hwmon (都不需要额外软件包) ----
	builder.WriteString("echo " + markPrefix + "thermal" + markSuffix + "\n")
	builder.WriteString(limit + " sh -c '" +
		"for zone in /sys/class/thermal/thermal_zone*; do " +
		"[ -r \"$zone/temp\" ] && echo \"$(cat $zone/type 2>/dev/null || basename $zone)=$(cat $zone/temp 2>/dev/null)\"; " +
		"done; " +
		"for hw in /sys/class/hwmon/hwmon*; do " +
		"for file in $hw/temp*_input; do " +
		"[ -r \"$file\" ] && echo \"$(cat ${file%_input}_label 2>/dev/null || basename $file)=$(cat $file 2>/dev/null)\"; " +
		"done; done; " +
		"command -v sensors >/dev/null 2>&1 && sensors 2>/dev/null | grep -E \"^[A-Za-z].*:.*\\+\" ' 2>&1 || true\n")

	// ---- 严重错误日志: journalctl / dmesg / 传统日志文件, 三路都试 ----
	builder.WriteString("echo " + markPrefix + "logs" + markSuffix + "\n")
	builder.WriteString(limit + " sh -c 'command -v journalctl >/dev/null 2>&1 && " +
		"journalctl -p err --since \"-24h\" -n 200 --no-pager -o short 2>&1 | head -200; " +
		"echo ---DMESG---; " +
		"dmesg --level=err,crit,alert,emerg 2>&1 | tail -200; " +
		"echo ---SYSLOG---; " +
		"for f in /var/log/messages /var/log/syslog; do " +
		"[ -r \"$f\" ] && tail -800 \"$f\" 2>/dev/null | grep -iE \"error|crit|fail|panic|oom|segfault\" | tail -100 && break; done' 2>&1 || true\n")

	// ---- 失败的服务单元 ----
	builder.WriteString("echo " + markPrefix + "services" + markSuffix + "\n")
	builder.WriteString(limit + " systemctl --failed --no-pager 2>&1 || true\n")

	builder.WriteString("echo " + markPrefix + "end" + markSuffix + "\n")
	return builder.String()
}

// splitSections 按标记把输出切成 id -> 内容。
//
// 用标记切分而不是依赖固定行号: 目标机的输出格式 (df 的列宽、journalctl 的字段)
// 各家略有差异, 但标记是我们自己打的。
func splitSections(output string) map[string]string {
	sections := map[string]string{}
	current := ""
	var buffer []string

	flush := func() {
		if current != "" {
			sections[current] = strings.TrimRight(strings.Join(buffer, "\n"), "\n")
		}
		buffer = buffer[:0]
	}

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, markPrefix) && strings.HasSuffix(trimmed, markSuffix) {
			name := strings.TrimSuffix(strings.TrimPrefix(trimmed, markPrefix), markSuffix)
			flush()
			current = name
			continue
		}
		if current != "" {
			buffer = append(buffer, line)
		}
	}
	flush()
	return sections
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// LoadAverage 是 /proc/loadavg 的解析结果。
type LoadAverage struct {
	Load1   float64
	Load5   float64
	Load15  float64
	Running int
	Total   int
}

// parseLoadAverage 解析 "0.52 0.58 0.59 1/234 5678"。
func parseLoadAverage(text string) (LoadAverage, bool) {
	fields := strings.Fields(strings.TrimSpace(firstLine(text)))
	if len(fields) < 3 {
		return LoadAverage{}, false
	}
	var result LoadAverage
	var err error
	if result.Load1, err = parseFloat(fields[0]); err != nil {
		return LoadAverage{}, false
	}
	if result.Load5, err = parseFloat(fields[1]); err != nil {
		return LoadAverage{}, false
	}
	if result.Load15, err = parseFloat(fields[2]); err != nil {
		return LoadAverage{}, false
	}
	if len(fields) >= 4 {
		if running, total, ok := parseRatio(fields[3]); ok {
			result.Running, result.Total = running, total
		}
	}
	return result, true
}

// MemoryInfo 是 /proc/meminfo 里我们关心的几项 (单位 kB)。
type MemoryInfo struct {
	TotalKB     int64
	AvailableKB int64
	FreeKB      int64
	SwapTotalKB int64
	SwapFreeKB  int64
}

// UsedPercent 返回已用百分比。
func (m MemoryInfo) UsedPercent() float64 {
	if m.TotalKB <= 0 {
		return 0
	}
	used := m.TotalKB - m.AvailableKB
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(m.TotalKB) * 100
}

// SwapUsedPercent 返回交换分区使用率; 未启用交换时返回 -1。
func (m MemoryInfo) SwapUsedPercent() float64 {
	if m.SwapTotalKB <= 0 {
		return -1
	}
	used := m.SwapTotalKB - m.SwapFreeKB
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(m.SwapTotalKB) * 100
}

// parseMeminfo 解析 /proc/meminfo。
func parseMeminfo(text string) (MemoryInfo, bool) {
	var info MemoryInfo
	found := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, ok := parseKB(fields[1])
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			info.TotalKB = value
			found = true
		case "MemAvailable":
			info.AvailableKB = value
		case "MemFree":
			info.FreeKB = value
		case "SwapTotal":
			info.SwapTotalKB = value
		case "SwapFree":
			info.SwapFreeKB = value
		}
	}
	if !found || info.TotalKB <= 0 {
		return MemoryInfo{}, false
	}
	// 老内核没有 MemAvailable 时退回 MemFree 估算
	if info.AvailableKB == 0 {
		info.AvailableKB = info.FreeKB
	}
	return info, true
}

// Mount 是一个挂载点的使用情况。
type Mount struct {
	Device     string
	SizeKB     int64
	UsedKB     int64
	AvailKB    int64
	UsePercent int
	MountPoint string
}

// parseDf 解析 `df -Pk` 输出。
//
// 只保留真实文件系统: tmpfs/devtmpfs/overlay 这些不是"磁盘用满了"该报警的对象,
// 混进来只会让巡检报告充满噪音。
func parseDf(text string) []Mount {
	mounts := []Mount{}
	for index, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if index == 0 && strings.EqualFold(fields[0], "Filesystem") {
			continue
		}
		percentText := strings.TrimSuffix(fields[4], "%")
		percent, err := parseInt(percentText)
		if err != nil {
			continue
		}
		size, _ := parseInt64(fields[1])
		used, _ := parseInt64(fields[2])
		avail, _ := parseInt64(fields[3])

		mount := Mount{
			Device:     fields[0],
			SizeKB:     size,
			UsedKB:     used,
			AvailKB:    avail,
			UsePercent: percent,
			MountPoint: strings.Join(fields[5:], " "),
		}
		if !isRealFilesystem(mount) {
			continue
		}
		mounts = append(mounts, mount)
	}
	sort.SliceStable(mounts, func(i, j int) bool {
		return mounts[i].UsePercent > mounts[j].UsePercent
	})
	return mounts
}

// isRealFilesystem 判断该挂载点是否值得纳入容量告警。
func isRealFilesystem(mount Mount) bool {
	device := mount.Device
	for _, pseudo := range []string{"tmpfs", "devtmpfs", "overlay", "shm", "squashfs", "ramfs"} {
		if strings.HasPrefix(device, pseudo) {
			return false
		}
	}
	for _, pseudo := range []string{"/dev", "/sys", "/proc", "/run"} {
		if mount.MountPoint == pseudo || strings.HasPrefix(mount.MountPoint, pseudo+"/") {
			return false
		}
	}
	return true
}

// Temperature 是一个温度读数。
type Temperature struct {
	Sensor  string
	Celsius float64
}

// parseThermal 解析 sysfs 的 "名称=毫摄氏度" 与 sensors 的 "+45.0°C"。
func parseThermal(text string) []Temperature {
	out := []Temperature{}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if index := strings.Index(trimmed, "="); index > 0 {
			name := strings.TrimSpace(trimmed[:index])
			if value, err := parseFloat(strings.TrimSpace(trimmed[index+1:])); err == nil {
				// sysfs 给的是毫摄氏度
				out = append(out, Temperature{Sensor: name, Celsius: value / 1000})
				continue
			}
		}
		// sensors 风格: "Package id 0:  +45.0°C  (high = +80.0°C)"
		if index := strings.LastIndex(trimmed, ":"); index > 0 {
			name := strings.TrimSpace(trimmed[:index])
			rest := trimmed[index+1:]
			if value, ok := firstCelsius(rest); ok {
				out = append(out, Temperature{Sensor: name, Celsius: value})
			}
		}
	}
	return out
}

// LogEntry 是一条严重日志。
type LogEntry struct {
	Source   string
	Severity string
	Text     string
}

// LogScan 是日志检查的结果。
type LogScan struct {
	Entries []LogEntry
	// SourcesUnavailable 记录哪几路日志读不到 (例如容器里没有 dmesg 权限)。
	SourcesUnavailable []string
}

// CountSeverity 统计某一级别的条数。
func (s LogScan) CountSeverity(severities ...string) int {
	count := 0
	for _, entry := range s.Entries {
		for _, severity := range severities {
			if entry.Severity == severity {
				count++
				break
			}
		}
	}
	return count
}

// parseLogs 解析日志段。
//
// 关键判断: **"日志读不到"与"没有错误日志"是两件事**。容器里 dmesg 常被内核限制
// (dmesg_restrict), 容器里没有 journalctl —— 这些都不能当作"系统健康"。
func parseLogs(text string) LogScan {
	scan := LogScan{Entries: []LogEntry{}}
	source := "journalctl"

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch trimmed {
		case "---DMESG---":
			source = "dmesg"
			continue
		case "---SYSLOG---":
			source = "syslog"
			continue
		}

		if isLogPlaceholder(trimmed) {
			continue
		}
		if reason, unavailable := unavailableReason(trimmed); unavailable {
			scan.SourcesUnavailable = append(scan.SourcesUnavailable, source+": "+reason)
			continue
		}

		severity := classifySeverity(trimmed)
		// dmesg 那一行来自 --level=err,crit,alert,emerg 的过滤, 因此它的每一条
		// 本来就是错误级; 关键字没命中不等于没事 (如 "db_root: cannot open: ...")
		if severity == "other" && source == "dmesg" {
			severity = "err"
		}
		scan.Entries = append(scan.Entries, LogEntry{
			Source: source, Severity: severity, Text: trimmed,
		})
	}
	return scan
}

// isLogPlaceholder 识别 journalctl 自己打的占位行。
//
// 这类行不是日志内容: 把它们算进条数会让"错误数"这个指标虚高, 也让报告里出现
// 毫无意义的条目 (曾经出现过 "-- No entries --" 被当成一条错误)。
func isLogPlaceholder(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
		return true // -- No entries -- / -- Logs begin at ... -- / -- Reboot --
	}
	if strings.HasPrefix(trimmed, "0 loaded units") {
		return true
	}
	return false
}

// unavailableReason 识别"这一路日志读不到"的提示行。
func unavailableReason(line string) (string, bool) {
	lowered := strings.ToLower(line)
	for _, marker := range []string{
		"command not found",
		"operation not permitted",
		"permission denied",
		"read kernel buffer failed",
		"no such file or directory",
		"systemd is not running",
		"failed to get boot id",
		"not supported",
		"is not a tty",
	} {
		if strings.Contains(lowered, marker) {
			return line, true
		}
	}
	return "", false
}

// classifySeverity 按关键字判定严重程度。
func classifySeverity(line string) string {
	lowered := strings.ToLower(line)
	for _, marker := range []string{
		"emerg", "alert", "panic", "critical", "kernel oops", "out of memory",
		"oom-kill", "segfault", "hardware error", "i/o error", "medium error",
	} {
		if strings.Contains(lowered, marker) {
			return "critical"
		}
	}
	for _, marker := range []string{"crit", "failed", "failure", "error", "fatal"} {
		if strings.Contains(lowered, marker) {
			return "err"
		}
	}
	return "other"
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func firstLine(text string) string {
	if index := strings.Index(text, "\n"); index != -1 {
		return text[:index]
	}
	return text
}

func truncateText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// parseInt64 解析可能超出 int 范围的容量数值 (大磁盘的 kB 数会超过 32 位)。
func parseInt64(value string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
}
