package web

import (
	"net/http"
	"strings"

	"backup-restore-agent/internal/inspect"
)

// 巡检接口。
//
// 安全约定 (与 inspect 包一致): 密码/私钥只在本次请求里被用到, 不进日志、不进历史、
// 不回显。响应里只说明"用了哪种认证方式"。

type inspectRequest struct {
	// Hosts 支持一次填多台 (逗号/空格/换行分隔) —— 巡检通常是成批做的。
	Hosts      []string `json:"hosts"`
	User       string   `json:"user"`
	Port       int      `json:"port"`
	Password   string   `json:"password"`
	PrivateKey string   `json:"private_key"`
	Passphrase string   `json:"passphrase"`
	// AnalyzeLogs 控制是否用模型分析日志 (影响与处置建议); 不传则用服务默认值。
	AnalyzeLogs *bool `json:"analyze_logs"`
}

// maxInspectHosts 限制单次巡检的主机数: 一次填几百台会把服务占满。
const maxInspectHosts = 50

// handleInspect 对给定主机执行巡检。
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if !s.inspectEnabled {
		writeError(w, http.StatusConflict, "inspect_disabled",
			s.t(r, "服务器巡检已关闭"), s.t(r, "请在 config.toml 里设置 server.inspect.enabled=true 后重启"), "")
		return
	}
	if s.inspector == nil {
		writeError(w, http.StatusInternalServerError, "internal", s.t(r, "巡检服务未初始化"), "", "")
		return
	}

	var request inspectRequest
	if !decodeBody(w, r, &request) {
		return
	}

	hosts, rejected := normalizeHosts(request.Hosts)
	if len(hosts) == 0 {
		message := s.t(r, "请至少提供一台主机地址")
		if len(rejected) > 0 {
			message = s.t(r, "提供的地址都不合法: ") + strings.Join(rejected, ", ")
		}
		writeError(w, http.StatusBadRequest, "invalid_request", message,
			s.t(r, "主机地址只允许字母、数字、点、短横线、下划线与冒号"), "")
		return
	}
	if len(hosts) > maxInspectHosts {
		writeError(w, http.StatusBadRequest, "invalid_request",
			s.t(r, "单次最多巡检 50 台主机"), s.t(r, "请分批执行"), "")
		return
	}
	user := strings.TrimSpace(request.User)
	if user == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", s.t(r, "请提供登录用户名"), "", "")
		return
	}
	if strings.TrimSpace(request.Password) == "" && strings.TrimSpace(request.PrivateKey) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			s.t(r, "请提供密码或私钥"), s.t(r, "两者都没有无法登录目标机"), "")
		return
	}

	targets := make([]inspect.Target, 0, len(hosts))
	for _, host := range hosts {
		targets = append(targets, inspect.Target{
			Host:       host,
			Port:       request.Port,
			User:       user,
			Password:   request.Password,
			PrivateKey: request.PrivateKey,
			Passphrase: request.Passphrase,
		})
	}

	reports := s.inspector.InspectWith(targets, inspect.RunOptions{
		AnalyzeLogs: request.AnalyzeLogs,
		Lang:        s.langOf(r),
	})

	// 日志只记"谁、几台、结果概述", 绝不记凭据
	failed := 0
	for _, report := range reports {
		if report.Error != "" {
			failed++
		}
	}
	s.logger.Printf("巡检 %d 台主机 (用户 %s, 认证 %s): 失败 %d 台",
		len(reports), user, targets[0].AuthMode(), failed)

	result := map[string]any{
		"reports": reports,
		"total":   len(reports),
		"failed":  failed,
	}
	// 被丢弃的地址必须回报出来: 静默忽略会让用户以为"这些也巡检过了"
	if len(rejected) > 0 {
		result["rejected"] = rejected
	}
	// 语料 (检查项名称、结论、规则库的影响与建议) 按当前语言重新呈现:
	// 报告是这次刚生成的, 生成时用的是请求语言, 但规则库文案是字典条目, 这里统一过一遍
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), result))
}

// handleListInspections 返回巡检历史 (不含凭据)。
func (s *Server) handleListInspections(w http.ResponseWriter, r *http.Request) {
	if s.inspector == nil {
		writeJSON(w, http.StatusOK, map[string]any{"reports": []any{}, "total": 0})
		return
	}
	reports := s.inspector.List()

	// 列表只要摘要, 不把整份报告 (含日志片段) 全发出去
	items := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		items = append(items, map[string]any{
			"id":          report.ID,
			"target":      report.Target,
			"host":        report.Host,
			"auth_mode":   report.AuthMode,
			"status":      report.Status,
			"summary":     report.SummaryLineLang(s.langOf(r)),
			"counts":      report.Counts(),
			"started_at":  report.StartedAt,
			"duration_ms": report.DurationMS,
			"error":       report.Error,
			"host_name":   report.HostName,
			"os":          report.OS,
		})
	}
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), map[string]any{
		"reports": items,
		"total":   len(items),
	}))
}

// handleGetInspection 返回单次巡检的完整报告。
func (s *Server) handleGetInspection(w http.ResponseWriter, r *http.Request) {
	if s.inspector == nil {
		writeError(w, http.StatusNotFound, "not_found", "巡检服务未启用", "", "")
		return
	}
	report, ok := s.inspector.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			s.tf(r, "巡检记录不存在: {0}", r.PathValue("id")), "", "")
		return
	}
	// 结构化文案 (检查项名称、状态、规则库结论) 按当前语言再呈现一遍:
	// 这样切换语言后, 之前生成的记录也是英文的。
	writeJSON(w, http.StatusOK, translatePayload(s.langOf(r), report))
}

func (s *Server) handleDeleteInspection(w http.ResponseWriter, r *http.Request) {
	if s.inspector == nil {
		writeError(w, http.StatusNotFound, "not_found", "巡检服务未启用", "", "")
		return
	}
	id := r.PathValue("id")
	if !s.inspector.Delete(id) {
		writeError(w, http.StatusNotFound, "not_found", "巡检记录不存在: "+id, "", "")
		return
	}
	s.logger.Printf("删除巡检记录 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// normalizeHosts 拆分并去重主机列表, 同时挡掉明显的非法输入。
//
// 主机地址会被作为独立参数交给 ssh (不经过 shell), 但仍做基本校验: 空值、带空格或
// 控制字符的地址没有正当用途, 早拒绝比让 ssh 报一句难懂的错更好。
func normalizeHosts(raw []string) (hosts []string, rejected []string) {
	seen := map[string]bool{}
	for _, entry := range raw {
		for _, part := range strings.FieldsFunc(entry, func(char rune) bool {
			return char == ',' || char == '，' || char == ';' || char == '；' ||
				char == '\n' || char == '\r' || char == '\t' || char == ' '
		}) {
			host := strings.TrimSpace(part)
			if host == "" || seen[host] {
				continue
			}
			if !isSaneHost(host) {
				rejected = append(rejected, host)
				continue
			}
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	return hosts, rejected
}

// isSaneHost 做最基本的合法性检查: 只允许主机名/IPv4/IPv6 里会出现的字符。
func isSaneHost(host string) bool {
	if len(host) > 255 {
		return false
	}
	for _, char := range host {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9':
			continue
		case char == '.' || char == '-' || char == '_' || char == ':':
			continue
		default:
			return false
		}
	}
	return true
}
