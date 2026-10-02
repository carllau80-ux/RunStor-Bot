package llm

import (
	"encoding/json"
	"strconv"
	"strings"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/nlp"
	"backup-restore-agent/internal/schema"
)

// demo 是内置的确定性客户端。
//
// 为什么值得认真实现 (而不是当成玩具替身): 部署环境常常没有配置模型 Key,
// 此时网页演示、冒烟测试、接口联调全部依赖它。它是 demo 模式的实际引擎。
//
// 明确的边界: 只覆盖常见中文说法, 识别能力远弱于真实模型; 未识别的内容不编造,
// 交由缺口分析转成追问。生产环境请配置 llm profile。

// 意图判定词
var restoreWords = []string{"恢复", "还原", "回滚", "restore", "recover", "rollback", "灾备切换"}

// 备份类型词
var (
	fullWords         = []string{"全备", "全量", "完整备份", "基线", "level 0"}
	incrementalWords  = []string{"增量", "incremental", "level 1"}
	differentialWords = []string{"差异", "differential"}
)

var clauseSplitters = strings.NewReplacer("，", "\n", ",", "\n", "。", "\n", ";", "\n", "；", "\n")

var localWords = []string{"本地", "本机", "磁盘", "local", "本地盘"}

var verifyWords = []string{"演练", "验证", "校验", "恢复测试", "试恢复"}

var verifyFrequencies = []struct {
	token string
	value string
}{
	{"每天", "daily"}, {"每日", "daily"},
	{"每周", "weekly"}, {"每星期", "weekly"},
	{"每月", "monthly"}, {"每个月", "monthly"},
	{"每季度", "quarterly"}, {"每季", "quarterly"},
}

var sourceNames = map[string]string{
	"oracle":     "Oracle 实例",
	"mysql":      "MySQL 实例",
	"postgresql": "PostgreSQL 实例",
	"file":       "文件服务器",
	"other":      "未明确的备份源",
}

// Demo 实现 Client。
type Demo struct{}

// NewDemo 构造 demo 客户端。
func NewDemo() *Demo { return &Demo{} }

// Name 实现 Client。
func (d *Demo) Name() string { return "demo" }

// Chat 实现 Client: 从消息里取出需求与问答, 重新解析出计划。
//
// 把追问答与原始需求拼在一起再解析, 相当于"重新读一遍完整需求" ——
// 与真实模型每轮重抽的行为一致, 因此能覆盖追问后的补全场景。
func (d *Demo) Chat(messages []Message) (string, error) {
	requirement, qa := ExtractUserInput(messages)

	combined := requirement
	if qa != "" {
		combined = requirement + " " + qa
	}
	if strings.TrimSpace(combined) == "" {
		combined = "备份"
	}

	// 优先采用提示词里声明的计划类型; 只有缺声明时才自行判定意图。
	// 调用方可以通过接口的 kind 参数强制指定类型, 这里必须尊重它。
	kind := ExtractKind(messages)
	if kind == "" {
		kind = DetectIntent(combined)
	}

	var plan any
	if kind == schema.KindRestore {
		plan = BuildRestorePlan(combined)
	} else {
		plan = BuildBackupPlan(combined)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DetectIntent 判定意图。真实模型由 intents 模块用 LLM 判定, 这里用关键词兜底。
func DetectIntent(text string) string {
	if nlp.ContainsAny(strings.ToLower(text), restoreWords) {
		return schema.KindRestore
	}
	return schema.KindBackup
}

// ---------------------------------------------------------------------------
// 调度
// ---------------------------------------------------------------------------

func clauses(text string) []string {
	parts := strings.Split(clauseSplitters.Replace(text), "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// frequencyOf 从单个子句里抽频率与时刻。
func frequencyOf(clause string) *schema.ScheduleSpec {
	weekday, hasWeekday := nlp.ParseWeekday(clause)
	dayOfMonth, hasDay := nlp.ParseDayOfMonth(clause)
	clock, hasClock := nlp.ParseClock(clause)

	spec := &schema.ScheduleSpec{Timezone: "Asia/Shanghai"}
	if hasClock {
		spec.Time = clock
	}
	if hasWeekday {
		spec.Frequency = "weekly"
		spec.Weekday = &weekday
		return spec
	}
	if hasDay {
		spec.Frequency = "monthly"
		spec.DayOfMonth = &dayOfMonth
		return spec
	}
	switch {
	case strings.Contains(clause, "每小时") || strings.Contains(clause, "每个小时") ||
		strings.Contains(clause, "hourly"):
		spec.Frequency = "hourly"
	case nlp.ContainsAny(clause, []string{"每天", "每日", "每晚", "天天", "daily"}):
		spec.Frequency = "daily"
	case strings.Contains(clause, "每月") || strings.Contains(clause, "每个月") ||
		strings.Contains(clause, "monthly"):
		spec.Frequency = "monthly"
	case hasClock:
		// 只给了时刻没给频率, 按最常见的"每天"理解
		spec.Frequency = "daily"
	default:
		return nil
	}
	return spec
}

// schedulesOf 按子句切分, 把频率关联到正确的备份类型上。
//
// "周天全备份, 每天增量备份" 这类输入里, 周天属于全备、每天属于增量, 必须分句处理,
// 否则两个描述会互相污染 (曾出现过全备被套上"每天"、增量被套上"周日"的情况)。
func schedulesOf(text string) schema.Schedule {
	var schedule schema.Schedule
	for _, clause := range clauses(text) {
		var kinds []string
		if nlp.ContainsAny(clause, fullWords) {
			kinds = append(kinds, "full")
		}
		if nlp.ContainsAny(clause, incrementalWords) {
			kinds = append(kinds, "incremental")
		}
		if nlp.ContainsAny(clause, differentialWords) {
			kinds = append(kinds, "differential")
		}
		if len(kinds) == 0 {
			continue
		}
		spec := frequencyOf(clause)
		if spec == nil {
			continue
		}
		for _, kind := range kinds {
			switch kind {
			case "full":
				if schedule.Full == nil {
					schedule.Full = spec
				}
			case "incremental":
				if schedule.Incremental == nil {
					schedule.Incremental = spec
				}
			case "differential":
				if schedule.Differential == nil {
					schedule.Differential = spec
				}
			}
		}
	}
	return schedule
}

// ---------------------------------------------------------------------------
// 保留与目的地
// ---------------------------------------------------------------------------

func retentionOf(text string) schema.Retention {
	var retention schema.Retention
	var unscoped *schema.RetentionSpec

	for _, clause := range clauses(text) {
		lowered := strings.ToLower(clause)
		if !strings.Contains(clause, "保留") && !strings.Contains(lowered, "keep") {
			continue
		}
		duration, ok := nlp.ParseDuration(clause)
		if !ok {
			continue
		}
		spec := &schema.RetentionSpec{Value: duration.Value, Unit: duration.Unit}

		switch {
		case nlp.ContainsAny(clause, incrementalWords):
			retention.Incremental = spec
		case nlp.ContainsAny(clause, differentialWords):
			retention.Differential = spec
		case nlp.ContainsAny(clause, fullWords):
			retention.Full = spec
		default:
			if unscoped == nil {
				unscoped = spec
			}
		}
	}

	if unscoped != nil {
		// 没指明类型时落到全量上 (最常见的语义)
		if retention.Full == nil {
			retention.Full = unscoped
		} else if retention.Incremental == nil {
			retention.Incremental = unscoped
		}
	}
	return retention
}

func destinationsOf(text string) []schema.Destination {
	lowered := strings.ToLower(text)
	found := []string{}
	for _, rule := range nlp.DestinationRules {
		if !strings.Contains(lowered, rule.Token) {
			continue
		}
		duplicate := false
		for _, existing := range found {
			if existing == rule.Kind {
				duplicate = true
				break
			}
		}
		if !duplicate {
			found = append(found, rule.Kind)
		}
	}

	hasNetwork := false
	for _, kind := range found {
		if nlp.NetworkDestinations[kind] {
			hasNetwork = true
		}
	}
	if !hasNetwork && nlp.ContainsAny(text, localWords) {
		// 词表可能已经命中过 local (如"本地盘"), 这里必须去重,
		// 否则同一处存储会产出两条目的地
		alreadyLocal := false
		for _, kind := range found {
			if kind == "local" {
				alreadyLocal = true
			}
		}
		if !alreadyLocal {
			found = append(found, "local")
		}
	}

	paths := nlp.Paths(text)
	destinations := make([]schema.Destination, 0, len(found))
	for index, kind := range found {
		dest := schema.Destination{
			ID:   "dest-" + strconv.Itoa(index+1),
			Type: kind,
		}
		if len(paths) > 0 && (kind == "local" || kind == "nfs" || kind == "tape") {
			dest.Path = paths[0]
		}
		if len(paths) > 0 && nlp.NetworkDestinations[kind] {
			dest.Bucket = strings.Split(strings.TrimPrefix(paths[0], "/"), "/")[0]
		}
		destinations = append(destinations, dest)
	}
	return destinations
}

func verificationOf(text string) schema.Verification {
	if !nlp.ContainsAny(text, verifyWords) {
		return schema.Verification{}
	}
	verification := schema.Verification{TestRestore: true}
	for _, entry := range verifyFrequencies {
		if strings.Contains(text, entry.token) {
			verification.Frequency = entry.value
			break
		}
	}
	return verification
}

func notificationOf(text string) schema.Notification {
	channels := []string{}
	if nlp.ContainsAny(text, []string{"邮件", "邮箱", "email", "mail"}) {
		channels = append(channels, "email")
	}
	if nlp.ContainsAny(text, []string{"短信", "sms"}) {
		channels = append(channels, "sms")
	}
	if nlp.ContainsAny(text, []string{"微信", "企业微信", "wechat"}) {
		channels = append(channels, "wechat")
	}
	if nlp.ContainsAny(text, []string{"webhook", "回调", "告警接口"}) {
		channels = append(channels, "webhook")
	}
	return schema.Notification{Channels: channels, Recipients: []string{}}
}

// ---------------------------------------------------------------------------
// 源与引擎参数
// ---------------------------------------------------------------------------

func sourceOf(text string) (schema.BackupSource, string, bool) {
	engine := driver.Resolve(text)
	kind := "other"
	if engine != nil {
		kind = engine.Kind
	}

	var databases []string
	if nlp.ContainsAny(text, []string{"整个实例", "整实例", "全部库", "所有库", "整个库", "全库"}) {
		databases = []string{"*"}
	}

	host, _ := nlp.FirstIP(text)

	name, ok := sourceNames[kind]
	if !ok && engine != nil {
		name = engine.Display
	}
	if !ok && engine == nil {
		name = sourceNames["other"]
	}

	sourceType := "mixed"
	if engine != nil {
		sourceType = engine.SourceType
	}

	return schema.BackupSource{
		Type: sourceType,
		Items: []schema.SourceItem{{
			ID:        "src-1",
			Name:      name,
			Kind:      kind,
			Host:      host,
			Databases: databases,
			Options:   map[string]any{},
		}},
	}, kind, engine != nil
}

func oracleOptions(text string) map[string]any {
	options := map[string]any{}

	switch {
	case nlp.ContainsAny(text, []string{"系统认证", "操作系统认证", "os认证", "os_auth", "免密"}):
		options["auth_mode"] = "os_auth"
	case nlp.ContainsAny(text, []string{"钱包", "wallet"}):
		options["auth_mode"] = "wallet"
	case nlp.ContainsAny(text, []string{"密码", "口令", "password"}):
		options["auth_mode"] = "password"
	}

	switch {
	case nlp.ContainsAny(text, []string{"整实例", "整个实例", "全实例", "整库", "全库"}):
		options["scope"] = "whole_instance"
	case nlp.ContainsAny(text, []string{"表空间", "tablespace"}):
		options["scope"] = "tablespace"
	case nlp.ContainsAny(text, []string{"pdb", "可插拔"}):
		options["scope"] = "pdb"
	case nlp.ContainsAny(text, []string{"数据文件", "datafile"}):
		options["scope"] = "datafile"
	case nlp.ContainsAny(text, []string{"仅归档", "只备归档"}):
		options["scope"] = "archivelog_only"
	}

	if channels, ok := channelsOf(text); ok {
		options["channels"] = channels
	}

	switch {
	case nlp.ContainsAny(text, []string{"sbt", "磁带", "带库", "libobk"}):
		options["target"] = "sbt"
	case nlp.ContainsAny(text, []string{"磁盘", "本地目录", "直接写"}):
		options["target"] = "disk"
	}

	if sid := sidOf(text); sid != "" {
		options["sid"] = sid
	}

	if paths := nlp.Paths(text); len(paths) > 0 {
		options["target_dir"] = paths[0]
	}

	return options
}

// channelsOf 抽取并发度, 兼容"并发4"/"并发设置为4"/"通道数 4"/"并行度=4"。
func channelsOf(text string) (int, bool) {
	for _, keyword := range []string{"并发", "通道", "并行", "channel", "parallelism"} {
		index := strings.Index(strings.ToLower(text), strings.ToLower(keyword))
		if index == -1 {
			continue
		}
		rest := text[index+len(keyword):]
		// 关键字后最多看 6 个字符, 中间允许"设置为/是/:/="这类连接词。
		// 必须按字符截取: 按字节切会把 "设置为4" 切成 "设置", 数字直接丢失。
		if value, ok := nlp.ParseInt(nlp.RunePrefix(rest, 6)); ok {
			return value, true
		}
	}
	return 0, false
}

// sidOf 抽取实例名。
func sidOf(text string) string {
	lowered := strings.ToLower(text)
	for _, keyword := range []string{"sid", "实例名", "服务名"} {
		index := strings.Index(lowered, keyword)
		if index == -1 {
			continue
		}
		rest := text[index+len(keyword):]
		fields := strings.FieldsFunc(nlp.RunePrefix(rest, 8), func(r rune) bool {
			return !(r == '_' || r == '$' || r == '-' ||
				(r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z'))
		})
		if len(fields) > 0 && fields[0] != "" {
			return fields[0]
		}
	}
	return ""
}

func mysqlOptions(text string) map[string]any {
	options := map[string]any{}
	switch {
	case nlp.ContainsAny(text, []string{"逻辑", "mysqldump", "dump"}):
		options["method"] = "logical"
	case nlp.ContainsAny(text, []string{"物理", "xtrabackup", "innobackupex", "克隆"}):
		options["method"] = "physical"
	}
	return options
}

func postgresOptions(text string) map[string]any {
	options := map[string]any{}
	switch {
	case nlp.ContainsAny(text, []string{"basebackup", "基础备份", "物理备份", "整集群"}):
		options["method"] = "pg_basebackup"
	case nlp.ContainsAny(text, []string{"wal", "连续归档", "日志归档"}):
		options["method"] = "wal_archive"
	case nlp.ContainsAny(text, []string{"pg_dumpall"}):
		options["method"] = "pg_dumpall"
	case nlp.ContainsAny(text, []string{"pg_dump", "逻辑备份", "dump"}):
		options["method"] = "pg_dump"
	}
	return options
}

func fileOptions(text string) map[string]any {
	options := map[string]any{}
	if nlp.ContainsAny(text, []string{"快照", "snapshot"}) {
		options["use_snapshot"] = true
	}
	return options
}

func engineOptions(kind, text string) map[string]any {
	switch kind {
	case "oracle":
		return oracleOptions(text)
	case "mysql":
		return mysqlOptions(text)
	case "postgresql":
		return postgresOptions(text)
	case "file":
		return fileOptions(text)
	default:
		return map[string]any{}
	}
}

// ---------------------------------------------------------------------------
// 组装备份计划
// ---------------------------------------------------------------------------

func confidenceOf(facts []bool) float64 {
	if len(facts) == 0 {
		return 0.1
	}
	positive := 0
	for _, fact := range facts {
		if fact {
			positive++
		}
	}
	value := float64(positive) / float64(len(facts))
	// 保留两位小数
	value = float64(int(value*100+0.5)) / 100
	if value < 0.1 {
		return 0.1
	}
	return value
}

// BuildBackupPlan 从自然语言组装备份计划 (demo 用)。
func BuildBackupPlan(text string) *schema.BackupPlan {
	source, kind, recognized := sourceOf(text)
	schedule := schedulesOf(text)
	retention := retentionOf(text)
	destinations := destinationsOf(text)
	verification := verificationOf(text)

	item := &source.Items[0]
	item.Options = engineOptions(kind, text)
	if kind == "file" {
		if paths := nlp.Paths(text); len(paths) > 0 {
			item.Paths = paths
		}
	}

	warnings := []string{}
	if !recognized {
		warnings = append(warnings, "未能识别具体引擎, 已按通用对象处理, 请确认备份对象类型")
	}
	if nlp.ContainsAny(text, []string{"异地", "离站", "容灾"}) {
		hasRemote := false
		for _, dest := range destinations {
			if nlp.NetworkDestinations[dest.Type] {
				hasRemote = true
			}
		}
		if !hasRemote {
			warnings = append(warnings, "提到异地/离站容灾, 但未指明异地存储位置, 请确认")
		}
	}

	taskName := item.Name + "备份"
	if schedule.Full != nil && schedule.Incremental != nil {
		taskName = item.Name + "周期性全备+增量备份"
	}

	return &schema.BackupPlan{
		SchemaVersion: schema.SchemaVersion,
		Kind:          schema.KindBackup,
		TaskName:      taskName,
		Source:        source,
		Schedule:      schedule,
		Retention:     retention,
		Destinations:  destinations,
		Verification:  verification,
		Notification:  notificationOf(text),
		Confidence: confidenceOf([]bool{
			recognized,
			schedule.Full != nil,
			schedule.Incremental != nil,
			retention.Full != nil || retention.Incremental != nil || retention.Differential != nil,
			len(destinations) > 0,
			len(item.Options) > 0,
		}),
		Warnings: warnings,
	}
}

// ---------------------------------------------------------------------------
// 组装恢复计划
// ---------------------------------------------------------------------------

func restorePointOf(text string) schema.RestorePoint {
	if index := indexOfKeyword(text, "scn"); index != -1 {
		rest := text[index+3:]
		if value, ok := nlp.ParseInt(nlp.RunePrefix(rest, 16)); ok {
			return schema.RestorePoint{Type: "scn", Value: strconv.Itoa(value), Timezone: "Asia/Shanghai"}
		}
	}
	if index := indexOfKeyword(text, "lsn"); index != -1 {
		rest := strings.TrimSpace(text[index+3:])
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			return schema.RestorePoint{Type: "lsn", Value: fields[0], Timezone: "Asia/Shanghai"}
		}
	}
	if value, ok := nlp.ParseTimestamp(text); ok {
		return schema.RestorePoint{Type: "timestamp", Value: value, Timezone: "Asia/Shanghai"}
	}
	return schema.RestorePoint{Type: "latest", Timezone: "Asia/Shanghai"}
}

func restoreTargetOf(text string) schema.RestoreTarget {
	alternate := nlp.ContainsAny(text, []string{
		"异机", "另一台", "别的机器", "新机器", "其他主机", "备机", "容灾机",
	})
	target := schema.RestoreTarget{Mode: "in_place"}
	if alternate {
		target.Mode = "alternate_host"
	}
	if host, ok := nlp.FirstIP(text); ok {
		target.Host = host
	}
	return target
}

func restoreScopeOf(text string) schema.RestoreScope {
	scope := schema.RestoreScope{Level: "whole", Items: []string{}}
	switch {
	case nlp.ContainsAny(text, []string{"单表", "某张表", "一张表", "某表"}):
		scope.Level = "table"
	case nlp.ContainsAny(text, []string{"表空间", "tablespace"}):
		scope.Level = "tablespace"
	case nlp.ContainsAny(text, []string{"数据文件", "datafile"}):
		scope.Level = "datafile"
	case nlp.ContainsAny(text, []string{"单个库", "某个库", "指定库"}):
		scope.Level = "database"
	case nlp.ContainsAny(text, []string{"目录", "文件夹"}) &&
		!nlp.ContainsAny(text, []string{"数据库", "实例"}):
		scope.Level = "path"
	}
	return scope
}

func alternateOf(text string) schema.AlternateLocation {
	alternate := schema.AlternateLocation{}
	if nlp.ContainsAny(text, []string{"换目录", "新路径", "不同路径", "另存", "其他目录", "改路径"}) {
		alternate.Enabled = true
		if paths := nlp.Paths(text); len(paths) > 0 {
			alternate.DataDir = paths[0]
		}
	}
	if nlp.ContainsAny(text, []string{"只校验", "仅校验", "validate", "试恢复", "验证备份可读"}) {
		alternate.Enabled = true
		alternate.ValidateOnly = true
	}
	return alternate
}

// BuildRestorePlan 从自然语言组装恢复计划 (demo 用)。
func BuildRestorePlan(text string) *schema.RestorePlan {
	source, _, recognized := sourceOf(text)
	point := restorePointOf(text)
	target := restoreTargetOf(text)
	scope := restoreScopeOf(text)
	alternate := alternateOf(text)

	verification := schema.RestoreVerification{
		ValidateBackup: nlp.ContainsAny(text, []string{"先校验", "校验备份", "validate"}),
		OpenDatabase:   !alternate.ValidateOnly,
	}

	warnings := []string{}
	if !recognized {
		warnings = append(warnings, "未能识别具体引擎, 已按通用对象处理, 请确认恢复对象类型")
	}
	if target.Mode == "alternate_host" && target.Host == "" {
		warnings = append(warnings, "提到异机恢复, 但未指明目标主机")
	}
	if point.Type == "latest" {
		warnings = append(warnings,
			"未指定恢复时间点, 默认取最近可用备份 (数据可能并未回到故障前)")
	}

	return &schema.RestorePlan{
		SchemaVersion:   schema.SchemaVersion,
		Kind:            schema.KindRestore,
		TaskName:        source.Items[0].Name + "恢复",
		Source:          source,
		SourceBackupRef: backupRefOf(text),
		Target:          target,
		Point:           point,
		Scope:           scope,
		Alternate:       alternate,
		Verification:    verification,
		Notification:    notificationOf(text),
		Confidence: confidenceOf([]bool{
			recognized,
			point.Type != "latest",
			target.Mode == "in_place" || target.Host != "",
			backupRefOf(text) != "",
			scope.Level != "whole",
		}),
		Warnings: warnings,
	}
}

// backupRefOf 尝试抽出"从 XX 备份恢复"里的备份引用。
func backupRefOf(text string) string {
	index := strings.Index(text, "从")
	if index == -1 {
		return ""
	}
	rest := text[index+len("从"):]
	end := strings.Index(rest, "恢复")
	if end == -1 {
		return ""
	}
	candidate := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[:end]), "备份"))
	if len([]rune(candidate)) < 2 || len([]rune(candidate)) > 30 {
		return ""
	}
	return candidate
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func indexOfKeyword(text, keyword string) int {
	return strings.Index(strings.ToLower(text), keyword)
}
