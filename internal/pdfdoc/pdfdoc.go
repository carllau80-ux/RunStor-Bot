package pdfdoc

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Color 是 RGB 颜色, 分量 0..1。
type Color struct{ R, G, B float64 }

func rgb(r, g, b float64) Color { return Color{R: r, G: g, B: b} }

// 报告里用到的颜色。放在一处便于统一调整。
var (
	ColorInk     = rgb(0.13, 0.15, 0.18) // 正文
	ColorMuted   = rgb(0.45, 0.49, 0.55) // 次要信息
	ColorRule    = rgb(0.82, 0.85, 0.88) // 分隔线
	ColorPanel   = rgb(0.96, 0.97, 0.98) // 底纹
	ColorOk      = rgb(0.06, 0.53, 0.34) // 正常
	ColorWarn    = rgb(0.72, 0.45, 0.03) // 告警
	ColorCritic  = rgb(0.72, 0.11, 0.11) // 严重
	ColorUnknown = rgb(0.42, 0.45, 0.50) // 无法判定
)

// Options 是文档级设置。
type Options struct {
	Width, Height float64   // 页面尺寸 (点), 默认 A4
	Title         string    // 文档标题 (PDF 元信息)
	CreatedAt     time.Time // 生成时间; 零值取当前时间
	Timezone      *time.Location
}

// Document 累积页面内容, 最后一次性序列化成 PDF。
//
// 分两阶段的原因: 字体子集要等"所有文字都排完"才知道该嵌入哪些字形, 而字体对象
// 又必须出现在 PDF 对象表里。所以先排版收集指令, 再统一编号序列化。
type Document struct {
	set    *Set
	pages  [][]string
	texts  []string
	width  float64
	height float64
	title  string
	now    time.Time
}

// New 构造文档。
func New(set *Set, options Options) *Document {
	size := Options{Width: 595.276, Height: 841.89} // A4: 210mm × 297mm
	if options.Width > 0 {
		size.Width = options.Width
	}
	if options.Height > 0 {
		size.Height = options.Height
	}
	now := options.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	document := &Document{
		set:    set,
		width:  size.Width,
		height: size.Height,
		title:  options.Title,
		now:    now,
	}
	document.AddPage()
	return document
}

// Set 返回字体集合。
func (d *Document) Set() *Set { return d.set }

// Width 返回页面宽度。
func (d *Document) Width() float64 { return d.width }

// Height 返回页面高度。
func (d *Document) Height() float64 { return d.height }

// PageCount 返回页数。
func (d *Document) PageCount() int { return len(d.pages) }

// Texts 返回已经排进文档的所有文本 (用于统计缺字形字符、做字体子集)。
func (d *Document) Texts() []string { return d.texts }

// AddPage 追加一页并返回它。
func (d *Document) AddPage() *Page {
	d.pages = append(d.pages, nil)
	return &Page{document: d, index: len(d.pages) - 1}
}

// Current 返回当前 (最后一页)。
func (d *Document) Current() *Page { return &Page{document: d, index: len(d.pages) - 1} }

// Page 是一页的绘图入口。坐标以**左上角为原点、y 向下**为正, 与排版直觉一致;
// 写出 PDF 指令时再翻成 PDF 的左下角坐标系。
type Page struct {
	document *Document
	index    int
}

// Index 返回页序号 (从 0 开始)。
func (p *Page) Index() int { return p.index }

// Width 返回页面宽度。
func (p *Page) Width() float64 { return p.document.width }

// Height 返回页面高度。
func (p *Page) Height() float64 { return p.document.height }

func (p *Page) emit(op string) { p.document.pages[p.index] = append(p.document.pages[p.index], op) }

// toPDFY 把"从上往下"的 y 翻成 PDF 的"从下往上"。
func (p *Page) toPDFY(y float64) float64 { return p.document.height - y }

// Text 在 (x, y) 处画一行文本。y 是**基线**位置 (从上往下量)。
func (p *Page) Text(x, y, size float64, color Color, text string, bold bool) {
	if text == "" {
		return
	}
	p.document.texts = append(p.document.texts, text)

	var builder strings.Builder
	fmt.Fprintf(&builder, "BT\n%.3f %.3f %.3f rg\n", color.R, color.G, color.B)
	// 文字渲染模式必须每次都显式设置。
	//
	// Tr 属于文本状态, 而文本状态是图形状态的一部分 —— ET 不会把它复位, q/Q 也只在
	// 成对时生效。所以"标题设了 2 Tr"会一路影响后面所有文字 (整篇看起来都像加粗),
	// 必须在不加粗时显式写回 0 Tr。
	if bold {
		// 只有一份字重, 加粗用"填充+描边"近似 (文字渲染模式 2)。
		//
		// 不用"错开一点描两遍": 那样同一段文字在 PDF 里存了两份, 复制或抽取时会重复,
		// 而报告里的文字是要被粘到工单里去的。
		fmt.Fprintf(&builder, "%.3f %.3f %.3f RG\n%.3f w\n2 Tr\n",
			color.R, color.G, color.B, size*0.035)
	} else {
		builder.WriteString("0 Tr\n")
	}
	fmt.Fprintf(&builder, "1 0 0 1 %.3f %.3f Tm\n", x, p.toPDFY(y))
	for _, run := range p.document.set.Split(text, size) {
		if len(run.Runes) == 0 {
			continue
		}
		fmt.Fprintf(&builder, "/F%d %.3f Tf\n", run.Font, size)
		builder.WriteString("<")
		for _, r := range run.Runes {
			if _, glyph, ok := p.document.set.Resolve(r); ok {
				fmt.Fprintf(&builder, "%04X", glyph)
				continue
			}
			// 字体缺这个字形: 用替代字符顶替 (排版阶段已统计并会在报告里说明)
			if _, glyph, ok := p.document.set.replacement(); ok {
				fmt.Fprintf(&builder, "%04X", glyph)
			} else {
				fmt.Fprintf(&builder, "%04X", 0)
			}
		}
		builder.WriteString("> Tj\n")
	}
	builder.WriteString("ET")
	p.emit(builder.String())
}

// TextWidth 返回文本排出来有多宽。
func (p *Page) TextWidth(text string, size float64) float64 {
	return p.document.set.Measure(text, size)
}

// Line 画一条直线。
func (p *Page) Line(x1, y1, x2, y2, width float64, color Color) {
	p.emit(fmt.Sprintf("q\n%.3f %.3f %.3f RG\n%.3f w\n%.3f %.3f m %.3f %.3f l S\nQ",
		color.R, color.G, color.B, width, x1, p.toPDFY(y1), x2, p.toPDFY(y2)))
}

// Rect 画矩形 (fill 为 nil 表示只描边)。
func (p *Page) Rect(x, y, width, height float64, fill *Color, stroke *Color, lineWidth float64) {
	var builder strings.Builder
	builder.WriteString("q\n")
	if fill != nil {
		fmt.Fprintf(&builder, "%.3f %.3f %.3f rg\n", fill.R, fill.G, fill.B)
	}
	if stroke != nil {
		fmt.Fprintf(&builder, "%.3f %.3f %.3f RG\n%.3f w\n", stroke.R, stroke.G, stroke.B, lineWidth)
	}
	fmt.Fprintf(&builder, "%.3f %.3f %.3f %.3f re\n", x, p.toPDFY(y+height), width, height)
	switch {
	case fill != nil && stroke != nil:
		builder.WriteString("B\n")
	case fill != nil:
		builder.WriteString("f\n")
	default:
		builder.WriteString("S\n")
	}
	builder.WriteString("Q")
	p.emit(builder.String())
}

// ---------------------------------------------------------------------------
// 序列化
// ---------------------------------------------------------------------------

// Bytes 生成 PDF 文件内容。
func (d *Document) Bytes() ([]byte, error) {
	subsets, err := d.set.Subsets(d.texts...)
	if err != nil {
		return nil, err
	}

	// 对象编号: 1 目录 / 2 页树 / 3 元信息 / 之后是页与内容流 / 再之后每份字体 5 个对象
	const (
		catalogNumber = 1
		pagesNumber   = 2
		infoNumber    = 3
	)
	pageBase := 4
	contentBase := pageBase + len(d.pages)
	fontBase := contentBase + len(d.pages)

	var body bytes.Buffer
	offsets := make([]int, 0, fontBase+len(subsets)*5)
	write := func(number int, payload string) {
		for len(offsets) < number {
			offsets = append(offsets, 0)
		}
		offsets[number-1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", number, payload)
	}
	writeStream := func(number int, dict string, data []byte) error {
		compressed, err := deflate(data)
		if err != nil {
			return err
		}
		for len(offsets) < number {
			offsets = append(offsets, 0)
		}
		offsets[number-1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n<< %s /Length %d >>\nstream\n", number, dict, len(compressed))
		body.Write(compressed)
		body.WriteString("\nendstream\nendobj\n")
		return nil
	}

	// 字体对象
	fontNames := make([]string, len(subsets))
	for index := range subsets {
		fontNames[index] = fmt.Sprintf("F%d", index)
	}
	for index, subset := range subsets {
		base := fontBase + index*5
		subsetTagFont := subset.Font
		psName := subsetTagFont.Name()
		if psName == "" {
			psName = fmt.Sprintf("Embedded%d", index)
		}
		tag := subsetTag(index, len(subsets))
		write(base, fmt.Sprintf(
			"<< /Type /Font /Subtype /Type0 /BaseFont /%s+%s /Encoding /Identity-H "+
				"/DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>",
			tag, psName, base+1, base+4))
		write(base+1, fmt.Sprintf(
			"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /%s+%s "+
				"/CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> "+
				"/FontDescriptor %d 0 R /DW 1000 /W %s /CIDToGIDMap /Identity >>",
			tag, psName, base+2, widthArray(subset.Widths)))
		write(base+2, fmt.Sprintf(
			"<< /Type /FontDescriptor /FontName /%s+%s /Flags 4 /FontBBox [%s] "+
				"/ItalicAngle %.1f /Ascent %.1f /Descent %.1f /CapHeight %.1f /StemV %.1f "+
				"/FontFile2 %d 0 R >>",
			tag, psName, bboxText(subsetTagFont), subsetTagFont.ItalicAngle(),
			subsetTagFont.Ascent(), subsetTagFont.Descent(), subsetTagFont.CapHeight(),
			stemV(subsetTagFont), base+3))
		if err := writeStream(base+3, "/Filter /FlateDecode /Length1 "+strconv.Itoa(len(subset.File)),
			subset.File); err != nil {
			return nil, err
		}
		if err := writeStream(base+4, "/Filter /FlateDecode", subset.ToUnicodeCMap()); err != nil {
			return nil, err
		}
	}

	// 页与内容流
	kids := make([]string, len(d.pages))
	for index := range d.pages {
		kids[index] = fmt.Sprintf("%d 0 R", pageBase+index)
	}
	write(catalogNumber, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesNumber))
	write(pagesNumber, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(d.pages)))
	write(infoNumber, fmt.Sprintf("<< /Title <%s> /Producer <%s> /Creator <%s> /CreationDate (%s) >>",
		hexUpper(utf16TextString(d.title)), hexUpper(utf16TextString(producer)),
		hexUpper(utf16TextString(producer)), pdfDate(d.now)))
	for index := range d.pages {
		resources := make([]string, len(fontNames))
		for position, name := range fontNames {
			resources[position] = fmt.Sprintf("/%s %d 0 R", name, fontBase+position*5)
		}
		write(pageBase+index, fmt.Sprintf(
			"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %.3f %.3f] "+
				"/Resources << /Font << %s >> /ProcSet [/PDF /Text] >> /Contents %d 0 R >>",
			pagesNumber, d.width, d.height, strings.Join(resources, " "), contentBase+index))
		content := strings.Join(d.pages[index], "\n")
		if err := writeStream(contentBase+index, "/Filter /FlateDecode", []byte(content)); err != nil {
			return nil, err
		}
	}

	// 交叉引用表
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	out.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})
	headerSize := out.Len()
	out.Write(body.Bytes())

	xrefOffset := out.Len()
	count := len(offsets) + 1
	fmt.Fprintf(&out, "xref\n0 %d\n", count)
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset+headerSize)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root %d 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		count, catalogNumber, infoNumber, xrefOffset)
	return out.Bytes(), nil
}

// subsetTag 生成字体子集前缀 (PDF 要求 6 个大写字母)。
func subsetTag(index, total int) string {
	letters := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	value := index
	tag := make([]byte, 6)
	for position := 5; position >= 0; position-- {
		tag[position] = letters[value%26]
		value /= 26
	}
	return string(tag)
}

// widthArray 生成 /W 数组, 连续的字形编号合并成一段以省空间。
func widthArray(widths map[uint16]float64) string {
	glyphs := make([]int, 0, len(widths))
	for glyph := range widths {
		glyphs = append(glyphs, int(glyph))
	}
	sort.Ints(glyphs)

	var builder strings.Builder
	builder.WriteString("[")
	for index := 0; index < len(glyphs); {
		start := glyphs[index]
		end := index
		for end+1 < len(glyphs) && glyphs[end+1] == glyphs[end]+1 {
			end++
		}
		fmt.Fprintf(&builder, " %d [", start)
		for position := index; position <= end; position++ {
			fmt.Fprintf(&builder, "%.1f ", widths[uint16(glyphs[position])])
		}
		builder.WriteString("]")
		index = end + 1
	}
	builder.WriteString(" ]")
	return builder.String()
}

func bboxText(font *Font) string {
	box := font.BBox()
	parts := make([]string, 4)
	for index, value := range box {
		parts[index] = strconv.FormatFloat(value, 'f', 1, 64)
	}
	return strings.Join(parts, " ")
}

// stemV 是字体描述里的笔画粗细, 阅读器只用它做替代渲染, 取不到精确值时按经验给。
func stemV(font *Font) float64 {
	if font.weightClass >= 600 {
		return 140
	}
	return 80
}

// deflate 做 zlib 压缩 (FlateDecode)。
//
// 用默认级别而不是最高级别: 嵌入的字体子集有几百 KB, 最高级别为了多压几个百分点
// 要把整个文件找一遍最长匹配, 实测占了生成一份报告 75% 的时间 (报告导出要等十几秒);
// 默认级别快十几倍, 体积只差个位数百分比。
func deflate(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := zlib.NewWriterLevel(&buffer, zlib.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// producer 是 PDF 元信息里的生成者。
const producer = "RunStor Bot"

// pdfDate 生成 PDF 的日期字面量: D:YYYYMMDDHHmmSS+HH'mm'。
func pdfDate(value time.Time) string {
	_, offset := value.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("D:%s%s%02d'%02d'", value.Format("20060102150405"), sign, offset/3600, (offset%3600)/60)
}
