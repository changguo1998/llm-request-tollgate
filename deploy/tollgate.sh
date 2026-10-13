#!/usr/bin/env bash
# tollgate 常驻服务的快捷命令 (包装 systemctl 用户服务)。
# 用法: ./deploy/tollgate.sh <cmd>
#   install    安装并启动 (首次, 同 install_service.sh)
#   status     查看运行状态
#   logs       跟看服务日志 (journalctl -f, Ctrl-C 退出)
#   restart    重启 (改代码重建后用它)
#   stop       停止
#   start      启动
#   hup        热加载配置, 不用重启 (改 config.local.json 别名/上游后)
#   uninstall  停止并删除服务
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SVC=tollgate.service
U="$HOME/.config/systemd/user/$SVC"

# 无登录 shell / 定时任务里常缺 DBUS 环境; 有 user bus 就自动补上。
if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ] && [ -S "/run/user/$(id -u)/bus" ]; then
  export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
  export DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$(id -u)/bus"
fi

case "${1:-}" in
  install)
    bash "$ROOT/deploy/install_service.sh"
    ;;
  status)
    # 有 user bus 就用 systemctl; 拿不到 bus (定时任务 / 受限沙箱 / 无会话) 时退回进程视角,
    # 否则 status 会被误当成服务故障。
    if systemctl --user status "$SVC" --no-pager 2>/dev/null; then
      exit 0
    fi
    echo "systemctl 不可用 (连不上 user bus), 退回进程检查:"
    pid="$(pgrep -f "$ROOT/llm-request-tollgate -config" | head -1 || true)"
    if [ -n "$pid" ]; then
      echo "  running  pid=$pid  启动于 $(stat -c %y "/proc/$pid" 2>/dev/null | cut -d. -f1)"
      echo "  二进制   $ROOT/llm-request-tollgate 建于 $(stat -c %y "$ROOT/llm-request-tollgate" | cut -d. -f1)"
      exit 0
    fi
    echo "  not running (没找到 llm-request-tollgate 进程)"
    exit 1
    ;;
  logs)
    journalctl --user -u "$SVC" -f
    ;;
  restart)
    systemctl --user restart "$SVC"
    systemctl --user --no-pager status "$SVC" | head -6
    ;;
  stop)
    systemctl --user stop "$SVC"
    ;;
  start)
    systemctl --user start "$SVC"
    ;;
  hup)
    systemctl --user kill -s HUP "$SVC"
    echo "SIGHUP sent: config hot-reload (别名/上游)"
    ;;
  uninstall)
    systemctl --user disable --now "$SVC"
    rm -f "$U"
    systemctl --user daemon-reload
    echo "uninstalled: $SVC"
    ;;
  *)
    echo "用法: $0 <install|status|logs|restart|stop|start|hup|uninstall>"
    exit 2
    ;;
esac
