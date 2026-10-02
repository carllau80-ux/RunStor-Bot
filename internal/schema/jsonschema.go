package schema

// 本文件负责生成喂给 LLM 的 JSON Schema。
//
// 为什么不引第三方库 (如 invopop/jsonschema): 目标机与开发机都不通外网。
// 更实际的原因是, 我们要的 schema 里含大量中文说明与业务约束, 手写反而更准;
// 而防漂移由测试保证 —— TestSchemaCoversAllFields 用反射比对结构体字段与
// schema 属性, 漏写或多写都会失败。

// Obj 是 JSON Schema 片段的简写。
type Obj = map[string]any

func strSchema(desc string) Obj {
	return Obj{"type": "string", "description": desc}
}

func enumSchema(desc string, values []string) Obj {
	return Obj{"type": "string", "description": desc, "enum": values}
}

func intSchema(desc string, min, max int) Obj {
	return Obj{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func boolSchema(desc string) Obj {
	return Obj{"type": "boolean", "description": desc}
}

func arraySchema(desc string, items Obj) Obj {
	return Obj{"type": "array", "description": desc, "items": items}
}

func refSchema(desc, ref string) Obj {
	return Obj{"description": desc, "$ref": ref}
}

func objectSchema(desc string, properties Obj, required ...string) Obj {
	out := Obj{"type": "object", "description": desc, "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// sharedDefs 返回 $defs 下的公共类型定义。
func sharedDefs() Obj {
	return Obj{
		"schedule_spec": objectSchema(
			"一次备份的调度",
			Obj{
				"frequency": enumSchema("执行频率", Frequencies),
				"time": Obj{
					"type":        "string",
					"description": "执行时刻, 24 小时制 HH:MM。用户未指明时必须留空",
					"pattern":     `^([01]\d|2[0-3]):[0-5]\d$`,
				},
				"weekday":      intSchema("每周几执行, 1=周一 ... 7=周日。frequency=weekly 时必填", 1, 7),
				"day_of_month": intSchema("每月几号执行。frequency=monthly 时必填", 1, 31),
				"timezone":     strSchema("时区, 未指定用 Asia/Shanghai"),
			},
			"frequency", "timezone",
		),
		"schedule": objectSchema(
			"全量/增量/差异三种调度, 未提及的留空",
			Obj{
				"full":         refSchema("全量(基线)备份的调度", "#/$defs/schedule_spec"),
				"incremental":  refSchema("增量备份的调度", "#/$defs/schedule_spec"),
				"differential": refSchema("差异备份的调度", "#/$defs/schedule_spec"),
			},
		),
		"retention_spec": objectSchema(
			"一段保留期",
			Obj{
				"value": intSchema("保留数量的数值部分", 1, 100000),
				"unit":  enumSchema("保留数量的单位", RetentionUnits),
			},
			"value", "unit",
		),
		"retention": objectSchema(
			"保留策略, 至少填 full 或 incremental 之一",
			Obj{
				"full":         refSchema("全量备份保留多久", "#/$defs/retention_spec"),
				"incremental":  refSchema("增量备份保留多久", "#/$defs/retention_spec"),
				"differential": refSchema("差异备份保留多久", "#/$defs/retention_spec"),
				"min_copies":   intSchema("最少保留副本数", 1, 100),
			},
		),
		"destination": objectSchema(
			"一处备份落地位置",
			Obj{
				"id":       strSchema("目的地唯一标识, 如 dest-1"),
				"type":     enumSchema("目的地类型", DestTypes),
				"bucket":   strSchema("对象存储的桶名"),
				"region":   strSchema("对象存储的区域, 如 cn-hangzhou"),
				"endpoint": strSchema("自定义端点地址"),
				"path":     strSchema("本地/NFS/SBT 的目录路径"),
				"media_server": strSchema(
					"选中的介质服务器名称。目标介质由运行时清单给出时填这一项, " +
						"便于把计划对应回环境里真实存在的那台服务器",
				),
				"retention":   refSchema("该目的地单独的保留策略, 覆盖全局设置", "#/$defs/retention_spec"),
				"encryption":  enumSchema("加密算法", Encryptions),
				"compression": enumSchema("压缩算法", Compressions),
			},
			"id", "type",
		),
		"sla": objectSchema(
			"服务级别目标",
			Obj{
				"rpo": strSchema(`可接受的数据丢失量, 如 "24h"、"15m"`),
				"rto": strSchema(`可接受的恢复时间, 如 "4h"`),
			},
		),
		"verification": objectSchema(
			"恢复演练设置",
			Obj{
				"test_restore": boolSchema("是否做恢复演练"),
				"frequency":    enumSchema("恢复演练频率", VerifyFrequencies),
			},
		),
		"notification": objectSchema(
			"告警设置",
			Obj{
				"channels":   arraySchema("告警通道", enumSchema("通道", NotifyChannels)),
				"recipients": arraySchema("告警接收人", strSchema("接收人")),
			},
		),
		"source_item": objectSchema(
			"一个被保护对象",
			Obj{
				"id":   strSchema("源唯一标识, 如 src-1"),
				"name": strSchema("对象名称, 如 生产Oracle实例"),
				"kind": strSchema(
					"对象类型。常见: oracle / mysql / postgresql / mongodb / redis / " +
						"sqlserver / file / vm / kubernetes / application / other",
				),
				"host":           strSchema("主机地址或实例名"),
				"port":           intSchema("服务端口", 1, 65535),
				"databases":      arraySchema(`库名列表, 整实例或全部库用 ["*"]`, strSchema("库名")),
				"paths":          arraySchema("文件/目录路径列表", strSchema("路径")),
				"credential_ref": strSchema(`凭据引用, 如 "secret://cred/appuser"。绝不要输出明文密码`),
				"options": Obj{
					"type": "object",
					"description": "该 kind 专属的参数, 字段名见下方 options_schemas。" +
						"未提到的引擎可留空对象",
				},
			},
			"id", "name", "kind",
		),
		"backup_source": objectSchema(
			"被保护对象集合",
			Obj{
				"type":  enumSchema("整体对象类型", SourceTypes),
				"items": arraySchema("被保护对象列表", refSchema("对象", "#/$defs/source_item")),
			},
		),
	}
}

// BackupPlanSchema 返回备份计划的完整 JSON Schema。
//
// optionsSchemas: kind -> options 的 schema, 由 driver 包从槽位表生成。
func BackupPlanSchema(optionsSchemas Obj) Obj {
	return Obj{
		"type":                 "object",
		"additionalProperties": false,
		"$defs":                sharedDefs(),
		"properties": Obj{
			"schema_version": Obj{"type": "string", "const": SchemaVersion},
			"kind":           Obj{"type": "string", "const": KindBackup},
			"task_name":      strSchema("任务名称, 如 生产Oracle实例每周全备+每日增量"),
			"source":         refSchema("被保护对象集合", "#/$defs/backup_source"),
			"schedule":       refSchema("调度", "#/$defs/schedule"),
			"retention":      refSchema("保留策略", "#/$defs/retention"),
			"destinations":   arraySchema("备份存放位置, 至少一个", refSchema("目的地", "#/$defs/destination")),
			"sla":            refSchema("服务级别目标", "#/$defs/sla"),
			"verification":   refSchema("恢复演练设置", "#/$defs/verification"),
			"notification":   refSchema("告警设置", "#/$defs/notification"),
			"confidence": Obj{
				"type":        "number",
				"description": "整体置信度 0~1。信息完整给 0.8 以上, 缺失较多时给低值",
				"minimum":     0, "maximum": 1,
			},
			"warnings": arraySchema(
				"描述中存在的歧义、矛盾或不合理之处, 每条一句中文说明",
				strSchema("提示"),
			),
		},
		"required":          []string{"schema_version", "kind", "task_name", "source", "schedule", "retention", "destinations"},
		"x-options-schemas": optionsSchemas,
	}
}

// RestorePlanSchema 返回恢复计划的完整 JSON Schema。
func RestorePlanSchema(optionsSchemas Obj) Obj {
	defs := sharedDefs()
	defs["restore_point"] = objectSchema(
		"恢复到哪个时间点",
		Obj{
			"type": enumSchema("时间点类型", PointTypes),
			"value": strSchema(
				`具体取值。type=latest 时留空; timestamp 用 "YYYY-MM-DD HH:MM:SS"; ` +
					"scn/lsn 填数字; backup_set_id 填备份集标识",
			),
			"timezone": strSchema("时间点的时区"),
		},
		"type", "timezone",
	)
	defs["restore_target"] = objectSchema(
		"恢复到哪台机/哪个实例",
		Obj{
			"mode":           enumSchema("in_place=原机原实例; alternate_host=异机恢复", RestoreModes),
			"host":           strSchema("目标主机。异机恢复时必填"),
			"instance":       strSchema("目标实例名/SID/服务名。异机恢复时必填"),
			"credential_ref": strSchema(`目标机凭据引用。绝不要输出明文密码`),
		},
		"mode",
	)
	defs["restore_scope"] = objectSchema(
		"恢复范围",
		Obj{
			"level": enumSchema("范围层级", ScopeLevels),
			"items": arraySchema(
				`范围内具体对象名。level=whole 时留空; 全库用 ["*"]`,
				strSchema("对象名"),
			),
		},
		"level",
	)
	defs["alternate_location"] = objectSchema(
		"是否恢复到不同路径/不同实例名",
		Obj{
			"enabled":       boolSchema("是否启用"),
			"data_dir":      strSchema("数据文件落地的目标目录"),
			"instance_name": strSchema("重命名后的实例名/SID"),
			"validate_only": boolSchema("只做恢复校验不真正落盘"),
		},
	)
	defs["restore_verification"] = objectSchema(
		"恢复后的校验动作",
		Obj{
			"validate_backup":   boolSchema("恢复前先校验备份可读 (会显著拉长耗时)"),
			"open_database":     boolSchema("恢复后是否打开库"),
			"check_consistency": boolSchema("恢复后做一致性检查"),
		},
	)

	return Obj{
		"type":                 "object",
		"additionalProperties": false,
		"$defs":                defs,
		"properties": Obj{
			"schema_version": Obj{"type": "string", "const": SchemaVersion},
			"kind":           Obj{"type": "string", "const": KindRestore},
			"task_name":      strSchema("任务名称, 如 生产Oracle恢复到故障前时刻"),
			"source":         refSchema("要恢复的对象 (原始被保护对象)", "#/$defs/backup_source"),
			"source_backup_ref": strSchema(
				`从哪份备份恢复: 备份任务名或备份集标识, 如 "生产Oracle实例周全备"`,
			),
			"target":       refSchema("恢复到哪台机/哪个实例", "#/$defs/restore_target"),
			"point":        refSchema("恢复到哪个时间点", "#/$defs/restore_point"),
			"scope":        refSchema("恢复范围", "#/$defs/restore_scope"),
			"alternate":    refSchema("是否换路径/换实例名", "#/$defs/alternate_location"),
			"verification": refSchema("恢复后的校验动作", "#/$defs/restore_verification"),
			"sla":          refSchema("服务级别目标", "#/$defs/sla"),
			"notification": refSchema("告警设置", "#/$defs/notification"),
			"confidence": Obj{
				"type": "number", "description": "整体置信度 0~1",
				"minimum": 0, "maximum": 1,
			},
			"warnings": arraySchema(
				"歧义、矛盾或风险提示, 每条一句中文说明", strSchema("提示"),
			),
		},
		"required":          []string{"schema_version", "kind", "task_name", "source", "target", "point", "scope"},
		"x-options-schemas": optionsSchemas,
	}
}

// PlanSchema 按意图返回对应 schema。
func PlanSchema(kind string, optionsSchemas Obj) Obj {
	if kind == KindRestore {
		return RestorePlanSchema(optionsSchemas)
	}
	return BackupPlanSchema(optionsSchemas)
}
