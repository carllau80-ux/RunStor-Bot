package inspect

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/pdfdoc"
)

// 这一层把巡检报告画成 PDF。
//
// 报告的每个结论都带原始证据, 所以 PDF 里既要好看也要能追溯: 结论用颜色区分严重程度,
// 证据原样附在下面 (截断后), 并且**保持文字可复制** —— 运维常要把日志片段粘到工单里,
// 一份只能看不能选的文件是半个工具。

// Label 返回状态的中文标签。
//
// 界面里另有一份 JS 写法相同的标签 (那份管网页渲染); 报告要在服务端生成, 所以这里
// 也要有一份 —— 中文词汇不该只存在于前端。
func (s Status) Label() string {
	switch s {
	case StatusCritical:
		return "严重"
	case StatusWarn:
		return "告警"
	case StatusUnknown:
		return "无法判定"
	default:
		return "正常"
	}
}

// statusColor 返回状态对应的颜色。
func statusColor(status Status) pdfdoc.Color {
	switch status {
	case StatusCritical:
		return pdfdoc.ColorCritic
	case StatusWarn:
		return pdfdoc.ColorWarn
	case StatusUnknown:
		return pdfdoc.ColorUnknown
	default:
		return pdfdoc.ColorOk
	}
}

// PDFOptions 是生成 PDF 的可选设置。
type PDFOptions struct {
	// FontPath 指定替换内置拉丁字体的 TrueType 字体 (空则用内置字体)。
	FontPath string
	// Now 是报告里的生成时间 (零值取当前时间)。
	Now time.Time
	// Lang 是报告语言; 报告正文按生成时的语言存下来, 但版面文案 (标题、标签、
	// 页脚) 与结构化结论在**导出时**按当前语言呈现, 所以切语言后导出的 PDF 是新的语言。
	Lang i18n.Lang
}

// 字体集合只加载一次: 解析两万多个字形只为回答"这个字用哪份字体", 每个请求做一遍
// 是浪费。字体本身不可变, 共享安全; 子集化结果在字体内部另有缓存。
var (
	fontSetOnce   sync.Once
	fontSetCached *pdfdoc.Set
	fontSetErr    error
)

// sharedFontSet 返回内置字体集合。
func sharedFontSet() (*pdfdoc.Set, error) {
	fontSetOnce.Do(func() {
		fontSetCached, fontSetErr = pdfdoc.DefaultSet()
	})
	return fontSetCached, fontSetErr
}

// fontSetFor 按配置取字体集合: 指定了自定义字体就用它 + 内置中日韩字体。
//
// 自定义字体加载失败时**退回内置字体并继续出报告**, 而不是让导出失败 —— 用户改配置
// 打错路径不该导致拿不到报告; 失败原因由调用方记日志 (见 LastFontWarning)。
func fontSetFor(path string) (*pdfdoc.Set, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return sharedFontSet()
	}
	latin, err := pdfdoc.LoadFont(path)
	if err != nil {
		recordFontWarning(fmt.Sprintf("自定义字体 %s 不可用 (%v), 已改用内置字体", path, err))
		return sharedFontSet()
	}
	cjk, err := pdfdoc.LoadAsset(pdfdoc.CJKFontAsset)
	if err != nil {
		return nil, err
	}
	return pdfdoc.NewSet(latin, cjk), nil
}

var (
	fontWarningMu      sync.Mutex
	fontWarningMessage string
)

func recordFontWarning(message string) {
	fontWarningMu.Lock()
	defer fontWarningMu.Unlock()
	fontWarningMessage = message
}

// ValidatePDFFont 在启动时检查自定义字体, 返回可读的问题说明 (空表示没问题)。
//
// 提前检查而不是等导出时才发现: 配置里写错字体路径的话, 用户要等到点击"导出 PDF"
// 才知道; 在启动日志里说清楚, 现场就能改。检查结果同时记入降级说明供健康检查展示。
func ValidatePDFFont(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if _, err := pdfdoc.LoadFont(path); err != nil {
		message := fmt.Sprintf("巡检 PDF 自定义字体 %s 不可用 (%v), 将改用内置字体", path, err)
		recordFontWarning(message)
		return message
	}
	return ""
}

// FontWarning 返回最近一次字体降级的原因 (供启动日志与健康检查展示), 没有则为空。
func FontWarning() string {
	fontWarningMu.Lock()
	defer fontWarningMu.Unlock()
	return fontWarningMessage
}

// 版面尺寸 (点)。A4 宽 595.276, 高 841.89。
const (
	pdfMarginX      = 52.0
	pdfMarginTop    = 62.0
	pdfMarginBottom = 64.0
)

// RenderPDF 把报告渲染成 PDF 文件内容。
func RenderPDF(report Report, options PDFOptions) ([]byte, error) {
	set, err := fontSetFor(options.FontPath)
	if err != nil {
		return nil, err
	}
	// 两遍渲染: 页脚要写"第 N 页 / 共 M 页", 而总页数只有排完才知道。
	// 第二遍除页脚外完全相同, 因此分页不会变。
	data, pages, err := render(set, report, options, 0)
	if err != nil {
		return nil, err
	}
	if pages <= 1 {
		return data, nil
	}
	data, _, err = render(set, report, options, pages)
	return data, err
}

func render(set *pdfdoc.Set, report Report, options PDFOptions, totalPages int) ([]byte, int, error) {
	r := renderer{lang: options.Lang}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	title := r.tf("服务器巡检报告 · %s", report.Target)
	document := pdfdoc.New(set, pdfdoc.Options{Title: title, CreatedAt: now})

	flow := pdfdoc.NewFlow(document, pdfMarginX, pdfMarginTop, pdfMarginBottom)
	flow.NewPage = func(page *pdfdoc.Page, index int) {
		drawPageHeader(page, report, r)
	}
	flow.Footer = func(page *pdfdoc.Page, index int) {
		drawPageFooter(page, report, index, totalPages, now, r)
	}
	drawPageHeader(flow.Page(), report, r)

	renderTitle(flow, report, now, r)
	renderVerdict(flow, report, r)
	renderMetadata(flow, report, r)
	renderFindings(flow, report, r)
	renderNotes(flow, document, report, now, r)

	flow.Close()
	data, err := document.Bytes()
	return data, document.PageCount(), err
}

// ---------------------------------------------------------------------------
// 各区块
// ---------------------------------------------------------------------------

func drawPageHeader(page *pdfdoc.Page, report Report, r renderer) {
	page.Text(pdfMarginX, 40, 8.5, pdfdoc.ColorMuted,
		r.tf("服务器巡检报告   %s   用户 %s", report.Target, report.User), false)
}

func drawPageFooter(page *pdfdoc.Page, report Report, index, totalPages int, now time.Time, r renderer) {
	width := page.Width() - pdfMarginX
	page.Line(pdfMarginX, page.Height()-52, width, page.Height()-52, 0.6, pdfdoc.ColorRule)
	caption := r.tf("RunStor Bot 服务器巡检报告 · 生成于 %s", now.Format("2006-01-02 15:04:05"))
	page.Text(pdfMarginX, page.Height()-42, 8.5, pdfdoc.ColorMuted, caption, false)
	label := r.tf("第 %d 页", index+1)
	if totalPages > 0 {
		label = r.tf("第 %d 页 / 共 %d 页", index+1, totalPages)
	}
	page.Text(width-page.TextWidth(label, 8.5), page.Height()-42, 8.5, pdfdoc.ColorMuted, label, false)
}

func renderTitle(flow *pdfdoc.Flow, report Report, now time.Time, r renderer) {
	flow.Paragraph(r.t("服务器巡检报告"), pdfdoc.Style{Size: 20, Bold: true})
	flow.Space(4)
	subtitle := r.tf("目标 %s   报告编号 %s   采集时间 %s",
		report.Target, report.ID, report.StartedAt.Format("2006-01-02 15:04:05"))
	flow.Paragraph(subtitle, pdfdoc.Style{Size: 9, Color: pdfdoc.ColorMuted})
	flow.Space(14)
}

// renderVerdict 画结论条: 左侧色条 + 淡色底纹 + 状态与结论行。
func renderVerdict(flow *pdfdoc.Flow, report Report, r renderer) {
	const height = 52.0
	flow.EnsureSpace(height + 12)

	status := report.Status
	if report.Error != "" {
		status = StatusUnknown
	}
	color := statusColor(status)
	width := flow.Right - flow.Left
	page := flow.Page()
	panel := pdfdoc.ColorPanel
	page.Rect(flow.Left, flow.Y(), width, height, &panel, nil, 0)
	bar := color
	page.Rect(flow.Left, flow.Y(), 3.5, height, &bar, nil, 0)

	label := r.t(status.Label())
	page.Text(flow.Left+14, flow.Y()+21, 13, color, label, true)
	page.Text(flow.Left+14+page.TextWidth(label, 13)+8, flow.Y()+21, 9.5, pdfdoc.ColorMuted,
		r.t("— 总体结论"), false)
	page.Text(flow.Left+14, flow.Y()+38, 10.5, pdfdoc.ColorInk, verdictLine(report, r), false)
	flow.Space(height + 16)
}

// verdictLine 给出结论条里那句话。
func verdictLine(report Report, r renderer) string {
	if report.Error != "" {
		return r.t("巡检未完成: ") + report.Error
	}
	line := report.Summary
	if line == "" {
		line = report.SummaryLineLang(r.lang)
	}
	if len(report.Findings) > 0 {
		line = r.tf("%s (共 %d 项检查)", line, len(report.Findings))
	}
	return line
}

// renderMetadata 画采集信息表。
func renderMetadata(flow *pdfdoc.Flow, report Report, r renderer) {
	flow.Paragraph(r.t("采集信息"), pdfdoc.Style{Size: 11, Bold: true})
	flow.Space(2)

	rows := [][2]string{
		{r.t("目标地址"), report.Target},
		{r.t("登录用户"), r.tf("%s (认证方式: %s)", report.User, r.t(authModeLabel(report.AuthMode)))},
	}
	if report.HostName != "" {
		rows = append(rows, [2]string{r.t("主机名"), report.HostName})
	}
	if report.OS != "" {
		rows = append(rows, [2]string{r.t("操作系统"), report.OS})
	}
	if report.Kernel != "" {
		rows = append(rows, [2]string{r.t("内核版本"), report.Kernel})
	}
	if report.Uptime != "" {
		rows = append(rows, [2]string{r.t("运行时长"), report.Uptime})
	}
	if report.HostKeyFP != "" {
		rows = append(rows, [2]string{r.t("主机指纹"), report.HostKeyFP})
	}
	rows = append(rows,
		[2]string{r.t("采集耗时"), durationText(r, report.DurationMS)},
		[2]string{r.t("检查项"), r.tf("%d 项", len(report.Findings))},
	)

	const labelWidth = 62.0
	for _, row := range rows {
		flow.EnsureSpace(15)
		page := flow.Page()
		page.Text(flow.Left, flow.Y(), 9.5, pdfdoc.ColorMuted, row[0], false)
		// 值可能很长 (操作系统名、指纹), 按剩余宽度折行
		lines := flow.Wrap(row[1], 9.5, flow.Right-(flow.Left+labelWidth))
		for index, line := range lines {
			if index > 0 {
				flow.EnsureSpace(13)
				flow.Space(13)
			}
			flow.Page().Text(flow.Left+labelWidth, flow.Y(), 9.5, pdfdoc.ColorInk, line, false)
		}
		flow.Space(15)
	}
	flow.Space(10)
}

// renderFindings 逐项画检查结果。
func renderFindings(flow *pdfdoc.Flow, report Report, r renderer) {
	if len(report.Findings) == 0 {
		if report.Error != "" {
			flow.Paragraph(r.t("检查明细"), pdfdoc.Style{Size: 11, Bold: true})
			flow.Space(4)
			flow.Paragraph("本次巡检未能连上目标机, 没有采集到任何检查项。", pdfdoc.Style{Size: 10})
			flow.Space(10)
		}
		return
	}
	flow.Paragraph(r.t("检查明细"), pdfdoc.Style{Size: 11, Bold: true})
	flow.Space(4)

	for _, finding := range report.Findings {
		renderFinding(flow, finding, r)
	}
}

func renderFinding(flow *pdfdoc.Flow, finding Finding, r renderer) {
	// 标题行: 圆点 + 名称 (左) + 状态标签 (右对齐)
	flow.EnsureSpace(48)
	page := flow.Page()
	color := statusColor(finding.Status)
	page.Text(flow.Left, flow.Y(), 10, color, "●", false)
	page.Text(flow.Left+14, flow.Y(), 10.5, pdfdoc.ColorInk, finding.Name, true)
	label := r.t(finding.Status.Label())
	page.Text(flow.Right-page.TextWidth(label, 9.5), flow.Y(), 9.5, color, label, false)
	flow.Space(15)

	if finding.Summary != "" {
		flow.Paragraph(finding.Summary, pdfdoc.Style{Size: 10, Indent: 14})
	}
	for _, detail := range finding.Details {
		flow.Paragraph("· "+detail, pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorMuted, Indent: 20})
	}
	if finding.Error != "" {
		flow.Paragraph("该项检查未完成: "+finding.Error,
			pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorCritic, Indent: 20})
	}
	renderDiagnoses(flow, finding.Diagnoses, r)
	if evidence := strings.TrimSpace(finding.Evidence); evidence != "" {
		renderEvidence(flow, evidence, r)
	}
	flow.Space(12)
}

// renderDiagnoses 画"影响与处置建议"。
//
// 这是报告里最该被照着执行的部分, 所以排版上要和证据分开: 每条问题一个标题行
// (带条数与结论来源), 然后是"影响"一段, 再是编号的处置步骤。
func renderDiagnoses(flow *pdfdoc.Flow, items []Diagnosis, r renderer) {
	if len(items) == 0 {
		return
	}
	flow.Space(2)
	flow.Paragraph(r.t("影响与处置建议"), pdfdoc.Style{Size: 10, Bold: true, Color: pdfdoc.ColorInk, Indent: 14})
	flow.Space(2)

	for _, item := range items {
		flow.EnsureSpace(46)
		color := statusColor(Status(item.Severity))
		if item.Severity != string(StatusCritical) && item.Severity != string(StatusWarn) {
			color = pdfdoc.ColorMuted
		}
		page := flow.Page()
		page.Text(flow.Left+14, flow.Y(), 9.6, color, "▪", false)
		page.Text(flow.Left+26, flow.Y(), 9.8, pdfdoc.ColorInk, stripEmphasis(item.Title), true)
		// 条数与来源放在标题行右侧: 读者一眼知道这结论有多"硬"
		meta := diagnosisMeta(item)
		page.Text(flow.Right-page.TextWidth(meta, 8), flow.Y(), 8, pdfdoc.ColorMuted, meta, false)
		flow.Space(14)

		flow.Paragraph(r.t("影响: ")+r.t(stripEmphasis(item.Impact)),
			pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorInk, Indent: 26, Leading: 13})
		if item.Note != "" {
			flow.Paragraph("说明: "+stripEmphasis(item.Note),
				pdfdoc.Style{Size: 8.8, Color: pdfdoc.ColorWarn, Indent: 26, Leading: 12.5})
		}
		if len(item.Actions) > 0 {
			flow.Paragraph(r.t("处置:"), pdfdoc.Style{Size: 9.2, Indent: 26, Leading: 13})
			for index, action := range item.Actions {
				flow.Paragraph(fmt.Sprintf("%d) %s", index+1, stripEmphasis(action)),
					pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorInk, Indent: 34, Leading: 13})
			}
		}
		for _, ref := range item.Knowledge {
			flow.Paragraph(r.t("相关知识: ")+knowledgeText(ref, r),
				pdfdoc.Style{Size: 8.8, Color: pdfdoc.ColorOk, Indent: 26, Leading: 12.5})
		}
		if len(item.Samples) > 0 {
			flow.Paragraph(r.t("原始日志: ")+stripEmphasis(item.Samples[0]),
				pdfdoc.Style{Size: 8.2, Color: pdfdoc.ColorMuted, Indent: 26, Leading: 12})
		}
		flow.Space(8)
	}
}

// stripEmphasis 去掉 markdown 强调标记。
//
// 规则文案与**模型输出**都可能带 **这样** 的标记, 而 PDF 不是 markdown 渲染器 ——
// 不处理的话报告里就会出现成对的星号。这里统一清掉, 而不是要求每个写文案的人记住。
func stripEmphasis(text string) string {
	// 反引号在终端里表示"这是命令", 但 PDF 里只是噪音 —— 命令本身写清楚就够了
	return strings.ReplaceAll(strings.ReplaceAll(text, "**", ""), "`", "")
}

// knowledgeText 生成一条知识引用的可读形式: 《文档》§ 章节 — 片段摘要。
//
// 带上摘要而不是只给章节名: 读报告的人往往直接在纸面上判断"这条要不要点开看"。
func knowledgeText(ref KnowledgeRef, r renderer) string {
	location := r.t("《") + stripEmphasis(ref.Doc) + r.t("》")
	if strings.TrimSpace(ref.Heading) != "" {
		location += " § " + stripEmphasis(ref.Heading)
	}
	if snippet := strings.TrimSpace(ref.Snippet); snippet != "" {
		return location + " — " + stripEmphasis(snippet)
	}
	return location
}

// diagnosisMeta 生成标题行右侧的"条数 + 结论来源"。
func diagnosisMeta(item Diagnosis) string {
	parts := []string{fmt.Sprintf("%d 条", item.Count)}
	switch item.Origin {
	case OriginRule:
		parts = append(parts, "规则库")
	case OriginModel:
		parts = append(parts, "模型分析")
	default:
		parts = append(parts, "需人工确认")
	}
	return strings.Join(parts, " · ")
}

// renderEvidence 把原始证据画在淡色块里。
//
// 块会**跨页续画**, 而不是整块搬到下一页: 日志证据动辄几十行, 整块搬走会在上一页
// 留下大半个空白页 (实测就是这样), 而跨页续画只是把同一块内容分成两段, 左下角的
// 竖线与底纹会让它看起来仍是连续的一块。
func renderEvidence(flow *pdfdoc.Flow, evidence string, r renderer) {
	const (
		indent  = 14.0
		padding = 6.0
		size    = 8.6
		leading = 11.5
	)
	var lines []string
	for _, raw := range strings.Split(evidence, "\n") {
		raw = sanitizeEvidence(raw)
		if strings.TrimSpace(raw) == "" {
			continue
		}
		lines = append(lines, flow.Wrap(raw, size, flow.Right-flow.Left-indent-padding*2)...)
	}
	if len(lines) == 0 {
		return
	}
	// 证据太长时也有个上限, 免得一份报告几十页
	const maxLines = 200
	truncated := 0
	if len(lines) > maxLines {
		truncated = len(lines) - maxLines
		lines = lines[:maxLines]
	}

	// 页尾留给"省略了 N 行"那行的位置
	tailLines := 0
	if truncated > 0 {
		tailLines = 1
	}

	remaining := lines
	tail := ""
	if truncated > 0 {
		tail = fmt.Sprintf("(证据过长, 省略了 %d 行; 完整内容见巡检记录)", truncated)
	}

	for len(remaining) > 0 || tail != "" {
		// 当前页还能放几行 (扣掉上下留白)
		capacity := int((flow.Bottom - flow.Y() - padding*2) / leading)
		// 少于 3 行不值得再画一块, 翻页重来 (也避免"只放一行"这种难看的收尾)
		if capacity < 3 {
			flow.BreakPage()
			capacity = int((flow.Bottom - flow.Y() - padding*2) / leading)
		}
		if capacity < 1 {
			// 页面高度连 3 行都放不下 (理论上不会发生), 兜底直接画一行, 避免死循环
			capacity = 1
		}

		count := len(remaining)
		if count > capacity-tailLines {
			count = capacity - tailLines
		}
		if count < 0 {
			count = 0
		}
		if count == 0 && tail == "" {
			// 剩下的行数放不下了 (只剩 tailLines 的位置), 翻页再画
			flow.BreakPage()
			continue
		}

		chunk := remaining[:count]
		remaining = remaining[count:]
		drawEvidenceChunk(flow, chunk, tail, indent, padding, size, leading, r)
		if len(remaining) > 0 {
			flow.BreakPage()
			continue
		}
		tail = ""
	}
}

// drawEvidenceChunk 画证据块的一段 (底纹 + 左侧竖线 + 若干行)。
func drawEvidenceChunk(flow *pdfdoc.Flow, lines []string, tail string,
	indent, padding, size, leading float64, r renderer) {
	height := float64(len(lines))*leading + padding*2
	if tail != "" {
		height += leading
	}
	page := flow.Page()
	panel := pdfdoc.ColorPanel
	page.Rect(flow.Left+indent, flow.Y(), flow.Right-flow.Left-indent, height, &panel, nil, 0)
	page.Line(flow.Left+indent, flow.Y(), flow.Left+indent, flow.Y()+height, 2, pdfdoc.ColorRule)

	flow.Space(padding)
	for _, line := range lines {
		flow.Page().Text(flow.Left+indent+padding, flow.Y(), size, pdfdoc.ColorMuted, line, false)
		flow.Space(leading)
	}
	if tail != "" {
		flow.Page().Text(flow.Left+indent+padding, flow.Y(), size, pdfdoc.ColorMuted, tail, false)
		flow.Space(leading)
	}
	flow.Space(padding)
}

// renderNotes 收尾: 说明这份报告的来源与限制。
func renderNotes(flow *pdfdoc.Flow, document *pdfdoc.Document, report Report, now time.Time, r renderer) {
	flow.Space(6)
	flow.Paragraph(r.t("说明"), pdfdoc.Style{Size: 11, Bold: true})
	flow.Space(2)
	flow.Paragraph(r.t("本报告由 RunStor Bot 通过 SSH 登录目标机执行固定采集脚本生成, 只读取系统自身的信息, 不在目标机上做任何修改。判定阈值写在程序里 (负载/内存/磁盘/温度/日志), 原始输出见每项检查下方的证据块。"),
		pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorMuted})
	if report.Analysis != "" {
		flow.Space(4)
		flow.Paragraph(report.Analysis, pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorMuted})
		flow.Paragraph("带「规则库」标签的结论来自程序内置的故障特征库, 可复现可评审; "+
			"带「模型分析」标签的结论由大模型根据日志内容给出, 请结合现场判断。",
			pdfdoc.Style{Size: 8.8, Color: pdfdoc.ColorMuted})
	}
	flow.Space(4)
	flow.Paragraph(r.tf("报告编号 %s · 生成时间 %s", report.ID, now.Format("2006-01-02 15:04:05")),
		pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorMuted})

	// 字体缺字形时如实说明, 而不是让读者以为报告里本来就没有那个字
	if missing := document.Set().MissingGlyphs(document.Texts()...); len(missing) > 0 {
		flow.Space(4)
		flow.Paragraph(fmt.Sprintf(
			"注意: 有 %d 个字符当前字体无法显示, 已在正文中替换为 %c (%s)。",
			len(missing), '□', describeRunes(missing)),
			pdfdoc.Style{Size: 9.2, Color: pdfdoc.ColorWarn})
	}
}

// authModeLabel 返回认证方式的可读名称。
//
// 值本身就是中文 (见 Target.AuthMode): "公钥"/"密码"/"无凭据"。这里不做映射表,
// 只处理空值 —— 之前写了一版英文 key/password 的映射, 结果永远匹配不上, 全靠
// default 分支返回原值才看着对, 是典型"看着能跑其实没生效"的代码。
func authModeLabel(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "未知"
	}
	return mode
}

// durationText 按语言渲染耗时。
func durationText(r renderer, milliseconds int64) string {
	if milliseconds < 1000 {
		return r.tf("%d 毫秒", milliseconds)
	}
	return r.tf("%.2f 秒", float64(milliseconds)/1000)
}

// sanitizeEvidence 清掉控制字符: 它们进 PDF 只会变成看不见的乱码。
// 制表符换成空格, 回车丢弃 (换行由调用方按行处理)。
func sanitizeEvidence(line string) string {
	var builder strings.Builder
	for _, r := range line {
		switch {
		case r == '\t':
			builder.WriteString("    ")
		case r == '\r':
			// 丢弃: 行尾的 \r 会把内容挤出一个方框
		case r < 0x20:
			// 丢弃其他控制字符
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// describeRunes 把字符列表写成 "U+E000" 这样的形式 (这些字符本来就没法显示)。
func describeRunes(runes []rune) string {
	const maxListed = 8
	parts := make([]string, 0, maxListed+1)
	for index, r := range runes {
		if index >= maxListed {
			parts = append(parts, fmt.Sprintf("等 %d 个", len(runes)))
			break
		}
		parts = append(parts, fmt.Sprintf("U+%04X", r))
	}
	return strings.Join(parts, " ")
}
