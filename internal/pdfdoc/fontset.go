package pdfdoc

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed assets/DejaVuSans.ttf assets/DroidSansFallbackFull.ttf
var embeddedFonts embed.FS

// 内置字体的文件名。两份**互补**, 缺一不可:
//
//   - DejaVuSans 有拉丁字母、数字与常用符号, 但一个汉字也没有;
//   - DroidSansFallbackFull 有中日韩字形, 但连 A-Z / 0-9 / 斜杠都没有
//     (它是 Android 上给主字体"补字"的 fallback, 拉丁部分由主字体负责)。
//
// 而报告里这两类字符都密集出现 (「主机 web-01 磁盘 /data 使用率 95%」), 所以按字符
// 选择字体, 行内混排。
const (
	LatinFontAsset = "assets/DejaVuSans.ttf"
	CJKFontAsset   = "assets/DroidSansFallbackFull.ttf"
)

// Set 是一组字体, 按顺序回答"这个字符用哪份字体画"。
type Set struct {
	fonts []*Font
}

// DefaultSet 返回内置字体集合 (拉丁 + 中日韩)。
func DefaultSet() (*Set, error) {
	return LoadSet(LatinFontAsset, CJKFontAsset)
}

// LoadAsset 加载单个内置字体。
func LoadAsset(asset string) (*Font, error) {
	data, err := embeddedFonts.ReadFile(asset)
	if err != nil {
		return nil, fmt.Errorf("读取内置字体 %s 失败: %w", asset, err)
	}
	return ParseFont(data, nameOf(asset))
}

// LoadSet 按给定名字加载内置字体集合。
func LoadSet(assets ...string) (*Set, error) {
	set := &Set{}
	for _, asset := range assets {
		data, err := embeddedFonts.ReadFile(asset)
		if err != nil {
			return nil, fmt.Errorf("读取内置字体 %s 失败: %w", asset, err)
		}
		font, err := ParseFont(data, nameOf(asset))
		if err != nil {
			return nil, fmt.Errorf("解析内置字体 %s 失败: %w", asset, err)
		}
		set.fonts = append(set.fonts, font)
	}
	if len(set.fonts) == 0 {
		return nil, fmt.Errorf("没有可用字体")
	}
	return set, nil
}

// NewSet 用现成的字体构造集合 (测试与自定义字体用)。
func NewSet(fonts ...*Font) *Set { return &Set{fonts: fonts} }

// Fonts 返回集合里的字体 (下标即 PDF 里的字体序号)。
func (s *Set) Fonts() []*Font { return s.fonts }

// Resolve 找出某个字符该用哪份字体画。
//
// 返回 ok=false 表示所有字体都没有这个字形 —— 调用方应该换成替代字符, 而不是把
// 一个空字形塞进内容流 (那会渲染成空白, 报告里就"凭空少一个字")。
func (s *Set) Resolve(r rune) (index int, glyph uint16, ok bool) {
	for position, font := range s.fonts {
		if gid, found := font.cmap[r]; found {
			return position, gid, true
		}
	}
	return 0, 0, false
}

// Width 返回字符在 PDF 文本空间的宽度 (1000/em)。缺字形时用替代字符的宽度。
func (s *Set) Width(r rune) float64 {
	if index, _, ok := s.Resolve(r); ok {
		return s.fonts[index].Width(r)
	}
	if replacement, _, ok := s.replacement(); ok {
		return s.fonts[replacement].Width(replacementRune)
	}
	return 500
}

// replacementRune 是所有字体都缺字形时用来顶替的字符。
const replacementRune = '□'

// replacement 找出替代字符所在字体。
func (s *Set) replacement() (index int, glyph uint16, ok bool) {
	return s.Resolve(replacementRune)
}

// Run 是一段用同一份字体绘制的文本。
type Run struct {
	Font  int
	Runes []rune
	Width float64
}

// Measure 返回文本在 PDF 文本空间的宽度。
func (s *Set) Measure(text string, size float64) float64 {
	total := 0.0
	for _, r := range text {
		total += s.Width(r)
	}
	return total * size / 1000
}

// Split 把文本切成"同一份字体"的连续段, 并算出每段宽度。
func (s *Set) Split(text string, size float64) []Run {
	var runs []Run
	for _, r := range text {
		index := 0
		width := 0.0
		if position, _, ok := s.Resolve(r); ok {
			index = position
			width = s.fonts[position].Width(r)
		} else if position, _, ok := s.replacement(); ok {
			// 缺字形: 换成替代字符, 位置索引也按替代字符算
			index = position
			width = s.fonts[position].Width(replacementRune)
			r = replacementRune
		} else {
			continue
		}
		width = width * size / 1000
		if len(runs) > 0 && runs[len(runs)-1].Font == index {
			runs[len(runs)-1].Runes = append(runs[len(runs)-1].Runes, r)
			runs[len(runs)-1].Width += width
			continue
		}
		runs = append(runs, Run{Font: index, Runes: []rune{r}, Width: width})
	}
	return runs
}

// MissingGlyphs 返回无法显示、只能用替代字符顶替的字符 (按字符去重并排序)。
//
// 报告里要如实说明"有字没能显示", 而不是悄悄换个方框了事。
func (s *Set) MissingGlyphs(texts ...string) []rune {
	seen := map[rune]bool{}
	for _, text := range texts {
		for _, r := range text {
			if _, _, ok := s.Resolve(r); !ok {
				seen[r] = true
			}
		}
	}
	out := make([]rune, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Subsets 按实际用到的字符给每份字体生成子集字体文件。
func (s *Set) Subsets(texts ...string) ([]*SubsetFont, error) {
	wanted := make([]map[rune]bool, len(s.fonts))
	for index := range wanted {
		wanted[index] = map[rune]bool{}
	}
	for _, text := range texts {
		for _, r := range text {
			if index, _, ok := s.Resolve(r); ok {
				wanted[index][r] = true
			} else if index, _, ok := s.replacement(); ok {
				wanted[index][replacementRune] = true
			}
		}
	}

	out := make([]*SubsetFont, 0, len(s.fonts))
	for index, font := range s.fonts {
		// 子集化时至少要带上替代字符, 否则"缺字形"的提示自己都没法显示
		if index2, _, ok := s.replacement(); ok {
			wanted[index2][replacementRune] = true
		}
		file, err := font.Subset(wanted[index])
		if err != nil {
			return nil, fmt.Errorf("子集化字体 %s 失败: %w", font.Name(), err)
		}
		widths := map[uint16]float64{}
		for r := range wanted[index] {
			glyph := font.cmap[r]
			widths[glyph] = font.Width(r)
		}
		out = append(out, &SubsetFont{Font: font, File: file, Widths: widths, runes: wanted[index]})
	}
	return out, nil
}

// SubsetFont 是一份待嵌入 PDF 的子集字体。
type SubsetFont struct {
	Font   *Font
	File   []byte
	Widths map[uint16]float64
	runes  map[rune]bool
}

// ToUnicodeCMap 生成 ToUnicode CMap, 让 PDF 里的中文可以被复制、被 pdftotext 提取。
//
// 没有它, 阅读器只能看到字形编号, 复制出来是一串乱码 —— 报告里的日志片段就没法
// 粘到工单里, 这是很实际的损失。
func (s *SubsetFont) ToUnicodeCMap() []byte {
	glyphs := make([]uint16, 0, len(s.runes))
	for r := range s.runes {
		if glyph, ok := s.Font.cmap[r]; ok && glyph != 0 {
			glyphs = append(glyphs, glyph)
		}
	}
	sort.Slice(glyphs, func(i, j int) bool { return glyphs[i] < glyphs[j] })

	// 反查: 字形 → 字符 (同一字形可能对应多个字符, 取编码最小的那个)
	source := map[uint16]rune{}
	for r := range s.runes {
		if glyph, ok := s.Font.cmap[r]; ok && glyph != 0 {
			if existing, found := source[glyph]; !found || r < existing {
				source[glyph] = r
			}
		}
	}

	var builder strings.Builder
	builder.WriteString("/CIDInit /ProcSet findresource begin\n")
	builder.WriteString("12 dict begin\nbegincmap\n")
	builder.WriteString("/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n")
	builder.WriteString("/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n")
	builder.WriteString("1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	// bfchar 一次最多 100 项
	for start := 0; start < len(glyphs); start += 100 {
		end := start + 100
		if end > len(glyphs) {
			end = len(glyphs)
		}
		fmt.Fprintf(&builder, "%d beginbfchar\n", end-start)
		for _, glyph := range glyphs[start:end] {
			fmt.Fprintf(&builder, "<%04X> <%s>\n", glyph, hexUpper(utf16BE(string(source[glyph]))))
		}
		builder.WriteString("endbfchar\n")
	}
	builder.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return []byte(builder.String())
}

// hexUpper 把字节编成大写十六进制。
func hexUpper(data []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(data)*2)
	for _, value := range data {
		out = append(out, digits[value>>4], digits[value&0x0F])
	}
	return string(out)
}
