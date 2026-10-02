// Package pdfdoc 生成 PDF。
//
// 手写而不是引第三方库, 理由与项目里其他"手写"部分一致: 构建环境不通外网, 而
// 巡检报告要能在部署机上直接产出。用到的 PDF 特性都控制在 1.4 以内 (经典 xref 表、
// FlateDecode、Type0/CIDFontType2 复合字体), 这样连老的阅读器也能打开。
//
// 中文能显示的前提是**嵌入字体**: PDF 内置的 14 种标准字体一个汉字都没有, 不嵌入
// 就只能是空白或乱码。因为要嵌入, 就必须自己解析 TrueType (见 truetype.go), 取字符
// 到字形的映射、字宽、以及字形轮廓, 并按实际用到的字符做子集化。
package pdfdoc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"backup-restore-agent/internal/fileio"
	"unicode/utf16"
)

// tableRef 指向字体文件里某张表。
type tableRef struct {
	offset uint32
	length uint32
}

// Font 是一份解析好的 TrueType 字体。
//
// 目前只支持 TrueType 轮廓 (glyf/loca)。不支持的 CFF/OpenType 字体 (例如 Debian 上
// 的 Noto Sans CJK) 会在加载时被明确拒绝, 而不是硬塞进去产出一份打不开的 PDF。
type Font struct {
	name       string
	data       []byte
	tables     map[string]tableRef
	unitsPerEm int
	numGlyphs  int
	loca       []uint32
	advances   []uint16
	cmap       map[rune]uint16

	ascent      int16
	descent     int16
	capHeight   int16
	bbox        [4]int16
	fsType      uint16
	italicAngle float64
	weightClass uint16

	// 子集化结果缓存: 同一个字体 + 同一批字符只做一次, 报告页数多时省事。
	subsetOnce map[string][]byte
}

// LoadFont 从文件加载字体。.ttc 集合取第 0 个字面。
func LoadFont(path string) (*Font, error) {
	// 字体文件可以有几十 MB, 但也不能无界: 一个超大文件 (或被软链到 /dev/zero)
	// 会在读的时候把内存吃光。64 MiB 足够放下任何正常字体。
	data, err := fileio.ReadBounded(path, 64<<20)
	if err != nil {
		return nil, fmt.Errorf("读取字体失败: %w", err)
	}
	return ParseFont(data, nameOf(path))
}

// ParseFont 解析字体字节。
func ParseFont(data []byte, name string) (*Font, error) {
	if len(data) < 12 {
		return nil, errors.New("字体文件太短")
	}
	signature := binary.BigEndian.Uint32(data[0:4])
	base := uint32(0)
	switch signature {
	case 0x00010000, 0x74727565: // 普通 TrueType ('true')
	case 0x74746366: // 'ttcf' 字体集合
		offset, err := firstFontOffset(data)
		if err != nil {
			return nil, err
		}
		base = offset
	case 0x4F54544F: // 'OTTO' —— CFF 轮廓
		return nil, errors.New("这是 CFF/OpenType 轮廓的字体 (如 Noto CJK), 本程序只支持 TrueType 轮廓的字体")
	default:
		return nil, fmt.Errorf("无法识别的字体格式: 0x%08X", signature)
	}

	font := &Font{name: name, data: data, subsetOnce: map[string][]byte{}}
	if err := font.readTableDirectory(base); err != nil {
		return nil, err
	}
	if _, ok := font.tables["glyf"]; !ok {
		return nil, errors.New("字体没有 glyf 表 (不是 TrueType 轮廓)")
	}
	if err := font.readHead(); err != nil {
		return nil, err
	}
	if err := font.readHhea(); err != nil {
		return nil, err
	}
	if err := font.readMaxp(); err != nil {
		return nil, err
	}
	if err := font.readOS2(); err != nil {
		return nil, err
	}
	if err := font.readLoca(); err != nil {
		return nil, err
	}
	if err := font.readAdvances(); err != nil {
		return nil, err
	}
	if err := font.readCmap(); err != nil {
		return nil, err
	}
	if err := font.readPost(); err != nil {
		return nil, err
	}
	if font.fsType&0x0002 != 0 {
		return nil, errors.New("字体声明为禁止嵌入 (OS/2.fsType bit 1), 不能用于 PDF")
	}
	return font, nil
}

func nameOf(path string) string {
	base := path
	if index := strings.LastIndexAny(path, "/\\"); index >= 0 {
		base = path[index+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(base, ".ttf"), ".TTF")
}

// firstFontOffset 从 ttc 头里取第 0 个字面的偏移。
func firstFontOffset(data []byte) (uint32, error) {
	if len(data) < 16 {
		return 0, errors.New("字体集合文件太短")
	}
	count := binary.BigEndian.Uint32(data[8:12])
	if count == 0 {
		return 0, errors.New("字体集合里没有字体")
	}
	if len(data) < 12+int(count)*4 {
		return 0, errors.New("字体集合头不完整")
	}
	return binary.BigEndian.Uint32(data[12:16]), nil
}

// readTableDirectory 读表目录。
func (f *Font) readTableDirectory(base uint32) error {
	if int(base)+12 > len(f.data) {
		return errors.New("字体表目录越界")
	}
	count := int(binary.BigEndian.Uint16(f.data[base+4 : base+6]))
	f.tables = make(map[string]tableRef, count)
	for index := 0; index < count; index++ {
		entry := int(base) + 12 + index*16
		if entry+16 > len(f.data) {
			return errors.New("字体表目录越界")
		}
		tag := string(f.data[entry : entry+4])
		offset := binary.BigEndian.Uint32(f.data[entry+8 : entry+12])
		length := binary.BigEndian.Uint32(f.data[entry+12 : entry+16])
		if int(offset)+int(length) > len(f.data) {
			return fmt.Errorf("字体表 %q 越界", tag)
		}
		f.tables[tag] = tableRef{offset: offset, length: length}
	}
	return nil
}

// table 返回某张表的字节。
func (f *Font) table(tag string) ([]byte, bool) {
	ref, ok := f.tables[tag]
	if !ok {
		return nil, false
	}
	return f.data[ref.offset : ref.offset+ref.length], true
}

func (f *Font) readHead() error {
	head, ok := f.table("head")
	if !ok || len(head) < 54 {
		return errors.New("字体缺少 head 表")
	}
	f.unitsPerEm = int(binary.BigEndian.Uint16(head[18:20]))
	if f.unitsPerEm <= 0 {
		return errors.New("字体 unitsPerEm 非法")
	}
	f.bbox = [4]int16{
		int16(binary.BigEndian.Uint16(head[36:38])),
		int16(binary.BigEndian.Uint16(head[38:40])),
		int16(binary.BigEndian.Uint16(head[40:42])),
		int16(binary.BigEndian.Uint16(head[42:44])),
	}
	return nil
}

func (f *Font) readHhea() error {
	hhea, ok := f.table("hhea")
	if !ok || len(hhea) < 36 {
		return errors.New("字体缺少 hhea 表")
	}
	f.ascent = int16(binary.BigEndian.Uint16(hhea[4:6]))
	f.descent = int16(binary.BigEndian.Uint16(hhea[6:8]))
	return nil
}

func (f *Font) readMaxp() error {
	maxp, ok := f.table("maxp")
	if !ok || len(maxp) < 6 {
		return errors.New("字体缺少 maxp 表")
	}
	f.numGlyphs = int(binary.BigEndian.Uint16(maxp[4:6]))
	if f.numGlyphs <= 0 {
		return errors.New("字体 glyph 数为 0")
	}
	return nil
}

func (f *Font) readOS2() error {
	table, ok := f.table("OS/2")
	if !ok || len(table) < 74 {
		// OS/2 不是必须的: 拿不到就用 hhea 的升降部, 度量略粗但能用
		f.capHeight = f.ascent
		return nil
	}
	f.weightClass = binary.BigEndian.Uint16(table[4:6])
	f.fsType = binary.BigEndian.Uint16(table[8:10])
	// sCapHeight 在 version >= 2 的表里, 偏移 88
	if binary.BigEndian.Uint16(table[0:2]) >= 2 && len(table) >= 90 {
		f.capHeight = int16(binary.BigEndian.Uint16(table[88:90]))
	}
	if f.capHeight <= 0 {
		f.capHeight = f.ascent
	}
	return nil
}

func (f *Font) readPost() error {
	table, ok := f.table("post")
	if !ok || len(table) < 16 {
		return nil
	}
	f.italicAngle = float64(int32(binary.BigEndian.Uint32(table[4:8]))) / 65536
	return nil
}

// readLoca 读字形偏移表 (长度 numGlyphs+1)。
func (f *Font) readLoca() error {
	head, _ := f.table("head")
	format := int16(binary.BigEndian.Uint16(head[50:52]))
	table, ok := f.table("loca")
	if !ok {
		return errors.New("字体缺少 loca 表")
	}
	f.loca = make([]uint32, f.numGlyphs+1)
	if format == 0 {
		// 短格式: 每项 uint16, 实际偏移要乘 2
		if len(table) < (f.numGlyphs+1)*2 {
			return errors.New("loca 表长度不足")
		}
		for index := range f.loca {
			f.loca[index] = uint32(binary.BigEndian.Uint16(table[index*2:index*2+2])) * 2
		}
		return nil
	}
	if len(table) < (f.numGlyphs+1)*4 {
		return errors.New("loca 表长度不足")
	}
	for index := range f.loca {
		f.loca[index] = binary.BigEndian.Uint32(table[index*4 : index*4+4])
	}
	return nil
}

// readAdvances 读每个字形的水平步进宽度 (hmtx)。
func (f *Font) readAdvances() error {
	hhea, _ := f.table("hhea")
	hmtx, ok := f.table("hmtx")
	if !ok {
		return errors.New("字体缺少 hmtx 表")
	}
	metrics := int(binary.BigEndian.Uint16(hhea[34:36]))
	if metrics <= 0 || metrics > f.numGlyphs {
		metrics = f.numGlyphs
	}
	if len(hmtx) < metrics*4 {
		return errors.New("hmtx 表长度不足")
	}
	f.advances = make([]uint16, f.numGlyphs)
	last := uint16(0)
	for index := 0; index < metrics; index++ {
		last = binary.BigEndian.Uint16(hmtx[index*4 : index*4+2])
		f.advances[index] = last
	}
	// 超出 numberOfHMetrics 的字形沿用最后一个步进宽度 (规范如此)
	for index := metrics; index < f.numGlyphs; index++ {
		f.advances[index] = last
	}
	return nil
}

// readCmap 读 Unicode → 字形编号的映射。
func (f *Font) readCmap() error {
	table, ok := f.table("cmap")
	if !ok || len(table) < 4 {
		return errors.New("字体缺少 cmap 表")
	}
	count := int(binary.BigEndian.Uint16(table[2:4]))
	f.cmap = make(map[rune]uint16, 4096)

	// 优先 format 12 (含 BMP 之外), 其次是 format 4 (BMP)。宁可多读一份也不要漏字。
	var format4, format12 uint32
	for index := 0; index < count; index++ {
		entry := 4 + index*8
		if entry+8 > len(table) {
			break
		}
		platform := binary.BigEndian.Uint16(table[entry : entry+2])
		encoding := binary.BigEndian.Uint16(table[entry+2 : entry+4])
		offset := binary.BigEndian.Uint32(table[entry+4 : entry+8])
		if int(offset)+2 > len(table) {
			continue
		}
		format := binary.BigEndian.Uint16(table[offset : offset+2])
		unicode := platform == 0 || (platform == 3 && (encoding == 1 || encoding == 10))
		if !unicode {
			continue
		}
		switch format {
		case 4:
			if format4 == 0 {
				format4 = offset
			}
		case 12:
			if format12 == 0 {
				format12 = offset
			}
		}
	}
	if format4 == 0 && format12 == 0 {
		return errors.New("字体里没有可用的 Unicode cmap 子表")
	}
	if format4 != 0 {
		if err := f.readCmapFormat4(table, format4); err != nil {
			return err
		}
	}
	if format12 != 0 {
		if err := f.readCmapFormat12(table, format12); err != nil {
			return err
		}
	}
	return nil
}

func (f *Font) readCmapFormat4(table []byte, offset uint32) error {
	start := int(offset)
	if start+14 > len(table) {
		return errors.New("cmap format 4 子表越界")
	}
	segCount := int(binary.BigEndian.Uint16(table[start+6 : start+8]))
	segCount /= 2
	endCodes := start + 14
	startCodes := endCodes + segCount*2 + 2 // 跳过 reservedPad
	deltas := startCodes + segCount*2
	rangeOffsets := deltas + segCount*2
	if rangeOffsets+segCount*2 > len(table) {
		return errors.New("cmap format 4 子表越界")
	}

	for segment := 0; segment < segCount; segment++ {
		end := int(binary.BigEndian.Uint16(table[endCodes+segment*2 : endCodes+segment*2+2]))
		begin := int(binary.BigEndian.Uint16(table[startCodes+segment*2 : startCodes+segment*2+2]))
		delta := int16(binary.BigEndian.Uint16(table[deltas+segment*2 : deltas+segment*2+2]))
		rangeOffset := int(binary.BigEndian.Uint16(table[rangeOffsets+segment*2 : rangeOffsets+segment*2+2]))
		if begin > end {
			continue
		}
		for code := begin; code <= end && code <= 0xFFFF; code++ {
			var glyph uint16
			if rangeOffset == 0 {
				glyph = uint16(int32(code) + int32(delta))
			} else {
				index := rangeOffsets + segment*2 + rangeOffset + (code-begin)*2
				if index+2 > len(table) {
					continue
				}
				glyph = binary.BigEndian.Uint16(table[index : index+2])
				if glyph != 0 {
					glyph = uint16(int32(glyph) + int32(delta))
				}
			}
			if glyph != 0 {
				f.cmap[rune(code)] = glyph
			}
		}
	}
	return nil
}

func (f *Font) readCmapFormat12(table []byte, offset uint32) error {
	start := int(offset)
	if start+16 > len(table) {
		return errors.New("cmap format 12 子表越界")
	}
	groups := int(binary.BigEndian.Uint32(table[start+12 : start+16]))
	if start+16+groups*12 > len(table) {
		return errors.New("cmap format 12 子表越界")
	}
	// 防一手异常字体: 单组范围可以声明得极大 (0x10FFFF), 全展开会吃光内存
	const limit = 400000
	for index := 0; index < groups; index++ {
		entry := start + 16 + index*12
		begin := binary.BigEndian.Uint32(table[entry : entry+4])
		end := binary.BigEndian.Uint32(table[entry+4 : entry+8])
		glyph := binary.BigEndian.Uint32(table[entry+8 : entry+12])
		if begin > end || end > 0x10FFFF || end-begin > limit {
			continue
		}
		for code := begin; code <= end; code++ {
			if len(f.cmap) > limit {
				return nil
			}
			if _, exists := f.cmap[rune(code)]; !exists {
				f.cmap[rune(code)] = uint16(glyph + (code - begin))
			}
		}
	}
	return nil
}

// ---- 对外的度量接口 ----

// Name 返回字体名。
func (f *Font) Name() string { return f.name }

// UnitsPerEm 返回 em 单位数 (PDF 里宽度按 1000 折算)。
func (f *Font) UnitsPerEm() int { return f.unitsPerEm }

// NumGlyphs 返回字形总数。
func (f *Font) NumGlyphs() int { return f.numGlyphs }

// GlyphIndex 返回字符对应的字形编号, 没有对应字形时返回 0 (.notdef)。
func (f *Font) GlyphIndex(r rune) uint16 {
	if glyph, ok := f.cmap[r]; ok {
		return glyph
	}
	return 0
}

// Width 返回字符在 PDF 文本空间里的宽度 (按 1000/em 折算)。
func (f *Font) Width(r rune) float64 {
	glyph := f.cmap[r]
	if int(glyph) >= len(f.advances) {
		return 1000
	}
	return float64(f.advances[glyph]) * 1000 / float64(f.unitsPerEm)
}

// Ascent 返回升部 (已折算到 1000/em)。
func (f *Font) Ascent() float64 {
	return float64(f.ascent) * 1000 / float64(f.unitsPerEm)
}

// Descent 返回降部 (负数, 已折算)。
func (f *Font) Descent() float64 {
	return float64(f.descent) * 1000 / float64(f.unitsPerEm)
}

// CapHeight 返回大写高度 (已折算)。
func (f *Font) CapHeight() float64 {
	return float64(f.capHeight) * 1000 / float64(f.unitsPerEm)
}

// BBox 返回字体边界框 (已折算到 1000/em)。
func (f *Font) BBox() [4]float64 {
	scale := 1000 / float64(f.unitsPerEm)
	return [4]float64{
		float64(f.bbox[0]) * scale, float64(f.bbox[1]) * scale,
		float64(f.bbox[2]) * scale, float64(f.bbox[3]) * scale,
	}
}

// ItalicAngle 返回倾斜角。
func (f *Font) ItalicAngle() float64 { return f.italicAngle }

// fallbackRunes 是"字形缺失"时的替代顺序: 先找一个通用替代字符, 再退到问号。
//
// 直接把缺字形的字符丢进内容流会渲染成空白, 报告里就会出现"凭空少一个字" ——
// 对日志和主机名这种内容是误导。宁可显示一个可见的替代符号。
var fallbackRunes = []rune{'□', '?', ' '}

// GlyphFor 返回能真正显示该字符的字形编号, 找不到时返回替代字形与 false。
func (f *Font) GlyphFor(r rune) (uint16, bool) {
	if glyph, ok := f.cmap[r]; ok {
		return glyph, true
	}
	if r == '\t' {
		return f.GlyphFor(' ')
	}
	for _, candidate := range fallbackRunes {
		if glyph, ok := f.cmap[candidate]; ok {
			return glyph, false
		}
	}
	return 0, false
}

// Subset 生成只含所需字符的字体文件。
//
// 保留原来的字形编号 (glyph id), 只把用不到的字形轮廓清空 —— 这样 cmap/hmtx 都不用
// 重写, 复合字形 (由别的字形拼出来的, 例如带音标的字母) 的引用也依然成立。
// 代价是 loca/hmtx 仍按原字形总数占位, 换来的是不可能把字形编号搬错。
func (f *Font) Subset(runes map[rune]bool) ([]byte, error) {
	key := subsetKey(runes)
	if cached, ok := f.subsetOnce[key]; ok {
		return cached, nil
	}

	used := map[uint16]bool{0: true} // .notdef 始终保留
	for r := range runes {
		if glyph, _ := f.GlyphFor(r); glyph != 0 {
			used[glyph] = true
		}
	}
	// 复合字形的组成部分必须一起带上, 否则那些字会显示成缺笔画
	for pending := []uint16{}; ; {
		pending = pending[:0]
		for glyph := range used {
			for _, component := range f.components(glyph) {
				if !used[component] {
					used[component] = true
					pending = append(pending, component)
				}
			}
		}
		if len(pending) == 0 {
			break
		}
	}

	glyf, loca := f.buildGlyf(used)
	head, _ := f.table("head")
	patched := make([]byte, len(head))
	copy(patched, head)
	// 新 loca 一律用长格式, 并清掉校验和调整值 (最后统一算)
	binary.BigEndian.PutUint32(patched[8:12], 0)
	binary.BigEndian.PutUint16(patched[50:52], 1)

	replaced := map[string][]byte{
		"glyf": glyf,
		"loca": loca,
		"head": patched,
	}
	// 竖排相关表对本用途无用, 去掉可以少几十 KB (PDF 是横排文本)
	dropped := map[string]bool{"vhea": true, "vmtx": true}

	file, err := f.assemble(replaced, dropped)
	if err != nil {
		return nil, err
	}
	f.subsetOnce[key] = file
	return file, nil
}

// subsetKey 把字符集合压成一个稳定的缓存键。
func subsetKey(runes map[rune]bool) string {
	list := make([]rune, 0, len(runes))
	for r := range runes {
		list = append(list, r)
	}
	// 排序保证同一批字符无论顺序如何都命中同一个缓存
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	var builder strings.Builder
	builder.Grow(len(list) * 2)
	for _, r := range list {
		builder.WriteRune(r)
	}
	return builder.String()
}

// buildGlyf 生成子集后的 glyf 与 loca。
func (f *Font) buildGlyf(used map[uint16]bool) ([]byte, []byte) {
	glyf := make([]byte, 0, 64*1024)
	offsets := make([]uint32, f.numGlyphs+1)
	for glyph := 0; glyph < f.numGlyphs; glyph++ {
		offsets[glyph] = uint32(len(glyf))
		if !used[uint16(glyph)] {
			continue
		}
		data := f.glyphData(glyph)
		glyf = append(glyf, data...)
		for len(glyf)%4 != 0 {
			glyf = append(glyf, 0)
		}
	}
	offsets[f.numGlyphs] = uint32(len(glyf))

	loca := make([]byte, len(offsets)*4)
	for index, offset := range offsets {
		binary.BigEndian.PutUint32(loca[index*4:index*4+4], offset)
	}
	return glyf, loca
}

// glyphData 返回某字形的原始轮廓字节 (空字形返回 nil)。
func (f *Font) glyphData(glyph int) []byte {
	if glyph < 0 || glyph+1 >= len(f.loca) {
		return nil
	}
	begin, end := f.loca[glyph], f.loca[glyph+1]
	if begin >= end {
		return nil
	}
	table, ok := f.table("glyf")
	if !ok || int(end) > len(table) {
		return nil
	}
	return table[begin:end]
}

// components 返回复合字形引用的组成部分 (普通字形返回空)。
func (f *Font) components(glyph uint16) []uint16 {
	data := f.glyphData(int(glyph))
	if len(data) < 10 {
		return nil
	}
	if int16(binary.BigEndian.Uint16(data[0:2])) >= 0 {
		return nil // 轮廓数非负 = 普通字形
	}
	var out []uint16
	const (
		argWords    = 0x0001
		haveScale   = 0x0008
		moreParts   = 0x0020
		haveXYScale = 0x0040
		haveMatrix  = 0x0080
	)
	for offset := 10; offset+4 <= len(data); {
		flags := binary.BigEndian.Uint16(data[offset : offset+2])
		index := binary.BigEndian.Uint16(data[offset+2 : offset+4])
		out = append(out, index)
		offset += 4
		if flags&argWords != 0 {
			offset += 4
		} else {
			offset += 2
		}
		switch {
		case flags&haveScale != 0:
			offset += 2
		case flags&haveXYScale != 0:
			offset += 4
		case flags&haveMatrix != 0:
			offset += 8
		}
		if flags&moreParts == 0 {
			break
		}
	}
	return out
}

// assemble 把表重新组装成一个单字体的 sfnt 文件。
//
// 组装而不是"原样切片": 一来表被改过了 (glyf/loca/head), 二来原始文件可能是字体集合
// (ttc), 而 PDF 的 FontFile2 要的是一份独立字体。
func (f *Font) assemble(replaced map[string][]byte, dropped map[string]bool) ([]byte, error) {
	tags := make([]string, 0, len(f.tables))
	for tag := range f.tables {
		if dropped[tag] {
			continue
		}
		tags = append(tags, tag)
	}
	sortStrings(tags)

	count := len(tags)
	directorySize := 12 + count*16
	offset := directorySize
	placements := make([]placement, 0, count)
	for _, tag := range tags {
		var data []byte
		if replacement, ok := replaced[tag]; ok {
			data = replacement
		} else {
			table, _ := f.table(tag)
			data = table
		}
		placements = append(placements, placement{
			tag:      tag,
			offset:   offset,
			length:   len(data),
			checksum: tableChecksum(data),
		})
		offset += len(data)
		for offset%4 != 0 {
			offset++
		}
	}
	total := offset
	out := make([]byte, total)

	// sfnt 头
	binary.BigEndian.PutUint32(out[0:4], 0x00010000)
	binary.BigEndian.PutUint16(out[4:6], uint16(count))
	entrySelector := 0
	for (1 << (entrySelector + 1)) <= count {
		entrySelector++
	}
	searchRange := (1 << entrySelector) * 16
	binary.BigEndian.PutUint16(out[6:8], uint16(searchRange))
	binary.BigEndian.PutUint16(out[8:10], uint16(entrySelector))
	binary.BigEndian.PutUint16(out[10:12], uint16(count*16-searchRange))

	for index, item := range placements {
		entry := 12 + index*16
		copy(out[entry:entry+4], item.tag)
		binary.BigEndian.PutUint32(out[entry+4:entry+8], item.checksum)
		binary.BigEndian.PutUint32(out[entry+8:entry+12], uint32(item.offset))
		binary.BigEndian.PutUint32(out[entry+12:entry+16], uint32(item.length))
		var data []byte
		if replacement, ok := replaced[item.tag]; ok {
			data = replacement
		} else {
			data, _ = f.table(item.tag)
		}
		copy(out[item.offset:], data)
	}

	// head 的 checkSumAdjustment: 先把整个文件的校验和算出来, 再回填
	if headIndex := indexOfTag(placements2tags(placements), "head"); headIndex >= 0 {
		head := placements[headIndex]
		binary.BigEndian.PutUint32(out[head.offset+8:head.offset+12], 0)
		sum := tableChecksum(out)
		binary.BigEndian.PutUint32(out[head.offset+8:head.offset+12], 0xB1B0AFBA-sum)
	}
	return out, nil
}

type placement struct {
	tag      string
	offset   int
	length   int
	checksum uint32
}

func placements2tags(items []placement) []string {
	tags := make([]string, len(items))
	for index, item := range items {
		tags[index] = item.tag
	}
	return tags
}

func indexOfTag(tags []string, want string) int {
	for index, tag := range tags {
		if tag == want {
			return index
		}
	}
	return -1
}

// tableChecksum 计算 TrueType 表校验和 (按 4 字节累加, 不足补零)。
func tableChecksum(data []byte) uint32 {
	var sum uint32
	index := 0
	for ; index+4 <= len(data); index += 4 {
		sum += binary.BigEndian.Uint32(data[index : index+4])
	}
	if index < len(data) {
		var tail [4]byte
		copy(tail[:], data[index:])
		sum += binary.BigEndian.Uint32(tail[:])
	}
	return sum
}

// sortStrings 插入排序 (表数量很小, 且不引 sort 之外的东西没意义)。
func sortStrings(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// utf16BE 把字符串编成 UTF-16BE 的字节 (不含字节序标记)。
//
// 用于 ToUnicode CMap 的映射目标: 那里在 CMap 的上下文里编码已经是明确的,
// 带 BOM 反而会多解出一个零宽字符。
func utf16BE(value string) []byte {
	return encodeUTF16(utf16.Encode([]rune(value)))
}

// utf16TextString 把字符串编成 PDF 的文本串 (UTF-16BE **带**字节序标记)。
//
// 文档元信息 (标题/作者) 必须用这个形式: 规范规定只有以 BOM 开头的字符串才按
// UTF-16BE 解释, 否则按 PDFDocEncoding 解 —— 中文标题会显示成乱码。
func utf16TextString(value string) []byte {
	return append([]byte{0xFE, 0xFF}, utf16BE(value)...)
}

func encodeUTF16(units []uint16) []byte {
	out := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.BigEndian.PutUint16(out[index*2:index*2+2], unit)
	}
	return out
}
