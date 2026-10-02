// Package nlp 提供中文自然语言的确定性解析: 时刻、时长、数量、枚举、路径。
//
// 为什么单独成包: 两条路径都要用它 ——
//  1. demo 客户端从需求原文里抽取字段
//  2. 槽位机制解析用户对追问的回答 ("凌晨1点" -> 01:00)
//
// 共用一份实现, 避免两边行为漂移 (同一个"下午3点"在两处解析出不同结果是最难查的 bug)。
package nlp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 时段前缀。
var periods = []string{"凌晨", "清晨", "早上", "早晨", "上午", "中午", "下午", "傍晚", "晚上", "夜里"}

var clockPattern = regexp.MustCompile(
	`(?P<period>` + strings.Join(periods, "|") + `)?\s*` +
		`(?P<hour>\d{1,2})\s*[点时:：]\s*` +
		`(?:` +
		`(?P<minute>\d{1,2})\s*分?` +
		`|(?P<half>半)` +
		`)?`)

var durationPattern = regexp.MustCompile(`(?P<value>\d+)\s*(?P<unit>个月|小时|星期|天|日|周|时|月|年)`)

// weekdayPattern 匹配"周X/星期X/礼拜X"。
//
// 刻意不包含裸的"每"分支: 那样 "每天" 会被匹配成 每+天 (天=周日), 于是
// "每天增量备份" 被理解成"每周日增量备份" —— 这是高频词上的严重误判。
// "每周三"/"每星期天" 这类说法本来就含"周三"/"星期天", 不需要"每"参与匹配。
var weekdayPattern = regexp.MustCompile(`(?:周|星期|礼拜)([一二三四五六日天])`)

var dayOfMonthPattern = regexp.MustCompile(`每(?:个)?月\s*(\d{1,2})\s*[号日]`)

var datePattern = regexp.MustCompile(
	`(?P<year>\d{4})\s*[-/年]\s*(?P<month>\d{1,2})\s*[-/月]\s*(?P<day>\d{1,2})\s*日?` +
		`(?:\s*(?P<hour>\d{1,2})\s*[点时:：]\s*(?P<minute>\d{1,2})?\s*分?)?`)

var pathPattern = regexp.MustCompile(`(?:/[A-Za-z0-9._-]+){2,}`)

var ipPattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

var listSplitPattern = regexp.MustCompile(`[、,，;；/\s]+`)

var intPattern = regexp.MustCompile(`-?\d+`)

var durationUnits = map[string]string{
	"小时": "hours",
	"时":  "hours",
	"天":  "days",
	"日":  "days",
	"周":  "weeks",
	"星期": "weeks",
	"个月": "months",
	"月":  "months",
	"年":  "years",
}

var weekdayMap = map[string]int{
	"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "日": 7, "天": 7,
}

// Duration 是一段保留期。
type Duration struct {
	Value int
	Unit  string
}

// ParseClock 从自然语言里取时刻, 返回 "HH:MM"。
//
// 覆盖: 凌晨1点 / 下午3点半 / 23:30 / 晚上8点15分 / 中午12点
func ParseClock(text string) (string, bool) {
	match := clockPattern.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}

	hour := atoi(clockGroups["hour"], match)
	minute := atoi(clockGroups["minute"], match)
	if groupValue(match, clockGroups["half"]) != "" {
		minute = 30
	}
	return formatClock(adjustHour(hour, match), minute)
}

// adjustHour 把"下午3点"这类时段换算成 24 小时制。
func adjustHour(hour int, match []string) int {
	switch groupValue(match, clockGroups["period"]) {
	case "下午", "傍晚", "晚上", "夜里":
		if hour < 12 {
			return hour + 12
		}
	case "中午":
		// 中午12点 -> 12; 中午1点 -> 13。只对 1~2 点进位, "中午11点" 保持原样。
		if hour <= 2 {
			return hour + 12
		}
	case "凌晨", "清晨":
		if hour == 12 {
			return 0
		}
	}
	return hour
}

func formatClock(hour, minute int) (string, bool) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), true
}

// ParseInt 取文本里第一个整数。
func ParseInt(text string) (int, bool) {
	digits := intPattern.FindString(text)
	if digits == "" {
		return 0, false
	}
	value, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return value, true
}

// ParseDuration 解析"保留14天"这类时长。
func ParseDuration(text string) (Duration, bool) {
	match := durationPattern.FindStringSubmatch(text)
	if match == nil {
		return Duration{}, false
	}
	value := atoi(durationGroups["value"], match)
	unit, ok := durationUnits[groupValue(match, durationGroups["unit"])]
	if !ok || value < 1 {
		return Duration{}, false
	}
	return Duration{Value: value, Unit: unit}, true
}

var (
	trueWords  = []string{"是", "要", "需要", "开启", "启用", "做", "开", "yes", "y", "true", "1"}
	falseWords = []string{"否", "不", "不要", "不需要", "关闭", "禁用", "不做", "关", "no", "n", "false", "0"}
)

// ParseBool 识别是否类回答。否定词先判, 否则"不要"会被"要"命中。
func ParseBool(text string) (bool, bool) {
	lowered := strings.ToLower(strings.TrimSpace(text))
	if lowered == "" {
		return false, false
	}
	for _, word := range falseWords {
		if strings.Contains(lowered, word) {
			return false, true
		}
	}
	for _, word := range trueWords {
		if strings.Contains(lowered, word) {
			return true, true
		}
	}
	return false, false
}

// ParseList 按常见分隔符切分列表。
func ParseList(text string) []string {
	parts := listSplitPattern.Split(strings.TrimSpace(text), -1)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// ParseWeekday 识别"周天/周日/星期天"里的星期几 (1=周一 ... 7=周日)。
func ParseWeekday(text string) (int, bool) {
	match := weekdayPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	day, ok := weekdayMap[match[1]]
	return day, ok
}

// ParseDayOfMonth 识别"每月15号"。
func ParseDayOfMonth(text string) (int, bool) {
	match := dayOfMonthPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	day, err := strconv.Atoi(match[1])
	if err != nil || day < 1 || day > 31 {
		return 0, false
	}
	return day, true
}

// ParseTimestamp 识别"2026-09-25 10:30"这类日期时间, 返回 "YYYY-MM-DD HH:MM:SS"。
func ParseTimestamp(text string) (string, bool) {
	match := datePattern.FindStringSubmatch(text)
	if match == nil {
		return "", false
	}
	year, err1 := strconv.Atoi(match[dateGroups["year"]])
	month, err2 := strconv.Atoi(match[dateGroups["month"]])
	day, err3 := strconv.Atoi(match[dateGroups["day"]])
	if err1 != nil || err2 != nil || err3 != nil {
		return "", false
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return "", false
	}

	hour, minute := 0, 0
	if raw := groupValue(match, dateGroups["hour"]); raw != "" {
		hour, _ = strconv.Atoi(raw)
	}
	if raw := groupValue(match, dateGroups["minute"]); raw != "" {
		minute, _ = strconv.Atoi(raw)
	}
	if hour > 23 || minute > 59 {
		return "", false
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:00", year, month, day, hour, minute), true
}

// Paths 抽出文本里的绝对路径 (至少两级, 避免误抓斜杠)。
func Paths(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, match := range pathPattern.FindAllString(text, -1) {
		cleaned := strings.TrimRight(match, "。，,.;；、)")
		if cleaned == "" || seen[cleaned] {
			continue
		}
		seen[cleaned] = true
		out = append(out, cleaned)
	}
	return out
}

// FirstIP 抽出一个 IPv4 地址。
func FirstIP(text string) (string, bool) {
	match := ipPattern.FindString(text)
	return match, match != ""
}

// RunePrefix 按"字符"而不是"字节"截取前缀。
//
// Go 的 s[:n] 是按字节切的, 对中文会把字符切一半, 产生非法 UTF-8 ——
// 表现为"关键词后面的数字明明存在却抽不出来"。凡是要截取中文片段的地方都必须用它。
func RunePrefix(text string, n int) string {
	runes := []rune(text)
	if n >= len(runes) {
		return text
	}
	if n <= 0 {
		return ""
	}
	return string(runes[:n])
}

// DestinationRule 把中文说法映射到目的地类型。
type DestinationRule struct {
	Token string
	Kind  string
}

// DestinationRules 是目的地识别的词表, 顺序即优先级: 专有名词在前, 泛词在后。
//
// 放在 nlp 包里共享: demo 客户端从需求原文抽取目的地、槽位机制解析用户对
// "备份存储到哪里" 的回答, 两处必须用同一套词表, 否则同一个说法在两处会得到
// 不同结果 —— 这是最难排查的一类不一致。
var DestinationRules = []DestinationRule{
	{"aliyun", "aliyun_oss"},
	{"阿里云", "aliyun_oss"},
	{"oss", "aliyun_oss"},
	{"tencent", "tencent_cos"},
	{"腾讯云", "tencent_cos"},
	{"cos", "tencent_cos"},
	{"azure", "azure_blob"},
	{"blob", "azure_blob"},
	{"s3", "s3"},
	{"对象存储", "s3"},
	{"nas", "nfs"},
	{"nfs", "nfs"},
	{"磁带", "tape"},
	{"带库", "tape"},
	{"tape", "tape"},
	{"本地盘", "local"},
	{"本地", "local"},
	{"本机", "local"},
	{"磁盘", "local"},
}

// NetworkDestinations 是判定"是否算异地副本"用的类型集合。
var NetworkDestinations = map[string]bool{
	"nfs": true, "s3": true, "aliyun_oss": true,
	"tencent_cos": true, "azure_blob": true,
}

// DetectDestination 从文本里认出目的地类型。
func DetectDestination(text string) (string, bool) {
	lowered := strings.ToLower(text)
	for _, rule := range DestinationRules {
		if strings.Contains(lowered, rule.Token) {
			return rule.Kind, true
		}
	}
	return "", false
}

// ContainsAny 判断文本是否含任一词。
func ContainsAny(text string, words []string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 正则命名组辅助
//
// Go 的 FindStringSubmatch 只返回 []string, 按名字取值要先把下标算出来。
// 每个 pattern 必须有独立的索引表 —— 同名的组 (如 hour) 在不同 pattern 里
// 下标不同, 合并成一张表会让日期解析取到时钟的下标。
// ---------------------------------------------------------------------------

var (
	clockGroups    = groupIndex(clockPattern)
	durationGroups = groupIndex(durationPattern)
	dateGroups     = groupIndex(datePattern)
)

func groupIndex(pattern *regexp.Regexp) map[string]int {
	index := map[string]int{}
	for position, name := range pattern.SubexpNames() {
		if name != "" {
			index[name] = position
		}
	}
	return index
}

func groupValue(match []string, position int) string {
	if position <= 0 || position >= len(match) {
		return ""
	}
	return match[position]
}

func atoi(position int, match []string) int {
	value, err := strconv.Atoi(groupValue(match, position))
	if err != nil {
		return 0
	}
	return value
}
