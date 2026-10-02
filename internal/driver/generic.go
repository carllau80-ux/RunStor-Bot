package driver

// 兜底驱动 —— 未知或未注册的 kind 走这里。
//
// 存在的意义: 用户提到一个我们还没写驱动的引擎 (DB2、达梦、Kingbase、MinIO…),
// 流程不该因此中断。通用必填槽位照常追问, 计划照常产出, 只多一条"引擎未注册"
// 的提示, 说明该引擎的专属参数不会被校验。
func init() {
	Register(&Driver{
		Kind:       genericKind,
		Display:    "其他 / 未注册引擎",
		SourceType: "mixed",
		Aliases:    []string{"其他", "其它", "未知"},
		Slots:      nil,
		PromptHints: "遇到不在驱动清单里的引擎 (如 DB2、达梦、Kingbase、MinIO 等): " +
			"把 kind 填成该引擎的小写英文名, options 里只填你确信的参数, 不确定的不要编造。",
	})
}
