package pdfdoc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testDocument(t *testing.T) *Document {
	t.Helper()
	set, err := DefaultSet()
	if err != nil {
		t.Fatalf("加载内置字体失败: %v", err)
	}
	return New(set, Options{
		Title:     "服务器巡检报告",
		CreatedAt: time.Date(2026, 9, 25, 16, 30, 0, 0, time.FixedZone("CST", 8*3600)),
	})
}

func TestProducesParseableStructure(t *testing.T) {
	document := testDocument(t)
	page := document.Current()
	page.Text(60, 80, 16, ColorInk, "服务器巡检报告", true)
	page.Text(60, 110, 10, ColorInk, "主机 web-01 磁盘 /data 使用率 95%", false)
	page.Line(60, 120, 500, 120, 0.5, ColorRule)
	page.Rect(60, 130, 200, 40, &ColorPanel, nil, 0)
	document.AddPage().Text(60, 80, 10, ColorInk, "第二页", false)

	data, err := document.Bytes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	pdf := string(data)
	if !strings.HasPrefix(pdf, "%PDF-1.4") {
		t.Fatalf("缺少 PDF 头: %q", firstBytes(data, 20))
	}
	if !strings.HasSuffix(pdf, "%%EOF\n") {
		t.Error("缺少文件结尾标记")
	}

	// 交叉引用表的每一项都必须真的指向 "N 0 obj"
	objects := parseObjects(t, data)
	if len(objects) < 8 {
		t.Fatalf("对象太少: %d", len(objects))
	}
	for number, offset := range objects {
		expect := fmt.Sprintf("%d 0 obj", number)
		if !strings.HasPrefix(pdf[offset:], expect) {
			t.Errorf("对象 %d 的 xref 偏移不对: 该处是 %q", number, firstBytes(data[offset:], 24))
		}
	}
	// 目录、页树、每页、字体对象都要在
	for _, want := range []string{"/Type /Catalog", "/Type /Pages", "/Subtype /Type0",
		"/Subtype /CIDFontType2", "/FontFile2", "/ToUnicode", "/CIDToGIDMap /Identity"} {
		if !strings.Contains(pdf, want) {
			t.Errorf("PDF 里缺少 %q", want)
		}
	}
	if count := strings.Count(pdf, "/Type /Page "); count != 2 {
		t.Errorf("应有 2 个页面对象, 实际 %d", count)
	}
	if !strings.Contains(pdf, "/Count 2") {
		t.Error("页树的 /Count 应为 2")
	}
}

// 文本必须能被提取出来 —— 否则报告里的日志片段没法复制到工单里。
// 这里用自己实现的提取器解 ToUnicode, 等于从"阅读器那一侧"验证映射是否正确。
func TestTextCanBeExtracted(t *testing.T) {
	document := testDocument(t)
	want := []string{
		"主机 web-01 系统 Debian GNU/Linux 13",
		"/data 使用率 95% 已接近写满",
		"a1B2c3",
	}
	for index, text := range want {
		document.Current().Text(60, 80+float64(index)*20, 10, ColorInk, text, false)
	}
	data, err := document.Bytes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	extracted := extractTextHelper(t, data)
	for _, text := range want {
		if !strings.Contains(extracted, text) {
			t.Errorf("提取出的文本里没有 %q\n实际: %s", text, extracted)
		}
	}
}

// 页数、页码与翻页: 内容超出一页时必须真的加页, 且每页都有内容流。
func TestPaginationAcrossPages(t *testing.T) {
	document := testDocument(t)
	flow := NewFlow(document, 56, 70, 70)
	footers := 0
	flow.Footer = func(page *Page, index int) {
		footers++
		page.Text(56, document.Height()-40, 8, ColorMuted,
			fmt.Sprintf("第 %d 页", index+1), false)
	}
	for index := 0; index < 120; index++ {
		flow.Paragraph(fmt.Sprintf("第 %d 行: 这是一段用于验证自动翻页的文本, 内容重复以填满多页。", index+1),
			Style{Size: 10})
	}
	flow.Close()

	if document.PageCount() < 3 {
		t.Fatalf("120 行应排出多页, 实际只有 %d 页", document.PageCount())
	}
	if footers != document.PageCount() {
		t.Errorf("每页都该有页脚: 页数 %d, 页脚 %d", document.PageCount(), footers)
	}
	data, err := document.Bytes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("/Count %d", document.PageCount())) {
		t.Error("页树的 /Count 与实际页数不符")
	}
}

// 折行: 不超出右边界 (否则文字会被裁掉), 且中文按字断、西文按词断。
func TestWrapRespectsWidthAndRules(t *testing.T) {
	document := testDocument(t)
	flow := NewFlow(document, 56, 70, 70)
	const size = 10.0
	width := 200.0

	cases := []string{
		"磁盘分区 /data 使用率已达 95%, 已经接近写满, 建议尽快扩容或者清理历史归档文件。",
		strings.Repeat("verylongpathsegment", 8),
		"这是一段没有空格的中文文本用来验证逐字折行是否正确工作以及行首禁则是否生效。",
		"内存可用 4.5 GB / 7.8 GB, 已用 41.2%",
	}
	for _, text := range cases {
		lines := flow.Wrap(text, size, width)
		for index, line := range lines {
			if measured := document.set.Measure(line, size); measured > width+0.01 {
				t.Errorf("第 %d 行超出边界: %.2f > %.2f (%q)", index, measured, width, line)
			}
			runes := []rune(line)
			if len(runes) > 0 && isLineHeadForbidden(runes[0]) {
				t.Errorf("第 %d 行以收尾标点开头: %q", index, line)
			}
		}
	}

	// 没有空格的长路径也要硬断, 而且不能丢字符
	lines := flow.Wrap("路径/非常/长的/一段/中文路径/用于/验证/硬断", size, width)
	if len(lines) < 2 {
		t.Error("超宽文本应折成多行")
	}
	joined := strings.Join(lines, "")
	if joined != "路径/非常/长的/一段/中文路径/用于/验证/硬断" {
		t.Errorf("折行不该丢字符或改字符: %q", joined)
	}
}

// 两页的 /W 宽度表、Content 长度等都要自洽: 逐个内容流解压检查。
func TestContentStreamsAreValid(t *testing.T) {
	document := testDocument(t)
	document.Current().Text(60, 80, 10, ColorInk, "第一页内容", false)
	document.AddPage().Text(60, 80, 10, ColorInk, "第二页内容", false)
	data, err := document.Bytes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	streams := decodedStreams(data)
	if len(streams) < 2 {
		t.Fatalf("应至少有 2 个内容流 + 字体流, 实际 %d", len(streams))
	}
	content := 0
	for _, payload := range streams {
		text := string(payload)
		if strings.Contains(text, "BT") && strings.Contains(text, "/F") {
			content++
			if !strings.Contains(text, "ET") {
				t.Error("内容流里有未闭合的 BT/ET")
			}
		}
	}
	if content != 2 {
		t.Errorf("应有两个页面的文本内容流, 实际 %d", content)
	}
}

// 缺字形的字符要换成替代符号, 而不是渲染成空白; 并且要能被统计出来告知用户。
func TestMissingGlyphsBecomeReplacement(t *testing.T) {
	document := testDocument(t)
	// U+E000 是私用区, 内置字体不会有它的字形
	text := "正常  结束"
	document.Current().Text(60, 80, 10, ColorInk, text, false)
	data, err := document.Bytes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	extracted := extractTextHelper(t, data)
	if strings.Contains(extracted, "\uE000") {
		t.Error("缺字形的字符不该原样出现在文本里")
	}
	if !strings.Contains(extracted, "□") && !strings.Contains(extracted, "?") {
		t.Errorf("缺字形应替换为可见符号, 实际提取到: %q", extracted)
	}

	missing := document.Set().MissingGlyphs(text)
	if len(missing) != 1 || missing[0] != '\uE000' {
		t.Errorf("应统计出 1 个无法显示的字符, 实际 %v", missing)
	}
}

// ---------------------------------------------------------------------------
// 测试用的 PDF 解析 (与生成代码独立实现, 免得同错同对)
// ---------------------------------------------------------------------------

var xrefEntryPattern = regexp.MustCompile(`(?m)^(\d{10}) (\d{5}) ([nf]) \n`)

// bfcharPattern 匹配 ToUnicode CMap 里的一行映射: <字形编号> <目标字符的 UTF-16BE>

// parseObjects 解析交叉引用表, 返回 对象号 → 文件偏移。
func parseObjects(t *testing.T, data []byte) map[int]int {
	t.Helper()
	pdf := string(data)
	startIndex := strings.LastIndex(pdf, "startxref")
	if startIndex < 0 {
		t.Fatal("没有 startxref")
	}
	var xrefOffset int
	if _, err := fmt.Sscanf(pdf[startIndex:], "startxref\n%d", &xrefOffset); err != nil {
		t.Fatalf("解析 startxref 失败: %v", err)
	}
	if xrefOffset <= 0 || xrefOffset >= len(pdf) {
		t.Fatalf("startxref 偏移越界: %d", xrefOffset)
	}
	section := pdf[xrefOffset:]
	matches := xrefEntryPattern.FindAllStringSubmatch(section, -1)
	if len(matches) == 0 {
		t.Fatalf("xref 表里没有条目: %q", firstBytes([]byte(section), 60))
	}
	objects := map[int]int{}
	for index, match := range matches {
		if match[3] != "n" {
			continue // 第 0 项是空闲项
		}
		offset, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("xref 偏移不是数字: %q", match[1])
		}
		objects[index] = offset
	}
	return objects
}

func firstBytes(data []byte, count int) string {
	if len(data) < count {
		count = len(data)
	}
	return string(data[:count])
}

// 一行超长文本也要能在合理时间内折行。
//
// 日志里完全可能出现几万字符的单行 (某些程序把整个响应打成一行)。折行函数里曾经
// 每次循环都重新把整串转成 rune, 于是这种输入退化成 O(n²) —— 实测 4 万字符要十几秒,
// 而它就在导出 PDF 的请求路径上。
func TestWrapHandlesVeryLongSingleLine(t *testing.T) {
	document := testDocument(t)
	flow := NewFlow(document, 56, 70, 70)
	long := strings.Repeat("kernel: 这是一条被打成一行的超长日志 1234567890 abcdefghij ", 900)
	if len([]rune(long)) < 40000 {
		t.Fatalf("测试数据太短: %d 字符", len([]rune(long)))
	}

	started := time.Now()
	lines := flow.Wrap(long, 10, 400)
	elapsed := time.Since(started)
	if elapsed > 5*time.Second {
		t.Fatalf("折行耗时 %s, 退化成平方复杂度了", elapsed)
	}
	// 不能丢字符 (空白处可能被裁剪, 所以比对去空白后的结果)
	joined := strings.Join(lines, "")
	if stripSpaces(joined) != stripSpaces(long) {
		t.Error("折行后字符内容发生了变化")
	}
	for _, line := range lines {
		if measured := document.set.Measure(line, 10); measured > 400.01 {
			t.Fatalf("有行超出宽度: %.2f", measured)
		}
	}
}

func stripSpaces(value string) string {
	return strings.Join(strings.Fields(value), "")
}

// extractTextHelper 是测试里的薄封装 (实现见 pdfdoc.ExtractText)。
func extractTextHelper(t *testing.T, data []byte) string {
	t.Helper()
	text, err := ExtractText(data)
	if err != nil {
		t.Fatalf("提取文本失败: %v", err)
	}
	return text
}
