#!/usr/bin/env bash
# llm-request-tollgate 自测: 对本地假上游跑代理, 按用例断言 (设计 §4.7)。
#
# 用法: test/run_selftest.sh <case|all>
#   case: alias | keyenv | fallback | breaker | ratelimit | usage | stream | logging | overhead
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
  sleep 1.3
  code=$(post 18805 "$BODY")
  [ "$code" = "200" ] && pass "冷却后恢复 200" || fail "恢复失败 $code"
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
      'has("logical") and has("upstream") and has("model") and has("usage") and has("missing_usage") and has("queued") and has("status") and has("budget_allow") and .logical=="test-model" and .model=="deepseek-v4.1-flash" and .upstream=="fu"' \
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

# ---------- 分发 ----------

case_all() {
  case_alias
  case_keyenv
  case_fallback
  case_breaker
  case_stream
  case_usage
  case_ratelimit_min
  case_ratelimit_429
  case_logging
  case_overhead
}

main() {
  build
  case "${1:-all}" in
    alias)         case_alias ;;
    keyenv)        case_keyenv ;;
    fallback)      case_fallback ;;
    breaker)       case_breaker ;;
    stream)        case_stream ;;
    usage)         case_usage ;;
    ratelimit)     case_ratelimit_min; case_ratelimit_429 ;;
    logging)       case_logging ;;
    overhead)      case_overhead ;;
    all)           case_all ;;
    *) say "unknown case: $1"; exit 2 ;;
  esac
  say ""
  say "结果: $PASS 通过, $FAIL 失败"
  [ "$FAIL" = 0 ]
}

main "$@"
