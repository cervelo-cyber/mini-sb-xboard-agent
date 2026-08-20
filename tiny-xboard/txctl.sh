#!/bin/sh
# txctl.sh - tiny-xboard 交互式控制台（聚合全部 API）
#
# 用法:
#   ./txctl.sh                 # 交互式菜单
#   ./txctl.sh user            # 只看用户列表
#   ./txctl.sh config          # 只看节点配置
#   ./txctl.sh health          # 只看健康检查
#   ./txctl.sh vless           # 生成 vless:// Reality 链接
#   ./txctl.sh push UID UP DOWN # 上报流量 (可多次: ./txctl.sh push 1 100 200)
#
# 连接参数（优先级：参数 > 环境变量 > 本机 /etc/tiny-xboard 配置文件）:
#   TX_URL        API 地址, 默认 http://127.0.0.1:8080
#   TX_TOKEN      节点 token
#   TX_NODE_ID    节点 id, 默认 1
#   TX_NODE_TYPE  节点类型, 默认 vless
set -u

# ---------- 参数解析 ----------
CMD="${1:-}"
if [ "$CMD" = "user" ] || [ "$CMD" = "config" ] || [ "$CMD" = "health" ] || [ "$CMD" = "vless" ] || [ "$CMD" = "push" ] || [ "$CMD" = "traffic" ] || [ "$CMD" = "info" ]; then
  shift
  if [ "$CMD" = "push" ] && [ $# -ge 3 ]; then
    PUSH_UID="$1"; PUSH_UP="$2"; PUSH_DOWN="$3"
  fi
else
  [ -z "$CMD" ] || CMD=""
  set --
fi

# ---------- 连接参数：本机配置自动读取 ----------
DATA_DIR="${DATA_DIR:-/etc/tiny-xboard}"
if [ -f "$DATA_DIR/nodes.json" ]; then
  CFG_TOKEN="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/nodes.json" | head -n1)"
  CFG_NODE_ID="$(sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/nodes.json" | head -n1)"
  CFG_NODE_TYPE="$(sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/nodes.json" | head -n1)"
elif [ -f "$DATA_DIR/node.json" ]; then
  CFG_TOKEN="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
  CFG_NODE_ID="$(sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$DATA_DIR/node.json" | head -n1)"
  CFG_NODE_TYPE="$(sed -n 's/.*"type"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA_DIR/node.json" | head -n1)"
fi
URL="${TX_URL:-http://127.0.0.1:8080}"
TOKEN="${TX_TOKEN:-${CFG_TOKEN:-}}"
NODE_ID="${TX_NODE_ID:-${CFG_NODE_ID:-1}}"
NODE_TYPE="${TX_NODE_TYPE:-${CFG_NODE_TYPE:-vless}}"

# ---------- 工具函数 ----------
have_curl() { command -v curl >/dev/null 2>&1; }
have_wget() { command -v wget >/dev/null 2>&1; }

http_get() { # url -> body
  if have_curl; then curl -fsS --connect-timeout 5 "$1" 2>/dev/null
  elif have_wget; then wget -qO- "$1" 2>/dev/null
  fi
}

http_code() { # url -> http code
  if have_curl; then curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 "$1"
  elif have_wget; then wget -qO /dev/null "$1" && echo 200 || echo 000
  fi
}

http_post() { # url body -> body
  if have_curl; then curl -fsS --connect-timeout 5 -X POST -H 'Content-Type: application/json' -d "$2" "$1" 2>/dev/null
  elif have_wget; then wget -qO- --post-data="$2" --header='Content-Type: application/json' "$1" 2>/dev/null
  fi
}

pretty() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -m json.tool 2>/dev/null || cat
  else
    cat
  fi
}

qs() { printf '%s?token=%s&node_id=%s&node_type=%s' "$URL/api/v1/server/UniProxy/$1" "$TOKEN" "$NODE_ID" "$NODE_TYPE"; }

is_num() { case "$1" in ''|*[!0-9]*) return 1;; esac; }

req_token() {
  while [ -z "$TOKEN" ]; do
    printf '节点 token: '; IFS= read -r TOKEN || exit 1
  done
}

# ---------- 各功能 ----------
cmd_users() {
  req_token
  echo "==> GET /user"
  body="$(http_get "$(qs user)")" || { echo "ERROR: 请求失败 (HTTP $(http_code "$(qs user)"))" >&2; return 1; }
  if [ -z "$body" ]; then echo "ERROR: 空响应 (HTTP $(http_code "$(qs user)"))" >&2; return 1; fi
  echo "$body" | pretty
  echo
  if echo "$body" | grep -q '"users"'; then
    n="$(echo "$body" | grep -o '"id"' | wc -l)"
    echo "用户数: $n"
  fi
}

cmd_config() {
  req_token
  echo "==> GET /config"
  body="$(http_get "$(qs config)")" || { echo "ERROR: 请求失败 (HTTP $(http_code "$(qs config)"))" >&2; return 1; }
  [ -n "$body" ] || { echo "ERROR: 空响应 (HTTP $(http_code "$(qs config)"))" >&2; return 1; }
  echo "$body" | pretty
}

cmd_health() {
  req_token
  u="$(http_code "$(qs user)")"
  c="$(http_code "$(qs config)")"
  echo "URL:        $URL"
  echo "token:      $TOKEN"
  echo "node_id:    $NODE_ID   node_type: $NODE_TYPE"
  echo "/user  ->   $u"
  echo "/config ->  $c"
  [ "$u" = "200" ] && [ "$c" = "200" ] && echo "健康检查: PASS" || echo "健康检查: FAIL"
}

cmd_push() {
  req_token
  if [ -z "${PUSH_UID:-}" ]; then
    echo "==> POST /push（每行输入: uid 上传 下载，空行结束）"
    while :; do
      printf 'uid up down: '; IFS= read -r line || break
      [ -z "$line" ] && break
      set -- $line
      [ $# -ge 3 ] || { echo "  需要 3 个数字: uid up down" >&2; continue; }
      is_num "$1" && is_num "$2" && is_num "$3" || { echo "  必须是整数" >&2; continue; }
      body="$(http_post "$(qs push)" "{\"$1\":[$2,$3]}")"
      echo "  -> $body"
    done
  else
    echo "==> POST /push  uid=$PUSH_UID up=$PUSH_UP down=$PUSH_DOWN"
    body="$(http_post "$(qs push)" "{\"$PUSH_UID\":[$PUSH_UP,$PUSH_DOWN]}")"
    echo "$body" | pretty
  fi
}

cmd_traffic() {
  if [ -r "$DATA_DIR/traffic.json" ]; then
    echo "==> $DATA_DIR/traffic.json"
    cat "$DATA_DIR/traffic.json" | pretty
  else
    echo "本机无 $DATA_DIR/traffic.json（API 未提供流量查询接口，只能看服务端文件）" >&2
  fi
}

cmd_vless() {
  req_token
  echo "==> 生成 vless:// Reality 链接"
  cfg="$(http_get "$(qs config)")"
  [ -n "$cfg" ] || { echo "ERROR: 拿不到 /config (HTTP $(http_code "$(qs config)"))" >&2; return 1; }
  port="$(printf '%s' "$cfg" | sed -n 's/.*"server_port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' | head -n1)"
  [ -n "$port" ] || port=443
  sni="$(printf '%s' "$cfg" | sed -n 's/.*"server_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  pbk="$(printf '%s' "$cfg" | sed -n 's/.*"public_key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  sid="$(printf '%s' "$cfg" | sed -n 's/.*"short_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  flow="$(printf '%s' "$cfg" | sed -n 's/.*"flow"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$flow" ] || flow="xtls-rprx-vision"
  net="$(printf '%s' "$cfg" | sed -n 's/.*"network"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [ -n "$net" ] || net="tcp"

  uuid="$(http_get "$(qs user)" | sed -n 's/.*"uuid"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  if [ -z "$uuid" ]; then
    echo "  未在 /user 找到用户 UUID（users.json 可能为空）。" >&2
    printf '  请输入用户 UUID（留空退出）: '; IFS= read -r uuid || return 1
    [ -n "$uuid" ] || return 1
  fi

  printf '节点公网地址 [默认 127.0.0.1]: '; IFS= read -r host || return 1
  [ -n "$host" ] || host="127.0.0.1"

  echo
  echo "vless://$uuid@$host:$port?encryption=none&security=reality&sni=$sni&fp=chrome&type=$net&flow=$flow&pbk=$pbk&sid=$sid#$host-$port"
  echo
  echo "  host = $host:$port   sni = $sni   flow = $flow   pbk = $pbk   sid = $sid"
  if [ -n "${pbk}" ] && [ -n "${sni}" ] && [ -n "${sid}" ]; then
    echo "  提示: 此链接仅供 sing-box 真实节点使用；tiny-xboard 本身不提供代理流量。"
  fi
}

show_info() {
  echo "==> 当前连接配置"
  echo "URL:        $URL"
  echo "token:      ${TOKEN:-（未设置，执行时提示）}"
  echo "node_id:    $NODE_ID   node_type: $NODE_TYPE"
  echo "数据目录:   $DATA_DIR"
  echo "配置文件:   $(ls "$DATA_DIR"/node.json "$DATA_DIR"/nodes.json 2>/dev/null | tr '\n' ' ')"
}

menu() {
  while :; do
    echo
    echo "================ tiny-xboard 控制台 ================"
    echo "  URL=$URL   node=$NODE_ID/$NODE_TYPE"
    echo "  1) 用户列表 (/user)"
    echo "  2) 节点配置 (/config)"
    echo "  3) 健康检查 (/user + /config)"
    echo "  4) 上报流量 (/push)"
    echo "  5) 流量统计 (本地 traffic.json)"
    echo "  6) 生成 vless:// Reality 链接"
    echo "  7) 当前连接配置"
    echo "  q) 退出"
    printf '选择: '
    IFS= read -r ans || exit 0
    case "$ans" in
      1) cmd_users ;;
      2) cmd_config ;;
      3) cmd_health ;;
      4) cmd_push ;;
      5) cmd_traffic ;;
      6) cmd_vless ;;
      7) show_info ;;
      q|Q) exit 0 ;;
      *) echo "无效选择: $ans" >&2 ;;
    esac
  done
}

# ---------- 入口 ----------
if ! have_curl && ! have_wget; then
  echo "ERROR: 需要 curl 或 wget" >&2; exit 1
fi

case "$CMD" in
  user)   cmd_users ;;
  config) cmd_config ;;
  health) cmd_health ;;
  vless)  cmd_vless ;;
  push)   cmd_push ;;
  traffic) cmd_traffic ;;
  info)   show_info ;;
  *)      menu ;;
esac