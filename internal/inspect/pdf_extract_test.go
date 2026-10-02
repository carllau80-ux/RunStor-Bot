package inspect

import (
	"strings"
	"testing"

	"backup-restore-agent/internal/pdfdoc"
)

// 从生成的 PDF 里还原文本, 用于断言"报告里真的有这句话"。
//
// 复用 pdfdoc.ExtractText 而不是自己写一个: 提取必须**按字体分别建映射**, 自己实现的
// 版本曾经把所有字体的 ToUnicode 合成一张表 —— 同一个字形编号在 DejaVu 里是字母 t、
// 在 Droid 里是"《", 于是提取出来的文字张冠李戴, 让人误以为渲染坏了。
func reportTexts(t *testing.T, data []byte) []string {
	t.Helper()
	text, err := pdfdoc.ExtractText(data)
	if err != nil {
		t.Fatalf("提取 PDF 文本失败: %v", err)
	}
	return strings.Split(text, "\n")
}
