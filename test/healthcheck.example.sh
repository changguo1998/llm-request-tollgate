#!/usr/bin/env bash
# tollgate 自检 (公开示例版, 不依赖 fff dotfiles 生态): 二进制存在 -> 配置可解析
# (-check) -> 监听端口可服务。
#
# 本机实际用的 test/healthcheck.sh 比这多一项: 缺密钥时会按服务那套 source ~/fff
# 并跑 `aikey all` 自动加载密钥再重试 -check (见 docs/operations.md §3)。那份脚本
# 在本机 dotfiles 里维护, 不进公开仓库; 这里要求先自行 export key_env 对应的
# 环境变量。
#
# 用法: test/healthcheck.example.sh [config.json]   默认 config.local.json。
# 退出码 0 = 全部通过。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CFG="${1:-$ROOT/config.local.json}"
BIN="$ROOT/llm-request-tollgate"

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

# key_env 对应的环境变量要自己准备:
#   export USTC_API_KEY=... GROUP_API_KEY=...
if [ -x "$BIN" ] && "$BIN" -config "$CFG" -check >/dev/null 2>&1; then
  ck "配置可解析 (-check $CFG)" 0
else
  ck "配置可解析 (-check $CFG)" 1
  echo "  提示: key_env 的环境变量没就绪? 跑 $BIN -config $CFG -check 看具体报错"
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
