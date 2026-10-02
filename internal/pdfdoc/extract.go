package pdfdoc

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ExtractText 从生成的 PDF 里把文字还原出来 (按内容流顺序, 一行一个 BT 块)。
//
// 用途是**校验与诊断**, 不是阅读器: 一份报告"能看"和"文字能被复制/被 pdftotext 提取"
// 是两件事, 而后者是运维报告的实际需求 (把日志片段粘到工单里)。有了这个函数, 测试与
// 部署自检就能直接断言"生成出来的文字是对的", 而不必依赖外部工具。
//
// 关键是**按字体分别建映射**: 每份嵌入字体有自己的字形编号空间, 同一个编号在
// DejaVu 里可能是字母 t, 在 Droid 里却是"《"。把这些映射合成一张表就会张冠李戴
// (测试里曾经因此误判成渲染有 bug, 实际是提取错了)。
func ExtractText(data []byte) (string, error) {
	fontMaps, err := buildFontMaps(data)
	if err != nil {
		return "", err
	}
	if len(fontMaps) == 0 {
		return "", fmt.Errorf("PDF 里没有可用的 ToUnicode 映射")
	}

	var builder strings.Builder
	for _, payload := range decodedStreams(data) {
		text := string(payload)
		for _, block := range textBlockPattern.FindAllStringSubmatch(text, -1) {
			current := ""
			for _, match := range showTextPattern.FindAllStringSubmatch(block[1], -1) {
				if name := match[1]; name != "" {
					current = name
					continue
				}
				hex := match[2]
				table := fontMaps[current]
				for index := 0; index+4 <= len(hex); index += 4 {
					value, err := strconv.ParseUint(hex[index:index+4], 16, 32)
					if err != nil {
						return "", fmt.Errorf("内容流里的字形编号不是十六进制: %q", hex)
					}
					if decoded, ok := table[uint16(value)]; ok {
						builder.WriteString(decoded)
					}
				}
			}
			builder.WriteString("\n")
		}
	}
	return builder.String(), nil
}

var (
	streamLengthPattern = regexp.MustCompile(`/Length\s+(\d+)`)
	textBlockPattern    = regexp.MustCompile(`(?s)BT(.*?)ET`)
	showTextPattern     = regexp.MustCompile(`/(F\d+)\s+[\d.]+\s+Tf|<([0-9A-Fa-f]+)>\s*Tj`)
	bfcharPattern       = regexp.MustCompile(`<([0-9A-Fa-f]{4})>\s*<([0-9A-Fa-f]+)>`)
	typeZeroPattern     = regexp.MustCompile(`/Type\s*/Font\s*/Subtype\s*/Type0[^>]*?/ToUnicode\s+(\d+)\s+0\s+R`)
	fontDictPattern     = regexp.MustCompile(`/(F\d+)\s+(\d+)\s+0\s+R`)
	objectPattern       = regexp.MustCompile(`(?s)(\d+)\s+0\s+obj(.*?)endobj`)
	toUnicodeRefPat     = regexp.MustCompile(`/ToUnicode\s+(\d+)\s+0\s+R`)
)

// buildFontMaps 建立 资源名 (/F0 …) → (字形编号 → 字符) 的映射。
//
// 走的是 PDF 自己的引用链: 页资源里的 /F0 指向字体对象, 字体对象里的 /ToUnicode
// 指向 CMap 流。
func buildFontMaps(data []byte) (map[string]map[uint16]string, error) {
	objects := map[string][]byte{} // 对象号 → 对象片段 (含 dict 与流)
	for _, match := range objectPattern.FindAllSubmatch(data, -1) {
		objects[string(match[1])] = match[2]
	}

	// 资源名 → 字体对象号 (各页资源里的 /Font << /F0 N 0 R >>; 这里按整份文件收集,
	// 生成的文档里同名资源指向同一份字体)
	nameToObject := map[string]string{}
	for _, match := range fontDictPattern.FindAllSubmatch(data, -1) {
		name, object := string(match[1]), string(match[2])
		body := objects[object]
		if bytes.Contains(body, []byte("/Subtype /Type0")) || bytes.Contains(body, []byte("/Subtype/Type0")) {
			nameToObject[name] = object
		}
	}

	out := map[string]map[uint16]string{}
	for name, object := range nameToObject {
		body := objects[object]
		ref := toUnicodeRefPat.FindSubmatch(body)
		if ref == nil {
			// 兜底: 有些写法里 /ToUnicode 出现在对象里但没有被上面的正则捕获
			if alt := typeZeroPattern.FindSubmatch(body); alt != nil {
				ref = alt
			} else {
				continue
			}
		}
		cmapObject := objects[string(ref[1])]
		payload, ok := streamOf(cmapObject)
		if !ok {
			continue
		}
		out[name] = parseToUnicodeCMap(string(payload))
	}
	return out, nil
}

// parseToUnicodeCMap 解析 bfchar 映射。
func parseToUnicodeCMap(cmap string) map[uint16]string {
	table := map[uint16]string{}
	for _, match := range bfcharPattern.FindAllStringSubmatch(cmap, -1) {
		glyph, err := strconv.ParseUint(match[1], 16, 32)
		if err != nil {
			continue
		}
		if decoded, ok := decodeUTF16Hex(match[2]); ok {
			table[uint16(glyph)] = decoded
		}
	}
	return table
}

// streamOf 取对象片段里的流内容 (已解压)。
func streamOf(object []byte) ([]byte, bool) {
	payload, ok := streamPayload(object)
	if !ok {
		return nil, false
	}
	if !bytes.Contains(object, []byte("/FlateDecode")) {
		return payload, true
	}
	return inflate(payload)
}

// streamPayload 取对象里的流内容 (尚未解压), 按对象自己声明的 /Length 切片。
//
// 为什么不用"全文扫 stream…endstream": 二进制流里出现 0x0D 是常态, 而正则里的
// `\r?\nendstream` 会把流自己的**最后一个字节 0x0D 当成行结束符吃掉** —— 载荷少一个
// 字节, zlib 报 unexpected EOF (缺的正是 Adler-32 校验和的最后一字节), 于是整个流被
// 丢掉。症状极具误导性: PDF 本身完好 (阅读器与 pdftotext 都按 /Length 读), 只有提取
// 工具偶发"一个字都没有", 概率约 1/256, 看起来就像随机的假失败。
func streamPayload(object []byte) ([]byte, bool) {
	index := bytes.Index(object, []byte("stream"))
	if index < 0 {
		return nil, false
	}
	rest := object[index+len("stream"):]
	// stream 关键字之后必须是行结束符
	switch {
	case bytes.HasPrefix(rest, []byte("\r\n")):
		rest = rest[2:]
	case bytes.HasPrefix(rest, []byte("\n")):
		rest = rest[1:]
	default:
		return nil, false
	}

	if length, ok := streamLength(object); ok {
		if length > len(rest) {
			// 声明的长度超出对象: 文件被截断, 不能凭猜测继续
			return nil, false
		}
		return rest[:length], true
	}

	// 没有 /Length 的罕见情况 (不是本项目的产物): 退回找 endstream 标记。
	// 这时"载荷末尾的 0x0D"与"endstream 前的 CRLF"无法区分, 交给 inflate 兜底重试。
	if end := bytes.Index(rest, []byte("\nendstream")); end >= 0 {
		payload := rest[:end]
		if bytes.HasSuffix(payload, []byte("\r")) {
			payload = payload[:len(payload)-1]
		}
		return payload, true
	}
	return nil, false
}

// streamLength 读对象字典里的 /Length N。
func streamLength(object []byte) (int, bool) {
	match := streamLengthPattern.FindSubmatch(object)
	if match == nil {
		return 0, false
	}
	value, err := strconv.Atoi(string(match[1]))
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// inflate 解压 zlib 数据。
//
// 第一次失败且载荷来自"没有 /Length"的兜底路径时, 补一个 0x0D 再试一次:
// 那条路径可能少吃了一个属于载荷的 \r (见 streamPayload)。载荷本身完好时第一次就会
// 成功, 因此这个重试只会在真正需要时生效。
func inflate(payload []byte) ([]byte, bool) {
	if out, ok := zlibRead(payload); ok {
		return out, true
	}
	return zlibRead(append(append([]byte{}, payload...), '\r'))
}

func zlibRead(payload []byte) ([]byte, bool) {
	reader, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, false
	}
	out, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, false
	}
	return out, true
}

// decodedStreams 解出 PDF 里所有 FlateDecode 流的内容。
func decodedStreams(data []byte) [][]byte {
	var out [][]byte
	for _, match := range objectPattern.FindAllSubmatch(data, -1) {
		if !bytes.Contains(match[0], []byte("/FlateDecode")) {
			continue
		}
		if payload, ok := streamOf(match[0]); ok {
			out = append(out, payload)
		}
	}
	return out
}

// decodeUTF16Hex 把 UTF-16BE 的十六进制串还原成字符串。
func decodeUTF16Hex(hex string) (string, bool) {
	if len(hex)%4 != 0 {
		return "", false
	}
	units := make([]uint16, 0, len(hex)/4)
	for index := 0; index+4 <= len(hex); index += 4 {
		value, err := strconv.ParseUint(hex[index:index+4], 16, 32)
		if err != nil {
			return "", false
		}
		units = append(units, uint16(value))
	}
	return string(utf16.Decode(units)), true
}
