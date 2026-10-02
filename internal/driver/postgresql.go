package driver

// PostgreSQL 驱动。
func init() {
	Register(&Driver{
		Kind:        "postgresql",
		Display:     "PostgreSQL",
		SourceType:  "database",
		Aliases:     []string{"postgresql", "postgres", "pgsql", "pg"},
		OptionsDesc: "PostgreSQL 专属参数。只填用户明确提到的, 不要编造",
		Slots: []Slot{
			{
				Field:    "method",
				Question: "PostgreSQL 用哪种备份方式?",
				Kind:     SlotEnum,
				Desc: "备份方式。pg_dump=单库逻辑备份; pg_dumpall=全部库逻辑备份含角色; " +
					"pg_basebackup=整集群物理备份; wal_archive=仅连续归档 WAL",
				Choices: []string{
					"单库逻辑备份 pg_dump",
					"全部库逻辑备份 pg_dumpall",
					"整集群物理备份 pg_basebackup",
					"仅 WAL 连续归档",
				},
				ChoicesMap: map[string]string{
					"单库逻辑备份 pg_dump":        "pg_dump",
					"全部库逻辑备份 pg_dumpall":    "pg_dumpall",
					"整集群物理备份 pg_basebackup": "pg_basebackup",
					"仅 WAL 连续归档":            "wal_archive",
				},
				Example: "单库逻辑备份",
			},
			{
				Field:    "databases",
				Question: "要备份哪些库? (全部库回答\"全部\", 或列出库名)",
				Kind:     SlotList,
				Target:   TargetItem,
				Desc:     "库名列表; 全部库用 [\"*\"]",
				Example:  "全部",
				// 推荐值必须是能直接用的答案, 不能是"指定库名"这类提示语 ——
				// 点下去会把 databases 填成字面量 ["指定库名"]。要指定具体库时用户自己填。
				Suggest: []string{"全部"},
				Hint:    "物理备份与 WAL 归档按整个集群进行, 无需指定库名",
				// 条件必填: 物理备份 / WAL 归档时按整集群走, 不需要库名
				SatisfiedWhen: func(options map[string]any) bool {
					method, _ := options["method"].(string)
					return method == "pg_basebackup" || method == "wal_archive"
				},
			},
			{
				Field:    "format",
				Question: "逻辑备份用什么输出格式?",
				Kind:     SlotEnum,
				Desc:     "逻辑备份输出格式, 默认 plain (SQL 文本)",
				Choices:  []string{"SQL 文本 plain", "自定义格式 custom", "目录格式 directory", "tar 包"},
				ChoicesMap: map[string]string{
					"SQL 文本 plain":   "plain",
					"自定义格式 custom":   "custom",
					"目录格式 directory": "directory",
					"tar 包":          "tar",
				},
				Optional: true,
			},
			{
				Field:    "slot_name",
				Question: "物理备份使用的复制槽名是什么?",
				Kind:     SlotString,
				Desc:     "物理备份使用的复制槽名, 如 pg_backup_slot",
				Optional: true,
			},
		},
		PromptHints: "PostgreSQL 术语: \"pg_dump/逻辑备份\" -> method=pg_dump; " +
			"\"全部库/所有库\" 在逻辑备份语境下 -> method=pg_dumpall; " +
			"\"物理备份/基础备份/basebackup/整集群\" -> method=pg_basebackup; " +
			"\"WAL归档/连续归档/日志归档\" -> method=wal_archive; " +
			"\"全部库\" 在库名语境下 -> source.items[].databases=[\"*\"]。",
	})
}
