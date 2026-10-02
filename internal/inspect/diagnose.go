package inspect

import (
	"fmt"
	"sort"
	"strings"

	"backup-restore-agent/internal/i18n"
)

// 这一层回答"这条严重日志意味着什么、该怎么办"。
//
// 为什么以规则库为主而不是全部交给模型: 影响与处置建议是要被人照着做的, 写错比不写更糟。
// 常见故障 (OOM、磁盘 I/O 错误、ZFS 校验失败、iSCSI 断链、内核 panic…) 的签名是确定的,
// 用规则给出**可复现、可评审**的结论; 规则覆盖不到的再用模型补, 并且标明结论来自模型。
// 任何一条没被识别出来的日志都不编故事 —— 只写"需人工确认"并附原始行。

// Diagnosis 是一条日志的影响分析与处置建议。
type Diagnosis struct {
	// RuleID 是命中的规则 (或 model / unknown)。
	RuleID string `json:"rule_id"`
	Title  string `json:"title"`
	Impact string `json:"impact"`
	// Actions 是建议动作, 按先后顺序排列。
	Actions []string `json:"actions"`
	// Severity 取该组日志里最严重的那条的等级。
	Severity string `json:"severity"`
	// Count 是这组日志的条数 (去重后)。
	Count int `json:"count"`
	// Sources 是这组日志来自哪些日志源 (journald/dmesg/文件)。
	Sources []string `json:"sources,omitempty"`
	// Samples 是原始样本, 保留少量供核对 —— 结论要能追溯到证据。
	Samples []string `json:"samples,omitempty"`
	// Origin 说明结论来自哪里: rule / model / unknown。
	//
	// 报告里要显示出来: 规则结论与模型结论的可信度不同, 读者有权知道自己在看哪一种。
	Origin string `json:"origin"`
	// Note 是补充说明 (例如"未识别的日志已交给模型分析但模型未给出结论")。
	Note string `json:"note,omitempty"`
	// Knowledge 是知识库里与这个问题相关的章节 (来自导入的运维手册)。
	//
	// 放在这里而不是让读者自己去搜: 巡检给出"是什么问题", 手册给出"按你们自己的
	// 流程怎么做", 两者接上才是完整的处置闭环。
	Knowledge []KnowledgeRef `json:"knowledge,omitempty"`
}

// KnowledgeRef 是知识库里的一处引用 (由 kb 包实现)。
type KnowledgeRef struct {
	DocID   string  `json:"doc_id"`
	Doc     string  `json:"doc"`
	Heading string  `json:"heading,omitempty"`
	Snippet string  `json:"snippet,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// KnowledgeLookup 由知识库实现: 按一段文字找相关章节。
type KnowledgeLookup interface {
	LookupKnowledge(query string, limit int) []KnowledgeRef
}

// 结论来源。
const (
	OriginRule    = "rule"
	OriginModel   = "model"
	OriginUnknown = "unknown"
)

// remedyRule 是一条识别规则。
//
// 用关键字而不是正则: 日志行格式千奇百怪 (带时间戳、unit、pid、方括号), 关键字集合更
// 容易看出"这条规则到底认什么", 也更容易在评审时发现问题。
type remedyRule struct {
	ID      string
	Title   string
	Impact  string
	Actions []string
	// Severity 是这条规则认定的等级 (critical/err)。
	//
	// 通用关键字判定认不出所有严重模式 —— 例如 "checksum mismatch" 不含 error/failed 字样,
	// 会被判成"待确认", 于是整份报告的状态还是"正常", 而实际上数据可能已经坏了。
	// 规则表知道得更多, 所以由它来抬升等级。
	Severity string
	// All 里的关键字必须全部出现。
	All []string
	// Any 非空时, 其中任一出现即算命中。
	Any []string
	// None 里的关键字出现则排除本条 (用于区分相似故障)。
	None []string
}

// remedyRules 是规则库。**顺序有意义**: 排在前面的更具体, 先匹配先赢。
var remedyRules = []remedyRule{
	{
		ID:       "zfs_cksum",
		Severity: "critical",
		Title:    "ZFS 数据校验失败 (checksum mismatch)",
		Impact: "存储池读出的数据与校验和不符。若冗余副本能修好会自动修复, 修不好说明同一块数据" +
			"的所有副本都坏了 —— 这类故障是静默数据损坏, 比磁盘直接坏更危险: 文件可能已经读不出来," +
			"备份如果建立在同一池上也可能一起坏。",
		Actions: []string{
			"zpool status -v 看是哪个池、哪些文件受影响, 先把受影响的文件清单记下来",
			"确认最近的备份不在同一个池上, 并立即验证备份可读",
			"zpool scrub <pool> 让 ZFS 把能修的修掉, 修不掉的会列出来",
			"检查硬件: 内存 (ECC/内存检测)、数据线、HBA/背板、电源, 这类错误常源于链路而非盘体",
			"若伴随 vdev 错误数增长, 尽快更换对应磁盘",
		},
		Any: []string{"checksum mismatch", "zfs: checksum", "self-healed", "permanent errors"},
	},
	{
		ID:       "zfs_pool_fault",
		Severity: "critical",
		Title:    "ZFS 存储池降级或故障",
		Impact: "存储池冗余度下降或已失效。降级状态下还能读写, 但再坏一块盘就可能整池不可用;" +
			"FAULTED 则相关数据集已经读写失败, 跑在上面的服务会报 I/O 错误。",
		Actions: []string{
			"zpool status 确认池状态、缺哪块盘、是否可以 resilver",
			"换盘后确认 resilver 完成, 期间避免大压力操作",
			"检查是盘坏还是链路/背板问题 (同一槽位反复掉盘通常是链路)",
			"确认备份与快照策略覆盖当前数据",
		},
		Any: []string{"pool has encountered", "one or more devices", "state is degraded",
			"state is faulted", "zpool", "vdev", "resilver", "too many errors"},
	},
	{
		ID:       "oom",
		Severity: "critical",
		Title:    "内存耗尽, 进程被内核杀死 (OOM)",
		Impact: "OOM killer 直接杀掉了占用内存最多的进程。被杀的通常是数据库或应用主进程," +
			"表现为服务突然中断/重启; 未落盘的写入会丢, 正在执行的事务会被回滚。" +
			"若反复发生, 说明内存长期不足或有泄漏。",
		Actions: []string{
			"确认被杀的是哪个服务, 以及它当前是否已恢复 (systemctl status <unit>)",
			"journalctl -k | grep -i oom 找被杀进程与当时的内存水位",
			"查内存大户 (ps aux --sort=-%mem | head) 与 cgroup 用量 (systemd-cgtop)",
			"给关键服务设内存上限 (MemoryMax=) 并配置 OOMScoreAdjust, 避免它被优先杀掉",
			"短期加 swap 或扩容内存; 若是缓慢增长, 排查连接/缓存泄漏",
		},
		Any: []string{"out of memory", "oom-kill", "killed process", "oom_reaper"},
	},
	{
		ID:       "disk_io",
		Severity: "critical",
		Title:    "磁盘 I/O 错误",
		Impact: "磁盘读写失败。上层文件系统或数据库会报错, 严重时文件系统会转为只读挂载," +
			"业务写入全部失败; 也可能是磁盘即将完全失效的前兆。",
		Actions: []string{
			"smartctl -a /dev/sdX 看 SMART (重映射扇区、待定扇区、CRC 错误计数)",
			"检查线缆、背板、HBA 与电源 —— CRC 类错误多是链路问题",
			"确认数据可读、备份可用, 准备替换磁盘",
			"若文件系统已只读, 先确保业务停止写入, 再考虑 fsck",
		},
		Any: []string{"i/o error", "medium error", "blk_update_request", "failed command",
			"critical medium error", "read error", "write error", "sense key", "ata bus error",
			"nvme", "unrecovered read error"},
		None: []string{"zfs"},
	},
	{
		ID:       "fs_error",
		Severity: "critical",
		Title:    "文件系统检测到不一致",
		Impact: "内核已发现文件系统元数据不一致。常见后果是自动转为只读挂载 (Remounting " +
			"filesystem read-only), 此时一切写操作都失败; 继续使用可能扩大损坏范围。",
		Actions: []string{
			"立即确认挂载状态 (mount | grep <dev>) 与业务是否还在写入",
			"备份当前能读到的数据, 并记录报错涉及的 inode/块",
			"停业务 → umount → fsck -y (XFS 用 xfs_repair), 修复前不要再写入",
			"修复后从备份校验关键数据, 并检查磁盘健康",
		},
		Any: []string{"ext4-fs error", "xfs (", "filesystem error", "remounting filesystem read-only",
			"ext4-fs (", "jbd2", "metadata i/o error"},
	},
	{
		ID:       "kernel_panic",
		Severity: "critical",
		Title:    "内核 panic / oops",
		Impact: "内核级故障。panic 会导致整机宕机或重启 (业务全部中断); oops 可能只影响单个" +
			"子系统, 但常常伴随功能异常, 且可能反复出现。",
		Actions: []string{
			"收集完整日志 (journalctl -k -b -1 看上一次启动的尾部) 与串口/netconsole 输出",
			"回顾最近变更: 内核升级、驱动、内核模块、硬件插拔",
			"怀疑驱动时回退内核版本或改用发行版内核",
			"配置 kdump 以便下次抓到 vmcore; 反复发生时按硬件故障排查 (内存、主板)",
		},
		Any: []string{"kernel panic", "kernel oops", "bug: unable to handle", "general protection fault",
			"invalid opcode", "stack segment", "rcu_sched detected stalls", "hard lockup",
			"soft lockup"},
	},
	{
		ID:       "hardware",
		Severity: "critical",
		Title:    "硬件报错 (CPU/内存/总线)",
		Impact: "机器检查异常 (Machine Check) 说明 CPU、内存或总线报错。已纠正的错误" +
			"(corrected) 通常只影响性能, 未纠正的 (uncorrected) 可能造成静默数据错误或宕机。",
		Actions: []string{
			"记录报错地址、Bank 与错误类型 (mcelog / rasdaemon 的输出)",
			"运行内存检测 (memtest86+), 单条内存交叉验证",
			"检查固件/BIOS 版本与厂商已知问题",
			"未纠正错误反复出现时, 安排停机检修该硬件",
		},
		Any: []string{"hardware error", "machine check", "mce:", "corrected error",
			"uncorrected error", "cpu: 0 pid", "thermal event"},
	},
	{
		ID:       "iscsi_auth",
		Severity: "err",
		Title:    "iSCSI 存储链路认证/授权失败",
		Impact: "目标端拒绝了发起端的登录。存储卷挂不上或中途掉线,依赖它的文件系统/数据库" +
			"会挂起或报 I/O 错误; 若是新配的链路, 说明 ACL/CHAP 配置不一致。",
		Actions: []string{
			"核对 target 侧的 ACL: 发起端 IQN 是否在白名单里 (targetcli ls 或存储设备管理界面)",
			"核对 CHAP 用户名/密码两侧是否一致, 是否要求双向认证",
			"确认网络可达与端口 (3260), 以及是否被防火墙拦截",
			"反复重试会刷满日志, 先修配置再恢复会话 (iscsiadm -m session -R)",
		},
		Any: []string{"iscsi", "login negotiation failed", "security negotiation failed",
			"target portal group", "not authorized to access", "chap",
			"has not been successfully authenticated"},
	},
	{
		ID:       "scsi_quirk",
		Severity: "err",
		Title:    "SCSI 设备不支持被查询的能力 (兼容性提示)",
		Impact: "内核在探测存储设备能力时得到了\"不支持这个可选页\"的回应。这本身不代表读写失败, " +
			"常见于 iSCSI 目标、磁带机、虚拟/仿真设备。但同一设备若同时出现 I/O 错误, " +
			"就应按设备兼容性/固件问题处理, 而不是当噪音。",
		Actions: []string{
			"确认这条是哪个设备报的 (lsscsi、sg_inq /dev/sdX 看厂商与型号)",
			"检查同一设备是否同时有 I/O 错误或性能下降 (dmesg | grep -i \"i/o error\")",
			"只有伴随 I/O 错误时才需要处理: 更新存储端/设备固件, 或联系厂商确认兼容性",
			"若确认无影响, 可在日志过滤里屏蔽这类行, 免得刷满日志掩盖真问题",
		},
		Any: []string{"mode sense: unimplemented", "unimplemented page/subpage",
			"invalid command operation code", "sense key: illegal request"},
	},
	{
		ID:       "network_link",
		Severity: "err",
		Title:    "网络链路中断或超时",
		Impact: "网卡/链路断开或对端无响应。依赖网络的存储与集群心跳会中断, 可能触发" +
			"服务切换或进程进入不可中断等待 (D 状态, 表现为命令卡住)。",
		Actions: []string{
			"ip -s link 看错误计数与协商速率, 检查网线/光模块/交换机端口",
			"确认对端设备与中间链路状态, 排查是否有环路或丢包",
			"存储多路径场景检查 multipath -ll 路径状态与超时参数",
			"NFS 场景考虑加 soft,timeo= 避免进程永久卡住",
		},
		Any: []string{"link is down", "link is not ready", "nic link", "connection timed out",
			"nfs: server", "stale file handle", "multipath", "path failed", "no route to host",
			"cifs"},
	},
	{
		ID:       "auth_failure",
		Severity: "err",
		Title:    "认证失败 (登录被拒)",
		Impact: "账号认证不通过。若是域账号 (winbind/sssd), 说明与域控的通道出了问题 —— " +
			"域用户登录、Samba 共享、依赖域的定时任务都会失败; 也可能是外部在暴力破解, " +
			"同样会把日志刷满并带来风险。",
		Actions: []string{
			"先区分两类: 内部服务认证失败 vs 外部来源的爆破尝试 (看来源 IP 是否可信)",
			"域场景检查 net ads testjoin / wbinfo -t, 确认域控可达、时间同步 (Kerberos 对时间敏感)、机器账号密码未失效",
			"爆破类来源用防火墙/fail2ban 封禁, 并禁止 root 密码登录",
			"检查是否有服务还在用过期密码 (systemctl status 对应 unit)",
		},
		Any: []string{"authentication failure", "failed password", "invalid user",
			"pam_unix", "pam_winbind", "wbclogonuser", "wbc_err", "ntstatus", "auth error",
			"preauth", "krb5", "gssapi"},
	},
	{
		ID:       "service_failed",
		Severity: "err",
		Title:    "系统服务启动或运行失败",
		Impact: "该 unit 未处于运行状态, 它提供的功能当前不可用 (可能表现为端口不通、" +
			"定时任务不执行、挂载缺失)。",
		Actions: []string{
			"systemctl status <unit> --no-pager -l 看退出码与最近日志",
			"journalctl -u <unit> -n 100 --no-pager 找真正的原因 (常在报错行的上文)",
			"检查配置语法、依赖服务、端口占用与权限",
			"修好后 systemctl reset-failed <unit> && systemctl restart <unit> 并确认自启 (is-enabled)",
		},
		Any: []string{"failed to start", "entered failed state", "failed with result",
			"unit has failed", "start request repeated too quickly", "dependency failed"},
		None: []string{"iscsi"},
	},
	{
		ID:       "crash",
		Severity: "critical",
		Title:    "进程异常退出 (段错误/崩溃)",
		Impact: "进程收到非法内存访问或主动中止而退出。服务中断; 反复崩溃说明存在稳定复现的" +
			"缺陷或内存/二进制损坏。",
		Actions: []string{
			"确认进程与所属服务 (coredumpctl list)",
			"取栈回溯 (coredumpctl info) 定位崩溃点, 附版本号提工单",
			"怀疑内存问题时对该机做内存检测; 检查是否最近有升级/换库",
			"给服务加自动重启 (Restart=on-failure) 作为临时缓解",
		},
		Any: []string{"segfault", "core dumped", "signal 11", "signal 6", "aborted",
			"double free", "stack smashing"},
	},
	{
		ID:       "no_space",
		Severity: "critical",
		Title:    "空间不足",
		Impact: "写入失败。数据库可能拒绝新事务, 日志无法落盘, 严重时服务直接退出;" +
			"根分区写满还会连带影响登录与临时文件创建。",
		Actions: []string{
			"df -h 定位是哪个分区, du -xh --max-depth=1 / | sort -h 找占用大户",
			"优先清理日志/临时文件, 确认日志轮转 (logrotate) 是否生效",
			"检查是否有被删除但仍被进程占用的文件 (lsof +L1)",
			"长期方案: 扩容或迁移数据, 并对该分区加容量告警",
		},
		Any: []string{"no space left on device", "disk quota exceeded", "cannot allocate",
			"write failed: no space"},
	},
	{
		ID:       "database",
		Severity: "err",
		Title:    "数据库错误",
		Impact: "数据库报出错误。可能是个别语句失败 (业务报错但服务还在), 也可能是" +
			"崩溃恢复、表损坏、复制中断 —— 后者会影响数据一致性与可用性。",
		Actions: []string{
			"确认数据库进程是否还在 (systemctl status), 以及是否进入了恢复模式",
			"看数据库自己的错误日志 (MySQL: error.log; PostgreSQL: 日志目录) 找完整上下文",
			"若涉及表/索引损坏, 先备份数据文件再按官方文档做修复",
			"复制/主从场景确认延迟与中断原因, 再决定是否重建从库",
		},
		Any: []string{"mysqld", "mariadb", "innodb", "postgres", "postgresql", "database system",
			"table is marked as crashed", "replication", "db_root"},
	},
	{
		ID:       "thermal",
		Severity: "err",
		Title:    "温度或散热异常",
		Impact: "温度超过阈值。CPU 会降频导致性能骤降, 更高时触发保护性关机, " +
			"长期高温会加速硬件老化。",
		Actions: []string{
			"sensors 看具体部位温度与风扇转速",
			"检查风扇是否停转/积灰、机箱风道与机房温度",
			"必要时先降负载, 再安排停机清灰或更换风扇",
		},
		Any: []string{"over temperature", "critical temperature", "thermal", "fan failure",
			"cpu throttl", "temperature above threshold"},
	},
}

// DiagnoseLogs 把日志条目归成若干"问题", 每个问题给出影响与处置建议。
//
// 只在有严重日志时才需要调用 (没有严重日志时报告里不必塞一堆"无")。
func DiagnoseLogs(entries []LogEntry) []Diagnosis {
	grouped := map[string]*Diagnosis{}
	order := []string{}

	for _, entry := range entries {
		rule := matchRule(entry.Text)
		key := "unknown"
		diagnosis := &Diagnosis{
			RuleID:   OriginUnknown,
			Origin:   OriginUnknown,
			Severity: entry.Severity,
			Title:    "未识别的严重日志",
			Impact:   "这条日志没有匹配到已知故障特征, 无法凭它判断影响 —— 需要结合完整上下文人工确认, 这里不猜。",
			Actions: []string{
				"按时间点查完整上下文: journalctl --since \"<时间>\" -n 200 --no-pager (或对应日志文件)",
				"确认该时段是否有服务重启、配置变更或硬件告警",
				"若能确认影响, 把特征补进规则库 (internal/inspect/diagnose.go), 下次就能自动识别",
			},
			Count:   0,
			Sources: nil,
			Samples: nil,
		}
		if rule != nil {
			key = rule.ID
			diagnosis.RuleID = rule.ID
			diagnosis.Origin = OriginRule
			diagnosis.Title = rule.Title
			diagnosis.Impact = rule.Impact
			diagnosis.Actions = rule.Actions
		}

		existing, ok := grouped[key]
		if !ok {
			grouped[key] = diagnosis
			order = append(order, key)
			existing = diagnosis
		}
		existing.Count++
		existing.Severity = worseSeverityLabel(existing.Severity, entry.Severity)
		if entry.Source != "" && !containsString(existing.Sources, entry.Source) {
			existing.Sources = append(existing.Sources, entry.Source)
		}
		if len(existing.Samples) < 2 && !containsString(existing.Samples, entry.Text) {
			existing.Samples = append(existing.Samples, truncateText(entry.Text, 200))
		}
	}

	out := make([]Diagnosis, 0, len(grouped))
	for _, key := range order {
		out = append(out, *grouped[key])
	}
	sortDiagnoses(out)
	return out
}

// sortDiagnoses 让最该先看的排前面: 严重等级高的在前, 同级按条数多的在前,
// 未识别的放最后 (因为它的行动价值最低)。
func sortDiagnoses(items []Diagnosis) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if left.Severity != right.Severity {
			return severityRank(left.Severity) > severityRank(right.Severity)
		}
		if (left.Origin == OriginUnknown) != (right.Origin == OriginUnknown) {
			return right.Origin == OriginUnknown
		}
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		return left.Title < right.Title
	})
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 3
	case "err":
		return 2
	default:
		return 1
	}
}

func worseSeverityLabel(left, right string) string {
	if severityRank(right) > severityRank(left) {
		return right
	}
	return left
}

// matchRule 返回命中的规则 (没有命中返回 nil)。
func matchRule(line string) *remedyRule {
	lowered := strings.ToLower(line)
	for index := range remedyRules {
		rule := &remedyRules[index]
		// 三个条件的"空列表"含义各不相同, 所以显式判断长度而不是让辅助函数替我们决定:
		// Any/All 为空表示不限制, None 为空表示不排除。曾经共用一个"空列表返回 true 的
		// matchesAny", 结果 None 为空时被当成"命中排除", 把 15 条规则里的 12 条全静默
		// 关掉了 —— 表现是所有日志都归为"未识别"。
		if len(rule.Any) > 0 && !containsAnyKeyword(lowered, rule.Any) {
			continue
		}
		if len(rule.All) > 0 && !containsAllKeywords(lowered, rule.All) {
			continue
		}
		if len(rule.None) > 0 && containsAnyKeyword(lowered, rule.None) {
			continue
		}
		return rule
	}
	return nil
}

func containsAnyKeyword(lowered string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

func containsAllKeywords(lowered string, keywords []string) bool {
	for _, keyword := range keywords {
		if !strings.Contains(lowered, keyword) {
			return false
		}
	}
	return true
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// LogAnalyzer 用模型补充**规则没识别出来**的日志结论。
//
// 契约: 输入是未识别的日志组 (已带样本、条数与来源), 输出必须与输入**等长同序** ——
// 只填 Title/Impact/Actions/Severity 四项, 其余原样带回。这样模型不可能"漏掉几条"
// 让报告看起来完整: 少一条都能被发现, 发现后整批丢弃、保留原始的"未识别"结论。
type LogAnalyzer interface {
	// Available 报告当前是否真的会调用模型 (没配模型 / demo 模式时为 false)。
	Available() bool
	// AnalyzeUnknown 为规则未覆盖的日志组补结论; lang 决定提示词与产出语言。
	AnalyzeUnknown(lang i18n.Lang, groups []Diagnosis) ([]Diagnosis, error)
}

// MergeModelDiagnoses 把模型给出的分析并入规则结论。
//
// 规则结论永远保留 (可复现、可评审); 模型只接管规则认不出来的那些日志。
func MergeModelDiagnoses(ruled []Diagnosis, modeled []Diagnosis) []Diagnosis {
	var (
		out          []Diagnosis
		unrecognized []Diagnosis
	)
	for _, item := range ruled {
		if item.Origin == OriginUnknown {
			unrecognized = append(unrecognized, item)
			continue
		}
		out = append(out, item)
	}

	// 只有"每个未识别组都拿到了结论"才采用模型的输出, 否则宁可保留未识别 ——
	// 半套结论比没有结论更危险: 读者会以为报告已经把问题都分析过了。
	usable := len(modeled) == len(unrecognized) && len(modeled) > 0
	if usable {
		for index, item := range modeled {
			enriched := unrecognized[index] // 保留样本、条数、来源
			if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Impact) == "" ||
				len(item.Actions) == 0 {
				usable = false
				break
			}
			enriched.Title = item.Title
			enriched.Impact = item.Impact
			enriched.Actions = item.Actions
			enriched.Origin = OriginModel
			enriched.RuleID = "model"
			enriched.Severity = worseSeverityLabel(enriched.Severity, item.Severity)
			out = append(out, enriched)
		}
	}
	if !usable {
		note := "需人工确认"
		if len(modeled) > 0 {
			note = "模型给出的结论不完整, 需人工确认"
		}
		for _, item := range unrecognized {
			item.Note = note
			out = append(out, item)
		}
	}
	sortDiagnoses(out)
	return out
}

// DiagnosisSummaryLine 生成一句话概括 (给报告结论条与列表用)。
func DiagnosisSummaryLine(items []Diagnosis) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) == 1 {
		return items[0].Title
	}
	return fmt.Sprintf("%s 等 %d 类问题", items[0].Title, len(items))
}
