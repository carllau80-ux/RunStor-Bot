package agent

import (
	"fmt"

	"backup-restore-agent/internal/schema"
)

// businessWarnings 产出跨字段的业务提醒。
//
// 这些不是校验错误 (数据本身合法), 而是运维经验性的提示: 3-2-1 原则、
// 保留期倒挂、异机恢复未指定时间点等。写进计划的 warnings 一起交付,
// 让审阅的人看到风险点。
func businessWarnings(kind string, planMap map[string]any) []string {
	if kind == schema.KindRestore {
		return restoreWarnings(planMap)
	}
	return backupWarnings(planMap)
}

func backupWarnings(planMap map[string]any) []string {
	out := []string{}

	destinationCount := listLength(planMap, "destinations")
	if destinationCount == 1 {
		out = append(out, "只有一个备份目的地, 建议增加异地/离站副本 (3-2-1 原则)")
	}

	full := retentionHours(planMap, "retention.full")
	incremental := retentionHours(planMap, "retention.incremental")
	if full > 0 && incremental > full {
		out = append(out, "增量备份保留时间大于全量备份保留时间, 删除全量后增量将无法用于恢复, 请确认")
	}

	// 有增量调度但没有增量保留策略: 到期规则会含糊
	if hasAny(planMap, "schedule.incremental") && incremental == 0 {
		out = append(out, "已配置增量备份, 但未单独指定增量保留策略")
	}

	if value, ok := GetPath(planMap, "verification.test_restore"); ok && value == true {
		if !hasNonEmpty(planMap, "verification.frequency") {
			out = append(out, "已开启恢复演练但未指定频率, 建议补充 (例如: 每周)")
		}
	}

	if !hasNonEmpty(planMap, "sla.rpo") && !hasNonEmpty(planMap, "sla.rto") {
		out = append(out, "未提及 RPO/RTO, 建议明确可接受的数据丢失量与恢复时间 (例如 24h / 4h)")
	}

	// 引擎专属的组合检查
	for index := range sourceItems(planMap) {
		kind := itemKindOf(planMap, index)
		if kind != "oracle" {
			continue
		}
		target, _ := GetPath(planMap, fmt.Sprintf("source.items[%d].options.target", index))
		if target == "sbt" && !hasNonEmpty(planMap, fmt.Sprintf("source.items[%d].options.sbt_library", index)) {
			out = append(out, "Oracle 选择 SBT 介质通道, 但未指定 libobk.so 路径 (SBT_LIBRARY)")
		}
		if target == "disk" && !hasNonEmpty(planMap, fmt.Sprintf("source.items[%d].options.target_dir", index)) &&
			!hasNonEmpty(planMap, "destinations") {
			out = append(out, "Oracle 选择磁盘通道, 但既未指定备份目录也未指定目的地")
		}
	}

	return out
}

func restoreWarnings(planMap map[string]any) []string {
	out := []string{}

	pointType, _ := GetPath(planMap, "point.type")
	if pointType == "latest" {
		out = append(out, "未指定恢复时间点, 默认取最近可用备份 (数据可能并未回到故障前)")
	}
	if !hasNonEmpty(planMap, "source_backup_ref") {
		out = append(out, "未指明从哪一份备份恢复, 执行前需人工确认备份集")
	}

	if value, ok := GetPath(planMap, "verification.validate_backup"); ok && value == false {
		out = append(out, "建议恢复前先校验备份可读 (RESTORE ... VALIDATE), 否则可能恢复到一半才发现备份损坏")
	}

	if value, ok := GetPath(planMap, "alternate.validate_only"); ok && value == true {
		out = append(out, "当前为仅校验模式, 不会真正写回数据")
	}

	mode, _ := GetPath(planMap, "target.mode")
	if mode == "alternate_host" {
		out = append(out, "异机恢复需要目标机具备相同版本的数据库软件与足够空间")
	}

	return out
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func listLength(planMap map[string]any, path string) int {
	value, ok := GetPath(planMap, path)
	if !ok {
		return 0
	}
	list, ok := value.([]any)
	if !ok {
		return 0
	}
	return len(list)
}

func hasAny(planMap map[string]any, path string) bool {
	value, ok := GetPath(planMap, path)
	return ok && value != nil
}

// retentionHours 把保留期换算成小时, 便于比较; 缺失返回 0。
func retentionHours(planMap map[string]any, path string) float64 {
	value, ok := GetPath(planMap, path)
	if !ok || value == nil {
		return 0
	}
	table, ok := value.(map[string]any)
	if !ok {
		return 0
	}

	amount := 0.0
	switch typed := table["value"].(type) {
	case float64:
		amount = typed
	case int:
		amount = float64(typed)
	}
	unit, _ := table["unit"].(string)

	factors := map[string]float64{
		"hours": 1, "days": 24, "weeks": 24 * 7, "months": 24 * 30, "years": 24 * 365,
	}
	factor, ok := factors[unit]
	if !ok {
		return 0
	}
	return amount * factor
}

// sourceItems 返回源对象列表的长度 (只关心个数)。
func sourceItems(planMap map[string]any) []any {
	value, ok := GetPath(planMap, "source.items")
	if !ok {
		return nil
	}
	list, _ := value.([]any)
	return list
}
