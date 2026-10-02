package driver

import (
	"encoding/json"
	"strings"
	"testing"

	"backup-restore-agent/internal/schema"
)

func TestRegistryCoverage(t *testing.T) {
	kinds := Kinds()
	want := []string{"file", "mysql", "oracle", "other", "postgresql"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("注册的 kind 不符: 得到 %v, 期望 %v", kinds, want)
	}
	if len(All()) != len(want) {
		t.Fatalf("All() 数量不符: %d", len(All()))
	}
}

func TestGenericFallbackForUnknownKind(t *testing.T) {
	if _, ok := Find("db2"); ok {
		t.Fatal("db2 不应被注册")
	}
	if got := Get("db2").Kind; got != "other" {
		t.Fatalf("未知 kind 应兜底到 other, 得到 %q", got)
	}
}

func TestResolveByAlias(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"周天全备份oracle数据库", "oracle"},
		{"整个实例全备 rman", "oracle"},
		{"每天增量备份 MySQL", "mysql"},
		{"mariadb 备份", "mysql"},
		{"pg 整集群物理备份", "postgresql"},
		{"postgresql 单库逻辑备份", "postgresql"},
		{"备份文件服务器上的 /data", "file"},
		{"备份某台服务器上的目录", "file"},
		{"备份达梦数据库", ""},
	}
	for _, entry := range cases {
		got := ""
		if d := Resolve(entry.text); d != nil {
			got = d.Kind
		}
		if got != entry.want {
			t.Errorf("Resolve(%q) = %q, 期望 %q", entry.text, got, entry.want)
		}
	}
}

// pg 这类两字母别名最容易误伤, 单独固化边界行为。
func TestResolveASCIINeedsWordBoundary(t *testing.T) {
	for _, text := range []string{"page 目录备份", "gpg 密钥备份"} {
		if d := Resolve(text); d != nil && d.Kind == "postgresql" {
			t.Errorf("Resolve(%q) 误判为 postgresql", text)
		}
	}
	if d := Resolve("PG 数据库备份"); d == nil || d.Kind != "postgresql" {
		t.Errorf("大写 PG 应识别为 postgresql")
	}
}

// 长别名优先: postgresql 不能被更短的 pg 抢走, 文件服务器不能被文件抢走。
func TestResolvePrefersLongerAlias(t *testing.T) {
	if d := Resolve("postgresql 备份"); d == nil || d.Kind != "postgresql" {
		t.Errorf("postgresql 未识别")
	}
	if d := Resolve("文件服务器备份"); d == nil || d.Kind != "file" {
		t.Errorf("文件服务器未识别")
	}
	if d := Resolve("nas 备份"); d == nil || d.Kind != "file" {
		t.Errorf("nas 未识别为 file")
	}
}

func TestOptionsJSONSchemaShape(t *testing.T) {
	oracle, _ := Find("oracle")
	jsonSchema := oracle.OptionsJSONSchema()

	if jsonSchema["type"] != "object" {
		t.Fatalf("schema type 应为 object")
	}
	properties, ok := jsonSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema 缺少 properties")
	}

	// 每个槽位都必须出现在 properties 里 —— 否则模型不知道这个键存在
	for _, slot := range oracle.Slots {
		if _, exists := properties[slot.Field]; !exists {
			t.Errorf("槽位 %s 未出现在 options schema 中", slot.Field)
		}
	}

	// 必填项必须只包含非 Optional 的槽位
	required, _ := jsonSchema["required"].([]string)
	wantRequired := map[string]bool{"auth_mode": true, "channels": true, "scope": true}
	if len(required) != len(wantRequired) {
		t.Fatalf("required 数量不符: %v", required)
	}
	for _, field := range required {
		if !wantRequired[field] {
			t.Errorf("%s 不该是必填", field)
		}
	}
}

// 枚举的"中文标签 -> 代码值"映射必须写进 description, 否则模型无从完成映射。
func TestEnumSchemaDescribesCodeMapping(t *testing.T) {
	oracle, _ := Find("oracle")
	properties := oracle.OptionsJSONSchema()["properties"].(map[string]any)
	authSchema := properties["auth_mode"].(map[string]any)

	description, _ := authSchema["description"].(string)
	if !strings.Contains(description, "系统认证") || !strings.Contains(description, "os_auth") {
		t.Fatalf("auth_mode 描述缺少标签到代码的映射: %q", description)
	}
	enum, _ := authSchema["enum"].([]string)
	if len(enum) != 3 {
		t.Fatalf("auth_mode 应有 3 个候选代码值, 得到 %v", enum)
	}
	for _, code := range enum {
		if code == "系统认证" {
			t.Error("enum 里应该是代码值而不是中文标签")
		}
	}
}

func TestOptionsSchemasCoversAllDriversWithSlots(t *testing.T) {
	schemas := OptionsSchemas()
	for _, kind := range Kinds() {
		if kind == "other" {
			if _, exists := schemas[kind]; exists {
				t.Error("无槽位的驱动不该出现在 options schemas 中")
			}
			continue
		}
		if _, exists := schemas[kind]; !exists {
			t.Errorf("驱动 %s 缺少 options schema", kind)
		}
	}
}

func TestValidateOptionsAcceptsCompleteOracle(t *testing.T) {
	oracle, _ := Find("oracle")
	report := oracle.ValidateOptions(map[string]any{
		"scope":     "whole_instance",
		"auth_mode": "os_auth",
		"channels":  4,
	}, "source.items[0].options.")

	if errors := report.Errors(); len(errors) != 0 {
		t.Fatalf("完整参数不该报错: %v", report.Messages(schema.LevelError))
	}
}

func TestValidateOptionsReportsMissingRequired(t *testing.T) {
	oracle, _ := Find("oracle")
	report := oracle.ValidateOptions(map[string]any{"scope": "whole_instance"},
		"source.items[0].options.")

	messages := strings.Join(report.Messages(schema.LevelError), "\n")
	for _, field := range []string{"auth_mode", "channels"} {
		if !strings.Contains(messages, field) {
			t.Errorf("应报出缺少 %s, 实际: %s", field, messages)
		}
	}
	// 报错路径必须精确到字段, 才能回填给模型自纠错
	if !strings.Contains(messages, "source.items[0].options.channels") {
		t.Errorf("报错缺少完整 JSON 路径: %s", messages)
	}
}

func TestValidateOptionsRejectsOutOfRange(t *testing.T) {
	oracle, _ := Find("oracle")
	cases := []struct {
		name    string
		options map[string]any
		want    string
	}{
		{"并发度超上限", map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 64}, "不能大于 32"},
		{"并发度为零", map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": 0}, "不能小于 1"},
		{"枚举取值非法", map[string]any{"scope": "everything", "auth_mode": "os_auth", "channels": 2}, "取值必须是"},
		{"类型不符", map[string]any{"scope": "whole_instance", "auth_mode": "os_auth", "channels": "四个"}, "需要整数"},
	}
	for _, entry := range cases {
		report := oracle.ValidateOptions(entry.options, "options.")
		messages := strings.Join(report.Messages(schema.LevelError), "\n")
		if !strings.Contains(messages, entry.want) {
			t.Errorf("%s: 期望报出 %q, 实际: %s", entry.name, entry.want, messages)
		}
	}
}

// 模型偶尔会直接回填用户原话, 能认就认 —— 不该因为用了中文标签而报错。
func TestValidateOptionsAcceptsChineseLabel(t *testing.T) {
	oracle, _ := Find("oracle")
	report := oracle.ValidateOptions(map[string]any{
		"scope":     "整个实例",
		"auth_mode": "系统认证",
		"channels":  4,
	}, "options.")
	if errors := report.Errors(); len(errors) != 0 {
		t.Fatalf("中文标签应被接受: %v", report.Messages(schema.LevelError))
	}
}

func TestValidateOptionsWarnsUnknownKeys(t *testing.T) {
	oracle, _ := Find("oracle")
	report := oracle.ValidateOptions(map[string]any{
		"scope": "whole_instance", "auth_mode": "os_auth", "channels": 2,
		"compress": "zstd",
	}, "options.")
	warnings := strings.Join(report.Messages(schema.LevelWarning), "\n")
	if !strings.Contains(warnings, "compress") {
		t.Fatalf("应提示未知参数 compress, 实际: %s", warnings)
	}
}

// 条件必填: 物理备份时 databases 无意义, 不该报缺失。
func TestConditionalSlotSatisfiedWhen(t *testing.T) {
	pg, _ := Find("postgresql")

	physical := pg.ValidateOptions(map[string]any{"method": "pg_basebackup"}, "options.")
	if messages := strings.Join(physical.Messages(schema.LevelError), "\n"); strings.Contains(messages, "databases") {
		t.Errorf("物理备份不该要求 databases: %s", messages)
	}

	logical := pg.ValidateOptions(map[string]any{"method": "pg_dump"}, "options.")
	if messages := strings.Join(logical.Messages(schema.LevelError), "\n"); !strings.Contains(messages, "databases") {
		t.Errorf("逻辑备份应要求 databases: %s", messages)
	}
}

// 数值可能以 json.Number / float64 / int 三种形态到达, 都必须认。
func TestValidateOptionsNumberRepresentations(t *testing.T) {
	oracle, _ := Find("oracle")
	for _, value := range []any{4, int64(4), float64(4), json.Number("4")} {
		report := oracle.ValidateOptions(map[string]any{
			"scope": "whole_instance", "auth_mode": "os_auth", "channels": value,
		}, "options.")
		if errors := report.Errors(); len(errors) != 0 {
			t.Errorf("取值 %#v 应被接受: %v", value, report.Messages(schema.LevelError))
		}
	}
}
