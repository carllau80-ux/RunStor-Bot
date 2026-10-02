// 命令 bra (backup-restore-agent) 启动 HTTP 服务与内置网页。
//
// 单二进制, 零运行时依赖: 配置与网页都内嵌, 直接 scp 到目标机即可运行。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"backup-restore-agent/internal/agent"
	"backup-restore-agent/internal/config"
	"backup-restore-agent/internal/driver"
	"backup-restore-agent/internal/i18n"
	"backup-restore-agent/internal/inspect"
	"backup-restore-agent/internal/kb"
	"backup-restore-agent/internal/llm"
	"backup-restore-agent/internal/loganalysis"
	"backup-restore-agent/internal/web"
)

// version 可在构建时用 -ldflags "-X main.version=..." 覆盖。
var version = "0.1.0"

func main() {
	// askpass 钩子必须放在最前面: ssh 会以本程序作为"取密码的助手"再调一次,
	// 那次调用只该打印密码然后退出, 不能去解析命令行参数或启动服务。
	if inspect.IsAskpassInvocation() {
		os.Exit(inspect.RunAskpass(os.Args[1:]))
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "配置文件路径 (默认找 ./config.toml 与 /etc/backup-restore-agent/config.toml)")
		portFlag    = flag.Int("port", 0, "监听端口, 覆盖配置里的 server.port")
		hostFlag    = flag.String("host", "", "监听地址, 覆盖配置里的 server.host")
		showVersion = flag.Bool("version", false, "打印版本后退出")
		checkOnly   = flag.Bool("check", false, "只做配置与依赖自检, 不启动服务")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("backup-restore-agent %s\n", version)
		return nil
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *portFlag != 0 {
		if *portFlag < 1 || *portFlag > 65535 {
			return fmt.Errorf("-port 超出范围: %d", *portFlag)
		}
		cfg.Server.Port = *portFlag
	}
	if *hostFlag != "" {
		cfg.Server.Host = *hostFlag
	}

	registry := llm.NewRegistry(cfg)
	orchestrator := agent.New(registry, agent.Options{
		MaxTurns:    cfg.Server.MaxTurns,
		PromptLimit: cfg.Server.PromptLimit,
		SessionTTL:  time.Duration(cfg.Server.SessionTTLMin) * time.Minute,
		SessionPath: cfg.SessionPath,
		MaxSessions: cfg.Server.MaxSessions,
		Catalog:     cfg.Server.Catalog,
	})

	reportStartup(logger, cfg, registry)

	if *checkOnly {
		logger.Println("自检完成 (--check), 未启动服务")
		return nil
	}

	server := web.New(orchestrator, logger, version)

	// 巡检服务: 用系统 ssh 客户端连目标机执行固定脚本。
	inspectEnabled := cfg.Server.Inspect.Enabled == nil || *cfg.Server.Inspect.Enabled
	askpassBinary, _ := os.Executable()
	analyzer := loganalysis.New(loganalysis.Options{
		// 取注册表里当前生效的模型: 界面上换模型后, 日志分析跟着换
		Client: registry.ActiveClient,
	})
	inspector := inspect.NewService(inspect.Options{
		SSH: inspect.SSHOptions{
			ConnectTimeout: time.Duration(cfg.Server.Inspect.ConnectTimeoutSeconds) * time.Second,
			HostKeyPolicy:  cfg.Server.Inspect.HostKeyPolicy,
			KnownHostsPath: config.InspectKnownHostsPath(cfg, cfg.Runtime.Path()),
			AskpassBinary:  askpassBinary,
		},
		LogAnalyzer:    analyzer,
		AnalyzeLogs:    cfg.Server.Inspect.LogAnalysisEnabled(),
		CommandTimeout: cfg.Server.Inspect.CommandTimeoutSeconds,
		Concurrency:    cfg.Server.Inspect.Concurrency,
		StorePath:      config.InspectReportPath(cfg, cfg.Runtime.Path()),
		MaxReports:     cfg.Server.Inspect.MaxReports,
	})
	server.SetInspector(inspector, inspectEnabled)
	server.SetPDFFont(cfg.Server.Inspect.PDFFont)
	server.SetDefaultLang(i18n.Normalize(cfg.Server.Language))

	// 知识库: 导入 Markdown 手册, 巡检报告会引用其中相关章节, 界面里也能问答。
	var knowledge *kb.Base
	if cfg.Server.KB.KBEnabled() {
		knowledge = kb.New(kb.Options{
			Path:             config.KBPath(cfg, cfg.Runtime.Path()),
			MaxDocuments:     cfg.Server.KB.MaxDocuments,
			MaxDocumentBytes: cfg.Server.KB.MaxDocumentBytes,
			MaxTotalBytes:    cfg.Server.KB.MaxTotalBytes,
			TopK:             cfg.Server.KB.TopK,
			ImportDir:        config.KBImportDir(cfg, cfg.Runtime.Path()),
			MaxRecords:       cfg.Server.KB.MaxRecords,
		})
		server.SetKnowledge(knowledge, cfg.Server.KB.ExpandEnabled())
		inspector.SetKnowledge(knowledgePort{base: knowledge})
		if warning := knowledge.StaleWarning(); warning != "" {
			logger.Printf("配置提示: %s", warning)
		}
		if documents, chunks := knowledge.Count(); documents > 0 {
			logger.Printf("知识库: %d 份文档 / %d 个片段 (%s)", documents, chunks, knowledge.Path())
		} else {
			logger.Printf("知识库: 就绪, 尚未导入文档 (%s)", knowledge.Path())
		}
		// 预置文档同步: 缺什么补什么, 不覆盖操作者自己的文档
		if seedDir := config.KBSeedDir(cfg); seedDir != "" {
			if result, err := knowledge.Seed(seedDir); err != nil {
				logger.Printf("配置提示: 同步预置文档失败 (%s): %v", seedDir, err)
			} else if !result.EmptyDir {
				logger.Printf("预置文档: %s (%s)", result.Summary(), seedDir)
			} else {
				logger.Printf("预置文档: %s 为空或不存在, 跳过", seedDir)
			}
		}
		if dir := config.KBImportDir(cfg, cfg.Runtime.Path()); dir != "" {
			logger.Printf("知识库导入目录: %s (只读该目录里的 .md/.markdown/.txt)", dir)
		} else {
			logger.Printf("知识库导入目录: 已关闭 (server.kb.import_dir=\"-\")")
		}
	} else {
		logger.Printf("知识库: 已关闭 (server.kb.enabled=false)")
	}
	if warning := inspect.ValidatePDFFont(cfg.Server.Inspect.PDFFont); warning != "" {
		logger.Printf("配置提示: %s", warning)
	}
	address := net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port))
	httpServer := &http.Server{
		Addr:              address,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      180 * time.Second, // 抽取可能多次重试, 给足时间
		IdleTimeout:       120 * time.Second,
	}

	// 先监听再启动服务, 这样端口被占用时能立刻给出可读错误
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("无法监听 %s: %w (该端口可能已被占用)", address, err)
	}
	logger.Printf("服务已就绪: http://%s/  (监听 %s)", displayAddress(cfg.Server.Host, cfg.Server.Port), listener.Addr())

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		return err
	case signalReceived := <-stop:
		logger.Printf("收到信号 %v, 正在停止…", signalReceived)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅停止失败: %w", err)
	}
	logger.Println("已停止")
	return nil
}

func reportStartup(logger *log.Logger, cfg *config.Config, registry *llm.Registry) {
	if cfg.Path == "" {
		logger.Println("配置: 未找到配置文件 (仅用环境变量)")
	} else {
		logger.Printf("配置: %s", cfg.Path)
	}

	mode := registry.Mode()
	if mode == "demo" {
		logger.Printf("模型: demo 模式 (无可用 profile) —— 使用内置关键词规则, 无需 API Key")
	} else {
		profile := registry.ActiveProfile()
		logger.Printf("模型: %s (provider=%s model=%s base_url=%s key=%s)",
			profile.Name, profile.Provider, profile.Model, profile.BaseURL,
			config.Mask(profile.APIKey))
	}

	if cfg.Server.Inspect.Enabled == nil || *cfg.Server.Inspect.Enabled {
		policy := cfg.Server.Inspect.HostKeyPolicy
		if policy == "" {
			policy = "tofu"
		}
		logger.Printf("服务器巡检: 已启用 (并发 %d, 主机指纹策略 %s, 历史 %s)",
			cfg.Server.Inspect.Concurrency, policy, config.InspectReportPath(cfg, cfg.Runtime.Path()))
	} else {
		logger.Printf("服务器巡检: 已关闭 (inspect.enabled=false)")
	}
	if cfg.Server.Catalog.Enabled != nil && *cfg.Server.Catalog.Enabled && (cfg.Server.Catalog.URL != "" ||
		cfg.Server.Catalog.ApplicationsURL != "" || cfg.Server.Catalog.MediaServersURL != "") {
		logger.Printf("运行时清单: 来自接口 (应用与介质服务器将动态查询)")
	} else {
		logger.Printf("运行时清单: 使用内置清单 (未配置 catalog 接口)")
	}
	if cfg.SessionPath == "" {
		logger.Printf("对话历史: 仅内存 (重启即丢)")
	} else {
		logger.Printf("对话历史: %s", cfg.SessionPath)
	}
	logger.Printf("已注册引擎: %v", driver.Kinds())
	for _, warning := range cfg.Warnings {
		logger.Printf("配置提示: %s", warning)
	}
	for _, profile := range cfg.Profiles {
		if !profile.Available() {
			logger.Printf("profile %q 不可用: %s", profile.Name, profile.UnavailableReason())
		}
	}
}

// displayAddress 把 0.0.0.0 换成便于点击的地址。
func displayAddress(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

// knowledgePort 把知识库适配成巡检需要的接口。
//
// 两个都是功能包, 谁都不该依赖对方的类型: 巡检在 inspect 里声明它需要什么
// (KnowledgeLookup), 知识库在 kb 里提供能力, 中间的转换放在组装处 (这里)。
type knowledgePort struct{ base *kb.Base }

func (p knowledgePort) LookupKnowledge(query string, limit int) []inspect.KnowledgeRef {
	refs := p.base.LookupKnowledge(query, limit)
	out := make([]inspect.KnowledgeRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, inspect.KnowledgeRef{
			DocID: ref.DocID, Doc: ref.Doc, Heading: ref.Heading,
			Snippet: ref.Snippet, Score: ref.Score,
		})
	}
	return out
}
