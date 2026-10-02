package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// 本文件把计划渲染成"给人看的总结"。
//
// 为什么不放在前端拼: 字段的中文标签本来就登记在槽位表里 (ChoicesMap 是
// "中文标签 -> 代码值"), 前端再写一份必然漂移。放在后端还有个好处 —— 可以测试,
// 而一个 1000 行的内联脚本是没有测试的。
//
// 总结不只是展示: 可编辑的行会带上槽位路径与推荐值, 于是界面上的"修改这一项"
// 可以直接复用追问那套机制 (提交一个该槽位的答案即可)。

// summarize 产出总结。plan 为 nil 或类型不认识时返回 nil。
func summarize(kind string, plan any, cat catalog.Catalog) *schema.Summary {
	if plan == nil {
		return nil
	}
	planMap, err := schema.PlanToMap(plan)
	if err != nil {
		return nil
	}

	var rows []schema.SummaryRow
	if kind == schema.KindRestore {
		rows = restoreSummaryRows(planMap, cat)
	} else {
		rows = backupSummaryRows(planMap, cat)
	}

	// 给可编辑的行补上该槽位的候选值与推荐值: 界面上的"修改这一项"因此
	// 不必再回一次接口去问有哪些取值。
	for index := range rows {
		if rows[index].Slot == "" {
			continue
		}
		meta, ok := LookupSlotMeta(planMap, rows[index].Slot, cat)
		if !ok {
			continue
		}
		rows[index].Choices = outsideChoices(nil, meta.Choices)
		rows[index].Suggestions = outsideChoices(meta.Choices, meta.Suggest)
	}

	warnings := []string{}
	if raw, ok := GetPath(planMap, "warnings"); ok {
		if list, ok := raw.([]any); ok {
			for _, item := range list {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					warnings = append(warnings, text)
				}
			}
		}
	}

	return &schema.Summary{
		Title:      stringAt(planMap, "task_name"),
		Rows:       rows,
		Warnings:   warnings,
		Confidence: confidenceOf(planMap),
	}
}

func backupSummaryRows(planMap map[string]any, cat catalog.Catalog) []schema.SummaryRow {
	rows := []schema.SummaryRow{}

	// ---- 备份对象 ----
	if kind := stringAt(planMap, "source.items[0].kind"); kind != "" {
		display := kind
		if engine, ok := driver.Find(kind); ok {
			display = engine.Display
		}
		value := display
		if name := stringAt(planMap, "source.items[0].name"); name != "" && name != display {
			value = name + "（" + display + "）"
		}
		// 刻意不给这一行槽位: 换引擎会连带整套参数 (认证方式/范围/并发…) 都要重来,
		// 单点改写一个 kind 只会留下自相矛盾的参数组合。要换对象请在输入框里重新描述。
		rows = append(rows, schema.SummaryRow{Label: "备份对象", Value: value})
	}
	if host := stringAt(planMap, "source.items[0].host"); host != "" {
		rows = append(rows, schema.SummaryRow{Label: "主机", Value: host})
	}
	if databases := listAt(planMap, "source.items[0].databases"); len(databases) > 0 {
		rows = append(rows, schema.SummaryRow{
			Label: "数据库", Value: databaseListText(databases),
		})
	}

	// ---- 引擎专属参数 ----
	rows = append(rows, engineOptionRows(planMap, cat)...)

	// ---- 调度 ----
	if schedule := scheduleText(planMap, "schedule.full"); schedule != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "全量备份", Value: schedule, Slot: "schedule.full",
		})
	}
	if schedule := scheduleText(planMap, "schedule.incremental"); schedule != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "增量备份", Value: schedule, Slot: "schedule.incremental",
		})
	}
	if schedule := scheduleText(planMap, "schedule.differential"); schedule != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "差异备份", Value: schedule,
		})
	}

	// ---- 保留 ----
	if text := retentionText(planMap); text != "" {
		rows = append(rows, schema.SummaryRow{Label: "保留策略", Value: text, Slot: "retention"})
	}

	// ---- 目的地 ----
	if text := destinationText(planMap); text != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "备份目的地", Value: text, Slot: "destinations",
		})
	}

	// ---- 其他 ----
	if rpo := stringAt(planMap, "sla.rpo"); rpo != "" {
		rows = append(rows, schema.SummaryRow{Label: "RPO（可丢失）", Value: rpo})
	}
	if rto := stringAt(planMap, "sla.rto"); rto != "" {
		rows = append(rows, schema.SummaryRow{Label: "RTO（恢复耗时）", Value: rto})
	}
	if value, ok := GetPath(planMap, "verification.test_restore"); ok && value == true {
		text := "开启"
		if frequency := stringAt(planMap, "verification.frequency"); frequency != "" {
			text = frequencyLabel(frequency) + "一次"
		}
		rows = append(rows, schema.SummaryRow{Label: "恢复演练", Value: text})
	}
	if channels := listAt(planMap, "notification.channels"); len(channels) > 0 {
		labels := make([]string, 0, len(channels))
		for _, channel := range channels {
			labels = append(labels, channelLabel(channel))
		}
		rows = append(rows, schema.SummaryRow{
			Label: "告警通知", Value: strings.Join(labels, "、"),
		})
	}
	return rows
}

// engineOptionRows 把引擎专属参数渲染成可读行。
//
// 每一项都带槽位路径, 因此界面上都能"修改这一项"。
func engineOptionRows(planMap map[string]any, cat catalog.Catalog) []schema.SummaryRow {
	rows := []schema.SummaryRow{}

	raw, ok := GetPath(planMap, "source.items[0].options")
	if !ok {
		return rows
	}
	options, ok := raw.(map[string]any)
	if !ok || len(options) == 0 {
		return rows
	}

	kind := itemKindOf(planMap, 0)
	engine := driver.Get(kind)

	// 按驱动声明的槽位顺序输出, 保证每次顺序一致 (map 遍历是随机的)
	for _, slot := range engine.Slots {
		value, exists := options[slot.Field]
		if !exists || value == nil {
			continue
		}
		// 清单接口为应用声明了参数时, 取值标签也以清单为准 ——
		// 否则会出现"问题标签来自接口、值标签来自内置"的不一致。
		choicesMap := slot.ChoicesMap
		if meta, ok := catalogSlotMeta(cat, kind, slot.Field, ""); ok && len(meta.ChoicesMap) > 0 {
			choicesMap = meta.ChoicesMap
		}

		text := humanizeOption(choicesMap, value)
		if text == "" {
			continue
		}
		label := strings.TrimSpace(slot.Question)
		label = strings.TrimSuffix(label, "?")
		label = strings.TrimSuffix(label, "？")
		rows = append(rows, schema.SummaryRow{
			Label: shortLabel(label, slot, engine),
			Value: text,
			Slot:  fmt.Sprintf("source.items[0].options.%s", slot.Field),
		})
	}
	return rows
}

// shortLabel 用槽位表里登记的中文标签作为更短的字段名。
//
// 直接拿问句当标签会得到"Oracle 用哪种认证方式连接"这种整句, 总结里太啰嗦。
func shortLabel(question string, slot driver.Slot, engine *driver.Driver) string {
	switch slot.Field {
	case "scope":
		return "备份范围"
	case "auth_mode":
		return "认证方式"
	case "channels":
		return "并发度"
	case "sid":
		return "实例 SID"
	case "target":
		return "介质通道"
	case "target_dir":
		return "备份目录"
	case "sbt_library":
		return "介质库"
	case "sbt_compress":
		return "压缩方式"
	case "archivelog":
		return "归档日志"
	case "controlfile_autobackup":
		return "控制文件自动备份"
	case "method":
		return "备份方式"
	case "databases":
		return "数据库"
	case "paths":
		return "备份路径"
	case "exclude":
		return "排除路径"
	case "archive_format":
		return "归档格式"
	case "use_snapshot":
		return "存储快照"
	case "single_transaction":
		return "一致性快照"
	case "flush_logs":
		return "切割 binlog"
	case "format":
		return "输出格式"
	case "slot_name":
		return "复制槽"
	}
	_ = engine
	return question
}

// humanizeOption 把参数值翻成中文 (枚举走登记的"中文标签 -> 代码值"表)。
func humanizeOption(choicesMap map[string]string, value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "是"
		}
		return "否"
	case string:
		if label, ok := labelForCode(choicesMap, typed); ok {
			return label
		}
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				if text == "*" {
					parts = append(parts, "全部")
					continue
				}
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "、")
	default:
		return fmt.Sprint(value)
	}
}

// labelForCode 反查"代码值 -> 中文标签"。取第一个匹配, 并去掉标签里括号补充。
func labelForCode(choicesMap map[string]string, code string) (string, bool) {
	for label, value := range choicesMap {
		if value == code {
			return trimLabel(label), true
		}
	}
	return "", false
}

// trimLabel 去掉标签里较长的括号补充, 让总结更紧凑。
//
// "整个实例(全库+归档+控制文件+参数文件)" -> "整个实例";
// 保留 "rman target /" 这类真正必要的信息 (长度较短)。
func trimLabel(label string) string {
	for _, opening := range []string{"(", "（"} {
		index := strings.Index(label, opening)
		if index <= 0 {
			continue
		}
		head := strings.TrimSpace(label[:index])
		rest := label[index:]
		// 括号内很短时保留 (如 "SBT 介质通道 (libobk.so)")
		if len([]rune(head)) >= 4 && len([]rune(rest)) > 12 {
			return head
		}
	}
	return label
}

// scheduleText 把一条调度渲染成"每周 周日 01:00"。
func scheduleText(planMap map[string]any, path string) string {
	value, ok := GetPath(planMap, path)
	if !ok || value == nil {
		return ""
	}
	parts := []string{}

	if frequency := stringAt(planMap, path+".frequency"); frequency != "" {
		parts = append(parts, frequencyLabel(frequency))
	}
	switch frequency := stringAt(planMap, path+".frequency"); frequency {
	case "weekly":
		if weekday := intAt(planMap, path+".weekday"); weekday > 0 {
			parts = append(parts, weekdayLabel(weekday))
		}
	case "monthly":
		if day := intAt(planMap, path+".day_of_month"); day > 0 {
			parts = append(parts, fmt.Sprintf("%d 号", day))
		}
	}
	if clock := stringAt(planMap, path+".time"); clock != "" {
		parts = append(parts, clock)
	}
	return strings.Join(parts, " ")
}

func retentionText(planMap map[string]any) string {
	parts := []string{}
	if text := retentionSpecText(planMap, "retention.full"); text != "" {
		parts = append(parts, "全量 "+text)
	}
	if text := retentionSpecText(planMap, "retention.incremental"); text != "" {
		parts = append(parts, "增量 "+text)
	}
	if text := retentionSpecText(planMap, "retention.differential"); text != "" {
		parts = append(parts, "差异 "+text)
	}
	// 只配了目的地级保留时也要体现出来
	if len(parts) == 0 {
		if text := destinationRetentionText(planMap); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "，")
}

func retentionSpecText(planMap map[string]any, path string) string {
	value, ok := GetPath(planMap, path)
	if !ok || value == nil {
		return ""
	}
	amount := intAt(planMap, path+".value")
	unit := stringAt(planMap, path+".unit")
	if amount <= 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", amount, unitLabel(unit))
}

func destinationRetentionText(planMap map[string]any) string {
	raw, ok := GetPath(planMap, "destinations")
	if !ok {
		return ""
	}
	list, _ := raw.([]any)
	for index := range list {
		text := retentionSpecText(planMap, fmt.Sprintf("destinations[%d].retention", index))
		if text != "" {
			return "按目的地设置 " + text
		}
	}
	return ""
}

func destinationText(planMap map[string]any) string {
	raw, ok := GetPath(planMap, "destinations")
	if !ok {
		return ""
	}
	list, _ := raw.([]any)
	parts := make([]string, 0, len(list))
	for index := range list {
		prefix := fmt.Sprintf("destinations[%d]", index)
		destType := stringAt(planMap, prefix+".type")
		// 介质服务器名优先 (那才是用户实际选的东西), 否则按类型给内置中文标签
		label := stringAt(planMap, prefix+".media_server")
		if label == "" {
			label = catalog.DestTypeLabel(destType)
		}
		location := stringAt(planMap, prefix+".path")
		if location == "" {
			if bucket := stringAt(planMap, prefix+".bucket"); bucket != "" {
				location = "桶 " + bucket
			}
		}
		if location != "" {
			label += " " + location
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, "；")
}

func restoreSummaryRows(planMap map[string]any, cat catalog.Catalog) []schema.SummaryRow {
	rows := []schema.SummaryRow{}

	if kind := stringAt(planMap, "source.items[0].kind"); kind != "" {
		display := kind
		if engine, ok := driver.Find(kind); ok {
			display = engine.Display
		}
		// 同"备份对象": 换对象类型不是单点修改, 见 backupSummaryRows 里的说明
		rows = append(rows, schema.SummaryRow{Label: "恢复对象", Value: display})
	}
	if ref := stringAt(planMap, "source_backup_ref"); ref != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "备份来源", Value: ref, Slot: "source_backup_ref",
		})
	}
	rows = append(rows, engineOptionRows(planMap, cat)...)

	// ---- 时间点 ----
	pointType := stringAt(planMap, "point.type")
	pointValue := stringAt(planMap, "point.value")
	pointText := ""
	if meta, ok := genericSlots["point.type"]; ok {
		if label, found := labelForCode(meta.ChoicesMap, pointType); found {
			pointText = label
		}
	}
	if pointText == "" {
		pointText = pointType
	}
	if pointValue != "" {
		pointText += " " + pointValue
	}
	if strings.TrimSpace(pointText) != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "恢复时间点", Value: pointText, Slot: "point.type",
		})
	}

	// ---- 目标 ----
	targetText := ""
	if meta, ok := genericSlots["target.mode"]; ok {
		if label, found := labelForCode(meta.ChoicesMap, stringAt(planMap, "target.mode")); found {
			targetText = label
		}
	}
	if host := stringAt(planMap, "target.host"); host != "" {
		targetText += " " + host
	}
	if instance := stringAt(planMap, "target.instance"); instance != "" {
		targetText += " 实例 " + instance
	}
	if strings.TrimSpace(targetText) != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "恢复目标", Value: strings.TrimSpace(targetText), Slot: "target.mode",
		})
	}

	// ---- 范围 ----
	scopeText := ""
	if meta, ok := genericSlots["scope.level"]; ok {
		if label, found := labelForCode(meta.ChoicesMap, stringAt(planMap, "scope.level")); found {
			scopeText = label
		}
	}
	if items := listAt(planMap, "scope.items"); len(items) > 0 {
		scopeText += " " + strings.Join(items, "、")
	}
	if strings.TrimSpace(scopeText) != "" {
		rows = append(rows, schema.SummaryRow{
			Label: "恢复范围", Value: strings.TrimSpace(scopeText), Slot: "scope.level",
		})
	}

	// ---- 换路径 / 仅校验 ----
	if value, ok := GetPath(planMap, "alternate.validate_only"); ok && value == true {
		rows = append(rows, schema.SummaryRow{Label: "执行方式", Value: "仅校验，不写回数据"})
	} else {
		parts := []string{}
		if dataDir := stringAt(planMap, "alternate.data_dir"); dataDir != "" {
			parts = append(parts, "数据目录 "+dataDir)
		}
		if name := stringAt(planMap, "alternate.instance_name"); name != "" {
			parts = append(parts, "实例名 "+name)
		}
		if len(parts) > 0 {
			rows = append(rows, schema.SummaryRow{Label: "换路径/改名", Value: strings.Join(parts, "，")})
		}
	}

	// ---- 恢复前后动作 ----
	actions := []string{}
	if value, ok := GetPath(planMap, "verification.validate_backup"); ok && value == true {
		actions = append(actions, "恢复前校验备份可读")
	}
	if value, ok := GetPath(planMap, "verification.open_database"); ok && value == true {
		actions = append(actions, "恢复后打开数据库")
	}
	if value, ok := GetPath(planMap, "verification.check_consistency"); ok && value == true {
		actions = append(actions, "做一致性检查")
	}
	if len(actions) > 0 {
		rows = append(rows, schema.SummaryRow{Label: "恢复后动作", Value: strings.Join(actions, "、")})
	}

	return rows
}

// ---------------------------------------------------------------------------
// 取值与标签
// ---------------------------------------------------------------------------

func stringAt(data map[string]any, path string) string {
	value, ok := GetPath(data, path)
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

func intAt(data map[string]any, path string) int {
	value, ok := GetPath(data, path)
	if !ok {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case int64:
		return int(typed)
	case interface{ Int64() (int64, error) }:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return 0
}

func listAt(data map[string]any, path string) []string {
	value, ok := GetPath(data, path)
	if !ok {
		return nil
	}
	list, _ := value.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func frequencyLabel(frequency string) string {
	switch frequency {
	case "hourly":
		return "每小时"
	case "daily":
		return "每天"
	case "weekly":
		return "每周"
	case "monthly":
		return "每月"
	}
	return frequency
}

func weekdayLabel(weekday int) string {
	names := []string{"", "周一", "周二", "周三", "周四", "周五", "周六", "周日"}
	if weekday >= 1 && weekday <= 7 {
		return names[weekday]
	}
	return fmt.Sprintf("周%d", weekday)
}

func unitLabel(unit string) string {
	switch unit {
	case "hours":
		return "小时"
	case "days":
		return "天"
	case "weeks":
		return "周"
	case "months":
		return "个月"
	case "years":
		return "年"
	}
	return unit
}

func channelLabel(channel string) string {
	switch channel {
	case "email":
		return "邮件"
	case "sms":
		return "短信"
	case "webhook":
		return "Webhook"
	case "wechat":
		return "微信"
	}
	return channel
}

// warningsFromPlan 从存下的计划 JSON 里取出警告 (定稿时回显用)。
func warningsFromPlan(planJSON string) []string {
	out := []string{}
	var raw map[string]any
	if err := jsonUnmarshal(planJSON, &raw); err != nil {
		return out
	}
	if list, ok := raw["warnings"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// confidenceFromPlanJSON 从存下的计划 JSON 里取出置信度。
func confidenceFromPlanJSON(planJSON string) float64 {
	var raw map[string]any
	if err := jsonUnmarshal(planJSON, &raw); err != nil {
		return 0
	}
	return confidenceOf(raw)
}

// jsonUnmarshal 是 json.Unmarshal 的薄封装, 便于本文件其它函数复用。
func jsonUnmarshal(text string, target any) error {
	return json.Unmarshal([]byte(text), target)
}

// databaseListText 把库名列表渲染成中文。
//
// 通配符 "*" 是契约里的写法, 直接摆给用户看等于让人猜 —— 翻成"全部数据库"。
func databaseListText(databases []string) string {
	// 只要出现通配符, 语义就是"全部" —— 它涵盖其余项, 没必要再列出来
	for _, name := range databases {
		if name == "*" {
			return "全部数据库"
		}
	}
	return strings.Join(databases, "、")
}
