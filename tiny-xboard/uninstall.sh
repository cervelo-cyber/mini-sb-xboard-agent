#!/bin/sh
# tiny-xboard uninstaller
#
# Default: removes binary + service files, KEEPS state:
#   /etc/tiny-xboard/ (node.json / users.json / traffic.json, Reality key)
#
# --purge: additionally deletes state data, with warning + confirmation.
set -eu

APP="tiny-xboard"
INSTALL_DIR="/opt/$APP"
DATA_DIR="/etc/$APP"
SERVICE_NAME="$APP"
PURGE=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --purge) PURGE=1; shift ;;
    -y|--yes) PURGE_YES=1; shift ;;
    -h|--help)
      cat <<'EOF'
tiny-xboard uninstaller

  ./uninstall.sh         卸载 binary + 服务，保留 /etc/tiny-xboard 状态数据
  ./uninstall.sh --purge 同时删除 /etc/tiny-xboard 状态数据（需二次确认）
EOF
      exit 0 ;;
    *) echo "ERROR: 未知参数：$1" >&2; exit 1 ;;
  esac
done

if [ "$PURGE" = "1" ]; then
  echo "WARNING: --purge 将永久删除 $DATA_DIR（node.json / users.json / traffic.json 及 Reality key）。" >&2
  if [ "${PURGE_YES:-0}" != "1" ]; then
    printf '确认删除？输入 yes 继续: '
    IFS= read -r ans || ans=""
    [ "$ans" = "yes" ] || { echo "已取消。" >&2; exit 1; }
  fi
fi

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  rm -f "/etc/systemd/system/$SERVICE_NAME.service"
  systemctl daemon-reload 2>/dev/null || true
fi
if command -v rc-service >/dev/null 2>&1 && [ -f /sbin/openrc-run ]; then
  rc-service "$SERVICE_NAME" stop 2>/dev/null || true
  rc-update del "$SERVICE_NAME" default 2>/dev/null || true
  rm -f "/etc/init.d/$SERVICE_NAME" "/etc/conf.d/$SERVICE_NAME"
fi
pkill -f "$INSTALL_DIR/bin/current" 2>/dev/null || true

rm -rf "$INSTALL_DIR"

if [ "$PURGE" = "1" ]; then
  rm -rf "$DATA_DIR"
  echo "tiny-xboard 已彻底卸载（含状态数据 $DATA_DIR）。"
else
  echo "tiny-xboard 已卸载。"
  echo "状态数据保留在 $DATA_DIR（node.json / users.json / traffic.json 及 Reality key 未删除）。"
  echo "如需彻底删除：$0 --purge"
fi