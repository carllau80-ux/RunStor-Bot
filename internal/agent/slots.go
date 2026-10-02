package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"backup-restore-agent/internal/catalog"
	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/nlp"
	"backup-restore-agent/internal/schema"
)

// ---------------------------------------------------------------------------
// 槽位路径
//
// 槽位 id 就是计划内的 JSON 路径, 如 "source.items[0].options.auth_mode"。
// 用户答案因此能精确落回某个字段, 而不是靠匹配问题原文去猜。
// ---------------------------------------------------------------------------

type pathToken struct {
	name    string
	index   int
	isIndex bool
}

var pathTokenPattern = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)|\[(\d+)\]`)

// ParsePath 把槽位路径拆成 token 序列。
func ParsePath(path string) ([]pathToken, error) {
	tokens := []pathToken{}
	for _, match := range pathTokenPattern.FindAllStringSubmatch(path, -1) {
		if match[1] != "" {
			tokens = append(tokens, pathToken{name: match[1]})
			continue
		}
		index, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, fmt.Errorf("非法槽位路径: %q", path)
		}
		tokens = append(tokens, pathToken{index: index, isIndex: true})
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("非法槽位路径: %q", path)
	}
	return tokens, nil
}

// GetPath 按路径取值; 任一环节缺失返回 (nil, false)。路径非法也返回 false。
func GetPath(data any, path string) (any, bool) {
	tokens, err := ParsePath(path)
	if err != nil {
		return nil, false
	}
	node := data
	for _, token := range tokens {
		if token.isIndex {
			list, ok := node.([]any)
			if !ok || token.index >= len(list) {
				return nil, false
			}
			node = list[token.index]
			continue
		}
		table, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		next, exists := table[token.name]
		if !exists {
			return nil, false
		}
		node = next
	}
	return node, true
}

// SetPath 按路径写值。中间结构必须已存在 —— 槽位只会落在计划已建模的字段上。
func SetPath(data any, path string, value any) error {
	tokens, err := ParsePath(path)
	if err != nil {
		return err
	}
	node := data
	for position, token := range tokens {
		last := position == len(tokens)-1
		if token.isIndex {
			list, ok := node.([]any)
			if !ok || token.index >= len(list) {
				return fmt.Errorf("路径不存在: %s", path)
			}
			if last {
				list[token.index] = value
				return nil
			}
			node = list[token.index]
			continue
		}
		table, ok := node.(map[string]any)
		if !ok {
			return fmt.Errorf("路径不存在: %s", path)
		}
		if last {
			table[token.name] = value
			return nil
		}
		next, exists := table[token.name]
		if !exists {
			return fmt.Errorf("路径不存在: %s", path)
		}
		node = next
	}
	return fmt.Errorf("非法槽位路径: %s", path)
}

// ---------------------------------------------------------------------------
// 槽位元信息
//
// 给定一个槽位路径, 找出它的问句、取值类型与候选值。来源有两处:
//   - 通用槽位 (计划级字段): 本文件的 genericSlots 表
//   - 驱动槽位 (options / item 字段): 由该 item 的 kind 查驱动槽位表
// ---------------------------------------------------------------------------

// 槽位落点策略。
//
// 大部分槽位的路径直接指向一个标量字段 (如 schedule.full.time), 答案写进去即可。
// 但有少数槽位指向的是对象或数组, 而用户答案是自然语言 —— 例如
// "备份存储到哪里?" 的答案 "本地磁盘 /u01/backup" 应该构造出一个 Destination
// 对象, 而不是把整个 destinations 数组替换成一个字符串。
// 忽略这点会导致回填后计划校验失败, 本轮所有答案被整批丢弃 (包括已解析成功的)。
const (
	ApplyDirect      = "direct"      // 直接写入路径
	ApplyRetention   = "retention"   // {value,unit} -> retention.full
	ApplyDestination = "destination" // 自然语言描述 -> destinations[]
	ApplyFrequency   = "frequency"   // 频率描述 -> schedule.<name> 对象
)

// SlotMeta 描述一个槽位的界面与解析信息。
type SlotMeta struct {
	Slot       string
	Question   string
	Kind       string
	Example    string
	Choices    []string
	ChoicesMap map[string]string
	// Suggest 是推荐值: 用户可以点它, 也可以自己填别的。
	// 与 Choices 的分工 —— Choices 是封闭集合 (只能从中选), Suggest 是常用值。
	Suggest []string
	Hint    string
	Min     *int
	Max     *int
	// Numeric 表示该枚举槽位对应的计划字段是整数 (如 weekday 1-7)。
	// 枚举代码本身是字符串, 但写回计划时必须转成数字, 否则反序列化会失败。
	Numeric bool
	// Apply 是落点策略, 见上方常量。留空等价于 ApplyDirect。
	Apply string
}

var genericSlots = map[string]SlotMeta{
	"source.items": {
		Slot:     "source.items",
		Question: "要备份/恢复的对象是什么? (例如 Oracle 数据库、MySQL、某台文件服务器)",
		Kind:     string(driver.SlotString),
		Example:  "生产环境的 Oracle 数据库",
	},
	"schedule.full": {
		Slot:     "schedule.full",
		Question: "全量备份按什么频率执行?",
		Kind:     string(driver.SlotEnum),
		Example:  "每周日",
		Apply:    ApplyFrequency,
		ChoicesMap: map[string]string{
			"每小时": "hourly", "每天": "daily", "每周": "weekly", "每月": "monthly",
		},
	},
	"schedule.full.time": {
		Slot:     "schedule.full.time",
		Question: "全量备份在几点执行?",
		Kind:     string(driver.SlotTime),
		Example:  "凌晨1点",
		Suggest:  []string{"凌晨1点", "凌晨2点", "23:30"},
	},
	"schedule.full.weekday": {
		Slot:     "schedule.full.weekday",
		Question: "全量备份在每周几执行?",
		Kind:     string(driver.SlotEnum),
		Example:  "周日",
		Numeric:  true,
		ChoicesMap: map[string]string{
			"周一": "1", "周二": "2", "周三": "3", "周四": "4",
			"周五": "5", "周六": "6", "周日": "7",
		},
	},
	"schedule.full.day_of_month": {
		Slot:     "schedule.full.day_of_month",
		Question: "全量备份在每月几号执行?",
		Kind:     string(driver.SlotInt),
		Example:  "1",
		Suggest:  []string{"1", "15", "28"},
		Min:      driver.IntPtr(1),
		Max:      driver.IntPtr(31),
		Hint:     "填 1-31 之间的日期",
	},
	"schedule.incremental": {
		Slot:     "schedule.incremental",
		Question: "增量备份按什么频率执行?",
		Kind:     string(driver.SlotEnum),
		Example:  "每天",
		Apply:    ApplyFrequency,
		ChoicesMap: map[string]string{
			"每小时": "hourly", "每天": "daily", "每周": "weekly", "每月": "monthly",
		},
	},
	"schedule.incremental.time": {
		Slot:     "schedule.incremental.time",
		Question: "增量备份在几点执行?",
		Kind:     string(driver.SlotTime),
		Example:  "凌晨1点",
		Suggest:  []string{"凌晨1点", "凌晨2点", "23:30"},
	},
	"retention": {
		Slot:     "retention",
		Question: "备份保留多久?",
		Kind:     string(driver.SlotDuration),
		Example:  "保留14天",
		Apply:    ApplyRetention,
		Suggest: []string{
			"保留7天", "保留14天", "保留30天", "保留90天", "保留1年",
		},
	},
	"destinations": {
		Slot:     "destinations",
		Question: "备份存储到哪里?",
		Kind:     string(driver.SlotEnum),
		Example:  "本地磁盘 /u01/backup",
		Apply:    ApplyDestination,
		ChoicesMap: map[string]string{
			"本地磁盘":       "local",
			"共享目录":       "nfs",
			"S3 对象存储":    "s3",
			"阿里云 OSS":    "aliyun_oss",
			"腾讯云 COS":    "tencent_cos",
			"Azure Blob": "azure_blob",
			"磁带库":        "tape",
		},
	},
	// ---- 恢复侧 ----
	"source_backup_ref": {
		Slot:     "source_backup_ref",
		Question: "从哪一份备份恢复? (备份任务名或备份集标识)",
		Kind:     string(driver.SlotString),
		Example:  "生产Oracle实例周全备",
	},
	"target.mode": {
		Slot:     "target.mode",
		Question: "是恢复到原机原实例, 还是恢复到另一台机器?",
		Kind:     string(driver.SlotEnum),
		Example:  "原机",
		ChoicesMap: map[string]string{
			"原机原实例": "in_place",
			"异机恢复":  "alternate_host",
		},
	},
	"target.host": {
		Slot:     "target.host",
		Question: "异机恢复的目标主机是哪个?",
		Kind:     string(driver.SlotString),
		Example:  "192.0.2.11",
	},
	"target.instance": {
		Slot:     "target.instance",
		Question: "目标实例名 (SID/服务名/实例名) 是什么?",
		Kind:     string(driver.SlotString),
		Example:  "orcl",
	},
	"point.type": {
		Slot:     "point.type",
		Question: "恢复到哪个时间点?",
		Kind:     string(driver.SlotEnum),
		Example:  "故障前时刻",
		ChoicesMap: map[string]string{
			"最近可用":   "latest",
			"指定时刻":   "timestamp",
			"指定 SCN": "scn",
			"指定 LSN": "lsn",
			"指定备份集":  "backup_set_id",
		},
	},
	"point.value": {
		Slot:     "point.value",
		Question: "具体恢复到什么时刻? (格式 2026-09-25 10:30:00)",
		Kind:     string(driver.SlotString),
		Example:  "2026-09-25 10:30:00",
	},
	"scope.level": {
		Slot:     "scope.level",
		Question: "恢复范围是整库, 还是只恢复部分?",
		Kind:     string(driver.SlotEnum),
		Example:  "整库",
		ChoicesMap: map[string]string{
			"整实例/全库": "whole",
			"指定库":    "database",
			"表空间":    "tablespace",
			"单表":     "table",
			"数据文件":   "datafile",
			"目录或文件":  "path",
		},
	},
	"scope.items": {
		Slot:     "scope.items",
		Question: "具体要恢复哪些对象? (多个用逗号分隔)",
		Kind:     string(driver.SlotList),
		Example:  "users, orders",
	},
}

var (
	optionPathPattern = regexp.MustCompile(`^source\.items\[(\d+)\]\.options\.(.+)$`)
	itemPathPattern   = regexp.MustCompile(`^source\.items\[(\d+)\]\.(.+)$`)
)

// LookupSlotMeta 找出槽位路径对应的元信息。
//
// 清单 (cat) 决定两类东西: 目的是地槽位的候选项来自"环境里真实存在的介质服务器",
// 以及当清单接口给出了某应用的参数定义时, 以它为准而不是内置驱动。
func LookupSlotMeta(planMap map[string]any, path string, cat catalog.Catalog) (SlotMeta, bool) {
	if len(cat.Applications) == 0 {
		cat = catalog.Builtin()
	}
	// 目的地: 候选项来自清单里的介质服务器
	if path == "destinations" {
		meta := genericSlots["destinations"]
		meta.Choices = cat.ChoiceLabels()
		meta.ChoicesMap = cat.ChoiceMap()
		return withChoiceLabels(meta), true
	}
	// 驱动 options 槽位
	if match := optionPathPattern.FindStringSubmatch(path); match != nil {
		index, err := strconv.Atoi(match[1])
		if err != nil {
			return SlotMeta{}, false
		}
		kind := itemKindOf(planMap, index)
		// 清单接口若为该应用定义了参数, 就以它为准 —— 那是环境里真正支持的那些参数
		if meta, ok := catalogSlotMeta(cat, kind, match[2], path); ok {
			return withChoiceLabels(meta), true
		}
		engine := driver.Get(kind)
		slot, ok := engine.FindSlot(match[2])
		if !ok {
			return SlotMeta{}, false
		}
		return withChoiceLabels(slotMetaFrom(engine, slot, path)), true
	}

	// SourceItem 通用字段 (databases / paths) 的驱动槽位
	if match := itemPathPattern.FindStringSubmatch(path); match != nil {
		index, err := strconv.Atoi(match[1])
		if err != nil {
			return SlotMeta{}, false
		}
		kind := itemKindOf(planMap, index)
		if meta, ok := catalogSlotMeta(cat, kind, match[2], path); ok {
			return withChoiceLabels(meta), true
		}
		engine := driver.Get(kind)
		slot, ok := engine.FindSlot(match[2])
		if !ok {
			return SlotMeta{}, false
		}
		return withChoiceLabels(slotMetaFrom(engine, slot, path)), true
	}

	if meta, ok := genericSlots[path]; ok {
		return withChoiceLabels(meta), true
	}
	return SlotMeta{}, false
}

// withChoiceLabels 保证 Choices 一定有值: 只登记了 ChoicesMap 时从它的键补上。
//
// 这是个曾经的漏洞: 通用槽位 (destinations / schedule.full / target.mode 等) 只写了
// ChoicesMap, 而界面读的是 Choices —— 于是"备份存储到哪里?"从来没显示过任何可点的
// 候选值, 用户只能手打。在查找出口统一补齐, 比让每个消费者各自兼容可靠。
func withChoiceLabels(meta SlotMeta) SlotMeta {
	if len(meta.Choices) == 0 && len(meta.ChoicesMap) > 0 {
		labels := make([]string, 0, len(meta.ChoicesMap))
		for label := range meta.ChoicesMap {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		meta.Choices = labels
	}
	return meta
}

// slotMetaFrom 把驱动槽位翻译成槽位元信息。
func slotMetaFrom(engine *driver.Driver, slot driver.Slot, path string) SlotMeta {
	question := slot.Question
	if question == "" {
		question = "请提供 " + slot.Field
	}
	return SlotMeta{
		Slot:       path,
		Question:   question,
		Kind:       string(slot.Kind),
		Example:    slot.Example,
		Choices:    slot.ChoiceLabels(),
		ChoicesMap: slot.ChoicesMap,
		Suggest:    slot.Suggest,
		Hint:       slot.Hint,
		Min:        slot.Min,
		Max:        slot.Max,
	}
}

// itemKindOf 取第 index 个源对象的 kind。
func itemKindOf(planMap map[string]any, index int) string {
	value, ok := GetPath(planMap, fmt.Sprintf("source.items[%d].kind", index))
	if !ok {
		return "other"
	}
	kind, _ := value.(string)
	if kind == "" {
		return "other"
	}
	return kind
}

// ---------------------------------------------------------------------------
// 答案解析
// ---------------------------------------------------------------------------

// ParseAnswer 把用户对某个槽位的回答解析成可写入计划的值。
//
// 返回 (值, 错误)。解析失败不视为流程失败 —— 调用方应把原始问答继续交给模型,
// 由模型自己去理解 ("大概两周吧" 这种模型能懂, 规则不能)。
func ParseAnswer(meta SlotMeta, text string) (any, error) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return nil, fmt.Errorf("回答为空")
	}

	switch meta.Kind {
	case string(driver.SlotTime):
		clock, ok := nlp.ParseClock(raw)
		if !ok {
			return nil, fmt.Errorf("无法识别时刻 %q (示例: 凌晨1点 / 01:00)", raw)
		}
		return clock, nil

	case string(driver.SlotInt):
		value, ok := nlp.ParseInt(raw)
		if !ok {
			return nil, fmt.Errorf("无法识别数字 %q", raw)
		}
		if meta.Min != nil && value < *meta.Min {
			return nil, fmt.Errorf("不能小于 %d", *meta.Min)
		}
		if meta.Max != nil && value > *meta.Max {
			return nil, fmt.Errorf("不能大于 %d", *meta.Max)
		}
		return value, nil

	case string(driver.SlotDuration):
		duration, ok := nlp.ParseDuration(raw)
		if !ok {
			return nil, fmt.Errorf("无法识别时长 %q (示例: 14天 / 2周)", raw)
		}
		return map[string]any{"value": duration.Value, "unit": duration.Unit}, nil

	case string(driver.SlotBool):
		value, ok := nlp.ParseBool(raw)
		if !ok {
			return nil, fmt.Errorf("无法识别是否 %q", raw)
		}
		return value, nil

	case string(driver.SlotEnum):
		code := ""
		if matched, ok := driver.MatchChoice(meta.ChoicesMap, raw); ok {
			code = matched
		} else if weekday, ok := nlp.ParseWeekday(raw); ok && meta.Slot == "schedule.full.weekday" {
			// "星期天" 这类说法匹配不上"周日"标签, 走星期解析兜底
			code = strconv.Itoa(weekday)
		} else {
			return nil, fmt.Errorf("无法匹配候选值 %q", raw)
		}
		if meta.Numeric {
			number, err := strconv.Atoi(code)
			if err != nil {
				return nil, fmt.Errorf("候选值 %q 不是数字", code)
			}
			if meta.Min != nil && number < *meta.Min {
				return nil, fmt.Errorf("不能小于 %d", *meta.Min)
			}
			if meta.Max != nil && number > *meta.Max {
				return nil, fmt.Errorf("不能大于 %d", *meta.Max)
			}
			return number, nil
		}
		return code, nil

	case string(driver.SlotList):
		items := normalizeList(raw)
		if len(items) == 0 {
			return nil, fmt.Errorf("无法识别列表 %q", raw)
		}
		return items, nil

	default:
		return raw, nil
	}
}

// 表示"全部"的词, 统一转成通配符。
var allWords = []string{"全部", "所有", "整个", "整库", "全库", "都", "all"}

func normalizeList(raw string) []string {
	lowered := strings.ToLower(raw)
	for _, word := range allWords {
		if strings.Contains(lowered, word) {
			return []string{"*"}
		}
	}
	return nlp.ParseList(raw)
}

// ApplyAnswers 把已答槽位确定性地写回计划。
//
// 返回 (新计划, 已应用的槽位, 无法解析的说明)。
// 无法解析的项不改动计划, 但原始问答仍会进入下一轮提示词 —— 规则理解不了的说法
// ("大概两周吧") 交给模型去理解, 这才是模型该干的活。
func ApplyAnswers(kind string, planMap map[string]any, answers map[string]string,
	cat catalog.Catalog, metaLookup func(string) (SlotMeta, bool)) (any, []string, []string) {
	if len(cat.Applications) == 0 {
		cat = catalog.Builtin()
	}

	applied := []string{}
	problems := []string{}

	for _, path := range sortedKeys(answers) {
		raw := strings.TrimSpace(answers[path])
		if raw == "" {
			continue
		}
		meta, ok := metaLookup(path)
		if !ok {
			continue
		}

		var err error
		switch meta.Apply {
		case ApplyRetention:
			err = applyRetention(planMap, meta, raw)
		case ApplyDestination:
			err = applyDestination(planMap, raw, cat)
		case ApplyFrequency:
			err = applyFrequency(planMap, path, raw)
		default:
			var value any
			if value, err = ParseAnswer(meta, raw); err == nil {
				err = SetPath(planMap, path, value)
			}
		}

		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		applied = append(applied, path)
	}

	if len(applied) == 0 {
		return nil, applied, problems
	}

	// 回填后必须能通过严格反序列化 —— 否则说明写进去的值不合形状, 整批回滚,
	// 免得把半坏的计划交给下一轮
	plan, err := schema.PlanFromMap(kind, planMap)
	if err != nil {
		return nil, nil, append(problems,
			fmt.Sprintf("回填后的计划校验失败, 已忽略本轮答案: %v", err))
	}
	return plan, applied, problems
}

// applyRetention 把时长回答写入 retention.full。
//
// 只写 full: 一句"保留14天"是否同时适用于增量存在歧义, 不替用户做假设 ——
// 若计划含增量备份, businessWarnings 会专门提示确认。
func applyRetention(planMap map[string]any, meta SlotMeta, raw string) error {
	value, err := ParseAnswer(meta, raw)
	if err != nil {
		return err
	}
	retention, ok := planMap["retention"].(map[string]any)
	if !ok {
		retention = map[string]any{}
		planMap["retention"] = retention
	}
	retention["full"] = value
	return nil
}

var bucketPattern = regexp.MustCompile(`(?:桶|bucket)\s*(?:名|名字|为|是|:|=)?\s*([A-Za-z0-9._-]{2,63})`)

// applyDestination 把"存到哪里"的回答转成一个 Destination 条目。
func applyDestination(planMap map[string]any, raw string, cat catalog.Catalog) error {
	destination, err := parseDestinationAnswer(raw, cat)
	if err != nil {
		return err
	}

	existing, _ := planMap["destinations"].([]any)

	// 落点规则: 同类型替换, 不同类型追加。
	//
	// 为什么不是无脑追加: 模型或演示客户端常已从需求原文里抽出过一条目的地,
	// 用户再回答"存到哪里"时若追加, 就变成"改一次多一条", 越改越乱。
	// 为什么不是无脑替换: 计划本来就可能有多个目的地 (本地主备 + 异地副本),
	// 那是有意的, 不能因为改了一处就把另一处抹掉。
	// 按类型区分正好对上这两种意图: 改本地路径 = 同类型替换; 增加异地 = 新类型追加。
	for index, item := range existing {
		table, ok := item.(map[string]any)
		if !ok || table["type"] != destination["type"] {
			continue
		}
		// 保留原 id, 让"改动"看起来是改动而不是换了一条
		if id, ok := table["id"].(string); ok && id != "" {
			destination["id"] = id
		}
		existing[index] = destination
		planMap["destinations"] = existing
		return nil
	}

	destination["id"] = fmt.Sprintf("dest-%d", len(existing)+1)
	planMap["destinations"] = append(existing, destination)
	return nil
}

// parseDestinationAnswer 把自然语言描述解析成 Destination。
func parseDestinationAnswer(raw string, cat catalog.Catalog) (map[string]any, error) {
	destination := map[string]any{}

	// 命中清单里的介质服务器时, 落点直接取自服务器定义 —— 那才是环境里真实存在的位置
	if server, ok := cat.MediaServerFor(raw); ok {
		destination["type"] = server.Type
		destination["media_server"] = server.Name
		if server.Path != "" {
			destination["path"] = server.Path
		}
		if server.Bucket != "" {
			destination["bucket"] = server.Bucket
		}
		if server.Region != "" {
			destination["region"] = server.Region
		}
		if server.Endpoint != "" {
			destination["endpoint"] = server.Endpoint
		}
		// 用户额外写了路径时以他写的为准
		if paths := nlp.Paths(raw); len(paths) > 0 && paths[0] != server.Path {
			destination["path"] = paths[0]
		}
		return destination, nil
	}

	destType := ""
	if code, ok := driver.MatchChoice(cat.ChoiceMap(), raw); ok {
		destType = code
	} else if kind, ok := nlp.DetectDestination(raw); ok {
		destType = kind
	} else {
		return nil, fmt.Errorf(
			"无法识别存储类型 (示例: 本地磁盘 /u01/backup、阿里云 OSS、共享目录、磁带)")
	}
	destination["type"] = destType

	if paths := nlp.Paths(raw); len(paths) > 0 {
		switch destType {
		case "local", "nfs", "tape":
			destination["path"] = paths[0]
		default:
			destination["bucket"] = strings.Split(strings.TrimPrefix(paths[0], "/"), "/")[0]
		}
	}
	if match := bucketPattern.FindStringSubmatch(raw); match != nil {
		destination["bucket"] = match[1]
	}
	return destination, nil
}

// applyFrequency 把频率描述写入 schedule.<name>, 与已有内容合并。
//
// 是合并而不是覆盖: 用户只回答"每天"，不该把此前已确定的周日信息抹掉。
func applyFrequency(planMap map[string]any, path, raw string) error {
	name := strings.TrimPrefix(path, "schedule.")
	meta, ok := genericSlots["schedule."+name]
	if !ok || name == "" {
		return fmt.Errorf("无法识别的调度槽位: %s", path)
	}

	partial := map[string]any{}
	if code, ok := driver.MatchChoice(meta.ChoicesMap, raw); ok {
		partial["frequency"] = code
	}
	if weekday, ok := nlp.ParseWeekday(raw); ok {
		partial["frequency"] = "weekly"
		partial["weekday"] = weekday
	}
	if day, ok := nlp.ParseDayOfMonth(raw); ok {
		partial["frequency"] = "monthly"
		partial["day_of_month"] = day
	}
	if clock, ok := nlp.ParseClock(raw); ok {
		partial["time"] = clock
	}
	if len(partial) == 0 {
		return fmt.Errorf("无法识别备份频率 (示例: 每天凌晨1点、每周日)")
	}

	schedule, ok := planMap["schedule"].(map[string]any)
	if !ok {
		schedule = map[string]any{}
		planMap["schedule"] = schedule
	}
	merged := map[string]any{}
	if existing, ok := schedule[name].(map[string]any); ok {
		for key, value := range existing {
			merged[key] = value
		}
	}
	for key, value := range partial {
		merged[key] = value
	}
	if _, ok := merged["timezone"]; !ok {
		merged["timezone"] = "Asia/Shanghai"
	}
	schedule[name] = merged
	return nil
}

func sortedKeys(table map[string]string) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// catalogSlotMeta 从清单里取该应用的参数定义。
//
// 清单接口没给参数定义时返回 false, 由调用方回落到内置驱动 —— 许多清单接口只声明
// "有哪些应用", 不声明参数, 那种情况下内置驱动的参数定义仍然适用。
func catalogSlotMeta(cat catalog.Catalog, kind, field, path string) (SlotMeta, bool) {
	app, ok := cat.ApplicationFor(kind)
	if !ok {
		return SlotMeta{}, false
	}
	for _, option := range app.Options {
		if option.Field != field {
			continue
		}
		question := option.Label
		if question == "" {
			question = "请提供 " + field
		}
		return SlotMeta{
			Slot:       path,
			Question:   question,
			Kind:       option.Kind,
			Example:    option.Example,
			Choices:    option.Choices,
			ChoicesMap: option.ChoicesMap,
			Suggest:    option.Suggest,
			Hint:       option.Hint,
			Min:        option.Min,
			Max:        option.Max,
		}, true
	}
	return SlotMeta{}, false
}
