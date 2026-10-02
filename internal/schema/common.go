// Package schema 定义备份/恢复计划的数据模型、JSON Schema 生成与校验。
//
// 与 Python 版的关键差异: Go 没有 Pydantic 那种"模型即 Schema 即校验器"的能力,
// 因此这里刻意把枚举表抽成单一事实源 —— 同一份枚举表同时用于:
//   - 生成喂给 LLM 的 JSON Schema (jsonschema.go)
//   - 校验 LLM 回填的值 (validate.go)
//
// 引擎专属参数 (options) 则由 driver 包的槽位表驱动, 见 driver.Schema。
package schema

const SchemaVersion = "1.0"

// 枚举取值表 —— 单一事实源, 不要在多处重复字面量。
var (
	Frequencies       = []string{"hourly", "daily", "weekly", "monthly"}
	RetentionUnits    = []string{"hours", "days", "weeks", "months", "years"}
	DestTypes         = []string{"local", "nfs", "s3", "aliyun_oss", "tencent_cos", "azure_blob", "tape", "other"}
	Compressions      = []string{"zstd", "gzip", "lz4", "none"}
	Encryptions       = []string{"AES-128", "AES-256", "SM4", "none"}
	SourceTypes       = []string{"database", "file", "application", "vm", "container", "mixed"}
	AuthModes         = []string{"os_auth", "password", "wallet", "iam", "token", "other"}
	VerifyFrequencies = []string{"daily", "weekly", "monthly", "quarterly"}
	NotifyChannels    = []string{"email", "sms", "webhook", "wechat"}
	PointTypes        = []string{"latest", "timestamp", "scn", "lsn", "backup_set_id"}
	RestoreModes      = []string{"in_place", "alternate_host"}
	ScopeLevels       = []string{"whole", "database", "tablespace", "table", "datafile", "path"}
	SlotKinds         = []string{"time", "int", "enum", "string", "bool", "duration", "list"}
)

// TimeLayout 是计划里所有时刻的统一格式。
const TimeLayout = "15:04"

// 计划类型
const (
	KindBackup  = "backup"
	KindRestore = "restore"
)

// 回合状态
const (
	StatusReady              = "ready"
	StatusNeedsClarification = "needs_clarification"
)

// ScheduleSpec 描述一次备份的调度。
type ScheduleSpec struct {
	Frequency  string `json:"frequency"`
	Time       string `json:"time,omitempty"`    // 24 小时制 HH:MM
	Weekday    *int   `json:"weekday,omitempty"` // 1=周一 ... 7=周日
	DayOfMonth *int   `json:"day_of_month,omitempty"`
	Timezone   string `json:"timezone"`
}

// Schedule 集合了全量/增量/差异三种调度。
type Schedule struct {
	Full         *ScheduleSpec `json:"full,omitempty"`
	Incremental  *ScheduleSpec `json:"incremental,omitempty"`
	Differential *ScheduleSpec `json:"differential,omitempty"`
}

// RetentionSpec 是一段保留期。
type RetentionSpec struct {
	Value int    `json:"value"`
	Unit  string `json:"unit"`
}

// Retention 按备份类型分别描述保留期。
type Retention struct {
	Full         *RetentionSpec `json:"full,omitempty"`
	Incremental  *RetentionSpec `json:"incremental,omitempty"`
	Differential *RetentionSpec `json:"differential,omitempty"`
	MinCopies    int            `json:"min_copies"`
}

// Destination 是一处备份落地位置。
type Destination struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Path     string `json:"path,omitempty"`
	// MediaServer 记录选中的介质服务器名称 (由清单接口提供时使用),
	// 让计划能对应回环境里真实存在的那台服务器。
	MediaServer string         `json:"media_server,omitempty"`
	Retention   *RetentionSpec `json:"retention,omitempty"`
	Encryption  string         `json:"encryption,omitempty"`
	Compression string         `json:"compression,omitempty"`
}

// SLA 是可接受的数据丢失量与恢复时间。
type SLA struct {
	RPO string `json:"rpo,omitempty"`
	RTO string `json:"rto,omitempty"`
}

// Verification 是恢复演练设置。
type Verification struct {
	TestRestore bool   `json:"test_restore"`
	Frequency   string `json:"frequency,omitempty"`
}

// Notification 是告警设置。
type Notification struct {
	Channels   []string `json:"channels"`
	Recipients []string `json:"recipients"`
}

// SourceItem 是一个被保护对象。
//
// Kind 刻意用 string 而不是枚举: 引擎由 driver 注册表动态注册, 用枚举会把
// "新增一个引擎"变成"必须改核心模型"。未知 Kind 落到 generic 驱动。
type SourceItem struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	Host          string         `json:"host,omitempty"`
	Port          *int           `json:"port,omitempty"`
	Databases     []string       `json:"databases,omitempty"`
	Paths         []string       `json:"paths,omitempty"`
	CredentialRef string         `json:"credential_ref,omitempty"` // 只存引用, 不落明文
	Options       map[string]any `json:"options"`
}

// BackupSource 是对象集合。
type BackupSource struct {
	Type  string       `json:"type"`
	Items []SourceItem `json:"items"`
}

// ParseTime 校验 HH:MM 时刻格式。
func ParseTime(value string) (hour, minute int, ok bool) {
	if len(value) != 5 || value[2] != ':' {
		return 0, 0, false
	}
	for _, idx := range []int{0, 1, 3, 4} {
		if value[idx] < '0' || value[idx] > '9' {
			return 0, 0, false
		}
	}
	hour = int(value[0]-'0')*10 + int(value[1]-'0')
	minute = int(value[3]-'0')*10 + int(value[4]-'0')
	if hour > 23 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

// FormatTime 把时分格式化为 HH:MM。
func FormatTime(hour, minute int) string {
	return string([]byte{
		byte('0' + hour/10), byte('0' + hour%10), ':',
		byte('0' + minute/10), byte('0' + minute%10),
	})
}
