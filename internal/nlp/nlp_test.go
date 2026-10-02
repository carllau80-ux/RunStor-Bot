package nlp

import (
	"strings"
	"testing"
)

func TestParseClock(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"凌晨2点", "02:00"},
		{"凌晨1点", "01:00"},
		{"每天凌晨2点做全量备份", "02:00"},
		{"下午3点", "15:00"},
		{"下午3点半", "15:30"},
		{"晚上8点15分", "20:15"},
		{"晚上8点", "20:00"},
		{"上午9点", "09:00"},
		{"中午12点", "12:00"},
		{"中午1点", "13:00"},
		{"凌晨12点", "00:00"},
		{"23:30", "23:30"},
		{"每天23:30做全量", "23:30"},
		{"01:00", "01:00"},
		{"12点时", "12:00"},
		{"都是凌晨1点", "01:00"},
	}
	for _, entry := range cases {
		got, ok := ParseClock(entry.text)
		if !ok {
			t.Errorf("ParseClock(%q) 解析失败", entry.text)
			continue
		}
		if got != entry.want {
			t.Errorf("ParseClock(%q) = %q, 期望 %q", entry.text, got, entry.want)
		}
	}
}

func TestParseClockRejectsNonTime(t *testing.T) {
	for _, text := range []string{"每天备份", "保留30天", "并发4", ""} {
		if got, ok := ParseClock(text); ok {
			t.Errorf("ParseClock(%q) 不该解析出时刻, 得到 %q", text, got)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		text  string
		value int
		unit  string
	}{
		{"保留30天", 30, "days"},
		{"保留 14 天", 14, "days"},
		{"保留7天", 7, "days"},
		{"保留2周", 2, "weeks"},
		{"保留6个月", 6, "months"},
		{"保留1年", 1, "years"},
		{"保留48小时", 48, "hours"},
	}
	for _, entry := range cases {
		got, ok := ParseDuration(entry.text)
		if !ok {
			t.Errorf("ParseDuration(%q) 解析失败", entry.text)
			continue
		}
		if got.Value != entry.value || got.Unit != entry.unit {
			t.Errorf("ParseDuration(%q) = %+v, 期望 {%d %s}",
				entry.text, got, entry.value, entry.unit)
		}
	}
}

// "个月" 必须优先于 "月", 否则 "6个月" 会被解析成 6 月。
func TestParseDurationPrefersLongerUnit(t *testing.T) {
	got, ok := ParseDuration("保留6个月")
	if !ok || got.Value != 6 || got.Unit != "months" {
		t.Fatalf("6个月 解析错误: %+v ok=%v", got, ok)
	}
	if strings.Contains(got.Unit, "月月") {
		t.Fatal("单位匹配异常")
	}
}

func TestParseBool(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"是", true},
		{"需要", true},
		{"要", true},
		{"开启", true},
		{"不", false},
		{"不要", false},
		{"不需要", false},
		{"关闭", false},
		{"否", false},
	}
	for _, entry := range cases {
		got, ok := ParseBool(entry.text)
		if !ok {
			t.Errorf("ParseBool(%q) 解析失败", entry.text)
			continue
		}
		if got != entry.want {
			t.Errorf("ParseBool(%q) = %v, 期望 %v", entry.text, got, entry.want)
		}
	}
}

func TestParseWeekday(t *testing.T) {
	cases := map[string]int{
		"周天":    7,
		"周日":    7,
		"星期天":   7,
		"周一":    1,
		"周五":    5,
		"每周三":   3,
		"周天全备份": 7,
	}
	for text, want := range cases {
		got, ok := ParseWeekday(text)
		if !ok || got != want {
			t.Errorf("ParseWeekday(%q) = %d ok=%v, 期望 %d", text, got, ok, want)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"恢复到2026-09-25 10:30", "2026-09-25 10:30:00"},
		{"2026/09/25", "2026-09-25 00:00:00"},
		{"2026年9月25日", "2026-09-25 00:00:00"},
		{"2026-09-25 10点30分", "2026-09-25 10:30:00"},
	}
	for _, entry := range cases {
		got, ok := ParseTimestamp(entry.text)
		if !ok {
			t.Errorf("ParseTimestamp(%q) 解析失败", entry.text)
			continue
		}
		if got != entry.want {
			t.Errorf("ParseTimestamp(%q) = %q, 期望 %q", entry.text, got, entry.want)
		}
	}
}

func TestPaths(t *testing.T) {
	got := Paths("备份 /data/app 和 /etc/app 到 /u01/backup/orcl")
	want := []string{"/data/app", "/etc/app", "/u01/backup/orcl"}
	if len(got) != len(want) {
		t.Fatalf("Paths = %v, 期望 %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("Paths[%d] = %q, 期望 %q", index, got[index], want[index])
		}
	}
}

func TestPathsIgnoresSingleSegment(t *testing.T) {
	// "/" 与单级路径不该被抓成目录
	if got := Paths("读写比例 rw/ro 与 /tmp"); len(got) != 0 {
		t.Errorf("不该抓出路径, 得到 %v", got)
	}
}

func TestFirstIP(t *testing.T) {
	if got, ok := FirstIP("恢复到 192.0.2.11 上"); !ok || got != "192.0.2.11" {
		t.Fatalf("FirstIP 失败: %q ok=%v", got, ok)
	}
	if _, ok := FirstIP("没有地址"); ok {
		t.Fatal("不该识别出 IP")
	}
}

func TestParseList(t *testing.T) {
	got := ParseList("db1、db2, db3 db4")
	if len(got) != 4 {
		t.Fatalf("ParseList = %v", got)
	}
}

// 用户原句里的时刻关联必须能被逐句解析出来 —— 这是验收用例的前半段。
func TestParseClockOnAcceptanceSentence(t *testing.T) {
	sentence := "周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4"
	if _, ok := ParseClock(sentence); ok {
		t.Error("该句没有时刻, 不该解析出时刻")
	}
	weekday, ok := ParseWeekday(sentence)
	if !ok || weekday != 7 {
		t.Errorf("应识别出周日(7), 得到 %d ok=%v", weekday, ok)
	}
}
