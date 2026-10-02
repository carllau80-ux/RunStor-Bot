package agent

import (
	"strings"

	"backup-restore-agent/internal/schema"
)

// 意图路由: 判定用户这次说的是"备份"还是"恢复"。
//
// 与最初设想的差异: 原计划用"LLM 判定 + 关键词兜底", 实现时改成纯确定性规则 +
// 接口显式覆盖 (kind 参数)。理由是二分类问题上多打一次模型调用会让延迟翻倍,
// 而中文备份/恢复领域的关键词足够明确; 真正会出错的场景 (见下) 用规则也能覆盖,
// 且规则是可测试、可解释的。调用方若确知意图, 直接用 kind 参数指定即可。
//
// 需要处理的真实误判: "备份策略里要包含恢复演练" 含"恢复"二字, 但意图是备份。
// 因此先排除演练类说法, 再看是否有强恢复信号。

var (
	// 演练/校验类说法: 出现在备份需求里, 不代表要执行恢复
	drillWords = []string{"恢复演练", "恢复测试", "恢复验证", "试恢复", "演练", "校验恢复"}

	// 强恢复信号: 明确表达"把数据弄回去"
	strongRestoreWords = []string{
		"恢复到", "恢复至", "还原到", "还原至", "回滚", "恢复数据",
		"异机恢复", "重新恢复", "恢复数据库", "恢复实例", "恢复某", "恢复到某",
		"restore", "recover", "rollback", "point-in-time", "pitr",
	}

	// 弱恢复信号: 单独出现"恢复"字样
	weakRestoreWords = []string{"恢复", "还原"}

	// 能力性说法: 描述"备份能不能用来恢复", 而不是要求执行恢复。
	//
	// 这类句子里的"恢复到"是强恢复信号, 若不先摘掉就会误判 ——
	// 例如 "备份Oracle，保留30天，要能恢复到任意时间点" 其实是备份需求。
	// 顺序上长短语在前, 否则 "能恢复" 会先命中而留下半截文本。
	capabilityPhrases = []string{
		"要能恢复到", "是否支持恢复到", "是否支持恢复",
		"能够恢复到", "能够恢复", "能不能恢复到", "能不能恢复",
		"可以恢复到", "可以恢复", "可恢复到", "可恢复",
		"支持恢复到", "支持恢复", "确保能恢复", "保证能恢复",
		"具备恢复", "能恢复到", "能恢复", "可回滚",
	}

	// 备份信号
	backupWords = []string{"备份", "全备", "全量", "增量", "差异", "归档", "backup"}
)

// DetectIntent 判定意图。返回 schema.KindBackup 或 schema.KindRestore。
func DetectIntent(text string) string {
	lowered := strings.ToLower(text)

	// 先摘掉演练类说法, 否则 "每周五做一次恢复演练" 会被判成恢复
	stripped := lowered
	for _, word := range drillWords {
		stripped = strings.ReplaceAll(stripped, strings.ToLower(word), "")
	}
	// 再摘掉能力性说法, 否则 "要能恢复到任意时间点" 会被判成恢复
	for _, phrase := range capabilityPhrases {
		stripped = strings.ReplaceAll(stripped, strings.ToLower(phrase), "")
	}

	for _, word := range strongRestoreWords {
		if strings.Contains(stripped, strings.ToLower(word)) {
			return schema.KindRestore
		}
	}

	hasBackup := false
	for _, word := range backupWords {
		if strings.Contains(stripped, strings.ToLower(word)) {
			hasBackup = true
			break
		}
	}
	hasWeakRestore := false
	for _, word := range weakRestoreWords {
		if strings.Contains(stripped, strings.ToLower(word)) {
			hasWeakRestore = true
			break
		}
	}

	// 只提了恢复没提备份 -> 恢复; 两者都提 -> 按备份处理 (备份需求里常顺带提到恢复能力)
	if hasWeakRestore && !hasBackup {
		return schema.KindRestore
	}
	return schema.KindBackup
}

// NormalizeKind 校验调用方传入的 kind; 非法时返回空串由上层决定。
func NormalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case schema.KindBackup, "备份":
		return schema.KindBackup
	case schema.KindRestore, "恢复", "还原":
		return schema.KindRestore
	default:
		return ""
	}
}
