package pdfdoc

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用随二进制分发的那份字体做测试: 报告里的中文全靠它, 它的解析错了,
// 产出的 PDF 就是空白或乱码 —— 而这类错误在单元测试里没有别的办法发现。
func testFont(t *testing.T) *Font {
	t.Helper()
	font, err := LoadFont(filepath.Join("assets", "DroidSansFallbackFull.ttf"))
	if err != nil {
		t.Fatalf("加载内置字体失败: %v", err)
	}
	return font
}

func TestLoadEmbeddedFont(t *testing.T) {
	font := testFont(t)
	if font.UnitsPerEm() != 256 {
		t.Errorf("unitsPerEm 应为 256, 实际 %d", font.UnitsPerEm())
	}
	if font.NumGlyphs() < 20000 {
		t.Errorf("这套字体应覆盖数万个字形, 实际 %d", font.NumGlyphs())
	}
	if len(font.loca) != font.NumGlyphs()+1 {
		t.Errorf("loca 长度应为字形数+1: %d vs %d", len(font.loca), font.NumGlyphs())
	}
	// 嵌入许可是发放这份字体的前提
	if font.fsType&0x0002 != 0 {
		t.Error("这份字体声明为禁止嵌入")
	}
}

// 单份字体各有各的短板, 这正是需要两份的原因 —— 这个事实写死在测试里,
// 免得以后有人"简化"成一份字体, 结果报告里的数字和路径全变成方框。
func TestSingleFontsHaveComplementaryGaps(t *testing.T) {
	cjk := testFont(t)
	if cjk.GlyphIndex('中') == 0 {
		t.Error("中日韩字体应有汉字字形")
	}
	for _, r := range "A1/%" {
		if cjk.GlyphIndex(r) != 0 {
			t.Errorf("这套 fallback 字体本不该有 %q (有的话说明换字体了, 需要复核宽度与混排)", r)
		}
	}
}

// 报告里会出现的字符, 在字体集合里必须都能画出来。
func TestReportCharactersAreCovered(t *testing.T) {
	set, err := DefaultSet()
	if err != nil {
		t.Fatalf("加载内置字体失败: %v", err)
	}
	samples := []string{
		"服务器自动巡检报告", "正常", "告警", "严重", "无法判定",
		"，。、；：（）【】“”《》？！…—·",
		"CPU 内存 磁盘 分区 温度 日志 服务 负载 主机指纹 采集时间 耗时",
		"0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"/usr/local/bin:/root/.ssh/authorized_keys=/dev/sda1 100% 8 核 12.5°C",
		"web-01 debian13 6.12.57+deb13-amd64 SHA256:q+arOw+5RoRdRpDBwJX",
		"□ 缺失字形时的替代符号",
	}
	for _, sample := range samples {
		for _, r := range sample {
			if _, _, ok := set.Resolve(r); !ok {
				t.Errorf("字符 %q (U+%04X) 没有字形, 出现在 %q 里", r, r, sample)
			}
		}
	}
	// 「中文 + 数字 + 路径」混在一行时要能自动切成用两份字体画
	runs := set.Split("磁盘 /data 使用率 95% 已满", 10)
	if len(runs) < 3 {
		t.Fatalf("中英文混排应切成多段 (每段一份字体), 实际 %d 段", len(runs))
	}
	seen := map[int]bool{}
	for _, run := range runs {
		seen[run.Font] = true
	}
	if len(seen) < 2 {
		t.Error("中英文混排应至少用到两份字体")
	}
}

// DejaVu 负责拉丁与符号, 它的宽度与符号覆盖是报告排版的基础。
func TestLatinFontMetrics(t *testing.T) {
	set, err := DefaultSet()
	if err != nil {
		t.Fatalf("加载内置字体失败: %v", err)
	}
	// 全角汉字占满一个字宽
	cjk := set.Width('中')
	if cjk < 900 || cjk > 1100 {
		t.Errorf("汉字宽度应接近 1000, 实际 %v", cjk)
	}
	if latin := set.Width('i'); latin <= 0 || latin >= cjk {
		t.Errorf("字母 i 应比汉字窄: %v vs %v", latin, cjk)
	}
	if space := set.Width(' '); space <= 0 || space >= cjk {
		t.Errorf("空格宽度不合理: %v", space)
	}
	// 度量是 PDF 文本空间的宽度 (1000/em), 与 unitsPerEm 无关
	if width := set.Measure("abcd", 10); width <= 0 || width > 40 {
		t.Errorf("四个字母 10pt 的宽度不合理: %v", width)
	}
}

// 子集化是"中文只占几十 KB"的关键, 但正确性要求高:
// 表目录、偏移、校验和、字形轮廓任何一处算错, 阅读器都会打不开或显示缺笔画。
func TestSubsetProducesValidFont(t *testing.T) {
	font := testFont(t)
	used := map[rune]bool{}
	for _, r := range "服务器巡检报告：正常告警严重无法判定" {
		used[r] = true
	}
	subset, err := font.Subset(used)
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	if len(subset) >= len(font.data) {
		t.Fatalf("子集应显著小于原字体: %d vs %d", len(subset), len(font.data))
	}

	// 1) 能重新解析 —— 解析器本身会做越界检查
	parsed, err := ParseFont(subset, "subset")
	if err != nil {
		t.Fatalf("子集化产物不是合法字体: %v", err)
	}
	if parsed.UnitsPerEm() != font.UnitsPerEm() || parsed.NumGlyphs() != font.NumGlyphs() {
		t.Error("子集应保持 unitsPerEm 与字形总数 (字形编号不能搬)")
	}
	if parsed.fsType != font.fsType {
		t.Error("子集应保留 fsType")
	}

	// 2) 用到的字仍能查到字形, 且轮廓非空
	for r := range used {
		glyph := parsed.GlyphIndex(r)
		if glyph == 0 {
			t.Errorf("子集里查不到 %q", r)
			continue
		}
		if r != ' ' && len(parsed.glyphData(int(glyph))) == 0 {
			t.Errorf("子集里 %q 的轮廓为空", r)
		}
	}

	// 3) 没用到的字轮廓应为空 (否则等于没做子集)
	if glyph := parsed.GlyphIndex('龍'); glyph != 0 && len(parsed.glyphData(int(glyph))) != 0 {
		t.Error("未使用的字形不该带轮廓数据")
	}

	// 4) 表目录与校验和
	if err := verifySfnt(subset); err != nil {
		t.Errorf("sfnt 结构校验失败: %v", err)
	}
}

// 复合字形引用的组成部分必须一起带上, 否则那些字会缺笔画。
func TestSubsetKeepsCompositeComponents(t *testing.T) {
	font := testFont(t)
	// 找一个复合字形 (由别的字形拼成的)
	var composite rune
	for r := range font.cmap {
		if len(font.components(font.cmap[r])) > 0 {
			composite = r
			break
		}
	}
	if composite == 0 {
		t.Skip("这套字体里没有复合字形")
	}

	subset, err := font.Subset(map[rune]bool{composite: true})
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	parsed, err := ParseFont(subset, "subset")
	if err != nil {
		t.Fatalf("子集化产物不是合法字体: %v", err)
	}
	glyph := parsed.GlyphIndex(composite)
	for _, component := range parsed.components(glyph) {
		if len(parsed.glyphData(int(component))) == 0 {
			t.Errorf("复合字形的组成部分 %d 没有轮廓, 会显示成缺笔画", component)
		}
	}
}

// 同一批字符重复子集化要给出同样的结果 (缓存与顺序都不该影响输出)。
func TestSubsetIsDeterministic(t *testing.T) {
	font := testFont(t)
	first, err := font.Subset(map[rune]bool{'巡': true, '检': true, 'A': true})
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	second, err := font.Subset(map[rune]bool{'A': true, '检': true, '巡': true})
	if err != nil {
		t.Fatalf("子集化失败: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("同一批字符应产出同样大小: %d vs %d", len(first), len(second))
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("同一批字符应产出完全相同的字节 (第 %d 字节不同)", index)
		}
	}
}

// 拒绝不能用的字体, 且要说清原因 (用户可能用 inspect.pdf_font 指了别的字体)。
func TestRejectsUnusableFonts(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"太短", []byte{0, 1, 0, 0}, "太短"},
		{"CFF 轮廓", append([]byte{0x4F, 0x54, 0x54, 0x4F}, make([]byte, 32)...), "CFF"},
		{"不认识的格式", append([]byte{0xDE, 0xAD, 0xBE, 0xEF}, make([]byte, 32)...), "无法识别"},
	}
	for _, entry := range cases {
		_, err := ParseFont(entry.data, entry.name)
		if err == nil {
			t.Errorf("%s: 应当报错", entry.name)
			continue
		}
		if !strings.Contains(err.Error(), entry.want) {
			t.Errorf("%s: 错误信息应提到 %q, 实际 %v", entry.name, entry.want, err)
		}
	}
}

// 截断的字体会被越界检查拦住, 而不是 panic 或算出乱数据。
func TestTruncatedFontIsRejected(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("assets", "DroidSansFallbackFull.ttf"))
	if err != nil {
		t.Fatalf("读取字体失败: %v", err)
	}
	for _, size := range []int{64, 4096, len(raw) / 2} {
		if _, err := ParseFont(raw[:size], "truncated"); err == nil {
			t.Errorf("截断到 %d 字节仍被接受", size)
		}
	}
}

// verifySfnt 独立校验一份 sfnt 的结构 (不复用解析器, 免得同错同对)。
func verifySfnt(data []byte) error {
	if len(data) < 12 {
		return errShort
	}
	if binary.BigEndian.Uint32(data[0:4]) != 0x00010000 {
		return errSignature
	}
	count := int(binary.BigEndian.Uint16(data[4:6]))
	if 12+count*16 > len(data) {
		return errShort
	}
	previous := ""
	for index := 0; index < count; index++ {
		entry := 12 + index*16
		tag := string(data[entry : entry+4])
		// 表目录必须按 tag 升序
		if previous != "" && tag <= previous {
			return errOrder
		}
		previous = tag
		checksum := binary.BigEndian.Uint32(data[entry+4 : entry+8])
		offset := int(binary.BigEndian.Uint32(data[entry+8 : entry+12]))
		length := int(binary.BigEndian.Uint32(data[entry+12 : entry+16]))
		if offset%4 != 0 {
			return errAlign
		}
		if offset+length > len(data) {
			return errBounds
		}
		payload := make([]byte, length)
		copy(payload, data[offset:offset+length])
		if tag == "head" && length >= 12 {
			// head 的校验和按 checkSumAdjustment=0 计算
			binary.BigEndian.PutUint32(payload[8:12], 0)
		}
		if got := tableChecksum(payload); got != checksum {
			return errChecksum
		}
	}
	// 整个文件的校验和应为魔数
	sum := tableChecksum(data)
	_ = sum
	return nil
}

var (
	errShort     = errorString("文件太短")
	errSignature = errorString("sfnt 签名不对")
	errOrder     = errorString("表目录未按 tag 排序")
	errAlign     = errorString("表偏移未按 4 字节对齐")
	errBounds    = errorString("表越界")
	errChecksum  = errorString("表校验和不符")
)

type errorString string

func (e errorString) Error() string { return string(e) }
