#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# 明文密钥扫描 —— 对应基线红线「密钥/口令/API key 明文出现在 TOML、git、镜像层、日志、argv」。
#
# 扫的是"会被复制、会被提交、会进安装包"的那些文件: 配置示例、文档、部署脚本、Go 源码。
# 不扫运行态文件 (runtime.json / kb.json 等): 它们是服务自己写的、0600、由部署脚本管理。
#
# 用法: bash scripts/secret-scan.sh [根目录]
# 退出码: 0 = 干净, 1 = 命中
# ---------------------------------------------------------------------------
set -uo pipefail

ROOT=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
cd "$ROOT" || exit 1

FAIL=0
hits() { printf '  [命中] %s\n' "$1"; FAIL=1; }

mapfile -t FILES < <(
  find . \
    -path ./bin -prune -o \
    -path ./build -prune -o \
    -path ./releases -prune -o \
    -path ./.git -prune -o \
    -type f \
    \( -name '*.go' -o -name '*.toml' -o -name '*.json' -o -name '*.md' \
       -o -name '*.sh' -o -name '*.service' -o -name '*.example' -o -name 'Makefile' \) \
    -print | sort
)

echo "扫描 ${#FILES[@]} 个文件 (根目录: $ROOT)"

# 1) 私钥块 —— 出现即事故。
#
# 测试里需要"长得很像私钥的假样本", 这类文件必须显式声明 secret-scan:allow-test-key,
# 让人做一次判断; 否则一个真私钥被粘进测试文件就能悄悄过关。
for file in "${FILES[@]}"; do
  if grep -qE -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$file" 2>/dev/null; then
    if [[ "$file" == *_test.go ]] && grep -q 'secret-scan:allow-test-key' "$file"; then
      continue
    fi
    hits "$file 里有私钥块"
  fi
done

# 2) 形似真实密钥的长串。
#
# 覆盖各家前缀: OpenAI/DeepSeek 风格 sk-、Anthropic sk-ant-、AWS AKIA/ASIA、
# GitHub ghp_/gho_/github_pat_、Slack xox?-、Google AIza、HuggingFace hf_。
# 要求长度 ≥20, 这样测试里的 sk-test-123 / sk-plain-text 这类**短**假样本不会误报
# (它们本来就该长得不像真密钥)。
KEY_PATTERN='(sk-(ant-)?[A-Za-z0-9_-]{20,}|(AKIA|ASIA)[A-Z0-9]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abpsr]-[A-Za-z0-9-]{10,}|AIza[A-Za-z0-9_-]{30,}|hf_[A-Za-z0-9]{30,})'
for file in "${FILES[@]}"; do
  while IFS= read -r line; do
    [ -n "$line" ] && hits "$file:$line"
  done < <(grep -nE -- "$KEY_PATTERN" "$file" 2>/dev/null)
done

# 2b) 内网地址 —— 公开仓库里不该出现真实的内网地址。
#
# 放行两类"本来就是为了写示例"的地址: RFC 5737 文档保留段 (192.0.2.0/24, 198.51.100.0/24,
# 203.0.113.0/24) 与随处可见的 10.0.0.x。其余 RFC1918 段 (含 192.168.x、10.x 其它段)
# 命中就提示人工确认 —— 真内网地址被写进代码正是这条要拦的。
for file in "${FILES[@]}"; do
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    case "$line" in
      *192.0.2.*|*198.51.100.*|*203.0.113.*|*10.0.0.*) continue ;;
    esac
    hits "$file:$line (内网地址? 请确认)"
  done < <(grep -nE -- '(192\.168|172\.(1[6-9]|2[0-9]|3[01])|10\.[0-9]+\.[0-9]+)\.[0-9]+' "$file" 2>/dev/null)
done

# 3) 配置文件里的明文 api_key (值为空不算; 引用式写法 api_key_env / api_key_file 允许)
for file in "${FILES[@]}"; do
  case "$file" in
    *.toml|*.json|*.example)
      while IFS= read -r line; do
        [ -n "$line" ] && hits "$file:$line"
      done < <(grep -nE -- '^[[:space:]]*"?api_key"?[[:space:]]*[=:][[:space:]]*"[^"]+"' "$file" 2>/dev/null)
      ;;
  esac
done

# 4) 不该入库的密钥文件名
while IFS= read -r path; do
  [ -z "$path" ] && continue
  case "$path" in
    *example*|*testdata*) continue ;;
  esac
  hits "$path 像是密钥文件, 不该随仓库分发"
done < <(find . -path ./bin -prune -o -path ./.git -prune -o -type f \
  \( -name '*.pem' -o -name '*.key' -o -name 'id_rsa*' -o -name '*.p12' -o -name '*.pfx' \) -print)

# 5) 部署用的环境变量示例文件不能带真实值
if [ -f deploy/env.example ]; then
  while IFS= read -r line; do
    case "$line" in
      ''|'#'*) continue ;;
      *=) continue ;;   # 空值 = 占位
    esac
    hits "deploy/env.example:$line (示例文件里的变量必须是空的)"
  done < <(grep -vE '^[[:space:]]*#' deploy/env.example 2>/dev/null)
fi

if [ "$FAIL" -eq 0 ]; then
  echo "secret-scan: 未发现明文密钥"
else
  echo "secret-scan: 发现可疑内容 (见上)。密钥请改为 api_key_env / api_key_file 引用。"
fi
exit "$FAIL"
