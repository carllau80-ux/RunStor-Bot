package driver

// 文件 / 目录驱动 —— 覆盖文件服务器、共享盘、通用目录备份。
func init() {
	Register(&Driver{
		Kind:        "file",
		Display:     "文件 / 目录",
		SourceType:  "file",
		Aliases:     []string{"file", "文件", "目录", "文件服务器", "文件夹", "nas"},
		OptionsDesc: "文件类备份的附加参数。只填用户明确提到的",
		Slots: []Slot{
			{
				Field:    "paths",
				Question: "要备份哪些目录或文件? (多个用逗号分隔)",
				Kind:     SlotList,
				Target:   TargetItem,
				Desc:     "要备份的目录/文件路径列表",
				Example:  "/data/app, /etc/app",
			},
			{
				Field:    "exclude",
				Question: "有哪些路径需要排除?",
				Kind:     SlotList,
				Desc:     "排除的路径或模式, 如 *.tmp、/data/cache",
				Optional: true,
			},
			{
				Field:    "archive_format",
				Question: "用什么归档格式?",
				Kind:     SlotEnum,
				Desc:     "归档格式。none=按原样副本(文件级复制); rsync=增量同步",
				Choices:  []string{"不打包, 按原样副本", "tar 归档", "zip 归档", "rsync 增量同步"},
				ChoicesMap: map[string]string{
					"不打包, 按原样副本": "none",
					"tar 归档":     "tar",
					"zip 归档":     "zip",
					"rsync 增量同步": "rsync",
				},
				Optional: true,
			},
			{
				Field:    "use_snapshot",
				Question: "是否使用存储快照保证一致性?",
				Kind:     SlotBool,
				Desc:     "是否用存储快照做一致性备份 (LVM/ZFS 快照)",
				Optional: true,
			},
		},
		PromptHints: "文件类需求要把路径填进 source.items[].paths (字符串数组), " +
			"不要塞进 options; \"排除/不要备份\" 的路径填 options.exclude。",
	})
}
