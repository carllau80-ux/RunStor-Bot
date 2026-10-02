package kb

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/llm"
)

// fakeClient 是可控的模型客户端 (记录收到的提示词, 便于断言"资料确实进了提示词")。
type fakeClient struct {
	name     string
	reply    string
	err      error
	messages []llm.Message
	calls    int
	// replies 非空时按顺序返回 (用于"先扩展词、再回答"这种多次调用)
	replies []string
}

func (f *fakeClient) Name() string { return f.name }
func (f *fakeClient) Chat(messages []llm.Message) (string, error) {
	f.calls++
	f.messages = append(f.messages, messages...)
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) > 0 {
		reply := f.replies[0]
		f.replies = f.replies[1:]
		return reply, nil
	}
	return f.reply, nil
}

func baseWithDocs(t *testing.T) *Base {
	t.Helper()
	base := testBase(t)
	for name, content := range map[string]string{
		"zfs-troubleshooting.md":   zfsDoc,
		"iscsi-troubleshooting.md": iscsiDoc,
	} {
		if _, err := base.Import(name, content); err != nil {
			t.Fatalf("导入失败: %v", err)
		}
	}
	return base
}

// 正常路径: 模型基于资料回答, 引用要能对回文档与章节。
func TestAskWithModel(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "deepseek-chat",
		reply: "先确认受影响的文件清单 [1], 然后执行 `zpool scrub` 修复可修复的块 [1], 最后检查内存与数据线 [1]。"}

	answer := base.Ask(AskOptions{Question: "zpool 校验失败怎么处理", TopK: 3}, client, "deepseek-chat")

	if answer.Mode != "model" {
		t.Fatalf("应为模型回答模式, 实际 %q (%s)", answer.Mode, answer.Note)
	}
	if answer.Model != "deepseek-chat" {
		t.Errorf("应记录模型名: %q", answer.Model)
	}
	if len(answer.Citations) == 0 {
		t.Fatal("应有引用")
	}
	if !strings.Contains(answer.Citations[0].Doc, "ZFS") {
		t.Errorf("引用应指向 ZFS 文档, 实际 %q", answer.Citations[0].Doc)
	}
	if answer.Citations[0].Heading == "" {
		t.Error("引用应带上章节")
	}
	if !answer.Citations[0].Cited {
		t.Error("回答里引用了 [1], 该条应标记为已引用")
	}
	if answer.Note != "" {
		t.Errorf("正常回答不该有告警: %q", answer.Note)
	}
	// 资料必须真的进了提示词, 否则模型只能凭空回答
	if !strings.Contains(client.messages[1].Content, "checksum mismatch") {
		t.Error("提示词里应包含检索到的资料原文")
	}
	if !strings.Contains(client.messages[1].Content, "[1]") {
		t.Error("资料应带编号, 回答才能引用")
	}
	if !strings.Contains(client.messages[0].Content, "只根据给出的资料回答") {
		t.Error("系统提示词必须约束不许超出资料")
	}
}

// 检索不到资料时不能调用模型: 让模型凭常识答运维问题, 编出一条命令就可能造成故障。
func TestAskWithoutMatchDoesNotCallModel(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "deepseek-chat", reply: "应该用 rm -rf 清一下缓存"}
	answer := base.Ask(AskOptions{Question: "午饭吃什么", TopK: 3}, client, "deepseek-chat")

	if answer.Mode != "no_match" {
		t.Fatalf("应报无匹配, 实际 %q", answer.Mode)
	}
	if client.calls != 0 {
		t.Errorf("检索不到资料时不该调用模型, 实际调用 %d 次", client.calls)
	}
	if answer.Answer != "" {
		t.Errorf("无匹配时不该给答案: %q", answer.Answer)
	}
	if !strings.Contains(answer.Note, "没有找到") {
		t.Errorf("应说明没找到相关资料: %q", answer.Note)
	}
	if len(answer.Terms) == 0 {
		t.Error("应给出实际用于检索的词元, 便于用户换个说法")
	}
}

// 没有模型时退化成"只给片段", 而不是报错或装作不知道。
func TestAskWithoutModelReturnsRetrievalOnly(t *testing.T) {
	base := baseWithDocs(t)
	for _, client := range []llm.Client{nil, &fakeClient{name: llm.DemoName}} {
		answer := base.Ask(AskOptions{Question: "iSCSI 登录被拒绝"}, client, "")
		if answer.Mode != "retrieval_only" {
			t.Fatalf("应为仅检索模式, 实际 %q", answer.Mode)
		}
		if len(answer.Citations) == 0 {
			t.Fatal("仍应给出检索到的片段")
		}
		if !strings.Contains(answer.Note, "未配置可用的模型") {
			t.Errorf("应说明原因: %q", answer.Note)
		}
	}
}

// 模型调用失败时保留片段并说明原因 (检索结果本身有价值)。
func TestAskModelFailureDegrades(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", err: errors.New("401 unauthorized")}
	answer := base.Ask(AskOptions{Question: "iSCSI 登录被拒绝"}, client, "m")

	if answer.Mode != "retrieval_only" {
		t.Fatalf("模型失败应退化成仅检索, 实际 %q", answer.Mode)
	}
	if len(answer.Citations) == 0 {
		t.Error("失败时仍应给出检索结果")
	}
	if !strings.Contains(answer.Note, "401") {
		t.Errorf("应带上失败原因: %q", answer.Note)
	}
}

// 模型引用不存在的编号时必须点出来 —— 这比"没引用"更危险: 看起来更有依据。
func TestAskFlagsInventedCitations(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", reply: "按 [1] 和 [9] 处理即可。"}
	answer := base.Ask(AskOptions{Question: "zpool 校验失败", TopK: 2}, client, "m")

	if len(answer.Citations) != 2 {
		t.Fatalf("应检索到 2 条, 实际 %d", len(answer.Citations))
	}
	if !strings.Contains(answer.Note, "[9]") {
		t.Errorf("应点出引用了不存在的编号: %q", answer.Note)
	}
	if !strings.Contains(answer.Note, "没有依据") {
		t.Errorf("应说明该部分没有依据: %q", answer.Note)
	}
	if !answer.Citations[0].Cited {
		t.Error("[1] 是有效引用, 应标记")
	}
}

// 模型没标引用时也要提示, 不能让人以为答案是逐句有据的。
func TestAskWithoutCitationNotice(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", reply: "先执行 zpool scrub, 然后观察 permanent errors。"}
	answer := base.Ask(AskOptions{Question: "zpool 校验失败", TopK: 2}, client, "m")
	if !strings.Contains(answer.Note, "没有标注引用") {
		t.Errorf("应提示未标注引用: %q", answer.Note)
	}
	for _, citation := range answer.Citations {
		if citation.Cited {
			t.Error("没有标注时就都不该算已引用")
		}
	}
}

// 检索词扩展的第一个价值: 用户的话与文档用词完全不搭时也能找到资料。
//
// 这里的问题 "widget 报错了怎么办" 在文档里一个字面词元都对不上, 全靠模型给出的
// 扩展词把 checksum/mismatch/zpool 这些实际用词补进来。
func TestAskExpansionRescuesUnmatchedQuery(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", replies: []string{
		"checksum mismatch\nzpool scrub\nZFS 数据校验\n",
		"资料 [1] 说明了处置步骤。",
	}}
	answer := base.Ask(AskOptions{Question: "widget 报错了怎么办", TopK: 3, Expand: true}, client, "m")

	if len(answer.Expanded) == 0 {
		t.Fatal("应记录模型给出的扩展词")
	}
	if client.calls != 2 {
		t.Fatalf("应调用两次 (扩展 + 回答), 实际 %d", client.calls)
	}
	if answer.Mode != "model" {
		t.Fatalf("扩展后应能检索到资料并回答, 实际 %q (%s)", answer.Mode, answer.Note)
	}
	if len(answer.Citations) == 0 || !strings.Contains(answer.Citations[0].Text, "checksum mismatch") {
		t.Errorf("扩展词应把 ZFS 校验那节召回来: %s", describeCitations(answer.Citations))
	}
}

// 检索词扩展的边界: 扩展词只补充召回, 不能把用户原话已经命中的内容挤下去。
//
// 模型可能给出跑偏的词, 如果扩展词与用户原话等权, 一次跑偏就能让检索结果面目全非。
func TestAskExpansionDoesNotHijackRanking(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", replies: []string{
		"iscsi login\nchap 认证\niscsiadm -m session -R\n",
		"回答 [1]",
	}}
	answer := base.Ask(AskOptions{Question: "zpool 校验失败 (checksum mismatch) 怎么处理",
		TopK: 3, Expand: true}, client, "m")

	if len(answer.Citations) == 0 {
		t.Fatal("应有检索结果")
	}
	if !strings.Contains(answer.Citations[0].Text, "checksum mismatch") {
		t.Errorf("用户原话命中的片段应仍排第一, 实际: %s", describeCitations(answer.Citations))
	}
	if strings.Contains(answer.Citations[0].Text, "CHAP") {
		t.Error("扩展词把无关手册顶到了第一位")
	}
}

// 扩展失败不影响回答 (它只是锦上添花)。
func TestAskExpansionFailureIsSilent(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", err: errors.New("boom")}
	answer := base.Ask(AskOptions{Question: "zpool 校验失败", Expand: true}, client, "m")
	if answer.Mode == "" {
		t.Fatal("扩展失败不该让整个问答失败")
	}
	if len(answer.Citations) == 0 {
		t.Error("仍应给出检索结果")
	}
}

// 知识引用: 巡检报告要能指向手册章节。
func TestLookupKnowledge(t *testing.T) {
	base := baseWithDocs(t)
	refs := base.LookupKnowledge("iSCSI 存储链路认证/授权失败", 2)
	if len(refs) == 0 {
		t.Fatal("应找到相关知识")
	}
	if !strings.Contains(refs[0].Doc, "iSCSI") {
		t.Errorf("应指向 iSCSI 文档, 实际 %q", refs[0].Doc)
	}
	if refs[0].Snippet == "" || refs[0].DocID == "" {
		t.Error("引用要能定位 (文档 ID + 片段)")
	}
	// 空知识库时不该 panic 或返回垃圾
	empty := testBase(t)
	if refs := empty.LookupKnowledge("任意问题", 2); len(refs) != 0 {
		t.Errorf("空库应返回空引用, 实际 %d 条", len(refs))
	}
}

func describeCitations(citations []Citation) string {
	parts := make([]string, 0, len(citations))
	for _, citation := range citations {
		parts = append(parts, fmt.Sprintf("[%d] %.2f %s: %s", citation.Index, citation.Score,
			citation.Doc, Snippet(citation.Text, 40)))
	}
	if len(parts) == 0 {
		return "(无引用)"
	}
	return strings.Join(parts, " | ")
}

// 界面语言为英文时, 提示词也要是英文 —— 否则模型会用中文回答, 英文界面里出现中文段落。
func TestAskUsesEnglishPrompt(t *testing.T) {
	base := baseWithDocs(t)
	client := &fakeClient{name: "m", reply: "Run zpool scrub to repair [1]."}
	answer := base.Ask(AskOptions{Question: "zpool 校验失败", TopK: 2, Lang: i18n.EN}, client, "m")
	if answer.Mode != "model" {
		t.Fatalf("应为模型回答: %s", answer.Note)
	}
	if !strings.Contains(client.messages[0].Content, "Answer only from the given passages") {
		t.Errorf("英文模式应使用英文提示词: %q", client.messages[0].Content[:80])
	}
	// 中文模式仍是中文提示词
	zh := &fakeClient{name: "m", reply: "回答"}
	base.Ask(AskOptions{Question: "zpool 校验失败", TopK: 2, Lang: i18n.ZH}, zh, "m")
	if !strings.Contains(zh.messages[0].Content, "只根据给出的资料回答") {
		t.Error("中文模式应使用中文提示词")
	}
}
