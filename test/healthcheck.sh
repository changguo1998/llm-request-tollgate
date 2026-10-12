#!/usr/bin/env bash
# tollgate 自检 (O1): 二进制存在 -> 配置可解析 (-check) -> 监听端口可服务。
# 用法: test/healthcheck.sh [config.json]    默认 config.local.json。
# 退出码 0 = 全部通过。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CFG="${1:-$ROOT/config.local.json}"
BIN="$ROOT/llm-request-tollgate"

# key_env 密钥只存在环境变量(内存), 从 systemd 用户管理器同步 (与服务同源, 不落盘)。
while IFS= read -r line; do
  case "$line" in
    TOLLGATE_*=*) export "$line" ;;
  esac
done < <(systemctl --user show-environment 2>/dev/null)

PASS=0
FAIL=0

ck() { # <desc> <0|1>
  if [ "$2" = 0 ]; then PASS=$((PASS + 1)); echo "  PASS  $1";
  else FAIL=$((FAIL + 1)); echo "  FAIL  $1"; fi
}

if [ -x "$BIN" ]; then
  ck "二进制存在 ($BIN)" 0
else
  ck "二进制存在 ($BIN)" 1
  echo "  提示: cd $ROOT && go build -o llm-request-tollgate ."
fi

if [ -x "$BIN" ] && "$BIN" -config "$CFG" -check >/dev/null 2>&1; then
  ck "配置可解析 (-check $CFG)" 0
else
  ck "配置可解析 (-check $CFG)" 1
fi

listen=$(jq -r '.listen // empty' "$CFG" 2>/dev/null)
[ -z "$listen" ] && listen="127.0.0.1:8787"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://$listen/v1/models" 2>/dev/null)
if [ "$code" = "200" ]; then
  ck "端口可服务 ($listen /v1/models=200)" 0
else
  ck "端口可服务 ($listen /v1/models=200, got $code)" 1
fi

echo "结果: $PASS 通过, $FAIL 失败"
[ "$FAIL" = 0 ]
