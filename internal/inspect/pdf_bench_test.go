package inspect

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkRenderBigReport(b *testing.B) {
	report := sampleReport()
	var evidence strings.Builder
	for index := 0; index < 900; index++ {
		fmt.Fprintf(&evidence, "Sep 25 05:12:44 web-01 kernel: 日志行 %d\n", index)
	}
	report.Findings = []Finding{{ID: "logs", Name: "严重错误日志", Status: StatusCritical,
		Summary: "发现多条严重日志", Evidence: evidence.String()}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderPDF(report, PDFOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderSmallReport(b *testing.B) {
	report := sampleReport()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := RenderPDF(report, PDFOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
