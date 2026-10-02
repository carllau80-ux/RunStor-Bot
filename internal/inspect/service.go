package inspect

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"backup-restore-agent/internal/fileio"
	"backup-restore-agent/internal/i18n"
)

// Options 是巡检服务的参数。
type Options struct {
	SSH SSHOptions
	// LogAnalyzer 非空时, 用它为规则未覆盖的日志补影响分析与处置建议。
	LogAnalyzer LogAnalyzer
	// AnalyzeLogs 是默认是否做模型分析 (单次巡检可用 RunOptions 覆盖)。
	AnalyzeLogs bool
	// Knowledge 非空时, 给每个问题附上知识库 (运维手册) 里的相关章节。
	Knowledge KnowledgeLookup
	// CommandTimeout 是远端单项检查的超时 (秒), 会写进采集脚本。
	CommandTimeout int
	// Concurrency 限制同时巡检的主机数。
	Concurrency int
	Thresholds  Thresholds
	// StorePath 是巡检历史的落盘位置; 空表示仅内存。
	StorePath string
	// MaxReports 限制保留的历史条数。
	MaxReports int
}

// Service 编排巡检: 多主机并发, 汇总成报告并留存历史。
//
// 注意凭据的生命周期: Target 里的密码/私钥只在本方法调用期间存在, 既不写进报告,
// 也不写进历史, 更不写日志。
type Service struct {
	options Options
	store   *store
	// dialer 可注入, 便于测试; 为空时使用真实 SSH。
	dialer func(Target) (Runner, func(), error)
}

// NewService 构造巡检服务。
func NewService(options Options) *Service {
	if options.Concurrency <= 0 {
		options.Concurrency = 4
	}
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = 15
	}
	// 让连接层也知道单项超时: 它要据此推算一次执行的总预算 (没有总预算的话,
	// 在认证阶段卡住的主机会把请求永远挂着)。配置里只写了秒数, 这里换算一次。
	if options.SSH.CommandTimeout <= 0 {
		options.SSH.CommandTimeout = time.Duration(options.CommandTimeout) * time.Second
	}
	if options.MaxReports <= 0 {
		options.MaxReports = 30
	}
	if options.Thresholds == (Thresholds{}) {
		options.Thresholds = DefaultThresholds()
	}
	return &Service{
		options: options,
		store:   newStore(options.StorePath, options.MaxReports),
	}
}

// SetKnowledge 注入知识库 (给每个问题附上手册里的相关章节)。
func (s *Service) SetKnowledge(lookup KnowledgeLookup) {
	s.options.Knowledge = lookup
}

// SetDialer 注入自定义连接器 (测试用)。
func (s *Service) SetDialer(dialer func(Target) (Runner, func(), error)) {
	s.dialer = dialer
}

// StorePath 返回历史落盘位置。
func (s *Service) StorePath() string { return s.store.path }

// RunOptions 是单次巡检的行为覆盖 (不传则用服务默认值)。
type RunOptions struct {
	// AnalyzeLogs 覆盖默认值: 是否用模型分析规则未覆盖的日志。
	AnalyzeLogs *bool
	// Lang 是报告文案的语言 (零值按中文)。
	//
	// 报告在生成时就定语言: 结论行里带数字与单位, 事后逐词替换翻不出正确语序。
	Lang i18n.Lang
}

// analyzeLogs 决定本次是否调用模型分析日志。
func (s *Service) analyzeLogs(run RunOptions) bool {
	enabled := s.options.AnalyzeLogs
	if run.AnalyzeLogs != nil {
		enabled = *run.AnalyzeLogs
	}
	if !enabled || s.options.LogAnalyzer == nil {
		return false
	}
	return s.options.LogAnalyzer.Available()
}

// Inspect 并发巡检多台主机, 返回与输入顺序一致的报告。
//
// 单台失败不影响其它主机 —— 巡检最怕"一台连不上就整批没有结果"。
func (s *Service) Inspect(targets []Target) []Report {
	return s.InspectWith(targets, RunOptions{})
}

// InspectWith 与 Inspect 相同, 但允许单次覆盖行为 (是否调用模型分析日志)。
func (s *Service) InspectWith(targets []Target, run RunOptions) []Report {
	if len(targets) == 0 {
		return []Report{}
	}

	reports := make([]Report, len(targets))
	limit := s.options.Concurrency
	if limit > len(targets) {
		limit = len(targets)
	}

	semaphore := make(chan struct{}, limit)
	var waitGroup sync.WaitGroup
	for index, target := range targets {
		waitGroup.Add(1)
		go func(index int, target Target) {
			defer waitGroup.Done()
			// panic 边界: 一台主机的意外不该带走整个进程, 也不该让同批其它主机的
			// 结果一起丢掉。巡检天然是成批做的, 这里"少一台的结果"远好过"服务挂了"。
			// (响应里会如实写明这一台是内部错误, 不会被当成"检查通过"。)
			defer func() {
				if recovered := recover(); recovered != nil {
					reports[index] = failedReport(target, time.Now(), run.Lang,
						fmt.Errorf("巡检过程发生内部错误: %v", recovered))
				}
			}()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			reports[index] = s.InspectOneWith(target, run)
		}(index, target)
	}
	waitGroup.Wait()

	s.store.add(reports)
	return reports
}

// failedReport 生成一份"没能完成"的报告。
//
// 状态一律是 unknown + 结论行说明失败: 界面与 PDF 都据此显示"未完成",
// 而不是一片空白让人误以为"没发现问题"。
func failedReport(target Target, started time.Time, lang i18n.Lang, err error) Report {
	report := newReportFor(target, started)
	report.fail(lang, err)
	return report
}

// newReportFor 建一份"还没有结果"的报告: 标识、目标、起止时间已填, 状态待定。
func newReportFor(target Target, started time.Time) Report {
	return Report{
		ID:        newReportID(),
		Target:    target.Address(),
		User:      target.User,
		Host:      target.Host,
		AuthMode:  target.AuthMode(),
		StartedAt: started,
		Findings:  []Finding{},
	}
}

// fail 把报告标记为没能完成。
func (r *Report) fail(lang i18n.Lang, err error) {
	r.Status = StatusUnknown
	r.Error = err.Error()
	r.DurationMS = time.Since(r.StartedAt).Milliseconds()
	r.Summary = r.SummaryLineLang(lang)
}

// InspectOne 巡检一台主机。
func (s *Service) InspectOne(target Target) Report {
	return s.InspectOneWith(target, RunOptions{})
}

// InspectOneWith 与 InspectOne 相同, 但允许单次覆盖行为。
func (s *Service) InspectOneWith(target Target, run RunOptions) Report {
	started := time.Now()

	dial := s.dialer
	if dial == nil {
		dial = func(t Target) (Runner, func(), error) {
			runner, err := newSSHRunner(t, s.options.SSH)
			if err != nil {
				return nil, func() {}, err
			}
			return runner, runner.Cleanup, nil
		}
	}

	report := newReportFor(target, started)

	runner, cleanup, err := dial(target)
	if err != nil {
		report.fail(run.Lang, err)
		return report
	}
	defer cleanup()

	output, runErr := runner.Run(remoteScript(s.options.CommandTimeout))
	report.HostKeyFP = runner.HostKeyFingerprint()

	if runErr != nil && strings.TrimSpace(output) == "" {
		report.fail(run.Lang, runErr)
		return report
	}

	sections := splitSections(output)
	evaluated := Evaluate(target, sections, started, time.Since(started),
		report.HostKeyFP, s.options.Thresholds, run.Lang)

	// 保留本次生成的 id 与凭据说明 (Evaluate 不该知道这些)
	evaluated.ID = report.ID
	if runErr != nil {
		// 部分输出拿到了但 ssh 仍非零退出: 记下来, 但不当作整次失败
		evaluated.Findings = append(evaluated.Findings, Finding{
			ID: "ssh", Name: "采集完整性", Status: StatusWarn,
			Summary: "采集过程中 ssh 非正常退出, 结果可能不完整",
			Details: []string{runErr.Error()},
		})
		evaluated.finalize(run.Lang) // 追加了检查项, 重新汇总状态与结论行
	}

	// 模型补充分析: 规则库认不出来的日志交给模型, 结论标明来源。
	//
	// 放在规则分析之后而不是替代它: 常见故障 (OOM/磁盘/ZFS/iSCSI…) 的结论必须可复现、
	// 可评审, 模型只补长尾。模型不可用时这一步整体跳过, 报告里如实写明只有规则结论。
	modelProblem := ""
	if s.analyzeLogs(run) {
		modelProblem = s.enrichLogDiagnoses(&evaluated, run.Lang)
	}
	s.attachKnowledge(&evaluated)
	evaluated.Analysis = s.analysisNote(run, evaluated, modelProblem)
	return evaluated
}

// enrichLogDiagnoses 把未识别的日志组交给模型分析, 并与规则结论合并。
//
// 返回模型调用失败的原因 (没有失败时为空串), 由调用方一路带到报告里。
//
// 为什么用返回值而不是包级变量: 从前那个包级变量是**跨请求共享**的, 有两个后果 ——
// 并发巡检时会把别人的失败原因写进自己的报告; 而且它从不被清空, 于是一次失败之后
// 每一份报告都挂着"模型调用失败", 哪怕那次调用其实好好的。
func (s *Service) enrichLogDiagnoses(report *Report, lang i18n.Lang) string {
	var failures []string
	for index := range report.Findings {
		finding := &report.Findings[index]
		if len(finding.Diagnoses) == 0 {
			continue
		}
		var unrecognized []Diagnosis
		for _, item := range finding.Diagnoses {
			if item.Origin == OriginUnknown {
				unrecognized = append(unrecognized, item)
			}
		}
		if len(unrecognized) == 0 {
			continue
		}
		modeled, err := s.options.LogAnalyzer.AnalyzeUnknown(lang, unrecognized)
		if err != nil {
			// 模型失败不影响巡检结论: 保留规则结论, 记下原因
			failures = appendUnique(failures, err.Error())
			continue
		}
		if len(modeled) == 0 {
			continue
		}
		finding.Diagnoses = MergeModelDiagnoses(finding.Diagnoses, modeled)
	}
	return strings.Join(failures, "; ")
}

// appendUnique 追加不重复的字符串 (同一类失败往往在每个检查项上各报一次)。
func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// attachKnowledge 给每个已识别的问题附上知识库里的相关章节。
//
// 未识别的条目跳过: 它们连"是什么问题"都还没定, 拿标题去检索只会撞出一堆无关章节。
func (s *Service) attachKnowledge(report *Report) {
	if s.options.Knowledge == nil {
		return
	}
	for index := range report.Findings {
		finding := &report.Findings[index]
		for position := range finding.Diagnoses {
			diagnosis := &finding.Diagnoses[position]
			if diagnosis.Origin == OriginUnknown {
				continue
			}
			// 标题 + 规则 ID 一起查: 规则 ID 是英文术语 (iscsi_auth), 手册里常常
			// 用的正是这些英文词, 中文标题反而对不上
			query := diagnosis.Title + " " + diagnosis.RuleID
			if refs := s.options.Knowledge.LookupKnowledge(query, 2); len(refs) > 0 {
				diagnosis.Knowledge = refs
			}
		}
	}
}

// analysisNote 生成"这份报告的分析是怎么来的"。
//
// 写在报告里而不是只写在日志里: 规则结论与模型结论的可信度不同, 读报告的人有权知道。
func (s *Service) analysisNote(run RunOptions, report Report, modelProblem string) string {
	hasDiagnosis := false
	fromModel := false
	for _, finding := range report.Findings {
		for _, item := range finding.Diagnoses {
			hasDiagnosis = true
			if item.Origin == OriginModel {
				fromModel = true
			}
		}
	}
	if !hasDiagnosis {
		return ""
	}
	rulePart := "影响分析与处置建议: 内置规则库"
	if !s.analyzeLogs(run) {
		if s.options.LogAnalyzer == nil || !s.options.LogAnalyzer.Available() {
			return rulePart + " (未启用模型分析)"
		}
		return rulePart + " (本次未启用模型分析)"
	}
	note := rulePart + " + 模型"
	if fromModel {
		note += " (含模型给出的结论, 已在建议处标注)"
	} else {
		note += " (模型未补充新结论)"
	}
	if modelProblem != "" {
		note += "; 模型调用失败: " + modelProblem
	}
	return note
}

// List 返回历史巡检记录 (按时间倒序, 不含任何凭据)。
func (s *Service) List() []Report { return s.store.list() }

// Get 取一条历史记录。
func (s *Service) Get(id string) (Report, bool) { return s.store.get(id) }

// Delete 删除一条历史记录。
func (s *Service) Delete(id string) bool { return s.store.delete(id) }

// ---------------------------------------------------------------------------
// 历史留存
//
// 只存报告本身: 报告里没有密码、没有私钥 (Target 的凭据字段不进报告), 因此历史
// 可以放心落盘。
// ---------------------------------------------------------------------------

type store struct {
	mu      sync.Mutex
	reports []Report
	path    string
	max     int
}

func newStore(path string, max int) *store {
	instance := &store{reports: []Report{}, path: path, max: max}
	instance.load()
	return instance
}

func (s *store) load() {
	if s.path == "" {
		return
	}
	// 有界读取: 报告里带日志片段, 一个损坏或被塞大的文件不该在启动时把内存吃光。
	limit := int64(s.max) * (4 << 20)
	raw, err := fileio.ReadBounded(s.path, limit)
	if err != nil {
		return
	}
	var decoded struct {
		Reports []Report `json:"reports"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return
	}
	s.reports = decoded.Reports
}

func (s *store) add(reports []Report) {
	if len(reports) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	sort.SliceStable(s.reports, func(i, j int) bool {
		return s.reports[i].StartedAt.After(s.reports[j].StartedAt)
	})
	if len(s.reports) > s.max {
		s.reports = s.reports[:s.max]
	}
	s.persistLocked()
}

func (s *store) list() []Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Report, len(s.reports))
	copy(out, s.reports)
	return out
}

func (s *store) get(id string) (Report, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, report := range s.reports {
		if report.ID == id {
			return report, true
		}
	}
	return Report{}, false
}

func (s *store) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, report := range s.reports {
		if report.ID != id {
			continue
		}
		s.reports = append(s.reports[:index], s.reports[index+1:]...)
		s.persistLocked()
		return true
	}
	return false
}

// persistLocked 原子落盘 (0600 —— 报告含主机名、分区与日志片段, 不适合对所有人可读)。
//
// 失败要说出来: 内存里已经有这条记录了 (界面里看得到、能导出 PDF), 但重启后它会消失。
// 巡检报告是"事后要查"的东西, 静默丢掉最坑人。
func (s *store) persistLocked() {
	if s.path == "" {
		return
	}
	body, err := json.MarshalIndent(map[string]any{"reports": s.reports}, "", "  ")
	if err != nil {
		log.Printf("警告: 巡检记录序列化失败: %v; 这次巡检结果没有落盘", err)
		return
	}
	body = append(body, '\n')

	if err := fileio.WriteAtomic(s.path, body, 0o600); err != nil {
		log.Printf("警告: 巡检记录落盘失败 (%s): %v; 重启后这次巡检结果会丢失", s.path, err)
	}
}

func newReportID() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return "insp-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "insp-" + hex.EncodeToString(buffer)
}
