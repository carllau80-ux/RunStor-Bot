package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"backup-restore-agent/internal/inspect"
)

// handleInspectionPDF 把一条巡检记录导出成 PDF 报告。
//
// 报告是"按需生成"而不是巡检时就存下来: 生成是纯函数 (同一记录永远同一份内容),
// 没必要在磁盘上多存一份会过期的副本, 也省掉缓存失效的逻辑。
func (s *Server) handleInspectionPDF(w http.ResponseWriter, r *http.Request) {
	if s.inspector == nil {
		writeError(w, http.StatusNotFound, "not_found", "巡检服务未启用", "", "")
		return
	}
	id := r.PathValue("id")
	report, ok := s.inspector.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "巡检记录不存在: "+id, "", "")
		return
	}

	// 导出的语言跟着请求走: 同一条记录可以用中文或英文导出
	data, err := inspect.RenderPDF(report, inspect.PDFOptions{
		FontPath: s.pdfFontPath,
		Lang:     s.langOf(r),
	})
	if err != nil {
		// 字体缺失之类的问题在这里暴露: 明确告诉用户原因, 而不是给一个打不开的文件
		writeError(w, http.StatusInternalServerError, "pdf_failed",
			s.tf(r, "生成 PDF 失败: {0}", err.Error()), "", "")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", contentDisposition(pdfFileName(report)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		s.logger.Printf("写出 PDF 失败 (%s): %v", id, err)
	}
}

// pdfFileName 给下载的文件起个能认出来的名字。
func pdfFileName(report inspect.Report) string {
	name := report.HostName
	if strings.TrimSpace(name) == "" {
		name = report.Host
	}
	name = sanitizeFileName(name)
	stamp := report.StartedAt
	if stamp.IsZero() {
		stamp = time.Now()
	}
	return fmt.Sprintf("巡检报告-%s-%s.pdf", name, stamp.Format("20060102-1504"))
}

// sanitizeFileName 去掉文件名里不该出现的字符 (主机名来自用户输入)。
func sanitizeFileName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			builder.WriteRune(r)
		case r > 0x7F:
			// 中文主机名保留 (文件名里合法), 只是下面会做百分号编码
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	out := strings.Trim(builder.String(), "._-")
	if out == "" {
		return "report"
	}
	if len([]rune(out)) > 40 {
		out = string([]rune(out)[:40])
	}
	return out
}

// contentDisposition 生成下载头。
//
// 中文文件名必须走 RFC 5987 的 filename*, 同时给一个纯 ASCII 的 filename 兜底 ——
// 只写 filename= 的话, 老浏览器/命令行工具会拿到乱码文件名。
func contentDisposition(fileName string) string {
	ascii := make([]rune, 0, len(fileName))
	for _, r := range fileName {
		if r < 0x80 {
			ascii = append(ascii, r)
		}
	}
	fallback := strings.Trim(string(ascii), "._-")
	if fallback == "" || !strings.Contains(fallback, ".pdf") {
		fallback = "inspection-report.pdf"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		fallback, percentEncode(fileName))
}

// percentEncode 按 RFC 5987 编码 (不保留 = + 等字符的原始形式)。
func percentEncode(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var builder strings.Builder
	for _, b := range []byte(value) {
		if strings.IndexByte(unreserved, b) >= 0 {
			builder.WriteByte(b)
			continue
		}
		fmt.Fprintf(&builder, "%%%02X", b)
	}
	return builder.String()
}
