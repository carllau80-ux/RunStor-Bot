#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# TLS 配置门禁 —— 对应基线红线「InsecureSkipVerify: true 或任何关闭证书校验的开关」
# 与第 3 条「TLS 1.2 起, 缺省 1.3」。
#
# 检查三件事:
#   1. 源码里不出现 InsecureSkipVerify (关了就等于没有 TLS, 没有例外);
#   2. 出现 tls.Config 的文件必须显式写 MinVersion (Go 的缺省会随版本变, 不能靠它);
#   3. MinVersion 不得低于 TLS 1.2。
#
# 用法: bash scripts/verify-tls.sh [根目录]
# 退出码: 0 = 通过, 1 = 违规
# ---------------------------------------------------------------------------
set -uo pipefail

ROOT=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
cd "$ROOT" || exit 1

FAIL=0
bad() { printf '  [违规] %s\n' "$1"; FAIL=1; }

mapfile -t SOURCES < <(find . -path ./bin -prune -o -path ./.git -prune -o -type f -name '*.go' -print | sort)
echo "检查 ${#SOURCES[@]} 个 Go 文件"

# 1) 关闭证书校验
for file in "${SOURCES[@]}"; do
  while IFS= read -r line; do
    [ -n "$line" ] && bad "$file:$line"
  done < <(grep -n 'InsecureSkipVerify' "$file" 2>/dev/null)
done

# 2) 用了 tls.Config 就必须显式写 MinVersion
for file in "${SOURCES[@]}"; do
  if grep -q 'tls\.Config{' "$file" 2>/dev/null; then
    if ! grep -q 'MinVersion' "$file"; then
      bad "$file 用了 tls.Config 却没写 MinVersion (不能依赖 Go 的缺省值)"
    fi
  fi
done

# 3) 最低版本不得低于 1.2
for file in "${SOURCES[@]}"; do
  while IFS= read -r line; do
    [ -n "$line" ] && bad "$file:$line 低于 TLS 1.2"
  done < <(grep -nE 'MinVersion.*VersionTLS(10|11)\b' "$file" 2>/dev/null)
done

if [ "$FAIL" -eq 0 ]; then
  echo "verify-tls: 通过 (无关闭校验; tls.Config 均声明 MinVersion)"
else
  echo "verify-tls: 发现违规 (见上)"
fi
exit "$FAIL"
