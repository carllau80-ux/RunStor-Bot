package schema

import (
	"fmt"
	"strings"
)

// Level 表示问题的严重程度。
type Level int

const (
	// LevelError 表示内容不可用或自相矛盾, 优先交给模型自纠错。
	LevelError Level = iota
	// LevelWarning 表示建议性提示, 不阻塞流程。
	LevelWarning
)

func (l Level) String() string {
	if l == LevelWarning {
		return "warning"
	}
	return "error"
}

// Issue 是一条校验发现。
//
// Slot 是"降级通道": 若某个缺失字段是用户能补充的 (如备份时刻), 重试若干次仍
// 修不好时就转为向用户追问, 而不是把流程判死。Slot 为空表示只能由模型修正。
type Issue struct {
	Path    string
	Slot    string
	Message string
	Level   Level
}

// Report 汇总一次校验的结果。
type Report struct {
	Issues []Issue
}

// Errors 返回所有错误级问题。
func (r Report) Errors() []Issue {
	return r.filter(LevelError)
}

// Warnings 返回所有警告级问题。
func (r Report) Warnings() []Issue {
	return r.filter(LevelWarning)
}

func (r Report) filter(level Level) []Issue {
	out := make([]Issue, 0, len(r.Issues))
	for _, issue := range r.Issues {
		if issue.Level == level {
			out = append(out, issue)
		}
	}
	return out
}

// AddError 追加一条错误。
func (r *Report) AddError(path, slot, format string, args ...any) {
	r.Issues = append(r.Issues, Issue{
		Path: path, Slot: slot, Message: fmt.Sprintf(format, args...), Level: LevelError,
	})
}

// AddWarning 追加一条警告。
func (r *Report) AddWarning(path, format string, args ...any) {
	r.Issues = append(r.Issues, Issue{
		Path: path, Message: fmt.Sprintf(format, args...), Level: LevelWarning,
	})
}

// Messages 把指定级别的问题渲染成给模型看的多行文本。
func (r Report) Messages(level Level) []string {
	issues := r.filter(level)
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Path != "" {
			out = append(out, fmt.Sprintf("- %s: %s", issue.Path, issue.Message))
			continue
		}
		out = append(out, "- "+issue.Message)
	}
	return out
}

// Validate 校验计划的结构、枚举与取值范围。
//
// 这里只做"与引擎无关"的校验; 引擎专属参数由 driver.ValidateOptions 负责,
// 跨字段的业务规则由 agent 层补充。分层是为了让本包不依赖任何其他包。
func Validate(plan any) Report {
	switch typed := plan.(type) {
	case *BackupPlan:
		return validateBackup(typed)
	case *RestorePlan:
		return validateRestore(typed)
	default:
		var report Report
		report.AddError("", "", "未知的计划类型 %T", plan)
		return report
	}
}

func validateBackup(plan *BackupPlan) Report {
	var report Report

	if strings.TrimSpace(plan.Kind) != "" && plan.Kind != KindBackup {
		report.AddError("kind", "", "kind 必须是 %q, 实际是 %q", KindBackup, plan.Kind)
	}
	if plan.SchemaVersion != "" && plan.SchemaVersion != SchemaVersion {
		report.AddWarning("schema_version", "schema_version 期望 %q, 实际是 %q",
			SchemaVersion, plan.SchemaVersion)
	}

	validateSource(&report, plan.Source)
	validateSchedule(&report, "schedule", plan.Schedule)
	validateRetention(&report, "retention", plan.Retention)
	validateDestinations(&report, plan.Destinations)
	validateConfidence(&report, plan.Confidence)
	validateVerification(&report, plan.Verification)
	validateNotification(&report, plan.Notification)

	return report
}

func validateRestore(plan *RestorePlan) Report {
	var report Report

	if strings.TrimSpace(plan.Kind) != "" && plan.Kind != KindRestore {
		report.AddError("kind", "", "kind 必须是 %q, 实际是 %q", KindRestore, plan.Kind)
	}

	validateSource(&report, plan.Source)
	validateConfidence(&report, plan.Confidence)
	validateNotification(&report, plan.Notification)

	// ---- 恢复时间点 ----
	if !contains(PointTypes, plan.Point.Type) {
		report.AddError("point.type", "point.type",
			"取值必须在 %s 中, 实际是 %q", strings.Join(PointTypes, "/"), plan.Point.Type)
	}
	switch plan.Point.Type {
	case "latest":
		// 无需取值
	case "timestamp":
		if strings.TrimSpace(plan.Point.Value) == "" {
			report.AddError("point.value", "point.value",
				"type=timestamp 时必须给出时间点, 格式 YYYY-MM-DD HH:MM:SS")
		} else if !isTimestamp(plan.Point.Value) {
			report.AddError("point.value", "point.value",
				"时间格式不对: %q, 期望 YYYY-MM-DD HH:MM:SS", plan.Point.Value)
		}
	default:
		if strings.TrimSpace(plan.Point.Value) == "" {
			report.AddError("point.value", "point.value",
				"type=%s 时必须给出取值", plan.Point.Type)
		}
	}

	// ---- 恢复目标 ----
	if !contains(RestoreModes, plan.Target.Mode) {
		report.AddError("target.mode", "target.mode",
			"取值必须在 %s 中, 实际是 %q", strings.Join(RestoreModes, "/"), plan.Target.Mode)
	}
	if plan.Target.Mode == "alternate_host" {
		if strings.TrimSpace(plan.Target.Host) == "" {
			// 用户可能只是没提到目标机 —— 这属于"信息缺失", 走追问而非判错
			report.AddError("target.host", "target.host",
				"异机恢复必须指明目标主机")
		}
	}

	// ---- 恢复范围 ----
	if !contains(ScopeLevels, plan.Scope.Level) {
		report.AddError("scope.level", "scope.level",
			"取值必须在 %s 中, 实际是 %q", strings.Join(ScopeLevels, "/"), plan.Scope.Level)
	}
	if plan.Scope.Level != "whole" && len(plan.Scope.Items) == 0 {
		report.AddError("scope.items", "scope.items",
			"scope.level=%s 时必须列出具体对象名", plan.Scope.Level)
	}

	return report
}

// ---- 各片段校验 ----

func validateSource(report *Report, source BackupSource) {
	if strings.TrimSpace(source.Type) != "" && !contains(SourceTypes, source.Type) {
		report.AddError("source.type", "", "取值必须在 %s 中, 实际是 %q",
			strings.Join(SourceTypes, "/"), source.Type)
	}
	if len(source.Items) == 0 {
		report.AddError("source.items", "source.items", "未识别到任何备份源")
		return
	}
	for index, item := range source.Items {
		prefix := fmt.Sprintf("source.items[%d]", index)
		if strings.TrimSpace(item.ID) == "" {
			report.AddError(prefix+".id", "", "缺少 id")
		}
		if strings.TrimSpace(item.Name) == "" {
			report.AddError(prefix+".name", "", "缺少 name")
		}
		if strings.TrimSpace(item.Kind) == "" {
			// 类型未识别属于用户可补充的信息 (要备份什么)
			report.AddError(prefix+".kind", prefix+".kind", "缺少 kind (备份对象类型)")
		}
		if item.Port != nil && (*item.Port < 1 || *item.Port > 65535) {
			report.AddError(prefix+".port", "", "端口超出范围: %d", *item.Port)
		}
	}
}

func validateSchedule(report *Report, prefix string, schedule Schedule) {
	specs := []struct {
		name string
		spec *ScheduleSpec
	}{
		{"full", schedule.Full},
		{"incremental", schedule.Incremental},
		{"differential", schedule.Differential},
	}
	for _, entry := range specs {
		if entry.spec == nil {
			continue
		}
		path := prefix + "." + entry.name
		spec := entry.spec

		if !contains(Frequencies, spec.Frequency) {
			report.AddError(path+".frequency", path+".frequency",
				"取值必须在 %s 中, 实际是 %q", strings.Join(Frequencies, "/"), spec.Frequency)
			continue
		}
		if spec.Time != "" {
			if _, _, ok := ParseTime(spec.Time); !ok {
				report.AddError(path+".time", path+".time",
					"时刻格式不对: %q, 期望 24 小时制 HH:MM", spec.Time)
			}
		}
		switch spec.Frequency {
		case "weekly":
			if spec.Weekday == nil {
				report.AddError(path+".weekday", path+".weekday",
					"频率为每周时必须指明星期几 (1=周一 ... 7=周日)")
			} else if *spec.Weekday < 1 || *spec.Weekday > 7 {
				report.AddError(path+".weekday", path+".weekday",
					"星期几必须在 1~7 之间, 实际是 %d", *spec.Weekday)
			}
		case "monthly":
			if spec.DayOfMonth == nil {
				report.AddError(path+".day_of_month", path+".day_of_month",
					"频率为每月时必须指明几号")
			} else if *spec.DayOfMonth < 1 || *spec.DayOfMonth > 31 {
				report.AddError(path+".day_of_month", path+".day_of_month",
					"日期必须在 1~31 之间, 实际是 %d", *spec.DayOfMonth)
			}
		}
	}
}

func validateRetention(report *Report, prefix string, retention Retention) {
	specs := []struct {
		name string
		spec *RetentionSpec
	}{
		{"full", retention.Full},
		{"incremental", retention.Incremental},
		{"differential", retention.Differential},
	}
	for _, entry := range specs {
		if entry.spec == nil {
			continue
		}
		path := prefix + "." + entry.name
		if entry.spec.Value < 1 {
			report.AddError(path+".value", path+".value",
				"保留数量必须 >= 1, 实际是 %d", entry.spec.Value)
		}
		if !contains(RetentionUnits, entry.spec.Unit) {
			report.AddError(path+".unit", path+".unit",
				"单位必须在 %s 中, 实际是 %q",
				strings.Join(RetentionUnits, "/"), entry.spec.Unit)
		}
	}
	if retention.MinCopies < 0 || retention.MinCopies > 100 {
		report.AddError("retention.min_copies", "", "最少副本数必须在 0~100 之间")
	}
}

func validateDestinations(report *Report, destinations []Destination) {
	seen := map[string]int{}
	for index, dest := range destinations {
		path := fmt.Sprintf("destinations[%d]", index)
		if strings.TrimSpace(dest.ID) == "" {
			report.AddError(path+".id", "", "缺少 id")
		} else if first, dup := seen[dest.ID]; dup {
			report.AddError(path+".id", "", "id %q 与 destinations[%d] 重复", dest.ID, first)
		} else {
			seen[dest.ID] = index
		}
		if !contains(DestTypes, dest.Type) {
			report.AddError(path+".type", path+".type",
				"取值必须在 %s 中, 实际是 %q", strings.Join(DestTypes, "/"), dest.Type)
		}
		if dest.Encryption != "" && !contains(Encryptions, dest.Encryption) {
			report.AddError(path+".encryption", "",
				"取值必须在 %s 中, 实际是 %q", strings.Join(Encryptions, "/"), dest.Encryption)
		}
		if dest.Compression != "" && !contains(Compressions, dest.Compression) {
			report.AddError(path+".compression", "",
				"取值必须在 %s 中, 实际是 %q", strings.Join(Compressions, "/"), dest.Compression)
		}
		if dest.Retention != nil && dest.Retention.Value < 1 {
			report.AddError(path+".retention.value", "", "保留数量必须 >= 1")
		}
	}
}

func validateConfidence(report *Report, confidence float64) {
	if confidence < 0 || confidence > 1 {
		report.AddError("confidence", "", "置信度必须在 0~1 之间, 实际是 %v", confidence)
	}
}

func validateVerification(report *Report, verification Verification) {
	if verification.Frequency != "" && !contains(VerifyFrequencies, verification.Frequency) {
		report.AddError("verification.frequency", "",
			"取值必须在 %s 中, 实际是 %q",
			strings.Join(VerifyFrequencies, "/"), verification.Frequency)
	}
}

func validateNotification(report *Report, notification Notification) {
	for index, channel := range notification.Channels {
		if !contains(NotifyChannels, channel) {
			report.AddError(fmt.Sprintf("notification.channels[%d]", index), "",
				"取值必须在 %s 中, 实际是 %q",
				strings.Join(NotifyChannels, "/"), channel)
		}
	}
}

// ---- 小工具 ----

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// Contains 供其他包复用枚举判断。
func Contains(values []string, target string) bool { return contains(values, target) }

// isTimestamp 校验 "YYYY-MM-DD HH:MM:SS"。
func isTimestamp(value string) bool {
	if len(value) != 19 || value[4] != '-' || value[7] != '-' ||
		value[10] != ' ' || value[13] != ':' || value[16] != ':' {
		return false
	}
	for index, char := range value {
		if index == 4 || index == 7 || index == 10 || index == 13 || index == 16 {
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	month := int(value[5]-'0')*10 + int(value[6]-'0')
	day := int(value[8]-'0')*10 + int(value[9]-'0')
	hour := int(value[11]-'0')*10 + int(value[12]-'0')
	minute := int(value[14]-'0')*10 + int(value[15]-'0')
	second := int(value[17]-'0')*10 + int(value[18]-'0')
	return month >= 1 && month <= 12 && day >= 1 && day <= 31 &&
		hour <= 23 && minute <= 59 && second <= 59
}
