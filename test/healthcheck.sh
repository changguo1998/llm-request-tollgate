#!/usr/bin/env bash
# tollgate 自检 (O1): 二进制存在 -> 配置可解析 (-check) -> 监听端口可服务。
# 用法: test/healthcheck.sh [config.json]    默认 config.local.json。
# 退出码 0 = 全部通过。
set -u
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CFG="${1:-$ROOT/config.local.json}"
BIN="$ROOT/llm-request-tollgate"

# key_env 密钥只存在环境变量(内存)。两条来源: systemd 用户管理器里显式导入过的
# (import-environment 的情况), 以及服务自己用的那套 fff 加载方式 —— unit 里
# ExecStart 包了一层 shell 跑 `aikey all` (遍历 ~/.fff/keys/*.ai), 密钥从不进
# systemd 的环境, 所以只查 show-environment 会让这一项永远失败。
while IFS= read -r line; do
  case "$line" in
    TOLLGATE_*=*|USTC_API_KEY=*|GROUP_API_KEY=*|ARKCP_API_KEY=*|ARKAP_API_KEY=*) export "$line" ;;
  esac
done < <(systemctl --user show-environment 2>/dev/null)

load_fff_keys() { # 与 service 的 ExecStart 同源: 拿 aikey 再 aikey all
  [ -r "$HOME/fff/config/shellrc.sh" ] || return 1
  # shellcheck disable=SC1091
  . "$HOME/fff/config/shellrc.sh"
  for f in "$HOME/fff/config/aliases.d/25-env.sh" "$HOME/fff/config/aliases.d/10-ai.sh"; do
    [ -r "$f" ] && . "$f"
  done
  command -v aikey >/dev/null 2>&1 || return 1
  aikey all >/dev/null 2>&1
}

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

if [ -x "$BIN" ] && ! "$BIN" -config "$CFG" -check >/dev/null 2>&1; then
  load_fff_keys && : # 缺密钥时按服务那套加载一次再试
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
