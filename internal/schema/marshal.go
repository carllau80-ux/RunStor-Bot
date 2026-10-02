package schema

import (
	"bytes"
	"encoding/json"
)

// MarshalIndentNoEscape 序列化为带缩进的 JSON, 且不做 HTML 转义。
//
// 标准库默认会把 < > & 转成 \u003c \u003e \u0026, 这在接口响应里无所谓, 但
// schema 是要原样拼进提示词的 —— 让模型读到 "-\u003e" 而不是 "->" 纯属噪音,
// 也白白多耗 token。
func MarshalIndentNoEscape(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
