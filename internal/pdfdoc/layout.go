package pdfdoc

import (
	"strings"
	"unicode"
)

// Flow 是按"从上往下逐段排版"的游标: 负责换行、翻页、页脚。
//
// 报告是流式内容 (若干段落 + 逐条检查项), 用游标比逐行手算 y 更不容易出错:
// 每段都问"这一段放在当前位置还够不够", 不够就换页。
type Flow struct {
	document *Document
	// 版心 (从上往下量)
	Left, Right, Top, Bottom float64
	// Footer 非空时, 每次翻页/结束都会在页脚调用它 (用来画页码)
	Footer func(page *Page, index int)
	// NewPage 非空时, 每次新建页面后调用 (用来画页眉)
	NewPage func(page *Page, index int)

	y float64
}

// NewFlow 在文档当前页上开一个排版游标。
func NewFlow(document *Document, marginX, marginTop, marginBottom float64) *Flow {
	flow := &Flow{
		document: document,
		Left:     marginX,
		Right:    document.Width() - marginX,
		Top:      marginTop,
		Bottom:   document.Height() - marginBottom,
	}
	flow.y = flow.Top
	return flow
}

// Y 返回当前写入位置 (基线)。
func (f *Flow) Y() float64 { return f.y }

// SetY 直接移动游标。
func (f *Flow) SetY(value float64) { f.y = value }

// Space 向下留白。
func (f *Flow) Space(height float64) { f.y += height }

// Page 返回当前页。
func (f *Flow) Page() *Page { return f.document.Current() }

// NeedsNewPage 报告当前位置放不下 height 高度。
func (f *Flow) NeedsNewPage(height float64) bool { return f.y+height > f.Bottom }

// EnsureSpace 保证接下来 height 高度放得下, 放不下就翻页。
func (f *Flow) EnsureSpace(height float64) {
	if f.NeedsNewPage(height) {
		f.BreakPage()
	}
}

// BreakPage 翻页 (先把当前页页脚画上)。
func (f *Flow) BreakPage() {
	pageIndex := f.document.Current().Index()
	if f.Footer != nil {
		f.Footer(f.document.Current(), pageIndex)
	}
	page := f.document.AddPage()
	f.y = f.Top
	if f.NewPage != nil {
		f.NewPage(page, page.Index())
	}
}

// Close 收尾: 给最后一页画页脚。
func (f *Flow) Close() {
	if f.Footer != nil {
		page := f.document.Current()
		f.Footer(page, page.Index())
	}
}

// Style 描述一段文字的字号、行距与颜色。
type Style struct {
	Size    float64
	Leading float64 // 行距 (默认字号 × 1.6)
	Color   Color
	Bold    bool
	Indent  float64
}

// Paragraph 排一段文本 (可含 \n), 自动换行并翻页。
func (f *Flow) Paragraph(text string, style Style) {
	if style.Size <= 0 {
		style.Size = 10
	}
	if style.Leading <= 0 {
		style.Leading = style.Size * 1.6
	}
	if style.Color == (Color{}) {
		style.Color = ColorInk
	}
	left := f.Left + style.Indent
	for index, raw := range strings.Split(text, "\n") {
		if index > 0 {
			f.EnsureSpace(style.Leading)
			f.y += style.Leading
		}
		for _, line := range f.Wrap(raw, style.Size, f.Right-left) {
			f.EnsureSpace(style.Size)
			page := f.Page()
			page.Text(left, f.y, style.Size, style.Color, line, style.Bold)
			f.y += style.Leading
		}
		if strings.TrimSpace(raw) == "" {
			f.y += style.Leading * 0.4
		}
	}
}

// Wrap 按宽度折行。
//
// 规则与中文排版习惯一致:
//   - 西文在空格处折行, 连续西文单词即使超宽也不从中间断开 (路径、URL 例外会硬断);
//   - 中日韩逐字折行 (汉字之间本来就没有空格);
//   - 不在行首出现收尾标点 (。，、；：！？）】》”等), 也不在行尾留下起始标点 (（【《“等)。
func (f *Flow) Wrap(text string, size float64, maxWidth float64) []string {
	if strings.TrimSpace(text) == "" {
		return []string{""}
	}
	set := f.document.set
	widthOf := func(r rune) float64 { return set.Width(r) * size / 1000 }

	var lines []string
	var current []rune
	width := 0.0

	flush := func() {
		lines = append(lines, strings.TrimRight(string(current), " "))
		current = current[:0]
		width = 0
	}

	// 整串只转一次。曾经写在循环里 (每次迭代都转一遍), 于是一行超长文本 (日志里
	// 完全可能出现几万字符的单行) 会退化成 O(n²): 实测 4 万字符要跑十几秒, 而
	// 这个函数在导出 PDF 的请求路径上 —— 等于一次导出就能把请求挂住。
	runes := []rune(text)
	for index := 0; index < len(runes); {
		r := runes[index]
		if r == '\n' {
			flush()
			index++
			continue
		}
		if r == '\t' {
			r = ' '
		}

		// 西文单词整体处理: 从当前位置起, 连续的非 CJK、非空格字符算一个词
		if isLatinWordRune(r) {
			end := index
			for end < len(runes) && isLatinWordRune(runes[end]) {
				end++
			}
			word := runes[index:end]
			wordWidth := 0.0
			for _, w := range word {
				wordWidth += widthOf(w)
			}
			if width+wordWidth > maxWidth && width > 0 && !hasLineHeadForbidden(string(word)) {
				flush()
			}
			// 整词仍然放不下 (很长的路径/URL): 只能硬断, 但优先在分隔符后断
			if wordWidth > maxWidth && width == 0 {
				for _, w := range word {
					if width+widthOf(w) > maxWidth && width > 0 {
						flush()
					}
					current = append(current, w)
					width += widthOf(w)
				}
				index = end
				continue
			}
			current = append(current, word...)
			width += wordWidth
			index = end
			continue
		}

		runeWidth := widthOf(r)
		if width+runeWidth > maxWidth && width > 0 {
			// 行首禁则: 收尾标点不能落在下一行行首 —— 把它挤在上一行末尾
			if isLineHeadForbidden(r) {
				current = append(current, r)
				flush()
				index++
				continue
			}
			// 行尾禁则: 起始标点不能留在一行末尾 —— 让它跟着下一行走
			if isLineTailForbidden(r) && len(current) > 0 {
				flush()
			}
			flush()
		}
		current = append(current, r)
		width += runeWidth
		index++
	}
	if len(current) > 0 || len(lines) == 0 {
		lines = append(lines, strings.TrimRight(string(current), " "))
	}
	return lines
}

// isLatinWordRune 报告"这个字符参与西文单词的整词折行"。
func isLatinWordRune(r rune) bool {
	if r == ' ' {
		return false
	}
	return unicode.IsLetter(r) && r < 0x2E80 || unicode.IsDigit(r) && r < 0x2E80 ||
		unicode.IsPunct(r) && r < 0x2E80 && r != ' ' && !isLineHeadForbidden(r)
}

// lineHeadForbidden 是不能出现在行首的字符 (收尾标点)。
const lineHeadForbidden = "，。、；：！？）】》」』”’%…·,.;:!?)]}"

// lineTailForbidden 是不能出现在行尾的字符 (起始标点)。
const lineTailForbidden = "（【《「『“‘([{"

func isLineHeadForbidden(r rune) bool { return strings.ContainsRune(lineHeadForbidden, r) }
func isLineTailForbidden(r rune) bool { return strings.ContainsRune(lineTailForbidden, r) }

func hasLineHeadForbidden(word string) bool {
	runes := []rune(word)
	return len(runes) > 0 && isLineHeadForbidden(runes[0])
}
