// Package loganalysis 用模型为"规则没识别出来"的严重日志补上影响分析与处置建议。
//
// 为什么不是全部交给模型: 影响与处置建议是要被人照着执行的, 常见故障 (OOM、磁盘 I/O
// 错误、ZFS 校验失败、iSCSI 断链…) 的签名是确定的, 交给可复现、可评审的规则库更可靠
// (见 internal/inspect/diagnose.go)。模型的价值在于长尾: 那些只在特定业务/特定中间件
// 里出现的日志, 规则库覆盖不到, 模型能凭上下文给出初步判断。
//
// 因此这里的定位是**补充**, 而且报告里会标明结论来自模型 —— 读者有权知道自己在看哪一种。
package loganalysis

import (
	"encoding/json"
	"fmt"
	"strings"

	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/schema"
)

// Options 是分析器的参数。
type Options struct {
	// Client 每次调用时取**当前**生效的模型客户端。
	//
	// 取函数而不是取实例: 模型可以在界面上随时切换 (保存并启用), 分析器要跟着走,
	// 否则一次巡检用的还是进程启动时那个模型。
	Client func() llm.Client
	// MaxGroups 限制一次分析的组数 (每组一条日志特征), 防止把整份日志灌给模型。
	MaxGroups int
	// MaxSampleChars 限制每组样本的字符数。
	MaxSampleChars int
}

// Analyzer 实现 inspect.LogAnalyzer。
type Analyzer struct {
	options Options
}

// New 构造分析器。
func New(options Options) *Analyzer {
	if options.MaxGroups <= 0 {
		options.MaxGroups = 12
	}
	if options.MaxSampleChars <= 0 {
		options.MaxSampleChars = 600
	}
	return &Analyzer{options: options}
}

// Available 报告当前是否真的会调用模型 (demo 客户端不调用)。
func (a *Analyzer) Available() bool {
	return a != nil && a.client() != nil
}

// client 返回当前可用的模型客户端 (不可用时返回 nil)。
func (a *Analyzer) client() llm.Client {
	if a == nil || a.options.Client == nil {
		return nil
	}
	client := a.options.Client()
	if client == nil || client.Name() == llm.DemoName {
		// demo 客户端是关键词规则, 做不了日志分析 —— 让它"分析"只会产出假结论
		return nil
	}
	return client
}

// systemPrompt 是分析用的系统提示词。
//
// 三条硬约束 (都来自踩过的坑):
//   - 只根据给出的日志判断, 不推断没写出来的事实 (任务名/主机/组件名都不要编);
//   - 认不出来就说认不出来, 用"需人工确认", 不要凑一个听起来合理的结论;
//   - 处置建议必须是能在服务器上执行的具体动作, 不要"建议排查"这种空话。
const systemPrompt = `你是资深 Linux 运维工程师, 负责把服务器巡检发现的严重日志翻译成"影响"与"处置建议"。

对每一条日志给出:
- title: 一句话说明这是什么问题 (不超过 30 字, 中文)
- impact: 这个问题会造成什么影响。说清楚"哪个功能会不可用/数据会不会丢/会不会反复发生"。
- actions: 2~4 条处置建议, 每条一个具体动作 (能直接在服务器上执行或明确指向要检查的对象),
  按先后顺序排列。不要写"建议排查"这类空话。
- severity: critical / err 之一

必须遵守:
1. 只根据给出的日志内容判断。日志里没写的信息 (业务名、组件全名、原因) 一律不要编造。
2. 认不出来的日志, title 写"未识别的日志", impact 写"需要结合完整上下文人工确认",
   actions 只给查看上下文的方法, 不要猜原因。
3. 输出严格 JSON, 不要 markdown 代码块, 不要额外解释。

输出格式: {"items":[{"title":"...","impact":"...","actions":["..."],"severity":"err"}]}
items 的顺序与条数必须与输入日志一一对应。`

// systemPromptEN 是英文版 (界面语言为英文时用, 产出也是英文)。
const systemPromptEN = `You are a senior Linux operations engineer. Turn critical log lines found by a
server inspection into an "impact" and "remediation".

For each log line produce:
- title: one sentence saying what the problem is (at most 12 words)
- impact: what this problem causes. Be specific about which function becomes unavailable,
  whether data can be lost, and whether it will recur.
- actions: 2-4 remediation steps, each one concrete action (something that can be run on the
  server or a specific thing to check), in order. No "investigate further" filler.
- severity: either critical or err

Rules you must follow:
1. Judge only from the given log text. Never invent information that is not in the log
   (business names, component names, causes).
2. For logs you cannot identify, set title to "Unrecognized log", impact to
   "needs manual review with the full context", and give only steps for looking up context.
   Do not guess a cause.
3. Output strict JSON, no markdown fences, no extra explanation.

Output format: {"items":[{"title":"...","impact":"...","actions":["..."],"severity":"err"}]}
The number and order of items must match the input logs one for one.`

// systemPromptFor 按语言选提示词。
func systemPromptFor(lang i18n.Lang) string {
	if lang == i18n.EN {
		return systemPromptEN
	}
	return systemPrompt
}

// AnalyzeUnknown 让模型为未识别的日志组补结论。
//
// lang 决定提示词与产出语言: 让模型用中文答, 却在英文界面里显示, 是这套 i18n 里最容易
// 漏掉的一环 —— 界面翻得再好, 模型写的段落仍是中文。
func (a *Analyzer) AnalyzeUnknown(lang i18n.Lang, groups []inspect.Diagnosis) ([]inspect.Diagnosis, error) {
	if !a.Available() || len(groups) == 0 {
		return nil, nil
	}
	if len(groups) > a.options.MaxGroups {
		groups = groups[:a.options.MaxGroups]
	}

	var builder strings.Builder
	builder.WriteString("以下是巡检中发现的、未能被内置规则识别的日志。请逐条分析。\n")
	for index, group := range groups {
		fmt.Fprintf(&builder, "\n#%d (共 %d 条, 来源 %s)\n", index+1, group.Count,
			strings.Join(group.Sources, "/"))
		samples := group.Samples
		if len(samples) == 0 {
			samples = []string{"(无样本)"}
		}
		for _, sample := range samples {
			fmt.Fprintf(&builder, "  %s\n", truncate(sample, a.options.MaxSampleChars))
		}
	}

	content, err := a.client().Chat([]llm.Message{
		{Role: "system", Content: systemPromptFor(lang)},
		{Role: "user", Content: builder.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("模型分析日志失败: %w", err)
	}
	return parseItems(content, len(groups))
}

// parsedItem 是模型返回的一条结论。
type parsedItem struct {
	Title    string   `json:"title"`
	Impact   string   `json:"impact"`
	Actions  []string `json:"actions"`
	Severity string   `json:"severity"`
}

// parseItems 解析模型的输出。
//
// 复用 schema.JSONDecode 而不是自己 json.Unmarshal: 它已经处理了模型爱加的
// ``` 围栏和前后解释文字, 这两样在实际调用里几乎必然出现。
func parseItems(content string, expected int) ([]inspect.Diagnosis, error) {
	decoded, err := schema.JSONDecode(content)
	if err != nil {
		return nil, fmt.Errorf("模型输出不是 JSON: %w", err)
	}
	// 转成结构体而不是手工从 map 里取值: 字段名写错会在这里直接暴露
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("模型输出无法解析: %w", err)
	}
	var payload struct {
		Items []parsedItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("模型输出的字段结构不符: %w", err)
	}
	if len(payload.Items) != expected {
		// 条数对不上就整批丢弃: 报告里少列一条问题比列错更危险
		return nil, fmt.Errorf("模型返回 %d 条结论, 期望 %d 条", len(payload.Items), expected)
	}

	out := make([]inspect.Diagnosis, 0, len(payload.Items))
	for _, item := range payload.Items {
		actions := make([]string, 0, len(item.Actions))
		for _, action := range item.Actions {
			if trimmed := strings.TrimSpace(action); trimmed != "" {
				actions = append(actions, trimmed)
			}
		}
		out = append(out, inspect.Diagnosis{
			Title:    strings.TrimSpace(item.Title),
			Impact:   strings.TrimSpace(item.Impact),
			Actions:  actions,
			Severity: normalizeSeverity(item.Severity),
		})
	}
	return out, nil
}

func normalizeSeverity(value string) string {
	if strings.TrimSpace(value) == "critical" {
		return "critical"
	}
	return "err"
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
