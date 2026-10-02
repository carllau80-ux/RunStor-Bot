package agent

import (
	"time"

	"backup-restore-agent/internal/catalog"

	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// SmokeText 是抽取冒烟用的最小需求, 覆盖 源/调度/保留/目的地 四类槽位。
const SmokeText = "每天凌晨2点全量备份MySQL数据库到本地磁盘/u01/backup，保留7天"

// SmokeTest 对指定 profile 跑一次真实抽取, 看模型能否产出合法计划。
//
// 为什么单独做这件事 (而不是只 ping 一下): "key 错"和"模型中文抽取能力不行"的
// 现象都是"解析失败", 但处置方式完全不同 —— 前者换 Key, 后者换模型。
// 只有真跑一次抽取才能区分这两者。
func SmokeTest(registry *llm.Registry, profile string, cat catalog.Catalog) map[string]any {
	client := registry.Client(profile)
	started := time.Now()

	result, err := Extract(client, ExtractRequest{
		Kind: schema.KindBackup, Requirement: SmokeText, Catalog: cat,
	})
	elapsed := int(time.Since(started).Milliseconds())

	if err != nil {
		classified := llm.AsError(err)
		return map[string]any{
			"ok":         false,
			"elapsed_ms": elapsed,
			"message":    "抽取冒烟失败: " + classified.Error(),
			"hint":       "建议换用中文理解更强的模型, 或先切 demo 模式演示",
			"detail":     classified.Detail,
			"error_kind": string(classified.Kind),
		}
	}

	planMap, mapErr := schema.PlanToMap(result.Plan)
	if mapErr != nil {
		return map[string]any{
			"ok":         false,
			"elapsed_ms": elapsed,
			"message":    "抽取结果无法序列化: " + mapErr.Error(),
		}
	}

	kind, _ := GetPath(planMap, "source.items[0].kind")
	schedule, _ := GetPath(planMap, "schedule.full.frequency")
	clock, _ := GetPath(planMap, "schedule.full.time")

	destinations := []string{}
	if raw, ok := GetPath(planMap, "destinations"); ok {
		if list, ok := raw.([]any); ok {
			for _, item := range list {
				if table, ok := item.(map[string]any); ok {
					if text, ok := table["type"].(string); ok {
						destinations = append(destinations, text)
					}
				}
			}
		}
	}

	ok := len(result.ResidualErrors()) == 0
	message := "抽取成功"
	if !ok {
		message = "抽取完成, 但存在需要人工确认的问题"
	}

	return map[string]any{
		"ok":           ok,
		"elapsed_ms":   elapsed,
		"message":      message,
		"task_name":    stringField(planMap, "task_name"),
		"kind":         kind,
		"frequency":    schedule,
		"time":         clock,
		"destinations": destinations,
		"confidence":   confidenceOf(planMap),
		"attempts":     result.Attempts,
		"notices":      result.Notices,
	}
}

func stringField(planMap map[string]any, path string) string {
	value, ok := GetPath(planMap, path)
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}
