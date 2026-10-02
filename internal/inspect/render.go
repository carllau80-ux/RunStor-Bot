package inspect

import "backup-restore-agent/internal/i18n"

// renderer 负责把报告里的文案按语言输出。
//
// 报告是"给人看的产物", 所以文案必须在**生成时**就定语言: 结论行里带数字与单位
// (发现 130 条错误日志、可用 41.2%), 事后逐词替换翻不出正确语序, 只能整句产出。
type renderer struct{ lang i18n.Lang }

func (r renderer) t(text string) string { return i18n.T(r.lang, text) }

func (r renderer) tf(format string, args ...any) string {
	return i18n.Tf(r.lang, format, args...)
}
