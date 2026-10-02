package loganalysis

import (
	"backup-restore-agent/internal/i18n"
	"errors"
	"strings"
	"testing"

	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/llm"
)

// fakeClient 是一个可控的模型客户端。
type fakeClient struct {
	name     string
	reply    string
	err      error
	messages []llm.Message
}

func (f *fakeClient) Name() string { return f.name }
func (f *fakeClient) Chat(messages []llm.Message) (string, error) {
	f.messages = messages
	return f.reply, f.err
}

func groups() []inspect.Diagnosis {
	return []inspect.Diagnosis{{
		Origin: inspect.OriginUnknown, Title: "未识别的严重日志", Count: 3,
		Sources: []string{"journald"},
		Samples: []string{"whateverd[42]: widget rebalance deferred (code 17)"},
	}}
}

// 模型给出的结论要能解析, 并保留原有的条数/来源/样本。
func TestAnalyzeUnknownParsesModelReply(t *testing.T) {
	client := &fakeClient{name: "deepseek-chat", reply: `{"items":[{"title":"组件重平衡失败",` +
		`"impact":"重平衡期间请求会变慢, 严重时超时。","actions":["查看 widgetd 完整日志","确认配置上限"],` +
		`"severity":"err"}]}`}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	if !analyzer.Available() {
		t.Fatal("配了真实模型就该可用")
	}

	result, err := analyzer.AnalyzeUnknown(i18n.ZH, groups())
	if err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("应返回 1 条, 实际 %d", len(result))
	}
	if result[0].Title != "组件重平衡失败" || len(result[0].Actions) != 2 {
		t.Errorf("解析结果不对: %+v", result[0])
	}
	// 提示词里要带上原始日志, 否则模型只能凭空猜
	if len(client.messages) != 2 {
		t.Fatalf("应为 system+user 两条消息, 实际 %d", len(client.messages))
	}
	if !strings.Contains(client.messages[1].Content, "widget rebalance deferred") {
		t.Error("提示词里应包含原始日志")
	}
	if !strings.Contains(client.messages[0].Content, "不要编造") {
		t.Error("系统提示词里必须有'不许编造'的约束")
	}
}

// 条数对不上就整批丢弃: 报告里漏掉一条问题比不分析更危险。
func TestAnalyzeUnknownRejectsIncompleteReply(t *testing.T) {
	client := &fakeClient{name: "m", reply: `{"items":[{"title":"a","impact":"b","actions":["c"]}]}`}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	// 输入两组, 模型只回一条
	two := append(groups(), groups()[0])
	if _, err := analyzer.AnalyzeUnknown(i18n.ZH, two); err == nil {
		t.Fatal("条数不匹配应报错, 不能只采用其中一部分")
	}
}

// 模型返回带 ``` 围栏或前后解释文字时也要能解析 (实际调用里几乎必然出现)。
func TestAnalyzeUnknownToleratesFencedReply(t *testing.T) {
	client := &fakeClient{name: "m", reply: "好的, 分析如下:\n```json\n" +
		`{"items":[{"title":"t","impact":"i","actions":["a1"],"severity":"critical"}]}` + "\n```\n以上。\n"}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	result, err := analyzer.AnalyzeUnknown(i18n.ZH, groups())
	if err != nil {
		t.Fatalf("应容忍围栏: %v", err)
	}
	if len(result) != 1 || result[0].Title != "t" {
		t.Fatalf("解析结果不对: %+v", result)
	}
	if result[0].Severity != "critical" {
		t.Errorf("severity 应保留: %q", result[0].Severity)
	}
}

// demo 客户端不能用来做日志分析: 它是关键词规则, 只会产出看起来像分析的假结论。
func TestDemoClientIsNotUsed(t *testing.T) {
	client := &fakeClient{name: llm.DemoName, reply: `{"items":[]}`}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	if analyzer.Available() {
		t.Fatal("demo 模式下不该声称能做模型分析")
	}
	result, err := analyzer.AnalyzeUnknown(i18n.ZH, groups())
	if err != nil || result != nil {
		t.Errorf("demo 模式应直接返回空, 实际 %v / %v", result, err)
	}
	if client.messages != nil {
		t.Error("demo 模式下不该真的调用模型")
	}
}

// 没有模型时同样跳过, 而不是报错。
func TestNoClientIsSafe(t *testing.T) {
	analyzer := New(Options{})
	if analyzer.Available() {
		t.Error("没配模型时不该可用")
	}
	if _, err := analyzer.AnalyzeUnknown(i18n.ZH, groups()); err != nil {
		t.Errorf("没配模型时不该报错: %v", err)
	}
}

// 模型调用失败要把错误带出去 (报告里会记下原因)。
func TestModelFailureIsReported(t *testing.T) {
	client := &fakeClient{name: "m", err: errors.New("401 unauthorized")}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	if _, err := analyzer.AnalyzeUnknown(i18n.ZH, groups()); err == nil {
		t.Fatal("模型失败应返回错误")
	}
}

// 组数要有上限: 不能把整份日志灌给模型。
func TestGroupLimit(t *testing.T) {
	var many []inspect.Diagnosis
	for index := 0; index < 50; index++ {
		many = append(many, inspect.Diagnosis{Title: "x", Count: 1, Samples: []string{"line"}})
	}
	client := &fakeClient{name: "m", reply: `{"items":[]}`}
	analyzer := New(Options{Client: func() llm.Client { return client }, MaxGroups: 3})
	// 条数不匹配会报错, 但这里关心的是"发出去的只有 3 组"
	_, _ = analyzer.AnalyzeUnknown(i18n.ZH, many)
	if got := strings.Count(client.messages[1].Content, "#"); got > 3 {
		t.Errorf("提示词里最多 3 组, 实际 %d", got)
	}
}

// 英文模式要用英文提示词: 界面翻好了、模型却用中文写结论, 是这套 i18n 最容易漏的一环。
func TestAnalyzerUsesEnglishPrompt(t *testing.T) {
	client := &fakeClient{name: "m", reply: `{"items":[{"title":"t","impact":"i","actions":["a"],"severity":"err"}]}`}
	analyzer := New(Options{Client: func() llm.Client { return client }})
	if _, err := analyzer.AnalyzeUnknown(i18n.EN, groups()); err != nil {
		t.Fatalf("分析失败: %v", err)
	}
	if !strings.Contains(client.messages[0].Content, "You are a senior Linux operations engineer") {
		t.Errorf("英文模式应使用英文提示词: %q", client.messages[0].Content[:80])
	}
	if !strings.Contains(client.messages[0].Content, "Unrecognized log") {
		t.Error("英文提示词里也要有'认不出来就说认不出来'的约束")
	}
	zh := &fakeClient{name: "m", reply: `{"items":[{"title":"t","impact":"i","actions":["a"]}]}`}
	zhAnalyzer := New(Options{Client: func() llm.Client { return zh }})
	if _, err := zhAnalyzer.AnalyzeUnknown(i18n.ZH, groups()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zh.messages[0].Content, "你是资深 Linux 运维工程师") {
		t.Error("中文模式应使用中文提示词")
	}
}
