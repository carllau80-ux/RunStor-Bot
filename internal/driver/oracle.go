package driver

// Oracle 驱动。
//
// 这是本项目首个完整实现的引擎, 也是"驱动注册表"这套设计的样板:
// 槽位表 + 别名 + 术语映射, 三样东西就定义了 Oracle 的完整支持。
//
// 槽位取舍的说明 (为什么 channels 必填而 target_dir 不必填):
//   - scope / auth_mode / channels 会实质改变备份行为与窗口, 默认值有真实代价
//     (并发默认 1 可能让多 TB 库备一整夜), 所以值得问用户。
//   - target_dir / sbt_library / archivelog 在"只产出 JSON"的阶段不阻塞计划成立,
//     因此给安全默认并只出提示。等接入渲染器时它们才是硬前置。
func init() {
	Register(&Driver{
		Kind:        "oracle",
		Display:     "Oracle 数据库",
		SourceType:  "database",
		Aliases:     []string{"oracle", "orcl", "rman", "甲骨文"},
		OptionsDesc: "Oracle (RMAN) 专属参数。只填用户明确提到的, 不要编造",
		Slots: []Slot{
			{
				Field:    "scope",
				Question: "备份范围是整个实例, 还是只备份部分 (全库/表空间/数据文件/仅归档)?",
				Kind:     SlotEnum,
				Desc: "备份范围。whole_instance=整个实例(全库+归档+控制文件+参数文件); " +
					"database=仅全库; pdb=指定可插拔库; tablespace=指定表空间; " +
					"datafile=指定数据文件; archivelog_only=仅归档日志",
				Choices: []string{
					"整个实例(全库+归档+控制文件+参数文件)",
					"仅全库",
					"指定可插拔库 PDB",
					"指定表空间",
					"指定数据文件",
					"仅归档日志",
				},
				ChoicesMap: map[string]string{
					"整个实例(全库+归档+控制文件+参数文件)": "whole_instance",
					"仅全库":        "database",
					"指定可插拔库 PDB": "pdb",
					"指定表空间":      "tablespace",
					"指定数据文件":     "datafile",
					"仅归档日志":      "archivelog_only",
				},
				Example: "整个实例",
			},
			{
				Field:    "auth_mode",
				Question: "Oracle 用哪种认证方式连接?",
				Kind:     SlotEnum,
				Desc: "认证方式。os_auth=操作系统认证(即 rman target /, 免密); " +
					"password=数据库账号密码(凭据填 credential_ref); wallet=Oracle Wallet 钱包",
				Choices: []string{
					"操作系统认证(系统认证, rman target /)",
					"数据库账号密码",
					"Oracle Wallet 钱包",
				},
				ChoicesMap: map[string]string{
					"操作系统认证(系统认证, rman target /)": "os_auth",
					"数据库账号密码":                     "password",
					"Oracle Wallet 钱包":            "wallet",
				},
				Example: "系统认证",
			},
			{
				Field:    "channels",
				Question: "备份并发度 (RMAN 通道数) 设置为几?",
				Kind:     SlotInt,
				Desc:     "RMAN 通道数 (并发度), 决定 PARALLELISM 值",
				Example:  "4",
				Suggest:  []string{"2", "4", "8"},
				Hint:     "并发越高备份越快, 但会同时压满 IO; 通常 2-8",
				Min:      IntPtr(1),
				Max:      IntPtr(32),
			},
			{
				Field:    "sid",
				Question: "Oracle 实例的 SID 或服务名是什么?",
				Kind:     SlotString,
				Desc:     "Oracle 实例 SID 或服务名, 如 orcl",
				Optional: true,
			},
			{
				Field:    "target",
				Question: "备份写到本地/挂载目录, 还是走 SBT 介质通道?",
				Kind:     SlotEnum,
				Desc: "备份介质通道。disk=本地/挂载目录直接写(用 FORMAT 指定路径); " +
					"sbt=经介质管理库 libobk.so (SBT_TAPE)",
				Choices: []string{"本地或挂载目录", "SBT 介质通道 (libobk.so)"},
				ChoicesMap: map[string]string{
					"本地或挂载目录":              "disk",
					"SBT 介质通道 (libobk.so)": "sbt",
				},
				Example:  "SBT",
				Default:  "disk",
				Optional: true,
			},
			{
				Field:    "target_dir",
				Question: "备份片落到哪个目录?",
				Kind:     SlotString,
				Desc:     "备份片落地目录, 如 /u01/backup/orcl",
				Optional: true,
			},
			{
				Field:    "sbt_library",
				Question: "libobk.so 的绝对路径是什么?",
				Kind:     SlotString,
				Desc:     "target=sbt 时介质管理库的绝对路径, 如 $ORACLE_HOME/lib/libobk.so",
				Optional: true,
			},
			{
				Field:    "sbt_compress",
				Question: "SBT 通道是否需要压缩?",
				Kind:     SlotEnum,
				Desc:     "target=sbt 时的压缩方式",
				Choices:  []string{"不压缩", "zstd 压缩", "gzip 压缩"},
				ChoicesMap: map[string]string{
					"不压缩":     "none",
					"zstd 压缩": "zstd",
					"gzip 压缩": "gzip",
				},
				Optional: true,
			},
			{
				Field:    "archivelog",
				Question: "是否同时备份归档日志?",
				Kind:     SlotBool,
				Desc:     "是否同时备份归档日志 (RMAN 的 PLUS ARCHIVELOG)",
				Optional: true,
				Default:  true,
			},
			{
				Field:    "controlfile_autobackup",
				Question: "是否开启控制文件自动备份?",
				Kind:     SlotBool,
				Desc:     "是否开启控制文件自动备份 (强烈建议开启)",
				Optional: true,
				Default:  true,
			},
		},
		PromptHints: "Oracle 术语: \"整实例/整个实例/全实例\" -> scope=whole_instance; " +
			"\"系统认证/操作系统认证/免密/os认证\" -> auth_mode=os_auth; " +
			"\"并发/通道/channel/并行度\" 后跟的数字 -> channels; " +
			"\"SBT/磁带/带库/libobk\" -> target=sbt; " +
			"Oracle RMAN 里 \"全备\" 对应 incremental level 0, \"增量\" 对应 level 1。",
	})
}
