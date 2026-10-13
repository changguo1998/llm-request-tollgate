#!/usr/bin/env bash
# llm-request-tollgate 自测: 对本地假上游跑代理, 按用例断言 (设计 §4.7)。
#
# 用法: test/run_selftest.sh <case|all>
#   case: alias | keyenv | fallback | fallback-gate | breaker | budget | rotate | ratelimit | usage | usage-report | stream | logging | overhead | responses | hup | probe
# 每个用例独立起一个假上游 + 一个代理 (各自端口, 互不干扰)。
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$ROOT/test/tmp"
BIN="$TMP/bin"
mkdir -p "$TMP" "$BIN"
export GOCACHE="$ROOT/.cache/gocache"

PASS=0
FAIL=0
PROCS=()

say()  { printf '%s\n' "$*"; }
pass() { PASS=$((PASS + 1)); printf '  PASS  %s\n' "$*"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$*"; }

cleanup() {
  for p in "${PROCS[@]:-}"; do kill "$p" 2>/dev/null; done
}
trap cleanup EXIT

build() {
  say "[build] tollgate + fakeupstream"
  go build -o "$BIN/tollgate" . || { say "build tollgate failed"; exit 1; }
  go build -o "$BIN/fakeupstream" ./test/fakeupstream || { say "build fakeupstream failed"; exit 1; }
}

wait_code() { # <port> <expected http code for /v1/models>
  local port=$1 want=$2 got=""
  for _ in $(seq 1 100); do
    got=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/v1/models" 2>/dev/null)
    [ "$got" = "$want" ] && return 0
    sleep 0.05
  done
  return 1
}

start_fake() { # <port> <delay ms>
  "$BIN/fakeupstream" -addr "127.0.0.1:$1" -delay "$2" >"$TMP/fake-$1.log" 2>&1 &
  PROCS+=("$!")
  if wait_code "$1" 200; then pass "fake upstream up on $1"; else fail "fake upstream not ready on $1"; fi
}

start_proxy() { # <port> <config>
  "$BIN/tollgate" -config "$2" >"$TMP/proxy-$1.log" 2>&1 &
  PROCS+=("$!")
  if wait_code "$1" 200; then pass "proxy up on $1"; else fail "proxy not ready on $1"; fi
}

# gen_config1: 单上游配置。参数: 输出文件 监听端口 日志 假上游端口 上游id
#               include_usage 速率 突发 并发 排队上限 逻辑模型名
gen_config1() {
  cat >"$1" <<EOF
{
  "listen": "127.0.0.1:$2",
  "log_path": "$3",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": ${10} },
  "upstreams": [
    { "id": "$5", "base_url": "http://127.0.0.1:$4/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": $6, "limits": { "rate_per_sec": $7, "burst": $8, "concurrency": $9 } }
  ],
  "aliases": { "${11}": [["$5", "deepseek-v4.1-flash"]] }
}
EOF
}

post() { # <proxy_port> <body> -> 打印状态码, 响应体到 $TMP/resp.txt
  curl -s -o "$TMP/resp.txt" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$1/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$2"
}

post_stream() { # <proxy_port> <body> -> 打印 "starttransfer total code", 响应体到 $TMP/resp.txt
  curl -s -N -o "$TMP/resp.txt" -w '%{time_starttransfer} %{time_total} %{http_code}' \
    -X POST "http://127.0.0.1:$1/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$2"
}

post_resp() { # <proxy_port> <body> -> 打印状态码, 体到 $TMP/resp.txt, 头到 $TMP/hdr-resp.txt
  curl -s -D "$TMP/hdr-resp.txt" -o "$TMP/resp.txt" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$1/v1/responses" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$2"
}

# ---------- 用例 ----------

case_alias() {
  say "=== alias (白名单 404) ==="
  local LOG="$TMP/log-alias.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-alias.json" 18800 "$LOG" 19001 fu true 100 10 10 10000 test-model
  start_fake 19001 20
  start_proxy 18800 "$TMP/cfg-alias.json"

  local code
  code=$(post 18800 '{"model":"nope-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "404" ] && pass "未注册模型 -> 404" || fail "未注册模型 -> $code (want 404)"
  local ecode
  ecode=$(jq -r '.error.code' "$TMP/resp.txt")
  [ "$ecode" = "model_not_found" ] && pass "error.code=model_not_found" || fail "error.code=$ecode"
  code=$(curl -s -o "$TMP/resp.txt" -w '%{http_code}' -X GET "http://127.0.0.1:18800/v1/models")
  [ "$code" = "200" ] && pass "/v1/models -> 200 (O5)" || fail "/v1/models -> $code"
  grep -q '"test-model"' "$TMP/resp.txt" && pass "/v1/models 含逻辑名 test-model" || fail "/v1/models 缺 test-model"

  # 认证头 scheme: Authorization 缺省带 "Bearer " 前缀 (key_prefix 可覆盖)。
  curl -s -D "$TMP/hdr-auth.txt" -o /dev/null -X POST "http://127.0.0.1:18800/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer client-key' \
    -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
  auth=$(grep -i '^x-fake-auth:' "$TMP/hdr-auth.txt" | tr -d '\r' | sed 's/^[^:]*: *//')
  [ "$auth" = "Bearer fake-key" ] && pass "认证头替换为 Bearer <上游 key>" || fail "认证头=$auth (want Bearer fake-key)"
}

case_keyenv() {
  say "=== key_env (key 从环境变量取, 不落盘) ==="
  local CFG="$TMP/cfg-keyenv.json"
  cat >"$CFG" <<EOF
{
  "listen": "127.0.0.1:18812",
  "log_path": "$TMP/log-keyenv.jsonl",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "fu", "base_url": "http://127.0.0.1:19012/v1", "auth_header": "Authorization", "key_env": "TG_TEST_KEY", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": { "test-model": [["fu", "deepseek-v4.1-flash"]] }
}
EOF
  if env -u TG_TEST_KEY "$BIN/tollgate" -config "$CFG" -check >/dev/null 2>&1; then
    fail "缺 env 时应报错 (TG_TEST_KEY)"
  else
    pass "缺 env -> -check 报错"
  fi
  if TG_TEST_KEY=secret "$BIN/tollgate" -config "$CFG" -check >/dev/null 2>&1; then
    pass "有 env -> -check 通过"
  else
    fail "有 env -> -check 失败"
  fi
}

case_fallback() {
  say "=== fallback (候选依序, 失败切下一个) ==="
  local LOG="$TMP/log-fb.jsonl"
  rm -f "$LOG"
  cat >"$TMP/cfg-fb.json" <<EOF
{
  "listen": "127.0.0.1:18813",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "dead", "base_url": "http://127.0.0.1:19999/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fu", "base_url": "http://127.0.0.1:19013/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": {
    "test-model": [["dead", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]],
    "onlydead-model": [["dead", "deepseek-v4.1-flash"]]
  }
}
EOF
  start_fake 19013 20
  start_proxy 18813 "$TMP/cfg-fb.json"

  # dead 上游端口无人监听 -> 传输失败, 应切换到 fu
  local code
  code=$(post 18813 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "200" ] && pass "dead->fu 切换后 200" || fail "fallback -> $code"
  grep '"upstream":"fu"' "$LOG" | grep -q '"fallbacks":1' \
    && pass "日志记 upstream=fu, fallbacks=1" || fail "fallback 日志不对"

  # 全部候选失败 -> 502 透传
  code=$(post 18813 '{"model":"onlydead-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "502" ] && pass "全部失败 -> 502" || fail "allfail -> $code"
}

# fallback 换到另一个上游/模型后, 新候选要重新排它自己的队 (F4), 而不是拿前一个
# 候选的槽位直接发; 同时验证前一个候选的槽位已归还 (不然两个上游会互等成死锁)。
case_fallback_gate() {
  say "=== fallback 换候选后重新排队 ==="
  local LOG="$TMP/log-fbg.jsonl"
  rm -f "$LOG"
  cat >"$TMP/cfg-fbg.json" <<EOF
{
  "listen": "127.0.0.1:18816",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 300 },
  "upstreams": [
    { "id": "dead", "base_url": "http://127.0.0.1:19999/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fu", "base_url": "http://127.0.0.1:19016/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 1 } }
  ],
  "aliases": { "fbg-model": [["dead", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]] }
}
EOF
  start_fake 19016 200
  start_proxy 18816 "$TMP/cfg-fbg.json"

  # A: dead 传输失败 -> 落到 fu, 占住 fu 唯一的槽位 (流式, 每块 200ms, 约 1s)
  local apid
  curl -s -N -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:18816/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fbg-model","messages":[{"role":"user","content":"hi"}],"stream":true}' \
    >"$TMP/fbg-a.txt" 2>&1 &
  apid=$!
  sleep 0.3

  # B: 同样落到 fu, 必须重新排 fu 的队; 300ms 内拿不到槽位 -> 闸门拒
  local code gate
  code=$(curl -s -o "$TMP/resp.txt" -w '%{http_code}' -X POST "http://127.0.0.1:18816/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fbg-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "429" ] && pass "fallback 候选槽位占满 -> 429 (不再直接发)" || fail "fallback -> $code (want 429)"
  gate=$(jq -r 'select(.status==429) | .gate' "$LOG" | tail -1)
  [ "$gate" = "concurrency" ] && pass "429 日志 gate=concurrency" || fail "gate=$gate (want concurrency)"

  wait "$apid" 2>/dev/null
  [ "$(cat "$TMP/fbg-a.txt")" = "200" ] && pass "A 经 fallback 仍成功 200" || fail "A -> $(cat "$TMP/fbg-a.txt")"
  jq -r 'select(.upstream=="fu" and .status==200) | .fallbacks' "$LOG" | tail -1 | grep -q '^1$' \
    && pass "A 记 upstream=fu, fallbacks=1" || fail "A 的 fallback 记账不对"

  # dead 的槽位必须已归还: 否则 fu 会一直等它 -> 死锁, 这里就拿不到 200
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:18816/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"fbg-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "200" ] && pass "槽位已释放无死锁 -> 200" || fail "释放后 -> $code"
}

case_breaker() {
  say "=== breaker (连续失败熔断 + 冷却恢复) ==="
  local LOG="$TMP/log-br.jsonl"
  rm -f "$LOG" "$TMP/br-count.txt"
  cat >"$TMP/srvflaky.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
count = 0
fail_until = int(sys.argv[2])
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        global count
        count += 1
        open(sys.argv[3], 'w').write(str(count))
        if count <= fail_until:
            b = b'{"error":{"message":"flaky 503","type":"server_error","code":"server_error"}}'
            self.send_response(503)
        else:
            b = b'{"id":"chatcmpl-fake","object":"chat.completion","model":"deepseek-v4.1-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok now"}}],"usage":{"prompt_tokens":7,"completion_tokens":42,"total_tokens":49}}'
            self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY
  python3 "$TMP/srvflaky.py" 19991 2 "$TMP/br-count.txt" >/dev/null 2>&1 & FL=$!
  PROCS+=("$FL")
  # 等 python 假上游就绪 (TCP 探测, 不发 HTTP, 不消耗失败计数)
  for _ in $(seq 1 100); do
    if (exec 3<>/dev/tcp/127.0.0.1/19991) 2>/dev/null; then
      exec 3>&- 3<&- 2>/dev/null || true
      break
    fi
    sleep 0.05
  done
  cat >"$TMP/cfg-br.json" <<EOF
{
  "listen": "127.0.0.1:18818",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "flaky", "base_url": "http://127.0.0.1:19991/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "breaker_fail": 2, "breaker_cooldown_ms": 1000, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fu", "base_url": "http://127.0.0.1:19018/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": {
    "tb": [["flaky", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]],
    "tb-only": [["flaky", "deepseek-v4.1-flash"]]
  }
}
EOF
  start_fake 19018 20
  start_proxy 18818 "$TMP/cfg-br.json"
  local BODY='{"model":"tb","messages":[{"role":"user","content":"hi"}]}'
  local code n

  post 18818 "$BODY" >/dev/null   # A: flaky 503(#1) -> fu 200
  post 18818 "$BODY" >/dev/null   # B: flaky 503(#2) -> 熔断打开 -> fu 200
  code=$(post 18818 "$BODY")      # C: 窗口内, 跳过 flaky -> fu 200
  [ "$code" = "200" ] && pass "熔断窗口内跳过首选, 备用 200" || fail "窗口内请求 -> $code"
  n=$(cat "$TMP/br-count.txt" 2>/dev/null)
  [ "$n" = "2" ] && pass "熔断打开后 flaky 不再被请求 (计数=$n)" || fail "flaky 计数=$n (want 2)"

  code=$(post 18818 '{"model":"tb-only","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "502" ] && pass "唯一候选熔断 -> 快速 502" || fail "tb-only -> $code"

  # 冷却 1000ms 后自动恢复, 重新命中 flaky (此刻已恢复正常)
  sleep 1.4
  code=$(post 18818 "$BODY")
  [ "$code" = "200" ] && pass "冷却后恢复 200" || fail "恢复 -> $code"
  n=$(cat "$TMP/br-count.txt" 2>/dev/null)
  [ "$n" = "3" ] && pass "恢复后重新命中 flaky (计数=$n)" || fail "恢复后 flaky 计数=$n (want 3)"
  grep '"upstream":"flaky"' "$LOG" | tail -1 | grep -q '"logical":"tb"' \
    && pass "恢复后日志 upstream=flaky" || fail "恢复后日志上游不对"
}

case_budget() {
  say "=== budget (上游 429 = 限额信号: 冷却 + 切换 + 系数标定 D13) ==="
  local D="$TMP/budget"
  local LOG="$D/log.jsonl"
  rm -rf "$D"
  mkdir -p "$D"
  rm -f "$TMP/budget-count.txt"
  # 假上游: 永远 429 (额度类错误), 并记下被调用次数
  cat >"$TMP/srvbudget.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
count = 0
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        global count
        count += 1
        open(sys.argv[2], 'w').write(str(count))
        b = b'{"error":{"message":"Budget has been exceeded! Current cost: 30.0, Max budget: 30.0","type":"budget_exceeded","code":"429"}}'
        self.send_response(429)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY
  python3 "$TMP/srvbudget.py" 19994 "$TMP/budget-count.txt" >/dev/null 2>&1 & BL=$!
  PROCS+=("$BL")
  for _ in $(seq 1 100); do
    if (exec 3<>/dev/tcp/127.0.0.1/19994) 2>/dev/null; then exec 3>&- 3<&- 2>/dev/null || true; break; fi
    sleep 0.05
  done
  # 预置 50 token 的窗口累计: 撞墙时反推系数 = 30/50 = 0.6 (确定性断言用)
  printf '{"ts":"%s","logical":"test-model","upstream":"limited","model":"deepseek-v4.1-flash","usage":{"prompt":20,"completion":30,"total":50},"missing_usage":false}\n' \
    "$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())")" >>"$LOG"
  cat >"$TMP/cfg-budget.json" <<EOF
{
  "listen": "127.0.0.1:18820",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "limited", "base_url": "http://127.0.0.1:19994/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "budget": { "windows": { "3h": 30 }, "cooldown_ms": 1500, "refresh_sec": 1 }, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fu", "base_url": "http://127.0.0.1:19020/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": {
    "test-model": [["limited", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]],
    "only-limited": [["limited", "deepseek-v4.1-flash"]]
  }
}
EOF
  start_fake 19020 20
  start_proxy 18820 "$TMP/cfg-budget.json"

  local BODY='{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
  local code n retry unit

  # A: limited 429 -> 自动切 fu, 客户端仍 200
  code=$(post 18820 "$BODY")
  [ "$code" = "200" ] && pass "429 后切下一个候选 -> 200" || fail "fallback -> $code"
  grep '"upstream":"fu"' "$LOG" | tail -1 | grep -q '"fallbacks":1' \
    && pass "日志记 upstream=fu, fallbacks=1" || fail "fallback 日志不对"
  grep -q '"budget_hit":true' "$LOG" && pass "日志记 budget_hit=true" || fail "budget_hit 未记"

  # 系数标定: 30/50 = 0.6 落状态文件
  sleep 0.3
  if [ -f "$D/budget-units.json" ]; then
    unit=$(jq -r '.units.limited // empty' "$D/budget-units.json")
    if [ -n "$unit" ] && python3 -c "import sys; sys.exit(0 if abs(float('$unit') - 0.6) < 1e-9 else 1)"; then
      pass "撞墙反推系数 = 0.6 (30/50) 并落状态文件"
    else
      fail "状态文件系数=$unit (want 0.6)"
    fi
    jq -e '.cooldown_until.limited' "$D/budget-units.json" >/dev/null 2>&1 \
      && pass "状态文件记冷却截止时间" || fail "状态文件缺 cooldown_until"
  else
    fail "未生成状态文件 $D/budget-units.json"
  fi

  # B: 冷却期内不再真发该上游 (计数仍为 1), 仍由 fu 服务
  code=$(post 18820 "$BODY")
  [ "$code" = "200" ] && pass "冷却期内请求仍成功 (走备用)" || fail "冷却期请求 -> $code"
  n=$(cat "$TMP/budget-count.txt" 2>/dev/null)
  [ "$n" = "1" ] && pass "冷却期内不再请求该上游 (计数=$n)" || fail "冷却期仍请求了该上游 (计数=$n)"
  grep -q '"cooldown_skips":1' "$LOG" && pass "日志记 cooldown_skips=1" || fail "cooldown_skips 未记"

  # C: 唯一候选在冷却 -> 429 + Retry-After (不伪造 502)
  code=$(curl -s -D "$TMP/hdr-cool.txt" -o "$TMP/resp.txt" -w '%{http_code}' \
    -X POST "http://127.0.0.1:18820/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' \
    -d '{"model":"only-limited","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "429" ] && pass "候选全冷却 -> 429" || fail "全冷却 -> $code (want 429)"
  jq -e '.error.code == "upstream_cooling"' "$TMP/resp.txt" >/dev/null 2>&1 \
    && pass "error.code=upstream_cooling" || fail "429 error 体不对"
  retry=$(grep -i '^retry-after:' "$TMP/hdr-cool.txt" | tr -d '\r' | awk '{print $2}')
  if [ -n "$retry" ] && python3 -c "import sys; sys.exit(0 if int('$retry') >= 1 else 1)" 2>/dev/null; then
    pass "Retry-After: $retry"
  else
    fail "Retry-After 缺失/非法 ($retry)"
  fi

  # D: 冷却 1500ms 过后重新尝试该上游 (计数变 2)
  sleep 1.6
  code=$(post 18820 "$BODY")
  [ "$code" = "200" ] && pass "冷却结束后请求成功" || fail "冷却后 -> $code"
  n=$(cat "$TMP/budget-count.txt" 2>/dev/null)
  [ "$n" = "2" ] && pass "冷却结束后重新尝试该上游 (计数=$n)" || fail "冷却后计数=$n (want 2)"
}

case_rotate() {
  say "=== rotate (按本地日历天轮转 + 归档参与窗口累计) ==="
  local D="$TMP/rotate"
  local LOG="$D/log.jsonl"
  local DAY DECOY OLD
  DAY=$(date -d 'yesterday' +%F)
  DECOY="$D/log-$DAY.jsonl"
  OLD="$D/log-$DAY-1.jsonl"
  rm -rf "$D"
  mkdir -p "$D"
  # 既有归档 (同名) 不能被覆盖; 上一段日志应落到 -1 序号。
  echo '{"marker":"decoy"}' >"$DECOY"
  # 昨天那段日志: 50 token 记在 limited 上游 (撞墙标定要跨归档读到它)
  printf '{"ts":"%s","logical":"only","upstream":"limited","model":"deepseek-v4.1-flash","usage":{"prompt":20,"completion":30,"total":50},"missing_usage":false}\n' \
    "$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())")" >"$LOG"
  # mtime 拨到昨天 23:50: 模拟"昨天写的日志, 今天启动代理"。
  touch -d 'yesterday 23:50' "$LOG"
  # 429 上游 (额度类错误), 用来触发撞墙标定。
  cat >"$TMP/srvquota.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        b = b'{"error":{"message":"Budget has been exceeded! Max budget: 30.0","type":"budget_exceeded","code":"429"}}'
        self.send_response(429)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY
  python3 "$TMP/srvquota.py" 19995 >/dev/null 2>&1 & QL=$!
  PROCS+=("$QL")
  for _ in $(seq 1 100); do
    if (exec 3<>/dev/tcp/127.0.0.1/19995) 2>/dev/null; then exec 3>&- 3<&- 2>/dev/null || true; break; fi
    sleep 0.05
  done
  cat >"$TMP/cfg-rotate.json" <<EOF
{
  "listen": "127.0.0.1:18840",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "limited", "base_url": "http://127.0.0.1:19995/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true, "budget": { "windows": { "3h": 30 }, "cooldown_ms": 1000, "refresh_sec": 1 }, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": { "only": [["limited", "deepseek-v4.1-flash"]] }
}
EOF
  start_proxy 18840 "$TMP/cfg-rotate.json"

  grep -q '"marker":"decoy"' "$DECOY" && pass "既有归档不被覆盖" || fail "既有归档被覆盖"
  if [ -f "$OLD" ] && grep -q '"usage"' "$OLD"; then
    pass "隔天启动: 上一段日志归档为 $(basename "$OLD")"
  else
    fail "未生成归档 $OLD"
  fi
  [ ! -s "$LOG" ] && pass "当前日志新开且为空" || fail "当前日志未新开"

  # 撞墙: 若归档里的 50 token 没被读进窗口累计, 就反推不出系数 0.6。
  local code unit
  code=$(curl -s -o "$TMP/resp.txt" -w '%{http_code}' \
    -X POST "http://127.0.0.1:18840/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' \
    -d '{"model":"only","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "429" ] && pass "唯一候选 429 原样透传" || fail "429 透传 -> $code"
  sleep 0.3
  unit=$(jq -r '.units.limited // empty' "$D/budget-units.json" 2>/dev/null)
  if [ -n "$unit" ] && python3 -c "import sys; sys.exit(0 if abs(float('$unit') - 0.6) < 1e-9 else 1)"; then
    pass "归档记录计入窗口累计 (反推系数 0.6)"
  else
    fail "跨天归档未计入 (系数=${unit:-空})"
  fi
}

case_stream() {
  say "=== stream (SSE 逐块透传 + 工具调用/思考块 + F6) ==="
  local LOG="$TMP/log-stream.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-stream.json" 18802 "$LOG" 19002 fu true 100 10 10 10000 test-model
  start_fake 19002 150
  start_proxy 18802 "$TMP/cfg-stream.json"

  local BODY='{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}'
  local meta st tt code body
  meta=$(post_stream 18802 "$BODY")
  read -r st tt code <<<"$meta"
  [ "$code" = "200" ] && pass "stream -> 200" || fail "stream -> $code"
  body=$(cat "$TMP/resp.txt")
  printf '%s' "$body" | grep -q 'thinking step one' && pass "思考块透传" || fail "思考块缺失"
  printf '%s' "$body" | grep -q 'Hello ' && pass "文本块透传 (Hello)" || fail "文本块缺失"
  printf '%s' "$body" | grep -q 'world!' && pass "文本块透传 (world!)" || fail "world! 缺失"
  printf '%s' "$body" | grep -q 'get_weather' && pass "工具调用透传" || fail "工具调用缺失"
  printf '%s' "$body" | grep -q 'data: \[DONE\]' && pass "以 [DONE] 结束" || fail "无 [DONE]"
  # O9: 每个 data 块 (含 usage) 的 model 都改回逻辑名。
  models=$(printf '%s' "$body" | grep '^data: ' | grep -v '^data: \[DONE\]$' | sed 's/^data: //' | jq -r '.model // empty' | sort -u)
  [ "$models" = "test-model" ] && pass "流 chunk model 改回逻辑名" || fail "流 chunk model=$models (want test-model)"
  # 逐块不缓冲: 首字节远早于全流结束 (假上游每块延迟 150ms)
  if python3 -c "import sys; sys.exit(0 if float('$st') < 0.35 and float('$tt') > float('$st') + 0.2 else 1)"; then
    pass "逐块透传不缓冲 (首字节 ${st}s, 全流 ${tt}s)"
  else
    fail "疑似整流缓冲 (首字节 ${st}s, 全流 ${tt}s)"
  fi

  # F6: 上游中途断流 -> SSE error 事件, 不伪造 [DONE]
  meta=$(curl -s -N -o "$TMP/resp.txt" -w '%{http_code}' -X POST "http://127.0.0.1:18802/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'x-abort-after: 1' -d "$BODY")
  body=$(cat "$TMP/resp.txt")
  [ "$meta" = "200" ] && pass "断流 stream -> 200" || fail "断流 stream -> $meta"
  printf '%s' "$body" | grep -q 'event: error' && pass "断流后发 SSE error 事件 (F6)" || fail "无 error 事件"
  if printf '%s' "$body" | grep -q '\[DONE\]'; then
    fail "断流后不应有 [DONE]"
  else
    pass "断流不伪造 [DONE]"
  fi
}

case_usage() {
  say "=== usage (include_usage 注入 + 提取 + F7 兜底) ==="
  local LOG="$TMP/log-usage.jsonl"
  rm -f "$LOG"
  cat >"$TMP/cfg-usage.json" <<EOF
{
  "listen": "127.0.0.1:18803",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "upstreams": [
    { "id": "fu", "base_url": "http://127.0.0.1:19003/v1", "auth_header": "Authorization", "key": "k1", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fnu", "base_url": "http://127.0.0.1:19003/v1", "auth_header": "Authorization", "key": "k2", "headers": {}, "include_usage": false, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": {
    "test-model": [["fu", "deepseek-v4.1-flash"]],
    "no-usage-model": [["fnu", "deepseek-v4.1-flash"]]
  }
}
EOF
  start_fake 19003 20
  start_proxy 18803 "$TMP/cfg-usage.json"

  # 1) 流式 + include_usage 注入 -> 流末 usage 7/42/49
  post_stream 18803 '{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}' >/dev/null
  local line pu cu tu
  line=$(grep '"usage"' "$TMP/resp.txt" | tail -1)
  pu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.prompt_tokens')
  cu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.completion_tokens')
  tu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.total_tokens')
  [ "$pu" = "7" ] && [ "$cu" = "42" ] && [ "$tu" = "49" ] \
    && pass "流式 usage 提取 7/42/49" || fail "流式 usage=$pu/$cu/$tu (want 7/42/49)"
  grep -q '"usage":{"prompt":7,"completion":42,"total":49}' "$LOG" \
    && pass "日志 usage 与上游一致" || fail "日志 usage 不一致"

  # 2) 流式走 include_usage=false 的上游 -> 无 usage, F7 字符兜底:
  #    "Hello " 和 "world!" 按块各估 (6+3)/4=2, 合计 4
  post_stream 18803 '{"model":"no-usage-model","messages":[{"role":"user","content":"hi"}],"stream":true}' >/dev/null
  if grep -q '"usage"' "$TMP/resp.txt"; then
    fail "no-usage 上游不应有 usage chunk"
  else
    pass "no-usage 上游无 usage chunk"
  fi
  grep '"upstream":"fnu"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":0,"completion":4,"total":4}' \
    && pass "F7 兜底估算 completion=4" || fail "F7 兜底估算缺失"
  grep '"upstream":"fnu"' "$LOG" | tail -1 | grep -q '"missing_usage":true' \
    && pass "missing_usage 计数" || fail "missing_usage 未记"

  # 3) 非流式 -> 响应体 usage + 日志一致
  post 18803 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}' >/dev/null
  tu=$(jq -r '.usage.total_tokens // empty' "$TMP/resp.txt")
  [ "$tu" = "49" ] && pass "非流式 usage 提取 total=49" || fail "非流式 usage=$tu"
  grep '"upstream":"fu"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":7,"completion":42,"total":49}' \
    && pass "非流式日志 usage 一致" || fail "非流式日志 usage 不一致"
  # O9: 非流响应 model 改回逻辑名。
  m=$(jq -r '.model' "$TMP/resp.txt")
  [ "$m" = "test-model" ] && pass "非流响应 model 改回逻辑名" || fail "非流响应 model=$m (want test-model)"

  # 4) 缓存命中 token: 上游给 prompt_tokens_details.cached_tokens 时记入日志 (系数标定要用)。
  curl -s -o /dev/null -X POST "http://127.0.0.1:18803/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Fake-Cached: 5' \
    -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
  grep '"upstream":"fu"' "$LOG" | tail -1 | grep -q '"cached":5' \
    && pass "缓存命中 token 记入日志 (cached=5)" || fail "cached 未记入日志"
}

case_usage_report() {
  say "=== -usage (上游/模型 × 窗口 token 分解, 标定系数用) ==="
  local LOG="$TMP/log-usagecmd.jsonl"
  rm -f "$LOG" "$TMP/log-usagecmd-"*.jsonl
  local now old
  now=$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())")
  old=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(hours=8)).isoformat())")
  printf '{"ts":"%s","logical":"a","upstream":"ustc","model":"deepseek-flash","usage":{"prompt":1000,"completion":2000,"total":3000,"cached":500},"missing_usage":false}\n' "$now" >>"$LOG"
  printf '{"ts":"%s","logical":"b","upstream":"ustc","model":"qwen-chat","usage":{"prompt":10,"completion":20,"total":30},"missing_usage":false}\n' "$now" >>"$LOG"
  printf '{"ts":"%s","logical":"a","upstream":"ustc","model":"deepseek-flash","usage":{"prompt":7,"completion":3,"total":10},"missing_usage":false}\n' "$old" >>"$LOG"

  "$BIN/tollgate" -usage "$LOG" -windows 3h >"$TMP/usage3h.txt" 2>&1
  "$BIN/tollgate" -usage "$LOG" >"$TMP/usageall.txt" 2>&1

  local t c
  t=$(awk -v w="3h" -v k="ustc/deepseek-flash" '/^== 窗口 /{b=($0 ~ ("^== 窗口 " w " "))} b && $1==k{print $7}' "$TMP/usage3h.txt")
  [ "$t" = "3000" ] && pass "3h 窗口按模型汇总 deepseek-flash total=3000" || fail "3h deepseek-flash total=$t"
  t=$(awk -v w="3h" -v k="ustc/qwen-chat" '/^== 窗口 /{b=($0 ~ ("^== 窗口 " w " "))} b && $1==k{print $7}' "$TMP/usage3h.txt")
  [ "$t" = "30" ] && pass "3h 窗口另一模型单列 qwen-chat total=30" || fail "3h qwen-chat total=$t"
  c=$(awk -v w="3h" -v k="ustc/deepseek-flash" '/^== 窗口 /{b=($0 ~ ("^== 窗口 " w " "))} b && $1==k{print $6}' "$TMP/usage3h.txt")
  [ "$c" = "500" ] && pass "cached 列单列 (500)" || fail "cached 列=$c"
  t=$(awk -v w="全部" -v k="ustc/deepseek-flash" '/^== 窗口 /{b=($0 ~ ("^== 窗口 " w " "))} b && $1==k{print $7}' "$TMP/usageall.txt")
  [ "$t" = "3010" ] && pass "全部窗口含 8 小时前旧记录 (3010)" || fail "全部 total=$t"
}

case_ratelimit_min() {
  say "=== ratelimit (最小间隔 + 排队记账) ==="
  local LOG="$TMP/log-rl.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-rl.json" 18804 "$LOG" 19004 rl true 1 1 1 5000 rl-model
  start_fake 19004 20
  start_proxy 18804 "$TMP/cfg-rl.json"

  local BODY='{"model":"rl-model","messages":[{"role":"user","content":"hi"}]}'
  post 18804 "$BODY" >/dev/null                 # A: 消耗唯一令牌
  local code
  code=$(post 18804 "$BODY")                    # B: 立即再发, 排队等令牌
  [ "$code" = "200" ] && pass "排队后成功 200" || fail "排队请求 -> $code"
  local tt
  tt=$(curl -s -o /dev/null -w '%{time_total}' -X POST "http://127.0.0.1:18804/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$BODY")
  if python3 -c "import sys; sys.exit(0 if float('$tt') >= 0.7 else 1)"; then
    pass "最小间隔生效 (等待 ${tt}s >= 0.7s)"
  else
    fail "最小间隔未生效 ($tt)"
  fi
  grep '"upstream":"rl"' "$LOG" | grep -q '"queued":true' \
    && pass "排队记账 queued:true" || fail "无排队记录"
}

case_ratelimit_429() {
  say "=== ratelimit 429 (排队超限 + Retry-After + 恢复) ==="
  local LOG="$TMP/log-rl429.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-rl429.json" 18805 "$LOG" 19005 rl true 1 1 1 300 rl-model
  start_fake 19005 20
  start_proxy 18805 "$TMP/cfg-rl429.json"

  local BODY='{"model":"rl-model","messages":[{"role":"user","content":"hi"}]}'
  post 18805 "$BODY" >/dev/null                 # A: 消耗唯一令牌
  local code retry
  code=$(curl -s -D "$TMP/hdr.txt" -o "$TMP/resp.txt" -w '%{http_code}' \
    -X POST "http://127.0.0.1:18805/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$BODY")
  retry=$(grep -i '^retry-after:' "$TMP/hdr.txt" | tr -d '\r' | awk '{print $2}')
  [ "$code" = "429" ] && pass "排队超限 -> 429" || fail "code=$code (want 429)"
  if [ -n "$retry" ] && python3 -c "import sys; sys.exit(0 if int('$retry') >= 1 else 1)" 2>/dev/null; then
    pass "Retry-After: $retry"
  else
    fail "Retry-After 缺失/非法 ($retry)"
  fi
  jq -e '.error.code == "rate_limit_error"' "$TMP/resp.txt" >/dev/null 2>&1 \
    && pass "error.code=rate_limit_error" || fail "429 error 体不对"
  # 频率闸: 该配置并发槽位空闲, 卡住的一定是令牌桶, 日志必须写明是哪个闸门。
  local gate qms
  gate=$(jq -r 'select(.status==429) | .gate' "$LOG" | tail -1)
  qms=$(jq -r 'select(.status==429) | .queue_ms' "$LOG" | tail -1)
  [ "$gate" = "rate" ] && pass "429 日志 gate=rate (频率闸)" || fail "gate=$gate (want rate)"
  awk -v q="${qms:-0}" 'BEGIN{exit !(q>=200)}' \
    && pass "拒绝时记下等待时长 queue_ms=${qms}ms" || fail "queue_ms=$qms (want >=200)"
  sleep 1.3
  code=$(post 18805 "$BODY")
  [ "$code" = "200" ] && pass "冷却后恢复 200" || fail "恢复失败 $code"

  # 并发闸: 槽位被占满 -> 429 且 gate=concurrency (生产上踩的正是这一条)。
  local C2="$TMP/cfg-rl429c.json" L2="$TMP/log-rl429c.jsonl"
  rm -f "$L2"
  gen_config1 "$C2" 18815 "$L2" 19015 rlc true 100 10 1 200 rlc-model
  start_fake 19015 200
  start_proxy 18815 "$C2"
  local cpid
  curl -s -N -o /dev/null -X POST "http://127.0.0.1:18815/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"rlc-model","messages":[{"role":"user","content":"hi"}],"stream":true}' &
  cpid=$!
  sleep 0.15
  code=$(curl -s -o "$TMP/resp.txt" -w '%{http_code}' -X POST "http://127.0.0.1:18815/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"rlc-model","messages":[{"role":"user","content":"hi"}]}')
  wait "$cpid" 2>/dev/null
  [ "$code" = "429" ] && pass "并发槽位占满 -> 429" || fail "并发闸 -> $code"
  gate=$(jq -r 'select(.status==429) | .gate' "$L2" | tail -1)
  [ "$gate" = "concurrency" ] && pass "429 日志 gate=concurrency" || fail "gate=$gate (want concurrency)"
}

case_logging() {
  say "=== logging (JSONL 逐笔明细 + 模型名改写) ==="
  local LOG="$TMP/log-logging.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-logging.json" 18806 "$LOG" 19006 fu true 100 10 10 10000 test-model
  start_fake 19006 20
  start_proxy 18806 "$TMP/cfg-logging.json"

  post_stream 18806 '{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}' >/dev/null
  post 18806 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}' >/dev/null
  local n=0 bad=0
  while IFS= read -r line; do
    n=$((n + 1))
    if ! printf '%s' "$line" | jq -e \
      'has("logical") and has("upstream") and has("model") and has("usage") and has("missing_usage") and has("queued") and has("status") and .logical=="test-model" and .model=="deepseek-v4.1-flash" and .upstream=="fu"' \
      >/dev/null 2>&1; then
      bad=$((bad + 1))
    fi
  done <"$LOG"
  [ "$n" -ge 2 ] && [ "$bad" = 0 ] \
    && pass "日志 $n 行, 字段齐全, 模型名已改写为真实 id" || fail "日志校验: n=$n bad=$bad"

  # O3: source 头写入日志。
  curl -s -o /dev/null -X POST "http://127.0.0.1:18806/v1/chat/completions" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Tollgate-Source: agent-x' \
    -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
  grep '"source":"agent-x"' "$LOG" | tail -1 | grep -q '"logical":"test-model"' \
    && pass "日志记 source=agent-x (O3)" || fail "source 未记入日志"

  # O2: -summary 聚合, TOTAL 请求数 = 日志行数。
  "$BIN/tollgate" -summary "$LOG" >"$TMP/summary.txt" 2>&1
  grep -F 'test-model / agent-x' "$TMP/summary.txt" >/dev/null 2>&1 \
    && pass "-summary 含 test-model / agent-x 分组行 (O2)" || fail "-summary 缺分组行"
  n=$(wc -l <"$LOG")
  m=$(awk '$1=="TOTAL"{print $2}' "$TMP/summary.txt")
  [ "$n" = "$m" ] && pass "-summary TOTAL=$m 与日志行数 $n 一致" || fail "-summary TOTAL=$m, 日志行数 $n"
}

case_overhead() {
  say "=== overhead (代理首字节延迟增量) ==="
  local LOG="$TMP/log-oh.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-oh.json" 18807 "$LOG" 19007 fu true 1000 100 100 10000 test-model
  start_fake 19007 20
  start_proxy 18807 "$TMP/cfg-oh.json"

  local BODY='{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
  local i st d p delta
  : >"$TMP/direct.txt"
  : >"$TMP/proxied.txt"
  for i in $(seq 1 15); do
    st=$(curl -s -o /dev/null -w '%{time_starttransfer}' -X POST "http://127.0.0.1:19007/v1/chat/completions" \
      -H 'Content-Type: application/json' -H 'Authorization: Bearer x' -d "$BODY")
    echo "$st" >>"$TMP/direct.txt"
    st=$(curl -s -o /dev/null -w '%{time_starttransfer}' -X POST "http://127.0.0.1:18807/v1/chat/completions" \
      -H 'Content-Type: application/json' -H 'Authorization: Bearer x' -d "$BODY")
    echo "$st" >>"$TMP/proxied.txt"
  done
  d=$(awk '{s += $1} END{printf "%.4f", s / NR}' "$TMP/direct.txt")
  p=$(awk '{s += $1} END{printf "%.4f", s / NR}' "$TMP/proxied.txt")
  delta=$(awk -v d="$d" -v p="$p" 'BEGIN{printf "%.4f", p - d}')
  say "  直连平均首字节 ${d}s, 经代理 ${p}s, 增量 ${delta}s"
  if python3 -c "import sys; sys.exit(0 if float('$delta') < 0.05 else 1)"; then
    pass "代理首字节开销 < 50ms (+${delta}s)"
  else
    fail "代理开销过大 (+${delta}s)"
  fi
}

case_hup() {
  say "=== hup (SIGHUP 重载生效, limits 变更出声不静默) ==="
  local LOG="$TMP/log-hup.jsonl" CFG="$TMP/cfg-hup.json" PLOG="$TMP/proxy-18809.log"
  rm -f "$LOG" "$PLOG"
  gen_config1 "$CFG" 18809 "$LOG" 19009 fu true 100 10 10 10000 test-model
  start_fake 19009 20
  start_proxy 18809 "$CFG"
  local pid="${PROCS[${#PROCS[@]}-1]}"

  # 无变更的重载: 正常重载, 不该报 limits 告警。
  kill -HUP "$pid"
  sleep 0.4
  grep -q 'config reloaded' "$PLOG" && pass "SIGHUP 触发配置重载" || fail "未触发重载"
  grep -q 'limits 已改' "$PLOG" && fail "无变更却报 limits 告警" || pass "无变更时不误报"

  # 改 limits 后重载: 限速器状态有意保留, 但必须出声 (不能静默沿用旧值)。
  gen_config1 "$CFG" 18809 "$LOG" 19009 fu true 100 10 4 10000 test-model
  kill -HUP "$pid"
  sleep 0.4
  grep -q 'limits 已改' "$PLOG" \
    && pass "limits 变更在重载时告警 (不再静默失效)" || fail "limits 变更未告警"
  grep -q 'Concurrency:10' "$PLOG" && grep -q 'Concurrency:4' "$PLOG" \
    && pass "告警含新旧值 (10 -> 4)" || fail "告警缺新旧值"
  local code
  code=$(post 18809 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}')
  [ "$code" = "200" ] && pass "重载后仍正常服务 200" || fail "重载后 -> $code"
}

# ---------- 分发 ----------

case_responses() {
  say "=== responses (/v1/responses: 转发 + usage 口径归一 + 流终态 + model 改写) ==="
  local LOG="$TMP/log-responses.jsonl"
  rm -f "$LOG"
  gen_config1 "$TMP/cfg-responses.json" 18808 "$LOG" 19008 fu true 100 10 10 10000 test-model
  start_fake 19008 20
  start_proxy 18808 "$TMP/cfg-responses.json"

  # 1) 非流式: 转发成功 + 顶层 model 改回逻辑名。
  local code m so
  code=$(post_resp 18808 '{"model":"test-model","input":"hi"}')
  [ "$code" = "200" ] && pass "/v1/responses 非流式 -> 200" || fail "非流式 -> $code"
  m=$(jq -r '.model' "$TMP/resp.txt")
  [ "$m" = "test-model" ] && pass "非流式响应 model 改回逻辑名" || fail "model=$m (want test-model)"
  # 该接口没有 stream_options, 代理不得注入 include_usage。
  so=$(grep -i '^x-fake-stream-options:' "$TMP/hdr-resp.txt" | tr -d '\r' | awk '{print $2}')
  [ "$so" = "absent" ] && pass "未注入 stream_options (responses 无此字段)" || fail "stream_options=$so (want absent)"
  # input/output_tokens 归一成 prompt/completion 后记账。
  grep -q '"usage":{"prompt":7,"completion":42,"total":49}' "$LOG" \
    && pass "input/output 口径归一 (7/42/49)" || fail "responses usage 未归一"

  # 2) 流式: response.completed 是正常收尾, 不得当成"流意外结束"再补 error 事件。
  curl -s -N -o "$TMP/resp.txt" \
    -X POST "http://127.0.0.1:18808/v1/responses" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' \
    -d '{"model":"test-model","input":"hi","stream":true}'
  if grep -q 'event: error' "$TMP/resp.txt"; then
    fail "正常流被误补 error 事件 (F6 误报)"
  else
    pass "无 error 事件 (response.completed 认作正常收尾)"
  fi
  grep -q '"model":"test-model"' "$TMP/resp.txt" \
    && pass "事件里嵌套的 response.model 改回逻辑名" || fail "事件 model 未改写"
  grep -q 'deepseek-v4.1-flash' "$TMP/resp.txt" \
    && fail "响应泄漏真实上游模型 id" || pass "响应不含真实上游 id (O9)"
  grep '"path":"/v1/responses"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":7,"completion":42,"total":49}' \
    && pass "流式 usage 从 response.completed 提取并记账" || fail "流式 responses usage 未记账"
  grep '"path":"/v1/responses"' "$LOG" | tail -1 | grep -q '"missing_usage":false' \
    && pass "missing_usage=false (未走 F7 兜底)" || fail "missing_usage 标记不对"

  # 3) 未注册模型仍走白名单 404。
  code=$(post_resp 18808 '{"model":"nope","input":"hi"}')
  [ "$code" = "404" ] && pass "未注册模型 -> 404" || fail "未注册模型 -> $code"

  # 4) 上游中途断流 (没有终态事件) -> 仍按 F6 补 error 事件。
  curl -s -N -o "$TMP/resp.txt" \
    -X POST "http://127.0.0.1:18808/v1/responses" \
    -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Abort-After: 2' \
    -d '{"model":"test-model","input":"hi","stream":true}'
  grep -q 'event: error' "$TMP/resp.txt" \
    && pass "无终态事件时仍补 error 事件 (F6 未被削弱)" || fail "断流未补 error 事件"
}

case_probe() {
  say "=== probe (只观测不切换: 被动采样 + 只探 fallback 的下一个候选) ==="
  local DIR="$TMP/probe"
  local LOG="$DIR/log.jsonl" CFG="$DIR/cfg.json"
  rm -rf "$DIR"; mkdir -p "$DIR"
  cat >"$CFG" <<EOF
{
  "listen": "127.0.0.1:18821",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000, "queue_wait_ms": 10000 },
  "probe": { "enabled": true, "scope": "next", "interval_sec": 1, "max_tokens": 4,
             "timeout_ms": 5000, "window": 3, "targets": ["probe-model"] },
  "upstreams": [
    { "id": "fu1", "base_url": "http://127.0.0.1:19021/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } },
    { "id": "fu2", "base_url": "http://127.0.0.1:19021/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": true, "limits": { "rate_per_sec": 100, "burst": 10, "concurrency": 10 } }
  ],
  "aliases": {
    "probe-model": [["fu1", "deepseek-v4.1-flash"], ["fu2", "deepseek-v4.1-flash"]],
    "other-model": [["fu1", "glm-5.3"], ["fu2", "glm-5.3"]]
  }
}
EOF
  start_fake 19021 20
  start_proxy 18821 "$CFG"

  # 真实流式请求: 产生主候选的被动样本 (不额外发探测请求)
  local out
  out=$(post_stream 18821 '{"model":"probe-model","messages":[{"role":"user","content":"hi"}],"stream":true}')
  [ "$(echo "$out" | awk '{print $3}')" = "200" ] && pass "流式请求 200" || fail "流式请求 $(echo "$out" | awk '{print $3}')"

  # 等探测循环至少跑一轮 (interval 1s; probeOnce 在启动时先跑一次)
  local rep="" i
  for i in $(seq 1 40); do
    rep=$(curl -s "http://127.0.0.1:18821/v1/probe")
    [ "$(echo "$rep" | jq -r '.aliases["probe-model"][1].active // 0')" -ge 1 ] 2>/dev/null && break
    sleep 0.25
  done

  [ -n "$rep" ] && pass "/v1/probe 有响应" || fail "/v1/probe 无响应"
  [ "$(echo "$rep" | jq -r '.aliases["probe-model"][0].role')" = "primary" ] && pass "报告保持 fallback 顺序 (首个=primary)" || fail "role 顺序错"
  [ "$(echo "$rep" | jq -r '.aliases["probe-model"][1].role')" = "next" ] && pass "第二个候选 role=next" || fail "next role 错"
  local pas act ttfb
  pas=$(echo "$rep" | jq -r '.aliases["probe-model"][0].passive')
  act=$(echo "$rep" | jq -r '.aliases["probe-model"][1].active')
  ttfb=$(echo "$rep" | jq -r '.aliases["probe-model"][1].ttfb_ms')
  [ "${pas:-0}" -ge 1 ] && pass "主候选有被动样本 ($pas)" || fail "主候选被动样本=$pas"
  [ "${act:-0}" -ge 1 ] && pass "下一个候选被主动探测 ($act)" || fail "下一个候选主动样本=$act"
  awk -v t="${ttfb:-0}" 'BEGIN{exit !(t>0)}' && pass "探测取得 ttfb (${ttfb}ms)" || fail "ttfb=$ttfb"

  # targets 之外的别名不探
  [ "$(echo "$rep" | jq -r '.aliases["other-model"][1].active // 0')" = "0" ] && pass "targets 之外的别名未被探测" || fail "other-model 被探了"

  # 探测流量只打下一个候选, 且以 source=probe 入日志
  local probed
  probed=$(jq -r 'select(.source=="probe") | .upstream' "$LOG" 2>/dev/null | sort -u | tr '\n' ' ')
  [ "$probed" = "fu2 " ] && pass "探测只打下一个候选 (日志 source=probe: $probed)" || fail "探测候选=$probed (want fu2)"

  # D14 不接入: 探测不改变候选顺序, 新请求仍走第一个候选
  post 18821 '{"model":"probe-model","messages":[{"role":"user","content":"hi"}]}' >/dev/null
  local lastup
  lastup=$(jq -r 'select(.source!="probe") | .upstream' "$LOG" 2>/dev/null | tail -1)
  [ "$lastup" = "fu1" ] && pass "顺序未被探测改变 (仍走 fu1)" || fail "落到了 $lastup"

  [ -s "$DIR/probe.json" ] && pass "状态文件 probe.json 已写" || fail "probe.json 缺失"
  "$BIN/tollgate" -probe "$CFG" 2>/dev/null | grep -q 'probe-model' && pass "-probe 离线报告可读" || fail "-probe 报告失败"
}

case_all() {
  case_alias
  case_keyenv
  case_fallback
  case_fallback_gate
  case_breaker
  case_budget
  case_rotate
  case_stream
  case_usage
  case_usage_report
  case_ratelimit_min
  case_ratelimit_429
  case_logging
  case_overhead
  case_responses
  case_hup
  case_probe
}

main() {
  build
  case "${1:-all}" in
    alias)         case_alias ;;
    keyenv)        case_keyenv ;;
    fallback)      case_fallback ;;
    fallback-gate) case_fallback_gate ;;
    breaker)       case_breaker ;;
    budget)        case_budget ;;
    rotate)        case_rotate ;;
    stream)        case_stream ;;
    usage)         case_usage ;;
    usage-report)  case_usage_report ;;
    ratelimit)     case_ratelimit_min; case_ratelimit_429 ;;
    logging)       case_logging ;;
    overhead)      case_overhead ;;
    responses)     case_responses ;;
    hup)           case_hup ;;
    probe)         case_probe ;;
    all)           case_all ;;
    *) say "unknown case: $1"; exit 2 ;;
  esac
  say ""
  say "结果: $PASS 通过, $FAIL 失败"
  [ "$FAIL" = 0 ]
}

main "$@"
