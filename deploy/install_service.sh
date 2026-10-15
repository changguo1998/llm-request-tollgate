#!/usr/bin/env bash
# 把 tollgate 安装为 systemd 用户服务 (开机自启, 崩了自动拉起)。
# 前置: 仓库根目录已有 llm-request-tollgate 二进制与填好的 config.local.json。
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$HOME/.config/systemd/user/tollgate.service"

if [ ! -x "$ROOT/llm-request-tollgate" ]; then
  echo "缺少二进制: 先在仓库根目录 go build -o llm-request-tollgate ."; exit 1
fi
if [ ! -f "$ROOT/config.local.json" ]; then
  echo "缺少 config.local.json: 从 config.example.json 复制并填写后再装"; exit 1
fi
if [ ! -f "$ROOT/deploy/tollgate.service" ]; then
  echo "缺少 deploy/tollgate.service: 公开仓库只跟踪示例版, 先复制:"
  echo "  cp deploy/tollgate.service.example deploy/tollgate.service"; exit 1
fi
[ "$ROOT" = "/" ] && { echo "仓库根目录不应为 /"; exit 1; }

mkdir -p "$HOME/.config/systemd/user"
sed -e "s|__ROOT__|$ROOT|g" "$ROOT/deploy/tollgate.service" > "$DEST"
systemctl --user daemon-reload
systemctl --user enable --now tollgate.service
sleep 1
systemctl --user --no-pager status tollgate.service | head -10
echo "已安装: $DEST"
