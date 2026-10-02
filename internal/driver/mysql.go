package driver

// MySQL / MariaDB 驱动。
func init() {
	Register(&Driver{
		Kind:        "mysql",
		Display:     "MySQL / MariaDB",
		SourceType:  "database",
		Aliases:     []string{"mysql", "mariadb", "mysqldump", "innodb"},
		OptionsDesc: "MySQL 专属参数。只填用户明确提到的, 不要编造",
		Slots: []Slot{
			{
				Field:    "method",
				Question: "MySQL 用逻辑备份 (mysqldump) 还是物理备份 (xtrabackup)?",
				Kind:     SlotEnum,
				Desc: "备份方式。logical=逻辑备份 mysqldump, 单库/单表易恢复但慢; " +
					"physical=物理备份 xtrabackup, 大库快, 整实例",
				Choices: []string{"逻辑备份 mysqldump", "物理备份 xtrabackup"},
				ChoicesMap: map[string]string{
					"逻辑备份 mysqldump":  "logical",
					"物理备份 xtrabackup": "physical",
				},
				Example: "逻辑备份",
				Hint:    "单库/单表恢复选逻辑; 数据量大、要快速恢复选物理",
			},
			{
				Field:    "databases",
				Question: "要备份哪些库? (全部库回答\"全部\", 或列出库名)",
				Kind:     SlotList,
				Target:   TargetItem,
				Desc:     "库名列表; 整实例或全部库用 [\"*\"]",
				Example:  "全部",
				// 推荐值必须是能直接用的答案, 不能是"指定库名"这类提示语 ——
				// 点下去会把 databases 填成字面量 ["指定库名"]。要指定具体库时用户自己填。
				Suggest: []string{"全部"},
			},
			{
				Field:    "single_transaction",
				Question: "逻辑备份是否使用一致性快照?",
				Kind:     SlotBool,
				Desc:     "逻辑备份时使用一致性快照 (--single-transaction), InnoDB 建议开启",
				Optional: true,
				Default:  true,
			},
			{
				Field:    "flush_logs",
				Question: "备份时是否切割 binlog?",
				Kind:     SlotBool,
				Desc:     "备份时执行 FLUSH LOGS 切割 binlog",
				Optional: true,
			},
		},
		PromptHints: "MySQL 术语: \"全部库/所有库/整个实例\" -> " +
			"source.items[].databases=[\"*\"]; \"逻辑备份/mysqldump/dump\" -> method=logical; " +
			"\"物理备份/xtrabackup/热备/innobackupex\" -> method=physical。",
	})
}
