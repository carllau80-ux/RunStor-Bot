package catalog

import (
	"os"
	"sort"
	"time"

	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/schema"
)

// 内置兜底清单: 外部接口不可用 (或未配置) 时使用。
//
// 它不是"另一份真相", 而是把代码里本来就有的东西 (驱动注册表 + 目的地类型) 转成
// 清单的形状, 这样上层只需要面对一种数据结构, 不必到处写 "有接口时用接口, 否则用内置"。

// destTypeLabels 是目的地类型的中文标签。
//
// 放在清单层而不是界面里: 它属于"介质服务器"这套词汇, 而清单正是这套词汇的所有者。
var destTypeLabels = map[string]string{
	"local":       "本地磁盘",
	"nfs":         "共享目录",
	"s3":          "S3 对象存储",
	"aliyun_oss":  "阿里云 OSS",
	"tencent_cos": "腾讯云 COS",
	"azure_blob":  "Azure Blob",
	"tape":        "磁带库",
	"other":       "其他介质",
}

// DestTypeLabel 返回目的地类型的中文标签 (未知类型原样返回)。
func DestTypeLabel(destType string) string {
	if label, ok := destTypeLabels[destType]; ok {
		return label
	}
	return destType
}

// DestTypeLabels 返回类型 -> 中文标签的副本。
func DestTypeLabels() map[string]string {
	out := make(map[string]string, len(destTypeLabels))
	for key, value := range destTypeLabels {
		out[key] = value
	}
	return out
}

// builtin 缓存内置清单: 它由 init 期注册的驱动推导而来, 运行期不会变。
var builtin = builtinCatalog()

// Builtin 返回内置兜底清单 (包级, 供其它包在未拿到清单时使用)。
//
// 返回副本: 调用方可能往上写 Warning 之类的东西, 不该污染这份共享数据。
func Builtin() Catalog {
	out := builtin
	out.Applications = append([]Application{}, builtin.Applications...)
	out.MediaServers = append([]MediaServer{}, builtin.MediaServers...)
	return out
}

// ChoiceLabels 返回介质服务器的候选标签 (给用户点选)。
//
// 面向用户的是"服务器名 + 落点", 而不是 local/s3 这类代码值 ——
// 用户选的其实是具体哪台服务器上的哪个位置。
func (c Catalog) ChoiceLabels() []string {
	out := make([]string, 0, len(c.MediaServers))
	for _, server := range c.MediaServers {
		out = append(out, server.Display())
	}
	sort.Strings(out)
	return out
}

// ChoiceMap 返回"候选标签 -> 服务器标识"的映射, 供答案解析定位到具体服务器。
func (c Catalog) ChoiceMap() map[string]string {
	out := make(map[string]string, len(c.MediaServers))
	for _, server := range c.MediaServers {
		out[server.Display()] = server.ID
	}
	return out
}

// ServerByID 按标识取介质服务器。
func (c Catalog) ServerByID(id string) (MediaServer, bool) {
	for _, server := range c.MediaServers {
		if server.ID == id {
			return server, true
		}
	}
	return MediaServer{}, false
}

// builtinCatalog 把代码内置的应用与目的地类型转成清单。
func builtinCatalog() Catalog {
	result := Catalog{
		Source:    "builtin",
		FetchedAt: time.Now(),
	}

	for _, engine := range driver.All() {
		result.Applications = append(result.Applications, Application{
			Kind:       engine.Kind,
			Display:    engine.Display,
			SourceType: engine.SourceType,
			Aliases:    append([]string{}, engine.Aliases...),
			Options:    builtinOptions(engine),
		})
	}

	// 目的地类型按 schema 声明的顺序排, 保证输出稳定
	for _, destType := range schema.DestTypes {
		label := DestTypeLabel(destType)
		result.MediaServers = append(result.MediaServers, MediaServer{
			ID:   destType,
			Name: label,
			Type: destType,
		})
	}
	return result
}

// builtinOptions 把驱动槽位转成清单参数, 让内置清单与接口清单形状一致。
func builtinOptions(engine *driver.Driver) []Option {
	out := make([]Option, 0, len(engine.Slots))
	for _, slot := range engine.Slots {
		required := !slot.Optional
		out = append(out, Option{
			Field:      slot.Field,
			Label:      slot.Question,
			Kind:       string(slot.Kind),
			Choices:    slot.ChoiceLabels(),
			ChoicesMap: slot.ChoicesMap,
			Required:   &required,
			Example:    slot.Example,
			Suggest:    slot.Suggest,
			Hint:       slot.Hint,
			Min:        slot.Min,
			Max:        slot.Max,
		})
	}
	return out
}

// getenv 是 os.Getenv 的薄封装 (便于测试替换)。
func getenv(name string) string { return os.Getenv(name) }
