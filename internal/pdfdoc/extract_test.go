package pdfdoc

import (
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

// 按 /Length 取流内容, 而不是靠正则找 endstream。
//
// 这条守的是一个真实踩过的坑: 二进制流里出现 0x0D 是常态, 而 `\r?\nendstream` 会把流
// **自己的最后一个字节 0x0D 当成行结束符吃掉** —— 载荷少一字节, zlib 报 unexpected EOF
// (缺的正是 Adler-32 校验和最后一字节), 整个流被丢弃。症状: PDF 完好 (阅读器与
// pdftotext 按 /Length 读都没问题), 只有提取工具偶发"一个字都没有", 概率约 1/256,
// 于是被当成随机假失败, 反复重跑又不复现。
func TestStreamPayloadIgnoresTrailingCarriageReturn(t *testing.T) {
	payload := zlibPayloadEndingWith(t, '\r')

	object := objectBytes(payload)
	got, ok := streamPayload(object)
	if !ok {
		t.Fatal("应能取出流内容")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("载荷应逐字节一致: 期望 %d 字节, 实际 %d 字节 (末尾字节 0x%02X)",
			len(payload), len(got), got[len(got)-1])
	}

	// 解压也要成功 —— 这正是从前失败的地方
	decoded, ok := streamOf(object)
	if !ok {
		t.Fatal("载荷应以 0x0D 结尾时仍能解压")
	}
	if !strings.Contains(string(decoded), "巡检报告") {
		t.Errorf("解出的内容不对: %q", firstBytes(decoded, 40))
	}
}

// 没有 /Length 的对象 (不是本项目的产物) 走兜底路径: 这时"载荷末尾的 0x0D"与
// "endstream 前的 CRLF"无法区分, 必须靠重试把少掉的字节补回来。
func TestStreamPayloadFallsBackWithoutLength(t *testing.T) {
	payload := zlibPayloadEndingWith(t, '\r')
	object := append([]byte("<< /Filter /FlateDecode >>\nstream\n"), payload...)
	object = append(object, []byte("\nendstream\nendobj")...)

	decoded, ok := streamOf(object)
	if !ok {
		t.Fatal("没有 /Length 时也应能解压 (兜底补回 0x0D)")
	}
	if !strings.Contains(string(decoded), "巡检报告") {
		t.Errorf("解出的内容不对: %q", firstBytes(decoded, 40))
	}
}

// 声明长度超出对象 = 文件被截断, 不能凭猜测继续。
func TestStreamPayloadRejectsTruncatedObject(t *testing.T) {
	object := []byte("<< /Length 9999 /Filter /FlateDecode >>\nstream\nshort\nendstream\nendobj")
	if _, ok := streamPayload(object); ok {
		t.Error("声明长度超出实际数据时不该给出载荷")
	}
}

// 生成的文档: 反复渲染都不该出现"提取不到文字"。
//
// 上面的单测盯住根因, 这里盯住端到端 —— 内容流每一次都不同 (时间戳、尾部数据都会变),
// 从前大约每 256 次就有一次踩到 0x0D 结尾的载荷。
func TestExtractTextNeverEmptyAcrossRenders(t *testing.T) {
	set, err := DefaultSet()
	if err != nil {
		t.Fatalf("加载字体失败: %v", err)
	}
	for i := 0; i < 60; i++ {
		document := New(set, Options{Title: "服务器巡检报告"})
		page := document.Current()
		page.Text(60, 80, 16, ColorInk, "服务器巡检报告", true)
		page.Text(60, 110, 10, ColorInk, "主机 web-01 认证失败: 用户名、密码或密钥不正确", false)
		page.Text(60, 130, 10, ColorInk, strings.Repeat("填充内容 ", i%7+1), false)

		data, err := document.Bytes()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		text, err := ExtractText(data)
		if err != nil {
			t.Fatalf("第 %d 次提取失败: %v", i, err)
		}
		if !strings.Contains(text, "认证失败") {
			t.Fatalf("第 %d 次提取不到正文 (共 %d 字符): %q", i, len(text), firstBytes([]byte(text), 60))
		}
	}
}

// zlibPayloadEndingWith 造一段"压缩后以指定字节结尾"的载荷。
//
// zlib 以 Adler-32 校验和结尾, 末字节就是校验和的低字节。逐字节替换内容可以让它取遍
// 全部 256 个余数 —— 注意**不能靠追加空格**来试: 空格是 0x20, 每次让校验和 +32,
// 低字节只在 8 个值里循环, 0x0D 根本取不到 (这个坑当时白花了十几秒才发现)。
func zlibPayloadEndingWith(t *testing.T, last byte) []byte {
	t.Helper()
	base := []byte("服务器巡检报告 · 主机 web-01 · 认证失败: 用户名、密码或密钥不正确")
	for value := 0; value < 256; value++ {
		content := append(append([]byte{}, base...), byte(value))
		var buffer bytes.Buffer
		writer := zlib.NewWriter(&buffer)
		if _, err := writer.Write(content); err != nil {
			t.Fatalf("压缩失败: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("压缩失败: %v", err)
		}
		payload := buffer.Bytes()
		if payload[len(payload)-1] == last {
			return payload
		}
	}
	t.Fatalf("没能造出以 0x%02X 结尾的载荷", last)
	return nil
}

func objectBytes(payload []byte) []byte {
	object := []byte("<< /Length " + itoa(len(payload)) + " /Filter /FlateDecode >>\nstream\n")
	object = append(object, payload...)
	return append(object, []byte("\nendstream\nendobj")...)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
