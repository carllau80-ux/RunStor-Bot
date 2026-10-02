#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# 部署后自检 —— 纯 bash + curl, 不依赖 jq 或 python。
#
# 刻意不用 python/jq 解析: 本项目的部署优势就是单二进制零运行时依赖,
# 冒烟脚本不该把这个优势抵消掉。断言基于本项目固定的响应格式 (缩进与字段名
# 由 Go 端 MarshalIndentNoEscape 保证稳定), 更严格的校验由 Go 测试覆盖。
#
# 用法: bash deploy/smoke.sh [端口]
# ---------------------------------------------------------------------------
set -uo pipefail

CONFIG=${CONFIG:-/etc/backup-restore-agent/config.toml}

# 端口优先取命令行参数, 其次读配置文件 (install.sh 探测后写入的那个), 最后 8081
PORT=${1:-}
if [ -z "$PORT" ] && [ -f "$CONFIG" ]; then
  # 只看 [server] 段里的 port, 不引 toml 解析器 (与 install.sh 同一套办法)
  PORT="$(awk '
    /^[[:space:]]*\[/ { in_server = ($0 ~ /^[[:space:]]*\[server\][[:space:]]*$/); next }
    in_server && /^[[:space:]]*port[[:space:]]*=/ {
      sub(/^[^=]*=[[:space:]]*/, ""); gsub(/[^0-9]/, ""); print; exit
    }
  ' "$CONFIG" 2>/dev/null)"
fi
PORT=${PORT:-8081}
BASE="http://127.0.0.1:${PORT}"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()   { PASS=$((PASS + 1)); echo "  [PASS] $1"; }
bad()  { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }

# assert_contains <文件> <模式> <说明>
assert_contains() {
  if grep -qF -- "$2" "$1" 2>/dev/null; then ok "$3"; else
    bad "$3 (未找到: $2)"
    echo "        实际响应片段: $(head -c 240 "$1" | tr '\n' ' ')"
  fi
}

assert_count() {
  local actual
  actual="$(grep -cF -- "$2" "$1" 2>/dev/null || true)"
  if [ "$actual" = "$3" ]; then ok "$4"; else
    bad "$4 (期望 $3 个, 实际 ${actual:-0})"
  fi
}

echo "== RunStor Bot 部署自检 (${BASE}) =="

# ---------- 1. 健康检查 ----------
echo "-- 健康检查"
CODE="$(curl -fsS -o "$TMP/health.json" -w '%{http_code}' --max-time 10 "${BASE}/health" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /health 返回 200"
  assert_contains "$TMP/health.json" '"status": "ok"' "健康状态为 ok"
  assert_contains "$TMP/health.json" '"oracle"' "已注册 oracle 引擎"
  assert_contains "$TMP/health.json" '"mysql"' "已注册 mysql 引擎"
  assert_contains "$TMP/health.json" '"mode"' "报告了模型模式"
else
  bad "GET /health 返回 ${CODE} (服务未就绪?)"
fi

# ---------- 2. 内置网页 ----------
echo "-- 内置网页"
CODE="$(curl -fsS -o "$TMP/index.html" -w '%{http_code}' --max-time 10 "${BASE}/" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ] && grep -q "RunStor Bot" "$TMP/index.html"; then
  ok "GET / 返回内置页面"
else
  bad "GET / 未返回预期页面 (状态 ${CODE})"
fi

# ---------- 2b. 运行时清单 ----------
echo "-- 运行时清单"
CODE="$(curl -fsS -o "$TMP/catalog.json" -w '%{http_code}' --max-time 15 \
  "${BASE}/api/catalog" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /api/catalog 返回 200"
  assert_contains "$TMP/catalog.json" '"source"' "报告清单来源"
  assert_contains "$TMP/catalog.json" '"applications"' "带应用清单"
  assert_contains "$TMP/catalog.json" '"media_servers"' "带介质服务器清单"
  if grep -qF '"from_api": true' "$TMP/catalog.json"; then
    ok "清单来自运行时接口"
  else
    ok "清单为内置兜底（未配置接口或接口不可用）"
  fi
else
  bad "GET /api/catalog 返回 ${CODE}"
fi
assert_contains "$TMP/health.json" '"catalog"' "健康检查报告清单信息"

# ---------- 2c. 服务器巡检 ----------
echo "-- 服务器巡检"
CODE="$(curl -fsS -o "$TMP/inspect.json" -w '%{http_code}' --max-time 20 \
  "${BASE}/api/inspect" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /api/inspect 返回 200"
  assert_contains "$TMP/inspect.json" '"reports"' "带巡检历史列表"
else
  bad "GET /api/inspect 返回 ${CODE}"
fi

# 缺凭据必须被拒绝 (不能默默不做事)
CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
  -X POST -H 'Content-Type: application/json' \
  -d '{"hosts":["127.0.0.1"],"user":"root"}' "${BASE}/api/inspect" 2>/dev/null || echo 000)"
if [ "$CODE" = "400" ] || [ "$CODE" = "409" ]; then
  ok "巡检缺凭据时被拒绝 (${CODE})"
else
  bad "巡检缺凭据应被拒绝, 实际 ${CODE}"
fi

# 连不上也要给出可读原因, 而不是 500。
#
# 这里刻意用一个关闭的端口 (连接被拒) 而不是错密码: OpenSSH 9.8 起有"按来源惩罚",
# 同一来源连续认证失败会被临时拒绝连接 —— 每次部署自检都制造认证失败, 会把本机
# 自己关在门外, 连正常的巡检也一起失败。
CODE="$(curl -sS -o "$TMP/inspect-fail.json" -w '%{http_code}' --max-time 40 \
  -X POST -H 'Content-Type: application/json' \
  -d '{"hosts":["127.0.0.1"],"user":"nobody-here","password":"x","port":9}' \
  "${BASE}/api/inspect" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ] || [ "$CODE" = "409" ]; then
  ok "巡检连接失败时返回可读结果 (${CODE})"
else
  bad "巡检连接失败不该返回 ${CODE}"
fi

# 无效凭据必须"快速 + 如实"地失败。
#
# 这条断言来自真实故障: 只给私钥时 ssh 会在密钥被拒后转去试密码, 而 askpass 助手
# 缺标记时认不出自己, 会把自己当成"再启动一个服务实例"常驻 —— 于是 curl 在这里
# 永远等不到响应, 同时每次巡检在机器上留一个多余进程。后来还发现
# SSH_ASKPASS_REQUIRE 被写成 ssh 配置项 (-o), 于是"给了密码反而立刻失败"。
# 两个故障都不会报错, 只会悄悄烂掉, 所以放在部署自检里。
if command -v ssh-keygen >/dev/null 2>&1; then
  ssh-keygen -q -t ed25519 -f "$TMP/bogus_key" -N "" -C "bra-smoke-bogus" >/dev/null 2>&1
  BOGUS="$(sed -z 's/\\/\\\\/g; s/"/\\"/g; s/\n/\\n/g' "$TMP/bogus_key" 2>/dev/null | sed 's/\\n$//')"
  START="$(date +%s)"
  CODE="$(curl -sS -o "$TMP/inspect-bogus.json" -w '%{http_code}' --max-time 45 \
    -X POST -H 'Content-Type: application/json' \
    -d "{\"hosts\":[\"127.0.0.1\"],\"user\":\"root\",\"private_key\":\"${BOGUS}\"}" \
    "${BASE}/api/inspect" 2>/dev/null || echo 000)"
  ELAPSED=$(( $(date +%s) - START ))
  if [ "$CODE" = "200" ] && [ "$ELAPSED" -lt 25 ]; then
    ok "无效私钥被快速拒绝 (${ELAPSED}s, 没有挂死)"
  else
    bad "无效私钥应快速被拒绝, 实际 HTTP ${CODE} / ${ELAPSED}s"
  fi
  assert_contains "$TMP/inspect-bogus.json" '认证失败' "无效私钥如实报告为认证失败"
  if grep -qF 'configuration option' "$TMP/inspect-bogus.json" 2>/dev/null; then
    bad "ssh 参数里有它不认的配置项 (凭据还没送到就失败了)"
  else
    ok "ssh 参数被 ssh 接受 (没有配置项错误)"
  fi
else
  bad "缺少 ssh-keygen, 无法自检凭据路径"
fi

# 巡检记录要能导出成 PDF 报告。
#
# 借上面那条"无效私钥"的记录做端到端验证: 它必然是失败报告, 但导出路径完全相同,
# 而且正好覆盖"失败也要能归档"这个要求。中文能否显示取决于字体有没有嵌进去,
# 所以这里专门断言 PDF 里有 /FontFile2 —— 没嵌字体的话阅读器只会显示空白。
REPORT_ID="$(sed -n 's/.*"id": "\([^"]*\)".*/\1/p' "$TMP/inspect-bogus.json" 2>/dev/null | head -1)"
if [ -n "$REPORT_ID" ]; then
  CODE="$(curl -sS -o "$TMP/report.pdf" -D "$TMP/report.headers" -w '%{http_code}' --max-time 30 \
    "${BASE}/api/inspect/${REPORT_ID}/report.pdf" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "导出巡检报告 PDF (${CODE})"
  else
    bad "导出巡检报告 PDF 返回 ${CODE}"
  fi
  assert_contains "$TMP/report.pdf" '%PDF-' "导出的是 PDF 文件"
  assert_contains "$TMP/report.pdf" '/FontFile2' "PDF 内嵌了字体 (否则中文显示不出来)"
  assert_contains "$TMP/report.headers" 'application/pdf' "Content-Type 是 application/pdf"
  assert_contains "$TMP/report.headers" "filename*=UTF-8''" "中文文件名按 RFC 5987 编码"
  SIZE="$(wc -c < "$TMP/report.pdf" 2>/dev/null || echo 0)"
  if [ "${SIZE:-0}" -gt 10000 ]; then
    ok "PDF 大小合理 (${SIZE} 字节)"
  else
    bad "PDF 太小, 可能没画出内容 (${SIZE} 字节)"
  fi
  # 记录不存在时应给 404, 而不是 500
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
    "${BASE}/api/inspect/insp-does-not-exist/report.pdf" 2>/dev/null || echo 000)"
  if [ "$CODE" = "404" ]; then
    ok "不存在的记录导出返回 404"
  else
    bad "不存在的记录导出应返回 404, 实际 ${CODE}"
  fi

# 日志分析: 请求里的开关要被接受 (不传用默认值), 页面上也要有对应入口。
#
# 这里只能验证"接口与界面都在位": 真正有严重日志的目标机需要有效凭据, 部署自检里没有,
# 所以分析内容本身由 Go 测试与真机验证覆盖。
CODE="$(curl -sS -o "$TMP/inspect-analyze.json" -w '%{http_code}' --max-time 45 \
  -X POST -H 'Content-Type: application/json' \
  -d "{\"hosts\":[\"127.0.0.1\"],\"user\":\"root\",\"private_key\":\"${BOGUS}\",\"analyze_logs\":false}" \
  "${BASE}/api/inspect" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "巡检接受 analyze_logs 开关 (${CODE})"
else
  bad "巡检未接受 analyze_logs, 实际 ${CODE}"
fi

CODE="$(curl -fsS -o "$TMP/index.html" -w '%{http_code}' --max-time 15 "${BASE}/" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  assert_contains "$TMP/index.html" 'inspectAnalyze' "巡检表单有模型分析开关"
  assert_contains "$TMP/index.html" '用模型分析日志' "开关有可读文案"
  assert_contains "$TMP/index.html" 'function renderDiagnoses(' "界面能渲染影响与处置建议"
  assert_contains "$TMP/index.html" '影响与处置建议' "报告里显示影响与处置建议"
  assert_contains "$TMP/index.html" '需人工确认' "未识别的结论标注需人工确认"
  assert_contains "$TMP/index.html" 'id="tabKB"' "首页有知识库入口"
  assert_contains "$TMP/index.html" '导入文档' "知识库界面有导入入口"
  assert_contains "$TMP/index.html" 'function renderCitations(' "知识库界面能渲染引用"
else
  bad "首页返回 ${CODE}"
fi
else
  bad "没能从巡检响应里取到报告编号"
fi

# 自检产生的巡检记录也清掉: 记录是给用户看自己的巡检历史的, 不该被部署自检塞满。
# 只删自己这几条 (按编号)。
INSP_IDS="$(cat "$TMP/inspect-fail.json" "$TMP/inspect-bogus.json" "$TMP/inspect-analyze.json" 2>/dev/null \
  | sed -n 's/.*"id": "\(insp-[^"]*\)".*/\1/p' | sort -u)"
if [ -n "$INSP_IDS" ]; then
  CLEANED=0
  for INSP_ID in $INSP_IDS; do
    CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
      -X DELETE "${BASE}/api/inspect/${INSP_ID}" 2>/dev/null || echo 000)"
    [ "$CODE" = "200" ] && CLEANED=$((CLEANED + 1))
  done
  if [ "$CLEANED" -gt 0 ]; then
    ok "清掉自检产生的巡检记录 (${CLEANED} 条)"
  fi
fi

# 随部署预置的知识库文档: 目录装上了、服务把它同步进了知识库。
SEED_DIR_CHECK="${SEED_DIR:-/usr/share/backup-restore-agent/knowledge}"
if [ -d "$SEED_DIR_CHECK" ]; then
  SEED_FILES="$(find "$SEED_DIR_CHECK" -name '*.md' -type f 2>/dev/null | wc -l | tr -d ' ')"
  if [ "${SEED_FILES:-0}" -gt 0 ]; then
    ok "随部署预置文档已安装 (${SEED_FILES} 份)"
  else
    bad "预置文档目录是空的: $SEED_DIR_CHECK"
  fi
  # 同步过之后知识库里应有内容 (首次部署即为"装完就能用")
  CODE="$(curl -fsS -o "$TMP/kb-seeded.json" -w '%{http_code}' --max-time 15 "${BASE}/api/kb" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    if grep -q '"seeded": true' "$TMP/kb-seeded.json"; then
      ok "预置文档已同步进知识库"
      assert_contains "$TMP/kb-seeded.json" '"chunks"' "预置文档已完成切分"
    else
      # 操作者可能把预置文档全删了 (删过就不再补), 这不算故障
      ok "知识库里没有预置文档 (可能已被删除)"
    fi
  fi
else
  bad "缺少预置文档目录: $SEED_DIR_CHECK"
fi

# ---------- 2d. 知识库 ----------
echo "-- 知识库"
# 导入一份带唯一标记的文档 → 检索 → 删除。
#
# 用同一个文件名重复导入 (同名替换, 不会累积), 验完立即删除, 不往用户的知识库里
# 留东西。这里只验证检索链路: 问答会真的调用大模型, 部署自检不该每次都花用户的 token。
KB_MARK="巡检知识库自检标记ZQ7"
CODE="$(curl -sS -o "$TMP/kb-import.json" -w '%{http_code}' --max-time 20 \
  -X POST -H 'Content-Type: application/json' \
  -d "{\"documents\":[{\"name\":\"smoke-check.md\",\"content\":\"# 自检文档\\n\\n## 检索标记\\n\\n${KB_MARK} 用于验证导入与检索链路。\\n\"}]}" \
  "${BASE}/api/kb/import" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ] || [ "$CODE" = "409" ]; then
  # 409 = 知识库被配置关闭, 这是合法配置, 不算失败
  if [ "$CODE" = "200" ]; then
    ok "导入 Markdown 文档 (${CODE})"
    assert_contains "$TMP/kb-import.json" '"title"' "导入结果带文档标题"

    CODE="$(curl -fsS -o "$TMP/kb-search.json" -w '%{http_code}' --max-time 20 \
      --get --data-urlencode "q=${KB_MARK}" "${BASE}/api/kb/search" 2>/dev/null || echo 000)"
    if [ "$CODE" = "200" ]; then
      ok "按内容检索 (${CODE})"
      assert_contains "$TMP/kb-search.json" '自检文档' "检索命中了刚导入的文档"
      assert_contains "$TMP/kb-search.json" 'heading' "结果带章节定位"
      assert_contains "$TMP/kb-search.json" '"terms"' "结果带实际检索词"
    else
      bad "检索返回 ${CODE}"
    fi

    # 文档原文: 界面里"打开文档"靠这个接口
    KB_DOC="$(sed -n 's/.*"id": "\(doc-[^"]*\)".*/\1/p' "$TMP/kb-import.json" 2>/dev/null | head -1)"
    if [ -n "$KB_DOC" ]; then
      CODE="$(curl -fsS -o "$TMP/kb-content.json" -w '%{http_code}' --max-time 15 \
        "${BASE}/api/kb/${KB_DOC}/content" 2>/dev/null || echo 000)"
      if [ "$CODE" = "200" ]; then
        ok "取文档原文 (${CODE})"
        assert_contains "$TMP/kb-content.json" '"has_content": true' "标明保存了原文"
        assert_contains "$TMP/kb-content.json" '自检文档' "返回的是原文内容"
      else
        bad "取文档原文返回 ${CODE}"
      fi
    else
      bad "没能从导入响应里取到文档编号"
    fi

    CODE="$(curl -fsS -o "$TMP/kb-list.json" -w '%{http_code}' --max-time 15 "${BASE}/api/kb" 2>/dev/null || echo 000)"
    if [ "$CODE" = "200" ]; then
      ok "列出知识库 (${CODE})"
      assert_contains "$TMP/kb-list.json" '"chunks"' "列表带片段统计"
    else
      bad "知识库列表返回 ${CODE}"
    fi

    # 服务器目录导入 + 导入记录
    #
    # 自检以 root 通过 ssh 在目标机上跑, 所以自己往导入目录里放一个文件, 再用接口导入。
    # 文件要 0644: 服务以无特权用户 bra 运行, 0600 的 root 文件它读不了 (这本身就是
    # 权限边界该有的样子)。
    INBOX="${KB_INBOX:-/var/lib/backup-restore-agent/kb-inbox}"
    if [ -d "$INBOX" ] || mkdir -p "$INBOX" 2>/dev/null; then
      printf '# 目录导入自检\n\n## 标记\n\n%s\n' "$KB_MARK" > "$INBOX/smoke-inbox.md" 2>/dev/null && chmod 644 "$INBOX/smoke-inbox.md" 2>/dev/null
      CODE="$(curl -fsS -o "$TMP/kb-inbox.json" -w '%{http_code}' --max-time 20 "${BASE}/api/kb/inbox" 2>/dev/null || echo 000)"
      if [ "$CODE" = "200" ]; then
        ok "列出服务器导入目录 (${CODE})"
        assert_contains "$TMP/kb-inbox.json" 'smoke-inbox.md' "目录里能看到刚放的文件"
      else
        bad "列出导入目录返回 ${CODE}"
      fi
      CODE="$(curl -sS -o "$TMP/kb-path.json" -w '%{http_code}' --max-time 30 \
        -X POST -H 'Content-Type: application/json' \
        -d '{"paths":["smoke-inbox.md"]}' "${BASE}/api/kb/import-path" 2>/dev/null || echo 000)"
      if [ "$CODE" = "200" ]; then
        ok "从服务器目录导入 (${CODE})"
        assert_contains "$TMP/kb-path.json" '"origin_label": "服务器目录"' "记录里标出来源是服务器目录"
      else
        bad "从服务器目录导入返回 ${CODE}"
      fi
      # 越界路径必须被拒 (这是"读服务器文件"能力的边界)
      CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 \
        -X POST -H 'Content-Type: application/json' \
        -d '{"paths":["../../etc/passwd"]}' "${BASE}/api/kb/import-path" 2>/dev/null || echo 000)"
      if [ "$CODE" = "400" ]; then
        ok "越界路径被拒绝 (${CODE})"
      else
        bad "越界路径应返回 400, 实际 ${CODE}"
      fi
      rm -f "$INBOX/smoke-inbox.md"
    else
      ok "未启用服务器导入目录 (跳过)"
    fi

    CODE="$(curl -fsS -o "$TMP/kb-records.json" -w '%{http_code}' --max-time 15 "${BASE}/api/kb/records" 2>/dev/null || echo 000)"
    if [ "$CODE" = "200" ]; then
      ok "导入记录接口 (${CODE})"
      assert_contains "$TMP/kb-records.json" '"origin_label"' "记录带来源标签"
    else
      bad "导入记录接口返回 ${CODE}"
    fi

    # 清理: 删掉本次自检导入的**全部**文档 (不留东西在用户的知识库里)
    #
    # 两个来源各有一份: 浏览器上传的 smoke-check.md 与服务器目录导入的 smoke-inbox.md。
    # 只删一份的话, 另一份会一直躺在知识库里。
    KB_IDS="$(cat "$TMP/kb-import.json" "$TMP/kb-path.json" 2>/dev/null \
      | sed -n 's/.*"id": "\(doc-[^"]*\)".*/\1/p' | sort -u)"
    if [ -n "$KB_IDS" ]; then
      DELETED=0
      for KB_ID in $KB_IDS; do
        CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 \
          -X DELETE "${BASE}/api/kb/${KB_ID}" 2>/dev/null || echo 000)"
        [ "$CODE" = "200" ] && DELETED=$((DELETED + 1))
      done
      if [ "$DELETED" -gt 0 ]; then
        ok "删除自检文档 (${DELETED} 份)"
      else
        bad "删除自检文档失败"
      fi
    else
      bad "没能从导入响应里取到文档编号"
    fi
    # 自检产生的导入记录也清掉: 记录是给用户看自己的操作历史的, 不该被部署自检塞满。
    # 只删自己这几条 (按编号), 不碰用户已有的记录。
    REC_IDS="$(sed -n 's/.*"id": "\(imp-[^"]*\)".*/\1/p' "$TMP/kb-import.json" "$TMP/kb-path.json" 2>/dev/null | sort -u)"
    if [ -n "$REC_IDS" ]; then
      for REC_ID in $REC_IDS; do
        curl -sS -o /dev/null --max-time 15 -X DELETE "${BASE}/api/kb/records/${REC_ID}" 2>/dev/null || true
      done
      ok "清掉自检产生的导入记录"
    fi
    # 自检结束后知识库里不该留下自检文档 (记录里留着名字是历史, 文档不能留)
    LEFT="$(curl -fsS --max-time 15 "${BASE}/api/kb" 2>/dev/null \
      | sed -n 's/.*"documents": \[\(.*\)\], *"records".*/\1/p' | grep -c 'smoke-' || true)"
    if [ "${LEFT:-0}" = "0" ]; then
      ok "知识库里没有残留自检文档"
    else
      bad "知识库里还残留 ${LEFT} 处自检文档"
    fi
  else
    ok "知识库已按配置关闭 (${CODE})"
  fi
else
  bad "导入文档返回 ${CODE}"
fi

# ---------- 2e. 主题与语言 ----------
echo "-- 主题与语言"
CODE="$(curl -fsS -o "$TMP/index.html" -w '%{http_code}' --max-time 15 "${BASE}/" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  assert_contains "$TMP/index.html" 'id="themeAuto"' "主题切换是图标按钮"
  assert_contains "$TMP/index.html" 'data-theme="dark"' "有深色配色方案"
  assert_contains "$TMP/index.html" 'prefers-color-scheme' "支持跟随系统"
  assert_contains "$TMP/index.html" 'id="themeDark"' "深色按钮在位"
  assert_contains "$TMP/index.html" 'id="langEn"' "语言切换按钮在位"
  assert_contains "$TMP/index.html" 'id="docDrawer"' "知识库文档可在页面中打开"
  assert_contains "$TMP/index.html" 'kb-pick primary' "选择本地文件是按钮"
  assert_contains "$TMP/index.html" 'className = "kb-table"' "已导入文档以表格显示"
  if grep -qF 'kbPasteText' "$TMP/index.html"; then
    bad "粘贴 Markdown 的入口应已移除"
  else
    ok "已移除粘贴 Markdown 入口"
  fi
  assert_contains "$TMP/index.html" 'RunStor Bot' "应用名已改"
else
  bad "首页返回 ${CODE}"
fi

CODE="$(curl -fsS -o "$TMP/i18n.json" -w '%{http_code}' --max-time 15 "${BASE}/api/i18n?lang=en" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "文案词典接口 (${CODE})"
  assert_contains "$TMP/i18n.json" '"messages"' "返回中文→英文对照表"
  assert_contains "$TMP/i18n.json" '"lang": "en"' "标明语言"
else
  bad "词典接口返回 ${CODE}"
fi

# 英文模式下响应里不该再出现中文 (漏译会被这条抓住)
CODE="$(curl -fsS -o "$TMP/health-en.json" -w '%{http_code}' --max-time 15 "${BASE}/health?lang=en" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  if grep -q '[一-龥]' "$TMP/health-en.json"; then
    bad "英文模式下健康检查仍含中文: $(head -c 120 "$TMP/health-en.json")"
  else
    ok "英文模式响应无中文残留"
  fi
  CODE="$(curl -sS -o "$TMP/inspect-bad-en.json" -w '%{http_code}' --max-time 20 \
    -X POST -H 'Content-Type: application/json' -d '{"hosts":[],"user":""}' \
    "${BASE}/api/inspect?lang=en" 2>/dev/null || echo 000)"
  if grep -q '[一-龥]' "$TMP/inspect-bad-en.json"; then
    bad "英文模式下的报错仍是中文: $(head -c 120 "$TMP/inspect-bad-en.json")"
  else
    ok "英文模式报错也是英文"
  fi
else
  bad "健康检查 (lang=en) 返回 ${CODE}"
fi

# ---------- 3. 驱动列表 ----------
echo "-- 驱动列表"
CODE="$(curl -fsS -o "$TMP/drivers.json" -w '%{http_code}' --max-time 10 "${BASE}/api/drivers" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /api/drivers 返回 200"
  assert_contains "$TMP/drivers.json" '"auth_mode"' "oracle 驱动声明了 auth_mode 槽位"
  assert_contains "$TMP/drivers.json" '"channels"' "oracle 驱动声明了 channels 槽位"
else
  bad "GET /api/drivers 返回 ${CODE}"
fi

# ---------- 4. 模型配置不泄露密钥 ----------
echo "-- 模型配置"
CODE="$(curl -fsS -o "$TMP/profiles.json" -w '%{http_code}' --max-time 10 "${BASE}/api/llm/profiles" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /api/llm/profiles 返回 200"
  if grep -qE '"(api_key|apiKey)"[[:space:]]*:' "$TMP/profiles.json"; then
    bad "响应中出现了 api_key 字段 (存在泄露风险)"
  else
    ok "响应未包含 api_key 字段"
  fi
  assert_contains "$TMP/profiles.json" '"has_key"' "以 has_key 布尔值报告密钥有无"
else
  bad "GET /api/llm/profiles 返回 ${CODE}"
fi

# ---------- 5. 核心闭环: 追问 -> 补齐 -> ready ----------
echo "-- 解析与追问闭环"
DEMAND='周天全备份oracle数据库，每天增量备份，oracle使用系统认证，备份整个实例，并发设置为4'
cat > "$TMP/parse-req.json" <<JSON
{"text": "${DEMAND}"}
JSON

CODE="$(curl -fsS -o "$TMP/parse.json" -w '%{http_code}' --max-time 60 \
  -X POST -H 'Content-Type: application/json' --data-binary @"$TMP/parse-req.json" \
  "${BASE}/api/parse" 2>/dev/null || echo 000)"
if [ "$CODE" != "200" ]; then
  bad "POST /api/parse 返回 ${CODE}"
  echo "        响应: $(head -c 300 "$TMP/parse.json" 2>/dev/null | tr '\n' ' ')"
else
  ok "POST /api/parse 返回 200"
  assert_contains "$TMP/parse.json" '"status": "needs_clarification"' "信息不完整时进入追问"
  assert_contains "$TMP/parse.json" '"kind": "backup"' "意图识别为备份"
  assert_contains "$TMP/parse.json" '"kind": "oracle"' "识别出 Oracle 引擎"
  assert_contains "$TMP/parse.json" '"auth_mode": "os_auth"' "识别出系统认证"
  assert_contains "$TMP/parse.json" '"scope": "whole_instance"' "识别出整实例"
  assert_contains "$TMP/parse.json" '"channels": 4' "识别出并发度为 4"
  assert_contains "$TMP/parse.json" '"frequency": "weekly"' "识别出每周全备"
  assert_contains "$TMP/parse.json" '"frequency": "daily"' "识别出每日增量"
  # 用 "question" 计数而不是 "slot": 总结里的可编辑行也带 slot 字段
  # (界面上的"修改这一项"要用它), 拿 slot 计数会把总结行误算成追问。
  assert_count  "$TMP/parse.json" '"question":' 4 "恰好追问 4 项"
  # 追问要带可点的推荐值 (需求方明确要求"推荐选择 + 自定义回答")
  assert_contains "$TMP/parse.json" '"suggestions"' "追问带推荐值字段"
  assert_contains "$TMP/parse.json" '凌晨1点' "给出了执行时刻的推荐值"
  assert_contains "$TMP/parse.json" '保留30天' "给出了保留期的推荐值"
  assert_contains "$TMP/parse.json" '本地磁盘' "给出了存储类型候选值"

  SESSION="$(sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/parse.json" | head -1)"
  if [ -z "$SESSION" ]; then
    bad "未取到 session_id"
  else
    ok "取得 session_id (${SESSION})"

    cat > "$TMP/clarify-req.json" <<JSON
{"session_id": "${SESSION}", "answers": {
  "schedule.full.time": "凌晨1点",
  "schedule.incremental.time": "凌晨1点",
  "retention": "保留14天",
  "destinations": "本地磁盘 /u01/backup/orcl"
}}
JSON

    CODE="$(curl -fsS -o "$TMP/clarify.json" -w '%{http_code}' --max-time 60 \
      -X POST -H 'Content-Type: application/json' --data-binary @"$TMP/clarify-req.json" \
      "${BASE}/api/clarify" 2>/dev/null || echo 000)"
    if [ "$CODE" != "200" ]; then
      bad "POST /api/clarify 返回 ${CODE}"
      echo "        响应: $(head -c 300 "$TMP/clarify.json" 2>/dev/null | tr '\n' ' ')"
    else
      ok "POST /api/clarify 返回 200"
      assert_contains "$TMP/clarify.json" '"status": "ready"' "补齐后状态为 ready"
      assert_contains "$TMP/clarify.json" '"time": "01:00"' "回答的时刻已落盘"
      assert_contains "$TMP/clarify.json" '"value": 14' "保留期已落盘"
      assert_contains "$TMP/clarify.json" '"/u01/backup/orcl"' "备份路径已落盘"
      assert_contains "$TMP/clarify.json" '"questions": []' "ready 时不再有追问"
    fi
  fi
fi

# ---------- 6. 网页改配置 (选厂商 + 填 Key) ----------
echo "-- 网页改配置"
if grep -qF '"runtime_enabled": true' "$TMP/profiles.json"; then
  ok "网页改配置已启用"

  # 用一个本地预设保存, 不涉及任何真实密钥
  # 记下当前模型, 自检结束要确认没把它换掉
  ACTIVE_BEFORE="$(sed -n 's/.*"active_profile"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/health.json" 2>/dev/null | head -1)"

  # activate=false: 自检不该动用户选定的模型
  cat > "$TMP/save-req.json" <<'JSON'
{"preset": "ollama", "model": "qwen2.5:14b", "name": "smoke-local", "activate": false}
JSON
  CODE="$(curl -fsS -o "$TMP/save.json" -w '%{http_code}' --max-time 15 \
    -X POST -H 'Content-Type: application/json' --data-binary @"$TMP/save-req.json" \
    "${BASE}/api/llm/profiles" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "POST /api/llm/profiles 保存成功"
    assert_contains "$TMP/save.json" '"saved": "smoke-local"' "返回保存的配置名"
  else
    bad "保存配置返回 ${CODE}"
    echo "        响应: $(head -c 300 "$TMP/save.json" 2>/dev/null | tr '\n' ' ')"
  fi

  # 保存后列表里应能看到, 且再次拉取不得回显任何密钥
  CODE="$(curl -fsS -o "$TMP/profiles2.json" -w '%{http_code}' --max-time 10 \
    "${BASE}/api/llm/profiles" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    assert_contains "$TMP/profiles2.json" '"name": "smoke-local"' "新配置出现在列表中"
    if grep -qE '"(api_key|apiKey)"[[:space:]]*:' "$TMP/profiles2.json"; then
      bad "保存后列表出现了 api_key 字段"
    else
      ok "保存后列表仍未包含 api_key 字段"
    fi
  fi

  # 清理: 删除刚创建的配置, 让自检可重复执行
  CODE="$(curl -fsS -o "$TMP/del.json" -w '%{http_code}' --max-time 10 \
    -X DELETE "${BASE}/api/llm/profiles/smoke-local" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "DELETE /api/llm/profiles/{name} 删除成功"
  else
    bad "删除配置返回 ${CODE}"
  fi

  # 配置文件声明的项不该能在网页上删除 (demo 内置项同理不该存在)
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
    -X DELETE "${BASE}/api/llm/profiles/ghost" 2>/dev/null || echo 000)"
  if [ "$CODE" = "400" ]; then
    ok "删除不存在的配置返回 400"
  else
    bad "删除不存在的配置应返回 400, 实际 ${CODE}"
  fi

  # 关键回归点: 自检保存并删除了一个临时配置, 用户选定的模型必须原样不动。
  # 早期版本这里会默认 activate=true, 于是每次部署后用户的模型都悄悄退回 demo。
  curl -fsS -o "$TMP/health2.json" --max-time 10 "${BASE}/health" 2>/dev/null || true
  ACTIVE_AFTER="$(sed -n 's/.*"active_profile"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/health2.json" 2>/dev/null | head -1)"
  if [ "$ACTIVE_BEFORE" = "$ACTIVE_AFTER" ]; then
    ok "自检未扰动当前模型（${ACTIVE_AFTER:-demo}）"
  else
    bad "自检改变了当前模型: \"${ACTIVE_BEFORE}\" -> \"${ACTIVE_AFTER}\""
  fi
else
  ok "网页改配置已关闭 (由配置决定, 跳过写入检查)"
fi

# ---------- 7. 对话式界面: 历史列表 / 详情 / 自由文本补充 / 删除 ----------
echo "-- 对话历史"
CODE="$(curl -fsS -o "$TMP/sessions.json" -w '%{http_code}' --max-time 15 \
  "${BASE}/api/sessions" 2>/dev/null || echo 000)"
if [ "$CODE" = "200" ]; then
  ok "GET /api/sessions 返回 200"
  assert_contains "$TMP/sessions.json" '"sessions"' "响应包含会话列表"
  # 上一步解析出的会话应出现在历史里
  if [ -n "${SESSION:-}" ] && grep -qF "$SESSION" "$TMP/sessions.json"; then
    ok "刚解析的会话出现在历史列表中"
  else
    bad "刚解析的会话未出现在历史列表中"
  fi
  assert_contains "$TMP/sessions.json" '"title"' "列表项带标题（侧栏显示用）"
  assert_contains "$TMP/sessions.json" '"updated_at"' "列表项带活动时间（排序用）"
else
  bad "GET /api/sessions 返回 ${CODE}"
fi

if [ -n "${SESSION:-}" ]; then
  CODE="$(curl -fsS -o "$TMP/detail.json" -w '%{http_code}' --max-time 15 \
    "${BASE}/api/sessions/${SESSION}" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "GET /api/sessions/{id} 返回 200"
    assert_contains "$TMP/detail.json" '"transcript"' "详情带对话记录（用于重建对话）"
    assert_contains "$TMP/detail.json" '"role": "user"' "对话记录含用户发言"
    assert_contains "$TMP/detail.json" '"role": "agent"' "对话记录含 Agent 回合"
    assert_contains "$TMP/detail.json" '"plan"' "详情带计划"
  else
    bad "GET /api/sessions/{id} 返回 ${CODE}"
  fi

  # 自由文本补充: 对话式界面的主输入框走这条路
  cat > "$TMP/followup.json" <<JSON
{"session_id": "${SESSION}", "text": "保留期改成90天"}
JSON
  CODE="$(curl -fsS -o "$TMP/followup-resp.json" -w '%{http_code}' --max-time 60 \
    -X POST -H 'Content-Type: application/json' --data-binary @"$TMP/followup.json" \
    "${BASE}/api/clarify" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "POST /api/clarify 支持自由文本补充"
    assert_contains "$TMP/followup-resp.json" '"status"' "返回回合状态"
  else
    bad "自由文本补充返回 ${CODE}"
    echo "        响应: $(head -c 300 "$TMP/followup-resp.json" 2>/dev/null | tr '\n' ' ')"
  fi

  # 总结与定稿 (MVP 不执行备份, 提交 = 用户确认这份 JSON)
  assert_contains "$TMP/clarify.json" '"summary"' "信息完整时给出总结"
  assert_contains "$TMP/clarify.json" '"rows"' "总结含中文清单"
  assert_contains "$TMP/clarify.json" '认证方式' "总结里的字段是中文标签"
  if grep -qF 'os_auth' "$TMP/clarify.json"; then
    # 计划体里出现代码值是对的; 但总结里不该出现
    SUMMARY_PART="$(sed -n '/"summary"/,$p' "$TMP/clarify.json")"
    case "$SUMMARY_PART" in
      *os_auth*) bad "总结里出现了代码值 os_auth（应翻成中文）" ;;
      *) ok "总结用中文而非代码值" ;;
    esac
  fi

  CODE="$(curl -fsS -o "$TMP/submit.json" -w '%{http_code}' --max-time 15 \
    -X POST "${BASE}/api/sessions/${SESSION}/submit" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "POST /api/sessions/{id}/submit 提交成功"
    assert_contains "$TMP/submit.json" '"summary"' "提交后仍返回总结（界面显示已定稿）"
  else
    bad "提交返回 ${CODE}"
  fi
  curl -fsS -o "$TMP/sessions3.json" --max-time 10 "${BASE}/api/sessions" 2>/dev/null || true
  assert_contains "$TMP/sessions3.json" '"submitted": true' "历史列表标记已提交"

  # 清理, 让自检可重复执行
  CODE="$(curl -fsS -o /dev/null -w '%{http_code}' --max-time 15 \
    -X DELETE "${BASE}/api/sessions/${SESSION}" 2>/dev/null || echo 000)"
  if [ "$CODE" = "200" ]; then
    ok "DELETE /api/sessions/{id} 删除成功"
  else
    bad "删除会话返回 ${CODE}"
  fi
fi

# ---------- 8. 错误处理 ----------
echo "-- 错误处理"
CODE="$(curl -sS -o "$TMP/badparse.json" -w '%{http_code}' --max-time 10 \
  -X POST -H 'Content-Type: application/json' -d '{"text":""}' \
  "${BASE}/api/parse" 2>/dev/null || echo 000)"
if [ "$CODE" = "400" ]; then
  ok "空文本返回 400 而非 500"
else
  bad "空文本应返回 400, 实际 ${CODE}"
fi

CODE="$(curl -sS -o "$TMP/notfound.json" -w '%{http_code}' --max-time 10 \
  "${BASE}/api/no-such-endpoint" 2>/dev/null || echo 000)"
if [ "$CODE" = "404" ]; then
  ok "未知路径返回 404"
else
  bad "未知路径应返回 404, 实际 ${CODE}"
fi

# ---------- 8. 运行态文件权限 ----------
echo "-- 运行态文件权限"
STATE_DIR=/var/lib/backup-restore-agent
if [ -d "$STATE_DIR" ]; then
  MODE="$(stat -c '%a' "$STATE_DIR" 2>/dev/null || echo '?')"
  if [ "$MODE" = "700" ]; then
    ok "状态目录权限为 700"
  else
    bad "状态目录权限应为 700, 实际 ${MODE}"
  fi
  if [ -f "$STATE_DIR/runtime.json" ]; then
    RMODE="$(stat -c '%a' "$STATE_DIR/runtime.json" 2>/dev/null || echo '?')"
    if [ "$RMODE" = "600" ]; then
      ok "模型配置权限为 600（含密钥）"
    else
      bad "模型配置权限应为 600, 实际 ${RMODE}"
    fi
  fi
  if [ -f "$STATE_DIR/sessions.json" ]; then
    SMODE="$(stat -c '%a' "$STATE_DIR/sessions.json" 2>/dev/null || echo '?')"
    if [ "$SMODE" = "600" ]; then
      ok "对话历史权限为 600"
    else
      bad "对话历史权限应为 600, 实际 ${SMODE}"
    fi
  fi
else
  ok "无状态目录（当前部署未启用落盘）"
fi

# ---------- 汇总 ----------
echo
echo "== 自检结果: PASS=${PASS} FAIL=${FAIL} =="
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
exit 0
