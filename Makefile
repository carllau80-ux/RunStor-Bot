# 备份/恢复策略 Agent —— 构建、测试、打包、部署入口
#
# 部署目标机用 REMOTE 指定: make deploy REMOTE=root@<主机>
# 端口不硬编码: deploy/install.sh 会在 8081-8099 内实测挑选可用端口,
# 升级时沿用原端口 (避免端口漂移导致防火墙规则失效)。

SHELL      := /bin/bash
BINARY     := backup-restore-agent
VERSION    ?= 0.2.0
REMOTE     ?= root@your-server
REMOTE_DIR ?= /root/bra-pkg
BIN_DIR    := bin
RELEASE    := releases/$(BINARY)-$(VERSION).tar.gz

# 目标机为 x86_64 Linux; CGO_ENABLED=0 保证产出静态二进制, 不依赖目标机 libc 版本
GOOS       ?= linux
GOARCH     ?= amd64
export CGO_ENABLED=0

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help all build test test-race vet fmt fmt-check check check-cgo secret-scan verify-tls \
	staticcheck govulncheck ci hooks install-tools run package sync deploy status logs smoke clean

help: ## 显示本帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

all: check build ## 校验并构建

build: ## 编译单二进制到 bin/
	@mkdir -p $(BIN_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BIN_DIR)/$(BINARY) ./cmd/bra
	@echo "已构建 $(BIN_DIR)/$(BINARY) ($$(du -h $(BIN_DIR)/$(BINARY) | cut -f1))"

test: ## 运行全部测试
	go test ./...

test-verbose: ## 运行全部测试并列出用例名
	go test ./... -v

vet: ## 静态检查
	go vet ./...

fmt: ## 格式化代码 (本地用, 会改文件)
	gofmt -w ./cmd ./internal

fmt-check: ## 检查格式 (门禁用, 不改文件)
	@unformatted="$$(gofmt -l ./cmd ./internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化, 请执行 make fmt:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "fmt-check: OK"

check: fmt vet test ## 格式化 + vet + 测试
	@echo "校验通过"

# ---------------------------------------------------------------------------
# CI 门禁 —— 逐条对应基线「交付前自检清单」, 每条都是可执行的检查而不是提醒。
#
# 缺工具时**不静默通过**: staticcheck / govulncheck 未安装就报错并给出安装命令,
# 否则"跑了门禁"会变成一句空话 (基线: 默默绕过是最糟的结果)。
# ---------------------------------------------------------------------------

TOOLS_BIN   := $(shell go env GOPATH)/bin
STATICCHECK := $(TOOLS_BIN)/staticcheck
GOVULNCHECK := $(TOOLS_BIN)/govulncheck

check-cgo: ## CGO_ENABLED=0 必须能构建 (静态单二进制是交付形态)
	@CGO_ENABLED=0 go build ./... && echo "check-cgo: OK"

test-race: ## 竞态检测 (并发代码必过)
	@# 竞态检测器需要 cgo, 所以这里显式覆盖全局的 CGO_ENABLED=0。
	@# 只影响测试: 交付的二进制仍然是 CGO_ENABLED=0 的静态文件 (见 check-cgo)。
	CGO_ENABLED=1 go test -race ./...

secret-scan: ## 扫描明文密钥/私钥 (基线红线)
	@bash scripts/secret-scan.sh

verify-tls: ## 检查没有关闭证书校验、且声明了 TLS 最低版本
	@bash scripts/verify-tls.sh

staticcheck: ## 静态分析 (未安装会给出安装命令)
	@if [ -x "$(STATICCHECK)" ]; then "$(STATICCHECK)" ./... && echo "staticcheck: OK"; \
	else echo "staticcheck 未安装。安装: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; fi

govulncheck: ## 已知漏洞扫描 (未安装会给出安装命令)
	@if [ -x "$(GOVULNCHECK)" ]; then "$(GOVULNCHECK)" ./...; \
	else echo "govulncheck 未安装。安装: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; fi

ci: check-cgo fmt-check vet test-race secret-scan verify-tls staticcheck govulncheck ## 完整门禁 (提交前跑这个)
	@echo
	@echo "CI 门禁全部通过"

hooks: ## 启用提交前门禁 (密钥扫描 + 格式检查)
	git config core.hooksPath .githooks
	@echo "已启用 .githooks/pre-commit; 跳过用 git commit --no-verify"

install-tools: ## 安装开发期工具 (staticcheck / govulncheck)
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	@echo "已安装到 $(TOOLS_BIN); 若不在 PATH 里请加上: export PATH=\"$$PATH:$(TOOLS_BIN)\""

run: build ## 本地前台运行 (默认 8081, 无配置时为 demo 模式)
	@# 本地开发把运行态配置放到当前目录: 默认位置 /var/lib/... 普通用户写不了,
	@# 而网页上的"保存模型设置"需要写这个文件
	BRA_RUNTIME=./runtime.json ./$(BIN_DIR)/$(BINARY) -config config.toml

package: check build ## 打包成可分发的 tar.gz
	@mkdir -p releases
	@rm -rf build/pkg && mkdir -p build/pkg/bin
	@cp $(BIN_DIR)/$(BINARY) build/pkg/bin/
	@cp -r deploy build/pkg/
	@cp -r knowledge build/pkg/
	@cp README.md config.example.toml build/pkg/ 2>/dev/null || true
	@cp config.example.toml build/pkg/config.example.toml
	@tar -czf $(RELEASE) -C build/pkg .
	@echo "已打包 $(RELEASE)"
	@tar -tzf $(RELEASE) | head -12

sync: package ## 把安装包推到目标机
	@echo "推送到 $(REMOTE):$(REMOTE_DIR) …"
	@ssh $(REMOTE) "rm -rf $(REMOTE_DIR) && mkdir -p $(REMOTE_DIR)"
	@scp -q $(RELEASE) $(REMOTE):$(REMOTE_DIR)/pkg.tar.gz
	@ssh $(REMOTE) "tar -xzf $(REMOTE_DIR)/pkg.tar.gz -C $(REMOTE_DIR) && rm -f $(REMOTE_DIR)/pkg.tar.gz"
	@echo "已就位"

deploy: sync ## 部署到目标机 (自动挑选可用端口; 可用 PORT_FORCE=8081 指定)
	@echo "在 $(REMOTE) 上安装 …"
	@ssh $(REMOTE) "PORT_FORCE='$(PORT_FORCE)' bash $(REMOTE_DIR)/deploy/install.sh install"
	@echo
	@$(MAKE) --no-print-directory status

status: ## 查看目标机服务状态
	@ssh $(REMOTE) "bash /opt/$(BINARY)/deploy/install.sh status 2>/dev/null || \
		{ PORT=\$$(awk '/^[[:space:]]*\[/ { s = (\$$0 ~ /^[[:space:]]*\[server\][[:space:]]*\$$/); next } \
			s && /^[[:space:]]*port[[:space:]]*=/ { sub(/^[^=]*=[[:space:]]*/, \"\"); gsub(/[^0-9]/, \"\"); print; exit }' \
			/etc/$(BINARY)/config.toml); \
		echo \"服务端口: \$$PORT\"; systemctl is-active $(BINARY); \
		curl -fsS --max-time 5 http://127.0.0.1:\$$PORT/health; }"

logs: ## 跟踪目标机日志
	@ssh $(REMOTE) "journalctl -u $(BINARY) -f"

smoke: ## 在目标机跑部署自检
	@ssh $(REMOTE) "bash /opt/$(BINARY)/deploy/smoke.sh 2>/dev/null || bash $(REMOTE_DIR)/deploy/smoke.sh"

clean: ## 清理构建产物
	rm -rf $(BIN_DIR) build releases runtime.json
