#!/bin/sh
# tiny-xboard installer / upgrader
#
# Philosophy: the DEVELOPER machine builds binaries; this repository serves
# them as plain files; the TARGET server only downloads + verifies + runs.
# The installer NEVER compiles on the server and NEVER uses GitHub Releases.
#
# Binary source (plain repository files, no releases, no GitHub API):
#   https://raw.githubusercontent.com/<REPO>/<REF>/tiny-xboard/bin/tiny-xboard-linux-<arch>
#   https://raw.githubusercontent.com/<REPO>/<REF>/tiny-xboard/checksums/SHA256SUMS
#
# Overridable env:
#   TINY_XBOARD_BASE_URL  full base URL (defaults to the raw.githubusercontent URL above)
#   TINY_XBOARD_BINARY    path to a LOCAL binary (skip download)
set -eu

APP="tiny-xboard"
REPO="ashvvvvv/mini-sb-agent"
REF="master"
INSTALL_DIR="/opt/$APP"
BIN_DIR="$INSTALL_DIR/bin"
SERVICE_NAME="$APP"
DATA_DIR=""
LISTEN=""
NODE_ID="1"
NODE_NAME=""
NODE_TYPE="vless"
NODE_PORT="443"
REALITY_SERVER_NAME="www.microsoft.com"
REALITY_SHORT_ID="abcd1234"
REALITY_PRIVATE_KEY=""
SYNC_INTERVAL="60"
FLUSH_INTERVAL="300"
TOKEN=""
GOMEMLIMIT=""
GOGC=""
GOMAXPROCS=""
START_SERVICE="1"
FORCE="0"
ENABLE="0"
ASSUME_YES="0"
ADD_NODE="0"
INTERACTIVE="auto"

usage() {
  cat <<'EOF'
tiny-xboard installer (Tiny Xboard UniProxy mock API)

用法示例：

  # 一键安装 / 升级（从仓库 raw 文件下载二进制，无需 Git/Go/Release）
  curl -fsSL https://raw.githubusercontent.com/ashvvvvv/mini-sb-agent/master/tiny-xboard/install.sh | sh

  # 非交互安装：VLESS 节点
  sh install.sh --token '节点密钥' --node-id 1 --node-type vless --node-port 443 --yes

  # 非交互安装：Hysteria 2 节点
  sh install.sh --token '节点密钥' --node-id 2 --node-type hy2 --node-port 44311 --yes

  # 指定 commit/ref 安装
  sh install.sh --ref abc1234

  # 多节点：先在管理机安装一次，再用 --add-node 追加节点（同一通讯密钥）
  sh install.sh --add-node --node-id 5 --node-type hy2 --node-port 44311 --yes

参数：
  --token TOKEN                 节点 token；不填则自动生成随机 token
  --node-id N                   节点 ID，默认 1
  --node-name NAME              节点显示名
  --node-type TYPE              vless / hy2（hysteria2/hysteria 同义）
  --node-port N                 节点代理端口，默认 443
  --listen ADDR                 API 监听地址，默认 127.0.0.1:8080
  --data-dir PATH               状态目录，默认 /etc/tiny-xboard（升级时保留）
  --reality-private-key KEY     VLESS Reality 私钥（可选，供 mini-sb-agent 生成配置）
  --reality-server-name NAME    Reality 回落域名，默认 www.microsoft.com
  --reality-short-id ID         Reality short_id，默认 abcd1234
  --sync-interval N             面板同步间隔（秒），默认 60
  --flush-interval N            流量落盘间隔（秒），默认 300
  --gomemlimit VALUE            默认 32MiB；与 sing-box 同机运行时的保守上限
  --gogc N                      默认 70
  --gomaxprocs N                默认 1
  --ref REF                     Git ref（分支名或 commit），默认 master；二进制从该 ref 下的仓库文件下载
  --version REF                 --ref 的别名（旧参数兼容）
  --add-node                    追加一个节点到 nodes.json（管理机已装好后再用）
  --force                       强制重新下载并校验二进制；不会删除 node.json/users.json/traffic.json
  --enable                      启用开机自启（systemd enable / rc-update add）
  --yes                         非交互确认
  --interactive                 强制进入问答式安装
  --non-interactive             禁止问答；缺参数直接报错
  --no-start                    只安装不启动
  -h, --help                    显示帮助
  环境变量 TINY_XBOARD_BASE_URL 覆盖下载地址；TINY_XBOARD_BINARY 指定本地二进制

说明：
  * 服务器只运行 curl/wget + sha256sum/openssl + 二进制，不编译、不需要 Git/Go。
  * 升级采用版本化二进制 + current 软链，可自动回滚；状态目录永远不被删除。
  * 首次安装生成最小 node.json / users.json / traffic.json，不自动创建用户。
EOF
}

err() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }
need_root() { [ "$(id -u)" = "0" ] || err "请用 root 运行"; }

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

shell_quote() {
  printf '%s' "$1" | sed "s/'/'\\''/g; 1s/^/'/; \$s/\$/'/"
}

is_tty() { [ -t 1 ] && [ -r /dev/tty ]; }

read_from_tty() {
  if [ -t 1 ]; then
    IFS= read -r ans </dev/tty || ans=""
  else
    IFS= read -r ans || ans=""
  fi
}

prompt() {
  var="$1"
  label="$2"
  default="$3"
  secret="${4:-0}"
  eval cur="\${$var:-}"
  [ -n "$cur" ] && default="$cur"
  if [ "$secret" = "1" ]; then
    printf '%s' "$label"
    [ -n "$default" ] && printf ' [默认：%s]' "$default"
    printf ': '
    if [ -t 1 ]; then
      stty -echo </dev/tty 2>/dev/null || true
    fi
    read_from_tty
    if [ -t 1 ]; then
      stty echo </dev/tty 2>/dev/null || true
    fi
    printf '\n'
  else
    printf '%s' "$label"
    [ -n "$default" ] && printf ' [默认：%s]' "$default"
    printf ': '
    read_from_tty
  fi
  [ -n "$ans" ] || ans="$default"
  eval "$var=\"\$ans\""
}

gen_token() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16
  elif [ -r /dev/urandom ]; then
    head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
  else
    printf 'change-me-%s' "$(date +%s)"
  fi
}

# --- HTTP + hash helpers (target-server toolchain only) -------------------
HAVE_CURL=0
HAVE_WGET=0
HAVE_SHA256SUM=0
HAVE_OPENSSL=0
command -v curl >/dev/null 2>&1 && HAVE_CURL=1
command -v wget >/dev/null 2>&1 && HAVE_WGET=1
command -v sha256sum >/dev/null 2>&1 && HAVE_SHA256SUM=1
command -v openssl >/dev/null 2>&1 && HAVE_OPENSSL=1

fetch() {
  url="$1"
  out="$2"
  if [ "$HAVE_CURL" = "1" ]; then
    curl -fSL --retry 3 --connect-timeout 15 -o "$out" "$url"
  elif [ "$HAVE_WGET" = "1" ]; then
    wget -O "$out" "$url"
  else
    err "缺少 curl/wget，无法下载"
  fi
}

http_ok() {
  url="$1"
  if [ "$HAVE_CURL" = "1" ]; then
    [ "$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$url")" = "200" ]
  elif [ "$HAVE_WGET" = "1" ]; then
    wget -qO /dev/null "$url"
  else
    return 1
  fi
}

sha256_hex() {
  f="$1"
  if [ "$HAVE_SHA256SUM" = "1" ]; then
    sha256sum "$f" | awk '{print $1}'
  elif [ "$HAVE_OPENSSL" = "1" ]; then
    openssl dgst -sha256 "$f" | sed 's/^.*= //'
  else
    err "缺少 sha256sum/openssl，无法校验下载的二进制（禁止跳过校验）"
  fi
}

# gen_entry_json writes a canonical NodeEntry object (array-element indentation)
# into $ENTRY using the current NODE_* settings.
gen_entry_json() {
  NAME_ESC="$(json_escape "$NODE_NAME")"
  if [ "$NODE_TYPE" = "vless" ]; then
    ENTRY="{
    \"id\": $NODE_ID,
    \"name\": \"$NAME_ESC\",
    \"type\": \"vless\",
    \"enabled\": true,
    \"server\": {\"listen\": \"0.0.0.0\", \"port\": $NODE_PORT},
    \"protocol\": {\"type\": \"vless\", \"network\": \"tcp\", \"flow\": \"xtls-rprx-vision\", \"decryption\": \"none\"},
    \"tls\": {
      \"enabled\": true,
      \"server_name\": \"$(json_escape "$REALITY_SERVER_NAME")\",
      \"server_port\": 443,
      \"private_key\": \"$(json_escape "$REALITY_PRIVATE_KEY")\",
      \"short_id\": \"$(json_escape "$REALITY_SHORT_ID")\",
      \"allow_insecure\": false
    },
    \"runtime\": {\"sync_interval\": $SYNC_INTERVAL, \"traffic_flush_interval\": $FLUSH_INTERVAL}
  }"
  else
    ENTRY="{
    \"id\": $NODE_ID,
    \"name\": \"$NAME_ESC\",
    \"type\": \"hy2\",
    \"enabled\": true,
    \"server\": {\"listen\": \"0.0.0.0\", \"port\": $NODE_PORT},
    \"tls\": {
      \"enabled\": true,
      \"server_name\": \"bing.com\",
      \"allow_insecure\": true
    },
    \"runtime\": {\"sync_interval\": $SYNC_INTERVAL, \"traffic_flush_interval\": $FLUSH_INTERVAL}
  }"
  fi
}

# add_node_main appends one node to nodes.json, migrating a legacy node.json
# first if necessary. Runs standalone and exits the script.
add_node_main() {
  need_root
  case "$NODE_TYPE" in
    vless|VLESS|reality|Reality) NODE_TYPE="vless" ;;
    hy2|HY2|hysteria|hysteria2) NODE_TYPE="hy2" ;;
    *) err "--node-type 只能是 vless 或 hy2" ;;
  esac
  case "$NODE_ID" in ''|*[!0-9]*) err "--node-id 必须是正整数" ;; esac
  case "$NODE_PORT" in ''|*[!0-9]*) err "--node-port 必须是正整数" ;; esac
  case "$FLUSH_INTERVAL" in ''|*[!0-9]*) err "--flush-interval 必须是整数" ;; esac
  case "$SYNC_INTERVAL" in ''|*[!0-9]*) err "--sync-interval 必须是整数" ;; esac

  mkdir -p "$DATA_DIR"
  chmod 0700 "$DATA_DIR"
  gen_entry_json

  if [ -f "$DATA_DIR/nodes.json" ]; then
    if grep -qE "\"id\"[[:space:]]*:[[:space:]]*$NODE_ID([^0-9]|$)" "$DATA_DIR/nodes.json"; then
      err "nodes.json 中已存在节点 id=$NODE_ID"
    fi
    info "追加节点 id=$NODE_ID（$NODE_TYPE:$NODE_PORT）到 $DATA_DIR/nodes.json"
    tmp="$DATA_DIR/.nodes.json.tmp"
    awk '{a[NR]=$0} END{for(i=1;i<NR-1;i++) print a[i]}' "$DATA_DIR/nodes.json" > "$tmp"
    printf ',\n%s\n  ]\n}\n' "$ENTRY" >> "$tmp"
    mv "$tmp" "$DATA_DIR/nodes.json"
    chmod 0600 "$DATA_DIR/nodes.json"
  elif [ -f "$DATA_DIR/node.json" ]; then
    info "检测到单节点 node.json，迁移为 nodes.json（保留原通讯密钥）"
    token="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$token" ] || token="$(gen_token)"
    OID="$(sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OID" ] || OID=1
    ONAME="$(sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    OTYPE="$(sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OTYPE" ] || OTYPE="vless"
    OPORT="$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OPORT" ] || OPORT=443
    OSERVERNAME="$(sed -n 's/.*"server_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OSERVERNAME" ] || OSERVERNAME="www.microsoft.com"
    OPRIVATEKEY="$(sed -n 's/.*"private_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    OSHORTID="$(sed -n 's/.*"short_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OSHORTID" ] || OSHORTID="abcd1234"
    OSYNC="$(sed -n 's/.*"sync_interval"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OSYNC" ] || OSYNC=60
    OFLUSH="$(sed -n 's/.*"traffic_flush_interval"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1)"
    [ -n "$OFLUSH" ] || OFLUSH=300
    ONAME_ESC="$(json_escape "$ONAME")"
    OSERVERNAME_ESC="$(json_escape "$OSERVERNAME")"
    OPRIVATEKEY_ESC="$(json_escape "$OPRIVATEKEY")"
    OSHORTID_ESC="$(json_escape "$OSHORTID")"
    if [ "$OTYPE" = "vless" ]; then
      OENTRY="{
    \"id\": $OID,
    \"name\": \"$ONAME_ESC\",
    \"type\": \"vless\",
    \"enabled\": true,
    \"server\": {\"listen\": \"0.0.0.0\", \"port\": $OPORT},
    \"protocol\": {\"type\": \"vless\", \"network\": \"tcp\", \"flow\": \"xtls-rprx-vision\", \"decryption\": \"none\"},
    \"tls\": {
      \"enabled\": true,
      \"server_name\": \"$OSERVERNAME_ESC\",
      \"server_port\": 443,
      \"private_key\": \"$OPRIVATEKEY_ESC\",
      \"short_id\": \"$OSHORTID_ESC\",
      \"allow_insecure\": false
    },
    \"runtime\": {\"sync_interval\": $OSYNC, \"traffic_flush_interval\": $OFLUSH}
  }"
    else
      OENTRY="{
    \"id\": $OID,
    \"name\": \"$ONAME_ESC\",
    \"type\": \"hy2\",
    \"enabled\": true,
    \"server\": {\"listen\": \"0.0.0.0\", \"port\": $OPORT},
    \"tls\": {
      \"enabled\": true,
      \"server_name\": \"$OSERVERNAME_ESC\",
      \"allow_insecure\": true
    },
    \"runtime\": {\"sync_interval\": $OSYNC, \"traffic_flush_interval\": $OFLUSH}
  }"
    fi
    {
      printf '{\n  "version": 1,\n  "token": "%s",\n  "nodes": [\n' "$token"
      printf '%s,\n' "$OENTRY"
      printf '%s\n' "$ENTRY"
      printf '  ]\n}\n'
    } > "$DATA_DIR/nodes.json"
    chmod 0600 "$DATA_DIR/nodes.json"
  else
    err "未找到 $DATA_DIR/node.json 或 nodes.json：请先运行主安装脚本"
  fi

  echo
  info "完成：节点 id=$NODE_ID（$NODE_TYPE:$NODE_PORT）已加入 nodes.json。"
  echo "其他节点继续使用同一个通讯密钥接入，仅 node_id 不同。"
  echo "重启 tiny-xboard 使新节点生效："
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    echo "    systemctl restart tiny-xboard"
  elif command -v rc-service >/dev/null 2>&1; then
    echo "    rc-service tiny-xboard restart"
  else
    echo "    重新运行面板进程即可"
  fi
  exit 0
}

# --- service management ---------------------------------------------------
HAVE_SYSTEMD=0
HAVE_OPENRC=0
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then HAVE_SYSTEMD=1; fi
if [ "$HAVE_SYSTEMD" = "0" ] && command -v rc-service >/dev/null 2>&1 && [ -f /sbin/openrc-run ]; then HAVE_OPENRC=1; fi

svc_stop() {
  if [ "$HAVE_SYSTEMD" = "1" ]; then systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  elif [ "$HAVE_OPENRC" = "1" ]; then rc-service "$SERVICE_NAME" stop 2>/dev/null || true
  else pkill -f "$BIN_DIR/current" 2>/dev/null || true; fi
  sleep 0.5
}

svc_start() {
  if [ "$HAVE_SYSTEMD" = "1" ]; then systemctl start "$SERVICE_NAME" 2>/dev/null
  elif [ "$HAVE_OPENRC" = "1" ]; then rc-service "$SERVICE_NAME" start 2>/dev/null
  else
    "$BIN_DIR/current" -dir "$DATA_DIR" -listen "$LISTEN" >>/var/log/tiny-xboard.log 2>&1 &
  fi
}

svc_enabled() {
  if [ "$HAVE_SYSTEMD" = "1" ]; then systemctl is-enabled "$SERVICE_NAME" >/dev/null 2>&1
  elif [ "$HAVE_OPENRC" = "1" ]; then rc-update show default 2>/dev/null | grep -q "^[[:space:]]*$SERVICE_NAME"
  else return 1; fi
}

install_service() {
  if [ "$HAVE_SYSTEMD" = "1" ]; then
    cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=tiny-xboard (Tiny Xboard UniProxy mock API)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=GOMAXPROCS=$GOMAXPROCS
Environment=GOMEMLIMIT=$GOMEMLIMIT
Environment=GOGC=$GOGC
ExecStart=$INSTALL_DIR/run.sh
Restart=always
RestartSec=2
LimitNOFILE=1048576
WorkingDirectory=$INSTALL_DIR

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "/etc/systemd/system/$SERVICE_NAME.service"
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ "$ENABLE" = "1" ]; then
      systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
    fi
  elif [ "$HAVE_OPENRC" = "1" ]; then
    cat > "/etc/conf.d/$SERVICE_NAME" <<EOF
# Runtime options live in $INSTALL_DIR/env and are read by run.sh.
EOF
    chmod 0600 "/etc/conf.d/$SERVICE_NAME"
    cat > "/etc/init.d/$SERVICE_NAME" <<EOF
#!/sbin/openrc-run
name="$SERVICE_NAME"
description="tiny-xboard (Tiny Xboard UniProxy mock API)"
command="$INSTALL_DIR/run.sh"
command_background="yes"
pidfile="/run/\${RC_SVCNAME}.pid"
output_log="/var/log/tiny-xboard.log"
error_log="/var/log/tiny-xboard.err"
start_pre() {
  checkpath -d -m 0700 "$DATA_DIR"
}
EOF
    chmod 0755 "/etc/init.d/$SERVICE_NAME"
    if [ "$ENABLE" = "1" ]; then
      rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 || true
    fi
  fi
}

# --- health check ---------------------------------------------------------
get_token() {
  if [ -f "$DATA_DIR/nodes.json" ]; then
    sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/nodes.json" | head -n1
  elif [ -f "$DATA_DIR/node.json" ]; then
    sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1
  fi
}

get_node_id() {
  if [ -f "$DATA_DIR/nodes.json" ]; then
    sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/nodes.json" | head -n1
  elif [ -f "$DATA_DIR/node.json" ]; then
    sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1
  fi
}

get_node_type() {
  if [ -f "$DATA_DIR/nodes.json" ]; then
    sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/nodes.json" | head -n1
  elif [ -f "$DATA_DIR/node.json" ]; then
    sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1
  fi
}

health_check() {
  port="$(printf '%s' "$LISTEN" | sed 's/.*://')"
  [ -n "$port" ] || port=8080
  token="$(get_token)"
  node_id="$(get_node_id)"
  node_type="$(get_node_type)"
  [ -n "$token" ] || return 1
  [ -n "$node_id" ] || node_id=1
  [ -n "$node_type" ] || node_type=vless
  base="http://127.0.0.1:$port/api/v1/server/UniProxy"
  http_ok "$base/user?token=$token&node_id=$node_id&node_type=$node_type" || return 1
  http_ok "$base/config?token=$token&node_id=$node_id&node_type=$node_type" || return 1
  return 0
}

wait_health() {
  i=0
  while [ "$i" -lt 15 ]; do
    if health_check; then return 0; fi
    i=$((i+1))
    sleep 1
  done
  return 1
}

# --- binary acquisition ---------------------------------------------------
check_binary() {
  f="$1"
  want="$2"
  magic="$(head -c4 "$f" | od -An -tx1 | tr -d ' \n')"
  [ "$magic" = "7f454c46" ] || err "下载的二进制不是 ELF（magic=$magic）"
  if command -v file >/dev/null 2>&1; then
    ftype="$(file -b "$f")"
    case "$ftype" in
      *x86-64*) [ "$want" = "amd64" ] || err "二进制架构不匹配：$ftype（需要 amd64）" ;;
      *AArch64*|*aarch64*|*ARM*aarch64*) [ "$want" = "arm64" ] || err "二进制架构不匹配：$ftype（需要 arm64）" ;;
      *) err "无法识别的二进制类型：$ftype" ;;
    esac
  fi
  e_machine="$(od -An -tu1 -j18 -N2 "$f" 2>/dev/null | awk '{printf "%d\n", $1 + $2*256}')"
  if [ "$want" = "amd64" ]; then
    [ "$e_machine" = "62" ] || err "二进制 e_machine=$e_machine，需要 62（amd64）"
  else
    [ "$e_machine" = "183" ] || err "二进制 e_machine=$e_machine，需要 183（arm64）"
  fi
  "$f" --version >/dev/null 2>&1 || err "二进制无法执行（架构不符或文件损坏）"
}

binary_version_commit() {
  ver_line="$("$1" --version 2>/dev/null || true)"
  c="$(printf '%s' "$ver_line" | sed -n 's/.*commit=\([^ ]*\).*/\1/p')"
  if [ -n "$c" ] && [ "$c" != "unknown" ]; then
    printf '%s' "$c"
  else
    printf '%s' "$(sha256_hex "$1" | cut -c1-8)"
  fi
}

# --- argument parsing -----------------------------------------------------
while [ "$#" -gt 0 ]; do
  case "$1" in
    --token) TOKEN="${2:-}"; shift 2 ;;
    --node-id) NODE_ID="${2:-}"; shift 2 ;;
    --node-name) NODE_NAME="${2:-}"; shift 2 ;;
    --node-type) NODE_TYPE="${2:-}"; shift 2 ;;
    --node-port) NODE_PORT="${2:-}"; shift 2 ;;
    --listen) LISTEN="${2:-}"; shift 2 ;;
    --data-dir) DATA_DIR="${2:-}"; shift 2 ;;
    --reality-private-key) REALITY_PRIVATE_KEY="${2:-}"; shift 2 ;;
    --reality-server-name) REALITY_SERVER_NAME="${2:-}"; shift 2 ;;
    --reality-short-id) REALITY_SHORT_ID="${2:-}"; shift 2 ;;
    --sync-interval) SYNC_INTERVAL="${2:-}"; shift 2 ;;
    --flush-interval) FLUSH_INTERVAL="${2:-}"; shift 2 ;;
    --gomemlimit) GOMEMLIMIT="${2:-}"; shift 2 ;;
    --gogc) GOGC="${2:-}"; shift 2 ;;
    --gomaxprocs) GOMAXPROCS="${2:-}"; shift 2 ;;
    --ref) REF="${2:-}"; shift 2 ;;
    --version) REF="${2:-}"; shift 2 ;;
    --add-node) ADD_NODE="1"; shift ;;
    --force) FORCE="1"; shift ;;
    --enable) ENABLE="1"; shift ;;
    --yes) ASSUME_YES="1"; shift ;;
    --interactive) INTERACTIVE="1"; shift ;;
    --non-interactive) INTERACTIVE="0"; shift ;;
    --no-start) START_SERVICE="0"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) err "未知参数：$1" ;;
  esac
done

need_root

# --- platform detection ---------------------------------------------------
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ASSET_ARCH="amd64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) err "不支持的架构：$ARCH（本安装包提供 amd64 / arm64）" ;;
esac
ASSET="$APP-linux-$ASSET_ARCH"

# --- resolve runtime params from existing env (idempotent upgrade) --------
# Keep user-provided flags; otherwise reuse the values a previous install
# recorded in $INSTALL_DIR/env so an upgrade never changes runtime settings.
USER_DATA_DIR="$DATA_DIR"; USER_LISTEN="$LISTEN"
USER_GOMEMLIMIT="$GOMEMLIMIT"; USER_GOGC="$GOGC"; USER_GOMAXPROCS="$GOMAXPROCS"
if [ -f "$INSTALL_DIR/env" ]; then
  # shellcheck disable=SC1090
  . "$INSTALL_DIR/env"
fi
[ -n "$USER_DATA_DIR" ] && DATA_DIR="$USER_DATA_DIR"
[ -n "$USER_LISTEN" ] && LISTEN="$USER_LISTEN"
[ -n "$USER_GOMEMLIMIT" ] && GOMEMLIMIT="$USER_GOMEMLIMIT"
[ -n "$USER_GOGC" ] && GOGC="$USER_GOGC"
[ -n "$USER_GOMAXPROCS" ] && GOMAXPROCS="$USER_GOMAXPROCS"
DATA_DIR="${DATA_DIR:-/etc/$APP}"
LISTEN="${LISTEN:-127.0.0.1:8080}"
GOMEMLIMIT="${GOMEMLIMIT:-32MiB}"
GOGC="${GOGC:-70}"
GOMAXPROCS="${GOMAXPROCS:-1}"

if [ "$ADD_NODE" = "1" ]; then
  add_node_main
fi

HAVE_NODE_CONF=0
if [ -f "$DATA_DIR/node.json" ] || [ -f "$DATA_DIR/nodes.json" ]; then
  HAVE_NODE_CONF=1
fi

if [ "$INTERACTIVE" = "1" ] || { [ "$INTERACTIVE" = "auto" ] && is_tty && [ "$HAVE_NODE_CONF" = "0" ]; }; then
  info "进入交互式安装（已有节点配置时跳过）。"
  prompt NODE_TYPE "节点类型 (vless / hy2)" "$NODE_TYPE" 0
  prompt NODE_ID "节点 ID" "$NODE_ID" 0
  prompt NODE_PORT "节点代理端口" "$NODE_PORT" 0
  prompt TOKEN "节点 token（留空自动生成）" "$TOKEN" 1
fi

case "$NODE_TYPE" in
  vless|VLESS|reality|Reality) NODE_TYPE="vless" ;;
  hy2|HY2|hysteria|hysteria2) NODE_TYPE="hy2" ;;
  *) err "--node-type 只能是 vless 或 hy2" ;;
esac

case "$NODE_ID" in ''|*[!0-9]*) err "--node-id 必须是正整数" ;; esac
case "$NODE_PORT" in ''|*[!0-9]*) err "--node-port 必须是正整数" ;; esac
case "$FLUSH_INTERVAL" in ''|*[!0-9]*) err "--flush-interval 必须是整数" ;; esac
case "$SYNC_INTERVAL" in ''|*[!0-9]*) err "--sync-interval 必须是整数" ;; esac

if [ "$HAVE_CURL" = "0" ] && [ "$HAVE_WGET" = "0" ] && [ -z "${TINY_XBOARD_BINARY:-}" ]; then
  err "缺少 curl/wget：安装器需要下载二进制（不自动安装依赖）"
fi

# --- download + verify ----------------------------------------------------
TMPDIR="$(mktemp -d /tmp/$APP-install.XXXXXX)"
cleanup() { rm -rf "$TMPDIR"; }
trap cleanup EXIT HUP INT TERM

TMPBIN="$TMPDIR/$ASSET"
BIN_SIZE=""
BIN_SHA=""
if [ -n "${TINY_XBOARD_BINARY:-}" ]; then
  info "使用本地二进制 $TINY_XBOARD_BINARY"
  [ -f "$TINY_XBOARD_BINARY" ] || err "TINY_XBOARD_BINARY 不存在：$TINY_XBOARD_BINARY"
  cp "$TINY_XBOARD_BINARY" "$TMPBIN"
  chmod 0755 "$TMPBIN"
  check_binary "$TMPBIN" "$ASSET_ARCH"
  if [ "$HAVE_SHA256SUM" = "1" ] || [ "$HAVE_OPENSSL" = "1" ]; then
    BIN_SHA="$(sha256_hex "$TMPBIN")"
  else
    BIN_SHA="(local binary, no hash tool)"
  fi
else
  BASE_URL="${TINY_XBOARD_BASE_URL:-https://raw.githubusercontent.com/$REPO/$REF/tiny-xboard}"
  BIN_URL="$BASE_URL/bin/$ASSET"
  SUMS_URL="$BASE_URL/checksums/SHA256SUMS"
  info "下载 $ASSET（ref=$REF）"
  echo "URL: $BIN_URL"
  fetch "$BIN_URL" "$TMPBIN" || { echo ""; err "下载失败：$BIN_URL（网络错误或该 ref 下没有此二进制）"; }
  chmod 0755 "$TMPBIN"
  BIN_SHA="$(sha256_hex "$TMPBIN")"
  info "校验 SHA256（对照 $SUMS_URL）"
  fetch "$SUMS_URL" "$TMPDIR/SHA256SUMS" || err "无法获取 SHA256SUMS，拒绝安装（校验不可跳过）"
  if ! grep -q "^$BIN_SHA  bin/$ASSET\$" "$TMPDIR/SHA256SUMS"; then
    err "SHA256 校验失败：$ASSET 与 SHA256SUMS 不匹配"
  fi
  check_binary "$TMPBIN" "$ASSET_ARCH"
fi
BIN_SIZE="$(wc -c < "$TMPBIN" | awk '{printf "%.1f MB", $1/1048576}')"

# --- install versioned binary ---------------------------------------------
VERSION_COMMIT="$(binary_version_commit "$TMPBIN")"
VERSIONED_NAME="$APP-$VERSION_COMMIT"
VERSIONED_BIN="$BIN_DIR/$VERSIONED_NAME"

mkdir -p "$BIN_DIR"
chmod 0755 "$BIN_DIR"
install -m 0755 "$TMPBIN" "$VERSIONED_BIN"

# preserve existing runtime params that a previous install recorded
{
  echo "LISTEN=$(shell_quote "$LISTEN")"
  echo "DATA_DIR=$(shell_quote "$DATA_DIR")"
  echo "GOMAXPROCS=$(shell_quote "$GOMAXPROCS")"
  echo "GOMEMLIMIT=$(shell_quote "$GOMEMLIMIT")"
  echo "GOGC=$(shell_quote "$GOGC")"
} > "$INSTALL_DIR/env"
chmod 0600 "$INSTALL_DIR/env"

cat > "$INSTALL_DIR/run.sh" <<EOF
#!/bin/sh
set -eu
INSTALL_DIR="$INSTALL_DIR"
. "\$INSTALL_DIR/env"
export GOMAXPROCS GOMEMLIMIT GOGC
exec "\$INSTALL_DIR/bin/current" -dir "\$DATA_DIR" -listen "\$LISTEN"
EOF
chmod 0755 "$INSTALL_DIR/run.sh"

# --- first-install state init (NEVER touches existing files) --------------
mkdir -p "$DATA_DIR"
chmod 0700 "$DATA_DIR"
if [ ! -f "$DATA_DIR/node.json" ] && [ ! -f "$DATA_DIR/nodes.json" ]; then
  info "初始化最小 node.json（首次安装）"
  [ -n "$TOKEN" ] || TOKEN="$(gen_token)"
  NODE_NAME_ESC="$(json_escape "$NODE_NAME")"
  TLS_JSON=""
  if [ "$NODE_TYPE" = "vless" ]; then
    TLS_JSON="$(cat <<EOF
  "tls": {
    "enabled": true,
    "server_name": "$(json_escape "$REALITY_SERVER_NAME")",
    "server_port": 443,
    "private_key": "$(json_escape "$REALITY_PRIVATE_KEY")",
    "short_id": "$(json_escape "$REALITY_SHORT_ID")",
    "allow_insecure": false
  },
EOF
)"
  else
    TLS_JSON="$(cat <<EOF
  "tls": {
    "enabled": true,
    "server_name": "bing.com",
    "allow_insecure": true
  },
EOF
)"
  fi
  cat > "$DATA_DIR/node.json" <<EOF
{
  "version": 1,
  "node": {"id": $NODE_ID, "name": "$NODE_NAME_ESC", "type": "$NODE_TYPE", "enabled": true},
  "auth": {"token": "$TOKEN"},
  "server": {"listen": "0.0.0.0", "port": $NODE_PORT},
  "protocol": {"type": "$NODE_TYPE", "network": "tcp", "flow": "xtls-rprx-vision", "decryption": "none"},
$TLS_JSON
  "runtime": {"sync_interval": $SYNC_INTERVAL, "traffic_flush_interval": $FLUSH_INTERVAL}
}
EOF
  chmod 0600 "$DATA_DIR/node.json"
else
  info "保留现有节点配置（node.json/nodes.json 不覆盖）"
fi

if [ ! -f "$DATA_DIR/users.json" ]; then
  info "初始化 users.json（空用户列表）"
  printf '{\n  "version": 1,\n  "users": []\n}\n' > "$DATA_DIR/users.json"
  chmod 0600 "$DATA_DIR/users.json"
fi

if [ ! -f "$DATA_DIR/traffic.json" ]; then
  info "初始化 traffic.json"
  printf '{\n  "version": 1,\n  "users": {}\n}\n' > "$DATA_DIR/traffic.json"
  chmod 0600 "$DATA_DIR/traffic.json"
fi

# --- switch + rollback ----------------------------------------------------
OLD_CUR=""
if [ -L "$BIN_DIR/current" ]; then
  OLD_CUR="$(readlink "$BIN_DIR/current" 2>/dev/null || true)"
fi

info "停止旧服务（若有）"
svc_stop

fail_rollback() {
  msg="$1"
  echo "ERROR: $msg" >&2
  if [ -n "$OLD_CUR" ] && [ -e "$BIN_DIR/$OLD_CUR" ]; then
    info "回滚到 $OLD_CUR"
    ln -sfn "$OLD_CUR" "$BIN_DIR/current"
    svc_start || true
    if wait_health; then
      echo "回滚成功：服务已恢复到 $OLD_CUR"
    else
      echo "回滚后服务仍异常，请手动检查；旧版本文件保留在 $BIN_DIR/$OLD_CUR" >&2
    fi
  else
    info "首次安装失败：清理 $VERSIONED_BIN"
    rm -f "$VERSIONED_BIN"
    rm -f "$BIN_DIR/current"
  fi
  exit 1
}

info "切换 current -> $VERSIONED_NAME"
ln -sfn "$VERSIONED_NAME" "$BIN_DIR/current"

install_service

if [ "$START_SERVICE" = "1" ]; then
  info "启动服务"
  svc_start || fail_rollback "服务启动失败"
  sleep 1
  if ! wait_health; then
    fail_rollback "健康检查失败（/user 或 /config 未返回 200）"
  fi
  info "健康检查通过（/user + /config = 200）"
else
  info "--no-start：已安装未启动"
fi

# prune old versioned binaries, keep previous (for manual rollback) + current
if [ -n "$OLD_CUR" ]; then
  for f in "$BIN_DIR"/$APP-*; do
    [ -f "$f" ] || continue
    b="$(basename "$f")"
    [ "$b" = "$VERSIONED_NAME" ] && continue
    [ "$b" = "$OLD_CUR" ] && continue
    rm -f "$f"
  done
fi

# also ship a convenience uninstaller at the install dir
cat > "$INSTALL_DIR/uninstall.sh" <<EOF
#!/bin/sh
set -eu
APP="$APP"
INSTALL_DIR="$INSTALL_DIR"
DATA_DIR="$DATA_DIR"
SERVICE_NAME="$SERVICE_NAME"
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl stop "\$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "\$SERVICE_NAME" 2>/dev/null || true
  rm -f "/etc/systemd/system/\$SERVICE_NAME.service"
  systemctl daemon-reload 2>/dev/null || true
fi
if command -v rc-service >/dev/null 2>&1 && [ -f /sbin/openrc-run ]; then
  rc-service "\$SERVICE_NAME" stop 2>/dev/null || true
  rc-update del "\$SERVICE_NAME" default 2>/dev/null || true
  rm -f "/etc/init.d/\$SERVICE_NAME" "/etc/conf.d/\$SERVICE_NAME"
fi
pkill -f "\$INSTALL_DIR/bin/current" 2>/dev/null || true
rm -rf "\$INSTALL_DIR"
printf '%s\n' "tiny-xboard 已卸载；状态数据保留在 \$DATA_DIR（node.json/users.json/traffic.json 及 Reality key 未删除）。"
EOF
chmod 0755 "$INSTALL_DIR/uninstall.sh"

echo ""
echo "tiny-xboard installer"
echo "--------------------------------------------------"
echo "Architecture: $ARCH -> $ASSET_ARCH"
echo "Source:       GitHub repository (raw file, no Release)"
echo "Ref:          $REF"
echo "Commit:       $VERSION_COMMIT"
echo "Binary:       $BIN_SIZE"
echo "SHA256:       $BIN_SHA"
echo "Installing:   $VERSIONED_BIN"
if [ "$HAVE_SYSTEMD" = "1" ]; then
  echo "Service:      systemd ($SERVICE_NAME)"
elif [ "$HAVE_OPENRC" = "1" ]; then
  echo "Service:      OpenRC ($SERVICE_NAME)"
else
  echo "Service:      none (container/无 init，前台运行)"
fi
if svc_enabled 2>/dev/null; then
  echo "Boot:         enabled"
else
  echo "Boot:         disabled (用 --enable 开启开机自启)"
fi
echo "State:        $DATA_DIR"
if [ "$START_SERVICE" = "1" ]; then
  echo "Result:       SUCCESS"
else
  echo "Result:       SUCCESS (--no-start, 服务未启动)"
fi
echo "--------------------------------------------------"
echo "程序目录：$INSTALL_DIR（版本化二进制 + current 软链）"
echo "状态目录：$DATA_DIR（node.json / users.json / traffic.json）"
echo "卸载命令：$INSTALL_DIR/uninstall.sh（或仓库 uninstall.sh；--purge 才删除状态数据）"