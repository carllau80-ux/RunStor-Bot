package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// UnmarshalStrict 反序列化并拒绝未知字段。
//
// 拒绝未知字段是有意为之: 模型多写一个字段名 (如把 time 写成 backup_time) 时,
// 静默忽略会让错误一路潜行到用户面前; 报错则能进入自纠错重试, 由模型自己修。
func UnmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// UseNumber 只影响 interface{} 目标 (即 SourceItem.Options 的值),
	// 让其保持 json.Number 而不是 float64 —— 端口、并发度这类整数不该经过浮点。
	// 目标是具体数值类型的字段 (如 Retention.Value int) 不受影响。
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return formatJSONError(err)
	}
	if decoder.More() {
		return errors.New("JSON 结尾存在多余内容")
	}
	return nil
}

// formatJSONError 把 Go 的结构化解析错误翻译成可读、可回填给模型的中文说明。
func formatJSONError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			return fmt.Errorf("字段类型不对: 期望 %s, 实际是 %s", typeErr.Type, describeJSONKind(typeErr.Value))
		}
		return fmt.Errorf("字段类型不对: 期望 %s, 实际是 %s", typeErr.Type, describeJSONKind(typeErr.Value))
	}

	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("JSON 语法错误 (位置 %d): %s", syntaxErr.Offset, syntaxErr.Error())
	}

	message := err.Error()
	if strings.HasPrefix(message, "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(message, "json: unknown field "), `"`)
		return fmt.Errorf("存在 Schema 中未定义的字段 %q, 请删除或改用正确字段名", field)
	}
	return err
}

func describeJSONKind(value string) string {
	switch value {
	case "number":
		return "数字"
	case "string":
		return "字符串"
	case "bool":
		return "布尔值"
	case "object":
		return "对象"
	case "array":
		return "数组"
	case "null":
		return "null"
	default:
		return value
	}
}

// JSONDecode 解析模型输出, 容忍常见的包装: 代码块围栏、前后夹带解释文字。
func JSONDecode(raw string) (map[string]any, error) {
	text := strings.TrimSpace(raw)

	// 剥掉 ```json ... ``` 围栏
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		for _, prefix := range []string{"json", "JSON"} {
			text = strings.TrimPrefix(text, prefix)
		}
		text = strings.TrimSpace(text)
		if idx := strings.LastIndex(text, "```"); idx != -1 {
			text = text[:idx]
		}
		text = strings.TrimSpace(text)
	}

	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err == nil {
		return out, nil
	}

	// 退化为截取首尾花括号 —— 模型常在 JSON 前后写解释
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, fmt.Errorf("输出不是合法的 JSON 对象")
	}
	decoder = json.NewDecoder(strings.NewReader(text[start : end+1]))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("输出不是合法的 JSON 对象: %v", err)
	}
	return out, nil
}
