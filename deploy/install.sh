#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# RunStor Bot · 安装/升级脚本 (bundle 内执行, 需 root)
#
# 用法:
#   bash deploy/install.sh install          # 安装或升级
#   bash deploy/install.sh install --dry-run
#   bash deploy/install.sh status
#   bash deploy/install.sh uninstall
#
# 环境变量 (带缺省):
#   PREFIX=/opt/backup-restore-agent
#   CFG_DIR=/etc/backup-restore-agent
#   PORT_RANGE="8081 8099"   首选端口与搜索上界
#   SERVICE_USER=bra
#   NO_START=0
# ---------------------------------------------------------------------------
set -uo pipefail

PREFIX=${PREFIX:-/opt/backup-restore-agent}
CFG_DIR=${CFG_DIR:-/etc/backup-restore-agent}
SERVICE=backup-restore-agent
SERVICE_USER=${SERVICE_USER:-bra}
PORT_PREFERRED=${PORT_PREFERRED:-8081}
PORT_MAX=${PORT_MAX:-8099}
NO_START=${NO_START:-0}
DRY_RUN=0
ETC="$CFG_DIR"
# 状态目录由 systemd 的 StateDirectory= 创建 (服务用户属主), 这里给导入目录用
STATE_DIR=${STATE_DIR:-/var/lib/backup-restore-agent}
# 预置知识库文档的安装位置 (与 config 里的 server.kb.seed_dir 默认值一致)
SEED_DIR=${SEED_DIR:-/usr/share/backup-restore-agent/knowledge}
SRC="$(cd "$(dirname "$0")/.." && pwd)"

log()  { echo "[install] $*"; }
warn() { echo "[install] 警告: $*" >&2; }
die()  { echo "[install] 错误: $*" >&2; exit 1; }

run() {
  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] $*"
    return 0
  fi
  "$@"
}

# ---------------------------------------------------------------------------
# 端口探测
#
# 需求是"用一个可用的 80xx 端口", 因此这里不硬编码: 部署时实测。
# 全部占用时明确报错并列出占用者, 绝不静默换到 80xx 之外 —— 那样会让运维
# 按约定配好的防火墙规则失效。
# ---------------------------------------------------------------------------

listening_ports() {
  if command -v ss >/dev/null 2>&1; then
    ss -lnt 2>/dev/null | awk 'NR>1 {print $4}'
    return
  fi
  if command -v netstat >/dev/null 2>&1; then
    netstat -lnt 2>/dev/null | awk 'NR>2 {print $4}'
    return
  fi
  echo ""
}

port_in_use() {
  local port=$1
  listening_ports | grep -qE "[:.]${port}\$"
}

port_owner() {
  local port=$1
  if command -v ss >/dev/null 2>&1; then
    ss -lntp 2>/dev/null | awk -v p=":${port}\$" '$4 ~ p {print $6; exit}'
  fi
}

# port_held_by_us 判断端口是不是被本服务自己占着。
#
# 这个判断是升级场景的关键: 升级时服务正跑在原来的端口上, 若只用"端口是否被占用"
# 来决定, 就会误判为"需要换端口", 于是每次部署端口都往后漂一格。
#
# 两个信号: 本服务的 systemd 单元处于 active; 或端口属主看起来是我们的二进制。
# 注意 ss 会把进程名截断到 15 字符 (backup-restore-agent -> backup-restore-),
# 所以匹配用截断后的前缀。
port_held_by_us() {
  local port=$1
  if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
    return 0
  fi
  local owner prefix
  owner="$(port_owner "$port")"
  prefix="${SERVICE:0:15}"
  case "$owner" in
    *"$prefix"*) return 0 ;;
  esac
  return 1
}

pick_port() {
  local candidate
  for candidate in $(seq "$PORT_PREFERRED" "$PORT_MAX"); do
    if ! port_in_use "$candidate"; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

resolve_port() {
  # 运维明确指定端口时优先, 但仍要确认它是空闲的或本来就是我们在用
  if [ -n "${PORT_FORCE:-}" ]; then
    if port_in_use "$PORT_FORCE" && ! port_held_by_us "$PORT_FORCE"; then
      echo "[install] 错误: 指定的端口 $PORT_FORCE 已被其它进程占用 ($(port_owner "$PORT_FORCE"))" >&2
      return 1
    fi
    echo "$PORT_FORCE"
    return 0
  fi

  # 已安装过 -> 沿用配置里的端口 (升级时它被自己占着是正常的)
  local existing
  existing="$(read_configured_port)"
  if [ -n "$existing" ]; then
    if ! port_in_use "$existing" || port_held_by_us "$existing"; then
      echo "$existing"
      return 0
    fi
    echo "[install] 提示: 原端口 $existing 已被其它进程占用 ($(port_owner "$existing")), 将另选端口" >&2
  fi

  pick_port
}

read_configured_port() {
  local config="$ETC/config.toml"
  if [ ! -f "$config" ]; then
    # 升级场景: 老版本只有 config.json, 而 config.toml 还没生成。
    # 这里必须也认它 —— 否则"读不到端口"会被当成"首次安装", 于是重新挑一个端口,
    # 表现为升级后服务悄悄搬到了 8082, 而运维的防火墙规则/书签还指着 8081。
    read_legacy_port
    return 0
  fi
  # 不依赖 toml 解析器 (部署优势就是零运行时依赖): 只看 [server] 段里的第一个 port。
  # 必须限定在 [server] 段内 —— 否则别的段出现同名键就会抓错。
  awk '
    /^[[:space:]]*\[/ { in_server = ($0 ~ /^[[:space:]]*\[server\][[:space:]]*$/); next }
    in_server && /^[[:space:]]*port[[:space:]]*=/ {
      sub(/^[^=]*=[[:space:]]*/, ""); gsub(/[^0-9]/, ""); print; exit
    }
  ' "$config"
}

read_legacy_port() {
  local legacy="$ETC/config.json"
  [ -f "$legacy" ] || return 0
  sed -n '/"server"[[:space:]]*:/,/^[[:space:]]*}/p' "$legacy" 2>/dev/null \
    | sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]\{1,5\}\).*/\1/p' | head -1
}

# ---------------------------------------------------------------------------
# 依赖
# ---------------------------------------------------------------------------

check_prereqs() {
  local missing=""
  for tool in systemctl; do
    command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
  done
  if [ -n "$missing" ]; then
    die "缺少必需工具:$missing (本脚本面向 systemd 环境)"
  fi
  if ! command -v ss >/dev/null 2>&1 && ! command -v netstat >/dev/null 2>&1; then
    warn "没有 ss 也没有 netstat, 无法预检端口占用; 将直接尝试监听"
  fi
}

# ---------------------------------------------------------------------------
# 安装
# ---------------------------------------------------------------------------

install_bin() {
  local binary="$SRC/bin/$SERVICE"
  [ -f "$binary" ] || die "缺少 $binary; 请先在开发机执行 make package"
  run install -d -m 0755 "$PREFIX/bin"
  run install -m 0755 "$binary" "$PREFIX/bin/$SERVICE"
  log "已安装可执行文件到 $PREFIX/bin/$SERVICE"

  if [ -f "$SRC/README.md" ]; then
    run install -m 0644 "$SRC/README.md" "$PREFIX/README.md"
  fi

  # 一并装好运维脚本, 便于在目标机上直接跑 status / 自检
  if [ -d "$SRC/deploy" ]; then
    run install -d -m 0755 "$PREFIX/deploy"
    for script in install.sh smoke.sh; do
      [ -f "$SRC/deploy/$script" ] && run install -m 0755 "$SRC/deploy/$script" "$PREFIX/deploy/$script"
    done
    [ -f "$SRC/deploy/env.example" ] && run install -m 0644 "$SRC/deploy/env.example" "$PREFIX/deploy/env.example"
  fi
}

ensure_user() {
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    return
  fi
  log "创建系统用户 $SERVICE_USER (无登录、无特权)"
  run useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER" \
    || die "创建用户失败"
}

ensure_config() {
  run install -d -m 0750 "$ETC"
  local config="$ETC/config.toml"
  local legacy="$ETC/config.json"
  local port="$1"

  # 旧版本的 JSON 配置: 迁移提示。
  #
  # 不做"自动把 JSON 转成 TOML": 配置里可能有注释之外的语义差别 (比如 profiles 的
  # 嵌套写法), 自动转换一旦转换错了, 症状是"升级后模型不生效", 很难查。这里只做
  # 两件确定的事: 保底沿用旧端口, 把旧文件改名留档, 并明确告诉运维要手工搬哪些值。
  if [ -f "$legacy" ] && [ ! -f "$config" ]; then
    local legacy_backup="$legacy.migrated-$(date +%Y%m%d%H%M%S)"
    local legacy_port=""
    legacy_port="$(sed -n '/"server"[[:space:]]*:/,/^[[:space:]]*}/p' "$legacy" 2>/dev/null \
      | sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]\{1,5\}\).*/\1/p' | head -1)"
    [ -n "$legacy_port" ] && port="$legacy_port"
    warn "检测到旧格式配置 $legacy (本版本起只读 config.toml)"
    warn "  将生成 $config (端口沿用 ${port}); 旧文件保留为 $legacy_backup"
    warn "  旧配置里的 server.kb.* / 自定义 profile 等需要手工搬到 config.toml,"
    warn "  可对照 $PREFIX/README.md 或包内的 config.example.toml"
  fi

  if [ -f "$config" ]; then
    # 升级时不动已有配置, 只备份一份
    local backup="$config.bak-$(date +%Y%m%d%H%M%S)"
    run cp -p "$config" "$backup"
    log "已保留现有配置 (备份: $backup)"

    # 端口可能变了, 同步写回(保留其余内容不动)
    local want_port="$port"
    local have_port
    have_port="$(read_configured_port)"
    if [ "$have_port" != "$want_port" ]; then
      if [ "$DRY_RUN" = 1 ]; then
        echo "[dry-run] 更新端口 $have_port -> $want_port"
      else
        sed -i "0,/^\([[:space:]]*port[[:space:]]*=[[:space:]]*\)[0-9]\{1,5\}/\1${want_port}/" "$config"
        log "已更新监听端口为 $want_port"
      fi
    fi
  else
    if [ "$DRY_RUN" = 1 ]; then
      echo "[dry-run] 生成 $config (port=$port)"
    else
      cat > "$config" <<TOML
# RunStor Bot 配置 (TOML)。改完需 systemctl restart $SERVICE。
# 完整可选项与说明见 $PREFIX/README.md 与包内 config.example.toml。
# 注意: 未知的键会导致启动失败 (这是有意的, 避免"配置看着生效其实没生效")。

[server]
host = "0.0.0.0"
port = ${port}
prompt_limit = 20000
max_turns = 10
session_ttl_min = 120
language = "zh-CN"

[llm]
# 模型配置两种方式: 写在这里 (见 README), 或在网页上"模型设置"里填。
# 密钥请走环境变量或 0600 文件, 不要内联明文 (本地端点除外)。
default_profile = ""

[llm.profiles]
TOML
      log "已生成初始配置 $config (尚未配置模型, 启动后为 demo 模式)"
    fi
  fi
  # 旧文件改名留档 (不是删除: 里面可能有运维自己加的内容)
  if [ -f "$legacy" ]; then
    run mv "$legacy" "$legacy.migrated-$(date +%Y%m%d%H%M%S)"
  fi
  run chmod 0640 "$config"
  run chgrp "$SERVICE_USER" "$config"
  # 目录属组也必须跟着服务用户: 否则服务用户不在目录的属组里, 连穿越目录
  # 都做不到, 表现为"配置文件不存在" —— 而文件其实好好躺在那儿。
  run chgrp "$SERVICE_USER" "$ETC"
}

# ensure_kb_seed 安装"随部署预置的知识库文档"。
#
# 放 /usr/share 而不是 /etc 或 /var/lib: 它是产品自带的数据 (只读), 与运行态和配置分开。
# 服务启动时会把这里的 .md 同步进知识库 —— 缺什么补什么, 且不覆盖操作者自己的同名文档。
ensure_kb_seed() {
  local seed_dir="$SEED_DIR"
  if [ ! -d "$SRC/knowledge" ]; then
    log "本包没有预置文档 (knowledge/ 不存在), 跳过"
    return
  fi
  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] 安装预置文档到 $seed_dir"
    return
  fi
  run rm -rf "$seed_dir"
  run mkdir -p "$seed_dir"
  # 目录结构 (zh/ en/) 一起保留: 便于运维按语言找文件
  run cp -r "$SRC/knowledge/." "$seed_dir/"
  local count
  count="$(find "$seed_dir" -name '*.md' -type f 2>/dev/null | wc -l | tr -d ' ')"
  log "已安装 $count 份预置文档到 $seed_dir (服务启动时自动同步进知识库)"
}

# ensure_kb_inbox 准备"从服务器目录导入"用的目录。
#
# 属主给服务用户而不是 root: 目录里的文件是管理员放进去的, 服务只读; 但如果目录属
# root 且不可写, 用服务用户放文件 (或就地生成手册) 就会失败 —— 属主给服务用户最省事。
# 另外提醒一句权限: 服务以无特权用户运行, root 放进去的文件要可读 (默认 umask 0022
# 生成的 0644 就可以, 特意 chmod 600 的文件它读不到)。
ensure_kb_inbox() {
  local inbox="$STATE_DIR/kb-inbox"
  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] 准备知识库导入目录 $inbox"
    return
  fi
  run mkdir -p "$inbox"
  run chown "$SERVICE_USER:$SERVICE_USER" "$inbox"
  run chmod 0755 "$inbox"
  log "知识库导入目录: $inbox (把运维手册 .md 放这里, 界面上可一键导入)"
}

ensure_env_file() {
  local env_file="$ETC/env"
  if [ -f "$env_file" ]; then
    run chmod 0600 "$env_file"
    run chown root:root "$env_file"
    return
  fi
  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] 生成 $env_file"
    return
  fi
  if [ -f "$SRC/deploy/env.example" ]; then
    install -m 0600 -o root -g root "$SRC/deploy/env.example" "$env_file"
  else
    install -m 0600 -o root -g root /dev/null "$env_file"
  fi
  log "已生成密钥文件 $env_file (0600); 填入模型 Key 后重启即可接入模型"
}

install_unit() {
  local unit_src="$SRC/deploy/$SERVICE.service"
  [ -f "$unit_src" ] || die "缺少 $unit_src"
  run install -m 0644 "$unit_src" "/etc/systemd/system/$SERVICE.service"
  ensure_kb_seed
  ensure_kb_inbox
  run systemctl daemon-reload
  log "已安装 systemd 单元 $SERVICE"
}

start_service() {
  run systemctl enable "$SERVICE" >/dev/null 2>&1
  if [ "$NO_START" = 1 ]; then
    log "NO_START=1, 未启动服务"
    return
  fi
  run systemctl restart "$SERVICE"
  sleep 1
  if [ "$DRY_RUN" = 0 ] && ! systemctl is-active --quiet "$SERVICE"; then
    warn "服务未能启动, 最近日志:"
    journalctl -u "$SERVICE" -n 25 --no-pager 2>/dev/null | sed 's/^/    /' >&2
    die "启动失败"
  fi
}

check_firewall() {
  local port=$1
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "^Status: active"; then
    log "检测到 ufw 已启用; 如需外部访问请执行: ufw allow ${port}/tcp"
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state 2>/dev/null | grep -q running; then
    log "检测到 firewalld 已启用; 如需外部访问请执行: firewall-cmd --add-port=${port}/tcp --permanent && firewall-cmd --reload"
  fi
}

smoke() {
  local port=$1
  local script="$SRC/deploy/smoke.sh"
  [ -f "$script" ] || return 0
  log "运行部署后自检 …"
  if [ "$DRY_RUN" = 1 ]; then
    echo "[dry-run] bash $script $port"
    return 0
  fi
  if bash "$script" "$port"; then
    return 0
  fi
  warn "自检未全部通过; 服务可能仍在启动中, 可稍后手工执行: bash $script $port"
  return 0
}

# ---------------------------------------------------------------------------
# 动作
# ---------------------------------------------------------------------------

do_install() {
  [ "$(id -u)" = 0 ] || die "请以 root 运行 (或使用 sudo)"
  check_prereqs

  local port
  port="$(resolve_port)"
  if [ -z "$port" ]; then
    echo "[install] 错误: 未能确定可用端口 (指定了 PORT_FORCE 时见上方说明);" >&2
    echo "  搜索范围 ${PORT_PREFERRED}-${PORT_MAX} 的占用情况:" >&2
    local probe
    for probe in $(seq "$PORT_PREFERRED" "$PORT_MAX"); do
      if port_in_use "$probe"; then
        echo "    :$probe 被占用 $(port_owner "$probe")" >&2
      fi
    done
    echo "    请释放端口, 或用 PORT_PREFERRED/PORT_MAX 指定其它端口段。" >&2
    exit 1
  fi

  log "使用端口 $port"
  install_bin
  ensure_user
  ensure_config "$port"
  ensure_env_file
  install_unit
  start_service
  check_firewall "$port"

  echo
  log "安装完成"
  log "  管理地址: http://$(hostname -I 2>/dev/null | awk '{print $1}'):${port}/"
  log "  配置文件: $ETC/config.toml"
  log "  模型密钥: $ETC/env (0600)"
  log "  查看日志: journalctl -u $SERVICE -f"
  smoke "$port"
}

do_status() {
  local port
  port="$(read_configured_port)"
  echo "配置目录 : $ETC"
  echo "安装目录 : $PREFIX"
  echo "监听端口 : ${port:-未知}"
  echo "配置文件 : $ETC/config.toml"
  if [ -f "$ETC/env" ]; then
    echo "密钥文件 : $ETC/env ($(stat -c '%a' "$ETC/env" 2>/dev/null))"
  fi
  if systemctl list-unit-files "$SERVICE.service" >/dev/null 2>&1; then
    echo "服务状态 : $(systemctl is-active "$SERVICE" 2>/dev/null) / $(systemctl is-enabled "$SERVICE" 2>/dev/null)"
  fi
  if [ -n "$port" ] && command -v curl >/dev/null 2>&1; then
    echo -n "健康检查 : "
    curl -fsS --max-time 5 "http://127.0.0.1:${port}/health" 2>/dev/null || echo "无响应"
    echo
  fi
}

do_uninstall() {
  [ "$(id -u)" = 0 ] || die "请以 root 运行"
  run systemctl disable --now "$SERVICE" >/dev/null 2>&1
  run rm -f "/etc/systemd/system/$SERVICE.service"
  run systemctl daemon-reload
  run rm -rf "$PREFIX"
  log "已卸载程序与 systemd 单元"
  log "保留 $ETC 下的配置与密钥 (含密钥, 请确认后再手工删除)"
}

usage() {
  cat <<EOF
用法: bash deploy/install.sh <动作>

动作:
  install     安装或升级 (自动挑选可用端口)
  status      查看状态与健康检查
  uninstall   卸载程序 (保留配置)

环境变量:
  PREFIX=$PREFIX
  CFG_DIR=$CFG_DIR
  PORT_PREFERRED=$PORT_PREFERRED
  PORT_MAX=$PORT_MAX
  PORT_FORCE=${PORT_FORCE:-未设定}   # 明确指定端口, 跳过自动挑选
  SERVICE_USER=$SERVICE_USER
  NO_START=$NO_START
EOF
}

ACTION=${1:-}
shift 2>/dev/null || true
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    *) die "未知参数: $arg" ;;
  esac
done

case "$ACTION" in
  install)   do_install ;;
  status)    do_status ;;
  uninstall) do_uninstall ;;
  ""|-h|--help|help) usage ;;
  *) usage; exit 1 ;;
esac
