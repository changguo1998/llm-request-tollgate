#!/usr/bin/env bash
# llm-request-tollgate 自测: 对本地假上游跑代理, 按用例断言 (设计 §4.7)。
#
# 用法: test/run_selftest.sh <case|all>
#   case: alias | keyenv | fallback | fallback-gate | breaker | quota | rotate | ratelimit | usage | usage-report | stream | logging | overhead | responses | hup | probe
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

say() { printf '%s\n' "$*"; }
pass() {
    PASS=$((PASS + 1))
    printf '  PASS  %s\n' "$*"
}
fail() {
    FAIL=$((FAIL + 1))
    printf '  FAIL  %s\n' "$*"
}

cleanup() {
    for p in "${PROCS[@]:-}"; do kill "$p" 2> /dev/null; done
}
trap cleanup EXIT

build() {
    say "[build] tollgate + fakeupstream"
    go build -o "$BIN/tollgate" . || {
        say "build tollgate failed"
        exit 1
    }
    go build -o "$BIN/fakeupstream" ./test/fakeupstream || {
        say "build fakeupstream failed"
        exit 1
    }
}

wait_code() { # <port> <expected http code for /v1/models>
    local port=$1 want=$2 got=""
    for _ in $(seq 1 100); do
        got=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/v1/models" 2> /dev/null)
        [ "$got" = "$want" ] && return 0
        sleep 0.05
    done
    return 1
}

start_fake() { # <port> <delay ms>
    "$BIN/fakeupstream" -addr "127.0.0.1:$1" -delay "$2" > "$TMP/fake-$1.log" 2>&1 &
    PROCS+=("$!")
    if wait_code "$1" 200; then pass "fake upstream up on $1"; else fail "fake upstream not ready on $1"; fi
}

start_proxy() { # <port> <config>
    "$BIN/tollgate" -config "$2" > "$TMP/proxy-$1.log" 2>&1 &
    PROCS+=("$!")
    if wait_code "$1" 200; then pass "proxy up on $1"; else fail "proxy not ready on $1"; fi
}

# start_slow: 起一个"睡够再回 200"的假上游 (多线程, 能同时接住多笔), 用来把并发槽位占住。
# 参数: 端口 睡眠秒数 计数文件
start_slow() {
    cat > "$TMP/srvslow.py" << 'PYSRV'
import sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
body = b'{"model":"deepseek-v4.1-flash","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}'
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = 0
        try:
            n = int(open(sys.argv[3]).read())
        except Exception:
            pass
        open(sys.argv[3], 'w').write(str(n + 1))
        time.sleep(float(sys.argv[2]))
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
ThreadingHTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PYSRV
    python3 "$TMP/srvslow.py" "$1" "$2" "$3" > /dev/null 2>&1 &
    PROCS+=("$!")
    for _ in $(seq 1 100); do
        if (exec 3<> /dev/tcp/127.0.0.1/"$1") 2> /dev/null; then
            exec 3>&- 3<&- 2> /dev/null || true
            return 0
        fi
        sleep 0.05
    done
    return 1
}

# gen_config1: 单上游 v2 配置。参数: 输出文件 监听端口 日志 假上游端口 上游id
#   include_usage interval_ms sem_wait_ms queue_wait_ms 逻辑模型名 [gate 里的额外字段]
# 门控是全局的 (设计 §5.5.4): 节拍与等待都在这里给, 锁时长与退避序列取测试用的小值。
gen_config1() {
    local extra="${11:-}" gate
    gate="\"interval_ms\": ${7}, \"sem_wait_ms\": ${8}, \"queue_wait_ms\": ${9}"
    gate="$gate, \"concurrency_lock_ms\": 800, \"quota_lock_ms\": 2000, \"breaker_lock_ms\": 3000"
    gate="$gate, \"retry_backoff_ms\": [10, 20, 30]"
    [ -n "$extra" ] && gate="$gate, $extra"
    cat > "$1" << EOF
{
  "listen": "127.0.0.1:$2",
  "log_path": "$3",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { $gate },
  "upstreams": [
    { "id": "$5", "base_url": "http://127.0.0.1:$4/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": $6 }
  ],
  "aliases": { "${10}": [["$5", "deepseek-v4.1-flash"]] }
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
    gen_config1 "$TMP/cfg-alias.json" 18800 "$LOG" 19001 fu true 20 400 400 test-model
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
    cat > "$CFG" << EOF
{
  "listen": "127.0.0.1:18812",
  "log_path": "$TMP/log-keyenv.jsonl",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 400, "queue_wait_ms": 400, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "fu", "base_url": "http://127.0.0.1:19012/v1", "auth_header": "Authorization", "key_env": "TG_TEST_KEY", "headers": {}, "include_usage": true }
  ],
  "aliases": { "test-model": [["fu", "deepseek-v4.1-flash"]] }
}
EOF
    if env -u TG_TEST_KEY "$BIN/tollgate" -config "$CFG" -check > /dev/null 2>&1; then
        fail "缺 env 时应报错 (TG_TEST_KEY)"
    else
        pass "缺 env -> -check 报错"
    fi
    if TG_TEST_KEY=secret "$BIN/tollgate" -config "$CFG" -check > /dev/null 2>&1; then
        pass "有 env -> -check 通过"
    else
        fail "有 env -> -check 失败"
    fi
}

case_fallback() {
    say "=== fallback (候选依序, 失败切下一个) ==="
    local LOG="$TMP/log-fb.jsonl"
    rm -f "$LOG"
    cat > "$TMP/cfg-fb.json" << EOF
{
  "listen": "127.0.0.1:18813",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 400, "queue_wait_ms": 400, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 5000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "dead", "base_url": "http://127.0.0.1:19999/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true },
    { "id": "fu", "base_url": "http://127.0.0.1:19013/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true }
  ],
  "aliases": {
    "test-model": [["dead", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]],
    "onlydead-model": [["dead", "deepseek-v4.1-flash"]]
  }
}
EOF
    start_fake 19013 20
    start_proxy 18813 "$TMP/cfg-fb.json"

    # dead 上游端口无人监听: 传输失败按通用兜底退避重试同一候选 (设计 §5.5.1 e2),
    # 第 4 次进熔断锁 5s, 整条请求于是落到下一个候选 fu。
    local code fb
    code=$(post 18813 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "dead->fu 切换后 200" || fail "fallback -> $code"
    fb=$(jq -r 'select(.upstream=="fu" and .status==200) | .fallbacks' "$LOG" | tail -1)
    [ "${fb:-0}" -ge 1 ] && pass "日志记 upstream=fu, fallbacks=$fb" || fail "fallback 日志不对"

    # 唯一候选在锁里 / 排队超时: 请求根本没发出去, 合成 429 说明原因 (不是 502)。
    code=$(post 18813 '{"model":"onlydead-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "429" ] && pass "候选全被锁 -> 合成 429" || fail "allfail -> $code (want 429)"
    jq -e '.error.code == "upstream_locked"' "$TMP/resp.txt" > /dev/null 2>&1 &&
        pass "429 error.code=upstream_locked" || fail "429 error 体不对"
    jq -r 'select(.status==429) | .lock_skips' "$LOG" | tail -1 | grep -q '^[1-9]' &&
        pass "日志记 lock_skips" || fail "lock_skips 未记"
}

# fallback 换到另一个上游/模型后, 新候选要重新排它自己的队 (F4), 而不是拿前一个
# 候选的槽位直接发; 同时验证前一个候选的槽位已归还 (不然两个上游会互等成死锁)。
# v2 闸门: 拿不到发送资格就换下一个候选; 候选链试完仍拿不到 -> 合成 429 (F3/F4 的 v2 形态)。
# v2 闸门: 拿不到发送资格就换下一个候选; 候选链试完仍拿不到 -> 合成 429 (F3/F4 的 v2 形态)。
case_fallback_gate() {
    say "=== 闸门: 被锁的候选换下一个 + 并发满时的超发试探与挡下 ==="
    local LOG="$TMP/log-fbg.jsonl"
    rm -f "$LOG" "$TMP/fbg-slow-count.txt"
    cat > "$TMP/cfg-fbg.json" << EOF
{
  "listen": "127.0.0.1:18816",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 150, "queue_wait_ms": 150, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 5000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "dead", "base_url": "http://127.0.0.1:19999/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true },
    { "id": "fu", "base_url": "http://127.0.0.1:19016/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true },
    { "id": "slow", "base_url": "http://127.0.0.1:19996/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true }
  ],
  "aliases": {
    "fbg-model": [["dead", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]],
    "slow-model": [["slow", "deepseek-v4.1-flash"]]
  }
}
EOF
    start_fake 19016 20
    start_slow 19996 2 "$TMP/fbg-slow-count.txt" || fail "慢上游未就绪"
    start_proxy 18816 "$TMP/cfg-fbg.json"

    # A: dead 一直传输失败 -> 退避重试到熔断, 候选被锁后请求落到 fu。
    local code up
    code=$(post 18816 '{"model":"fbg-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "被锁的候选换下一个 -> 200" || fail "fallback -> $code"
    up=$(jq -r 'select(.status==200) | .upstream' "$LOG" | tail -1)
    [ "$up" = "fu" ] && pass "最终由 fu 服务" || fail "落到了 $up"

    # B: 并发上限起点 1: 一笔占槽位 + 一笔超发试探都在飞, 第三笔等信号量超时被挡下。
    local BODY='{"model":"slow-model","messages":[{"role":"user","content":"hi"}]}'
    local pids=() i
    for i in 1 2; do
        (curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:18816/v1/chat/completions" \
            -H 'Content-Type: application/json' -d "$BODY" > "$TMP/fbg-b$i.code") &
        pids+=("$!")
        [ "$i" = 1 ] && sleep 0.15
    done
    sleep 0.15
    local code3 gate
    code3=$(post 18816 "$BODY")
    [ "$code3" = "429" ] && pass "第三笔被闸门挡下 -> 合成 429" || fail "被挡下 -> $code3 (want 429)"
    gate=$(jq -r 'select(.status==429) | .gate' "$LOG" | tail -1)
    [ "$gate" = "sem_timeout" ] && pass "429 日志 gate=sem_timeout" || fail "gate=$gate (want sem_timeout)"
    for i in "${pids[@]}"; do wait "$i" 2> /dev/null; done
    [ "$(cat "$TMP/fbg-b1.code")" = "200" ] && [ "$(cat "$TMP/fbg-b2.code")" = "200" ] &&
        pass "占槽位与超发试探两笔都 200" || fail "两笔并发 -> $(cat "$TMP/fbg-b1.code")/$(cat "$TMP/fbg-b2.code")"
    [ "$(cat "$TMP/fbg-slow-count.txt")" = "2" ] &&
        pass "被挡下的那笔没有发到上游 (上游只收到 2 笔)" || fail "上游收到 $(cat "$TMP/fbg-slow-count.txt") 笔"
}

case_breaker() {
    say "=== breaker (连续失败退避 -> 熔断锁 -> 到期恢复) ==="
    local LOG="$TMP/log-br.jsonl"
    rm -f "$LOG" "$TMP/br-count.txt"
    cat > "$TMP/srvflaky.py" << 'PYSRV'
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
PYSRV
    python3 "$TMP/srvflaky.py" 19991 4 "$TMP/br-count.txt" > /dev/null 2>&1 &
    FL=$!
    PROCS+=("$FL")
    for _ in $(seq 1 100); do
        if (exec 3<> /dev/tcp/127.0.0.1/19991) 2> /dev/null; then
            exec 3>&- 3<&- 2> /dev/null || true
            break
        fi
        sleep 0.05
    done
    gen_config1 "$TMP/cfg-br.json" 18810 "$LOG" 19991 flaky true 20 200 200 br-model
    start_proxy 18810 "$TMP/cfg-br.json"

    # 一笔请求: 前 4 次都 503 (每次失败后退避重试同一候选), 第 4 次进熔断。
    local code
    code=$(post 18810 '{"model":"br-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "503" ] && pass "熔断前透传最后一次 503" || fail "code=$code (want 503)"
    [ "$(cat "$TMP/br-count.txt")" = "4" ] && pass "退避重试到第 4 次 (上游计数 4)" || fail "计数=$(cat "$TMP/br-count.txt") (want 4)"
    jq -r 'select(.status==503) | .fallbacks' "$LOG" | tail -1 | grep -q '^[1-9]' &&
        pass "日志记 fallbacks (连续错误计数进日志)" || fail "fallbacks 未记"

    # 熔断锁内: 不再真发, 合成 429。
    code=$(post 18810 '{"model":"br-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "429" ] && pass "熔断锁内 -> 429 (不真发)" || fail "锁内 -> $code"
    [ "$(cat "$TMP/br-count.txt")" = "4" ] && pass "锁内未再调用上游" || fail "锁内又发了请求"

    # 锁到期后恢复 (上游第 5 次开始成功)。
    sleep 3.2
    code=$(post 18810 '{"model":"br-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "锁到期后恢复 200" || fail "恢复后 -> $code"
}

# 额度限制 (设计 §5.5.2): 每个上游自己配「状态码 + body 关键词」规则, 命中就锁该候选
# 并把排队请求迁移到下一个候选; 锁落盘, 重启仍生效。
case_quota() {
    say "=== quota (错误分类规则 -> 锁候选 + 排队迁移) ==="
    local D="$TMP/quota"
    local LOG="$D/log.jsonl"
    rm -rf "$D"
    mkdir -p "$D"
    rm -f "$TMP/q-count.txt"
    cat > "$TMP/srvquota2.py" << 'PYSRV'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
body = b'{"error":{"message":"insufficient balance","type":"quota_exceeded"}}'
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = 0
        try:
            n = int(open(sys.argv[2]).read())
        except Exception:
            pass
        open(sys.argv[2], 'w').write(str(n + 1))
        self.send_response(429)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PYSRV
    python3 "$TMP/srvquota2.py" 19994 "$TMP/q-count.txt" > /dev/null 2>&1 &
    QL=$!
    PROCS+=("$QL")
    for _ in $(seq 1 100); do
        if (exec 3<> /dev/tcp/127.0.0.1/19994) 2> /dev/null; then
            exec 3>&- 3<&- 2> /dev/null || true
            break
        fi
        sleep 0.05
    done
    cat > "$TMP/cfg-quota.json" << EOF
{
  "listen": "127.0.0.1:18814",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 400, "queue_wait_ms": 400, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "quota", "base_url": "http://127.0.0.1:19994/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true,
      "errors": { "quota": [ { "status": [402, 429], "contains": ["insufficient balance"] } ] } },
    { "id": "fu", "base_url": "http://127.0.0.1:19014/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true }
  ],
  "aliases": { "q-model": [["quota", "deepseek-v4.1-flash"], ["fu", "deepseek-v4.1-flash"]] }
}
EOF
    start_fake 19014 20
    start_proxy 18814 "$TMP/cfg-quota.json"

    # 第一笔: quota 上游回 429 且关键词命中 -> 锁 2s + 排队迁移, 这一笔改由 fu 服务。
    local code up
    code=$(post 18814 '{"model":"q-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "额度限制命中后迁移到下一个候选 -> 200" || fail "迁移后 -> $code"
    up=$(jq -r 'select(.status==200) | .upstream' "$LOG" | tail -1)
    [ "$up" = "fu" ] && pass "最终由 fu 服务" || fail "落到了 $up"
    [ "$(cat "$TMP/q-count.txt")" = "1" ] && pass "quota 上游只被调用 1 次 (没退避重试)" || fail "quota 计数=$(cat "$TMP/q-count.txt")"
    jq -e '.locks.quota.reason == "quota"' "$D/gate-state.json" > /dev/null 2>&1 &&
        pass "锁状态落盘 (gate-state.json reason=quota)" || fail "锁未落盘"

    # 第二笔: quota 在锁内 -> 直接跳过, 不再真发。
    code=$(post 18814 '{"model":"q-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "锁内仍由 fu 服务 200" || fail "第二笔 -> $code"
    [ "$(cat "$TMP/q-count.txt")" = "1" ] && pass "锁内未再调用 quota 上游" || fail "锁内又发了"
    jq -r 'select(.status==200) | .lock_skips' "$LOG" | tail -1 | grep -q '^[1-9]' &&
        pass "日志记 lock_skips" || fail "lock_skips 未记"
}

case_rotate() {
    say "=== rotate (按本地日历天轮转 + 归档参与汇总) ==="
    local D="$TMP/rotate"
    local LOG="$D/log.jsonl"
    local DAY DECOY OLD
    DAY=$(date -d 'yesterday' +%F)
    DECOY="$D/log-$DAY.jsonl"
    OLD="$D/log-$DAY-1.jsonl"
    rm -rf "$D"
    mkdir -p "$D"
    # 既有归档 (同名) 不能被覆盖; 上一段日志应落到 -1 序号。
    echo '{"marker":"decoy"}' > "$DECOY"
    # 昨天那段日志: 50 token 记在 limited 上游 (汇总要跨归档读到它)
    printf '{"ts":"%s","logical":"only","upstream":"limited","model":"deepseek-v4.1-flash","usage":{"prompt":20,"completion":30,"total":50},"missing_usage":false}\n' \
        "$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())")" > "$LOG"
    # mtime 拨到昨天 23:50: 模拟"昨天写的日志, 今天启动代理"。
    touch -d 'yesterday 23:50' "$LOG"
    # 一直 429 的上游 (没有配错误分类规则 -> 走通用兜底退避)。
    rm -f "$TMP/rot-count.txt"
    cat > "$TMP/srv429.py" << 'PYSRV'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
body = b'{"error":{"message":"rate limited","type":"rate_limit_error"}}'
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = 0
        try:
            n = int(open(sys.argv[2]).read())
        except Exception:
            pass
        open(sys.argv[2], 'w').write(str(n + 1))
        self.send_response(429)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PYSRV
    python3 "$TMP/srv429.py" 19995 "$TMP/rot-count.txt" > /dev/null 2>&1 &
    QL=$!
    PROCS+=("$QL")
    for _ in $(seq 1 100); do
        if (exec 3<> /dev/tcp/127.0.0.1/19995) 2> /dev/null; then
            exec 3>&- 3<&- 2> /dev/null || true
            break
        fi
        sleep 0.05
    done
    cat > "$TMP/cfg-rotate.json" << EOF
{
  "listen": "127.0.0.1:18840",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 200, "queue_wait_ms": 200, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "limited", "base_url": "http://127.0.0.1:19995/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true }
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

    # 归档里的那笔用量能被 -summary 跨归档读到 (预算已不在服务里, 服务只写日志)。
    "$BIN/tollgate" -summary "$D" > "$TMP/rot-summary.txt" 2>&1
    if grep -q 'only' "$TMP/rot-summary.txt" && grep -q '50' "$TMP/rot-summary.txt"; then
        pass "归档参与汇总 (跨归档读到 50 token)"
    else
        fail "归档未计入汇总"
    fi

    # v2: 上游 429 走通用兜底 -> 退避重试到第 4 次熔断, 该笔透传 429。
    local code
    code=$(curl -s -o "$TMP/resp.txt" -w '%{http_code}' \
        -X POST "http://127.0.0.1:18840/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' \
        -d '{"model":"only","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "429" ] && pass "唯一候选 429 原样透传" || fail "429 透传 -> $code"
    [ "$(cat "$TMP/rot-count.txt")" = "4" ] && pass "退避重试到第 4 次后熔断 (计数 4)" || fail "计数=$(cat "$TMP/rot-count.txt")"
    jq -e '.locks.limited.reason == "breaker"' "$D/gate-state.json" > /dev/null 2>&1 &&
        pass "熔断锁落盘 (gate-state.json)" || fail "熔断锁未落盘"
}

case_stream() {
    say "=== stream (SSE 逐块透传 + 工具调用/思考块 + F6) ==="
    local LOG="$TMP/log-stream.jsonl"
    rm -f "$LOG"
    gen_config1 "$TMP/cfg-stream.json" 18802 "$LOG" 19002 fu true 20 400 400 test-model
    start_fake 19002 150
    start_proxy 18802 "$TMP/cfg-stream.json"

    local BODY='{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}'
    local meta st tt code body
    meta=$(post_stream 18802 "$BODY")
    read -r st tt code <<< "$meta"
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
    cat > "$TMP/cfg-usage.json" << EOF
{
  "listen": "127.0.0.1:18803",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 400, "queue_wait_ms": 400, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "fu", "base_url": "http://127.0.0.1:19003/v1", "auth_header": "Authorization", "key": "k1", "headers": {}, "include_usage": true },
    { "id": "fnu", "base_url": "http://127.0.0.1:19003/v1", "auth_header": "Authorization", "key": "k2", "headers": {}, "include_usage": false }
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
    post_stream 18803 '{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}' > /dev/null
    local line pu cu tu
    line=$(grep '"usage"' "$TMP/resp.txt" | tail -1)
    pu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.prompt_tokens')
    cu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.completion_tokens')
    tu=$(printf '%s' "$line" | sed 's/^data: //' | jq -r '.usage.total_tokens')
    [ "$pu" = "7" ] && [ "$cu" = "42" ] && [ "$tu" = "49" ] &&
        pass "流式 usage 提取 7/42/49" || fail "流式 usage=$pu/$cu/$tu (want 7/42/49)"
    grep -q '"usage":{"prompt":7,"completion":42,"total":49}' "$LOG" &&
        pass "日志 usage 与上游一致" || fail "日志 usage 不一致"

    # 2) 流式走 include_usage=false 的上游 -> 无 usage, F7 字符兜底:
    #    "Hello " 和 "world!" 按块各估 (6+3)/4=2, 合计 4
    post_stream 18803 '{"model":"no-usage-model","messages":[{"role":"user","content":"hi"}],"stream":true}' > /dev/null
    if grep -q '"usage"' "$TMP/resp.txt"; then
        fail "no-usage 上游不应有 usage chunk"
    else
        pass "no-usage 上游无 usage chunk"
    fi
    grep '"upstream":"fnu"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":0,"completion":4,"total":4}' &&
        pass "F7 兜底估算 completion=4" || fail "F7 兜底估算缺失"
    grep '"upstream":"fnu"' "$LOG" | tail -1 | grep -q '"missing_usage":true' &&
        pass "missing_usage 计数" || fail "missing_usage 未记"

    # 3) 非流式 -> 响应体 usage + 日志一致
    post 18803 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}' > /dev/null
    tu=$(jq -r '.usage.total_tokens // empty' "$TMP/resp.txt")
    [ "$tu" = "49" ] && pass "非流式 usage 提取 total=49" || fail "非流式 usage=$tu"
    grep '"upstream":"fu"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":7,"completion":42,"total":49}' &&
        pass "非流式日志 usage 一致" || fail "非流式日志 usage 不一致"
    # O9: 非流响应 model 改回逻辑名。
    m=$(jq -r '.model' "$TMP/resp.txt")
    [ "$m" = "test-model" ] && pass "非流响应 model 改回逻辑名" || fail "非流响应 model=$m (want test-model)"

    # 4) 缓存命中 token: 上游给 prompt_tokens_details.cached_tokens 时记入日志 (系数标定要用)。
    curl -s -o /dev/null -X POST "http://127.0.0.1:18803/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Fake-Cached: 5' \
        -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
    grep '"upstream":"fu"' "$LOG" | tail -1 | grep -q '"cached":5' &&
        pass "缓存命中 token 记入日志 (cached=5)" || fail "cached 未记入日志"
}

case_usage_report() {
    say "=== -usage (上游/模型 × 窗口 token 分解, 标定系数用) ==="
    local LOG="$TMP/log-usagecmd.jsonl"
    rm -f "$LOG" "$TMP/log-usagecmd-"*.jsonl
    local now old
    now=$(python3 -c "import datetime;print(datetime.datetime.now(datetime.timezone.utc).isoformat())")
    old=$(python3 -c "import datetime;print((datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(hours=8)).isoformat())")
    printf '{"ts":"%s","logical":"a","upstream":"ustc","model":"deepseek-flash","usage":{"prompt":1000,"completion":2000,"total":3000,"cached":500},"missing_usage":false}\n' "$now" >> "$LOG"
    printf '{"ts":"%s","logical":"b","upstream":"ustc","model":"qwen-chat","usage":{"prompt":10,"completion":20,"total":30},"missing_usage":false}\n' "$now" >> "$LOG"
    printf '{"ts":"%s","logical":"a","upstream":"ustc","model":"deepseek-flash","usage":{"prompt":7,"completion":3,"total":10},"missing_usage":false}\n' "$old" >> "$LOG"

    "$BIN/tollgate" -usage "$LOG" -windows 3h > "$TMP/usage3h.txt" 2>&1
    "$BIN/tollgate" -usage "$LOG" > "$TMP/usageall.txt" 2>&1

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
    say "=== ratelimit (出队节拍: 至少 interval_ms 才发下一笔) ==="
    local LOG="$TMP/log-rl.jsonl"
    rm -f "$LOG"
    # 节拍 600ms, 信号量给足: 卡住的只可能是节拍。
    gen_config1 "$TMP/cfg-rl.json" 18804 "$LOG" 19004 rl true 600 2000 2000 rl-model
    start_fake 19004 20
    start_proxy 18804 "$TMP/cfg-rl.json"

    local BODY='{"model":"rl-model","messages":[{"role":"user","content":"hi"}]}'
    post 18804 "$BODY" > /dev/null # A: 立刻可发 (队列空闲)
    local code tt
    tt=$(curl -s -o /dev/null -w '%{time_total}' -X POST "http://127.0.0.1:18804/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$BODY")
    code=$?
    [ "$code" = "0" ] && pass "紧接着的第二笔成功" || fail "第二笔 -> $code"
    if python3 -c "import sys; sys.exit(0 if float('$tt') >= 0.55 else 1)"; then
        pass "出队节拍生效 (第二笔等了 ${tt}s >= 0.55s)"
    else
        fail "节拍未生效 ($tt)"
    fi
    grep '"upstream":"rl"' "$LOG" | grep -q '"queued":true' &&
        pass "等待记账 queued:true" || fail "无等待记录"
    local qms
    qms=$(jq -r 'select(.upstream=="rl") | .queue_ms' "$LOG" | tail -1)
    python3 -c "import sys; sys.exit(0 if ${qms:-0} >= 400 else 1)" &&
        pass "queue_ms=${qms}ms (记下等节拍的时长)" || fail "queue_ms=$qms 未记"
}

case_ratelimit_429() {
    say "=== ratelimit 429 (并发满 + 试探在飞 -> 合成 429, 不再真发) ==="
    local D="$TMP/rl429"
    local LOG="$D/log.jsonl"
    rm -rf "$D"
    mkdir -p "$D"
    rm -f "$TMP/rl429-count.txt"
    cat > "$TMP/cfg-rl429.json" << EOF
{
  "listen": "127.0.0.1:18805",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 150, "queue_wait_ms": 150, "concurrency_lock_ms": 2000, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "upstreams": [
    { "id": "slow", "base_url": "http://127.0.0.1:19997/v1", "auth_header": "Authorization", "key": "k", "headers": {}, "include_usage": true }
  ],
  "aliases": { "rl-model": [["slow", "deepseek-v4.1-flash"]] }
}
EOF
    start_slow 19997 2 "$TMP/rl429-count.txt" || fail "慢上游未就绪"
    start_proxy 18805 "$TMP/cfg-rl429.json"

    local BODY='{"model":"rl-model","messages":[{"role":"user","content":"hi"}]}'
    local pids=() i
    for i in 1 2; do
        (curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:18805/v1/chat/completions" \
            -H 'Content-Type: application/json' -d "$BODY" > "$TMP/rl429-b$i.code") &
        pids+=("$!")
        [ "$i" = 1 ] && sleep 0.15
    done
    sleep 0.15
    local code retry gate
    code=$(curl -s -D "$TMP/hdr.txt" -o "$TMP/resp.txt" -w '%{http_code}' \
        -X POST "http://127.0.0.1:18805/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -d "$BODY")
    retry=$(grep -i '^retry-after:' "$TMP/hdr.txt" | tr -d '\r' | awk '{print $2}')
    [ "$code" = "429" ] && pass "第三笔被挡下 -> 429" || fail "code=$code (want 429)"
    if [ -n "$retry" ] && python3 -c "import sys; sys.exit(0 if int('$retry') >= 1 else 1)" 2> /dev/null; then
        pass "Retry-After: $retry"
    else
        fail "Retry-After 缺失/非法 ($retry)"
    fi
    jq -e '.error.code == "rate_limit_error"' "$TMP/resp.txt" > /dev/null 2>&1 &&
        pass "error.code=rate_limit_error" || fail "429 error 体不对"
    jq -r '.error.message' "$TMP/resp.txt" | grep -q 'no candidate' &&
        pass "429 正文说明原因" || fail "429 正文没写原因"
    gate=$(jq -r 'select(.status==429) | .gate' "$LOG" | tail -1)
    [ "$gate" = "sem_timeout" ] && pass "429 日志 gate=sem_timeout" || fail "gate=$gate (want sem_timeout)"
    for i in "${pids[@]}"; do wait "$i" 2> /dev/null; done
    [ "$(cat "$TMP/rl429-b1.code")" = "200" ] && [ "$(cat "$TMP/rl429-b2.code")" = "200" ] &&
        pass "占槽位与超发试探两笔都 200" || fail "两笔并发 -> $(cat "$TMP/rl429-b1.code")/$(cat "$TMP/rl429-b2.code")"
    [ "$(cat "$TMP/rl429-count.txt")" = "2" ] &&
        pass "被挡下的那笔没有发到上游 (上游只收到 2 笔)" || fail "上游收到 $(cat "$TMP/rl429-count.txt") 笔"
}

case_logging() {
    say "=== logging (JSONL 逐笔明细 + 模型名改写) ==="
    local LOG="$TMP/log-logging.jsonl"
    rm -f "$LOG"
    gen_config1 "$TMP/cfg-logging.json" 18806 "$LOG" 19006 fu true 20 400 400 test-model
    start_fake 19006 20
    start_proxy 18806 "$TMP/cfg-logging.json"

    post_stream 18806 '{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}' > /dev/null
    post 18806 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}' > /dev/null
    local n=0 bad=0
    while IFS= read -r line; do
        n=$((n + 1))
        if ! printf '%s' "$line" | jq -e \
            'has("logical") and has("upstream") and has("model") and has("usage") and has("missing_usage") and has("queued") and has("status") and .logical=="test-model" and .model=="deepseek-v4.1-flash" and .upstream=="fu"' \
            > /dev/null 2>&1; then
            bad=$((bad + 1))
        fi
    done < "$LOG"
    [ "$n" -ge 2 ] && [ "$bad" = 0 ] &&
        pass "日志 $n 行, 字段齐全, 模型名已改写为真实 id" || fail "日志校验: n=$n bad=$bad"

    # O3: source 头写入日志。
    curl -s -o /dev/null -X POST "http://127.0.0.1:18806/v1/chat/completions" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Tollgate-Source: agent-x' \
        -d '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
    grep '"source":"agent-x"' "$LOG" | tail -1 | grep -q '"logical":"test-model"' &&
        pass "日志记 source=agent-x (O3)" || fail "source 未记入日志"

    # O2: -summary 聚合, TOTAL 请求数 = 日志行数。
    "$BIN/tollgate" -summary "$LOG" > "$TMP/summary.txt" 2>&1
    grep -F 'test-model / agent-x' "$TMP/summary.txt" > /dev/null 2>&1 &&
        pass "-summary 含 test-model / agent-x 分组行 (O2)" || fail "-summary 缺分组行"
    n=$(wc -l < "$LOG")
    m=$(awk '$1=="TOTAL"{print $2}' "$TMP/summary.txt")
    [ "$n" = "$m" ] && pass "-summary TOTAL=$m 与日志行数 $n 一致" || fail "-summary TOTAL=$m, 日志行数 $n"
}

case_overhead() {
    say "=== overhead (代理首字节延迟增量) ==="
    local LOG="$TMP/log-oh.jsonl"
    rm -f "$LOG"
    # 节拍调到 1ms: 这里量的是代理自身开销, 不该被出队节拍盖住。
    gen_config1 "$TMP/cfg-oh.json" 18807 "$LOG" 19007 fu true 1 2000 2000 test-model
    start_fake 19007 20
    start_proxy 18807 "$TMP/cfg-oh.json"

    local BODY='{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
    local i st d p delta
    : > "$TMP/direct.txt"
    : > "$TMP/proxied.txt"
    for i in $(seq 1 15); do
        st=$(curl -s -o /dev/null -w '%{time_starttransfer}' -X POST "http://127.0.0.1:19007/v1/chat/completions" \
            -H 'Content-Type: application/json' -H 'Authorization: Bearer x' -d "$BODY")
        echo "$st" >> "$TMP/direct.txt"
        st=$(curl -s -o /dev/null -w '%{time_starttransfer}' -X POST "http://127.0.0.1:18807/v1/chat/completions" \
            -H 'Content-Type: application/json' -H 'Authorization: Bearer x' -d "$BODY")
        echo "$st" >> "$TMP/proxied.txt"
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
    say "=== hup (SIGHUP 重载: 别名生效 + 门控参数热加载) ==="
    local LOG="$TMP/log-hup.jsonl" CFG="$TMP/cfg-hup.json" PLOG="$TMP/proxy-18809.log"
    rm -f "$LOG" "$PLOG"
    gen_config1 "$CFG" 18809 "$LOG" 19009 fu true 20 2000 2000 test-model
    start_fake 19009 20
    start_proxy 18809 "$CFG"
    local pid="${PROCS[${#PROCS[@]} - 1]}"

    kill -HUP "$pid"
    sleep 0.4
    grep -q 'config reloaded' "$PLOG" && pass "SIGHUP 触发配置重载" || fail "未触发重载"
    local code
    code=$(post 18809 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}')
    [ "$code" = "200" ] && pass "重载后仍正常服务 200" || fail "重载后 -> $code"

    # 改门控节拍 (20ms -> 600ms) 后热加载: 紧接着的第二笔必须等节拍, 日志 queue_ms 能看出来。
    gen_config1 "$CFG" 18809 "$LOG" 19009 fu true 600 2000 2000 test-model
    kill -HUP "$pid"
    sleep 0.4
    post 18809 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}' > /dev/null
    code=$(post 18809 '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}')
    local qms
    qms=$(jq -r 'select(.upstream=="fu") | .queue_ms' "$LOG" | tail -1)
    [ "$code" = "200" ] && python3 -c "import sys; sys.exit(0 if ${qms:-0} >= 400 else 1)" &&
        pass "门控节拍热加载生效 (queue_ms=${qms}ms >= 400)" || fail "节拍未热加载 (code=$code queue_ms=$qms)"
}

# ---------- 分发 ----------

case_responses() {
    say "=== responses (/v1/responses: 转发 + usage 口径归一 + 流终态 + model 改写) ==="
    local LOG="$TMP/log-responses.jsonl"
    rm -f "$LOG"
    gen_config1 "$TMP/cfg-responses.json" 18808 "$LOG" 19008 fu true 20 400 400 test-model
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
    grep -q '"usage":{"prompt":7,"completion":42,"total":49}' "$LOG" &&
        pass "input/output 口径归一 (7/42/49)" || fail "responses usage 未归一"

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
    grep -q '"model":"test-model"' "$TMP/resp.txt" &&
        pass "事件里嵌套的 response.model 改回逻辑名" || fail "事件 model 未改写"
    grep -q 'deepseek-v4.1-flash' "$TMP/resp.txt" &&
        fail "响应泄漏真实上游模型 id" || pass "响应不含真实上游 id (O9)"
    grep '"path":"/v1/responses"' "$LOG" | tail -1 | grep -q '"usage":{"prompt":7,"completion":42,"total":49}' &&
        pass "流式 usage 从 response.completed 提取并记账" || fail "流式 responses usage 未记账"
    grep '"path":"/v1/responses"' "$LOG" | tail -1 | grep -q '"missing_usage":false' &&
        pass "missing_usage=false (未走 F7 兜底)" || fail "missing_usage 标记不对"

    # 3) 未注册模型仍走白名单 404。
    code=$(post_resp 18808 '{"model":"nope","input":"hi"}')
    [ "$code" = "404" ] && pass "未注册模型 -> 404" || fail "未注册模型 -> $code"

    # 4) 上游中途断流 (没有终态事件) -> 仍按 F6 补 error 事件。
    curl -s -N -o "$TMP/resp.txt" \
        -X POST "http://127.0.0.1:18808/v1/responses" \
        -H 'Content-Type: application/json' -H 'Authorization: Bearer dummy' -H 'X-Abort-After: 2' \
        -d '{"model":"test-model","input":"hi","stream":true}'
    grep -q 'event: error' "$TMP/resp.txt" &&
        pass "无终态事件时仍补 error 事件 (F6 未被削弱)" || fail "断流未补 error 事件"
}

case_probe() {
    say "=== probe (只观测不切换: 被动采样 + 只探 fallback 的下一个候选) ==="
    local DIR="$TMP/probe"
    local LOG="$DIR/log.jsonl" CFG="$DIR/cfg.json"
    rm -rf "$DIR"
    mkdir -p "$DIR"
    cat > "$CFG" << EOF
{
  "listen": "127.0.0.1:18821",
  "log_path": "$LOG",
  "timeouts": { "connect_ms": 1000, "response_header_ms": 5000, "stream_idle_ms": 3000 },
  "gate": { "interval_ms": 20, "sem_wait_ms": 400, "queue_wait_ms": 400, "concurrency_lock_ms": 800, "quota_lock_ms": 2000, "breaker_lock_ms": 3000, "retry_backoff_ms": [10, 20, 30] },
  "probe": { "enabled": true, "scope": "next", "interval_sec": 1, "max_tokens": 4,
             "timeout_ms": 5000, "window": 3, "targets": ["probe-model"] },
  "upstreams": [
    { "id": "fu1", "base_url": "http://127.0.0.1:19021/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": true },
    { "id": "fu2", "base_url": "http://127.0.0.1:19021/v1", "auth_header": "Authorization", "key": "fake-key", "headers": {}, "include_usage": true }
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
        [ "$(echo "$rep" | jq -r '.aliases["probe-model"][1].active // 0')" -ge 1 ] 2> /dev/null && break
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
    probed=$(jq -r 'select(.source=="probe") | .upstream' "$LOG" 2> /dev/null | sort -u | tr '\n' ' ')
    [ "$probed" = "fu2 " ] && pass "探测只打下一个候选 (日志 source=probe: $probed)" || fail "探测候选=$probed (want fu2)"

    # D14 不接入: 探测不改变候选顺序, 新请求仍走第一个候选
    post 18821 '{"model":"probe-model","messages":[{"role":"user","content":"hi"}]}' > /dev/null
    local lastup
    lastup=$(jq -r 'select(.source!="probe") | .upstream' "$LOG" 2> /dev/null | tail -1)
    [ "$lastup" = "fu1" ] && pass "顺序未被探测改变 (仍走 fu1)" || fail "落到了 $lastup"

    [ -s "$DIR/probe.json" ] && pass "状态文件 probe.json 已写" || fail "probe.json 缺失"
    "$BIN/tollgate" -probe "$CFG" 2> /dev/null | grep -q 'probe-model' && pass "-probe 离线报告可读" || fail "-probe 报告失败"
}

case_all() {
    case_alias
    case_keyenv
    case_fallback
    case_fallback_gate
    case_breaker
    case_quota
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
        alias) case_alias ;;
        keyenv) case_keyenv ;;
        fallback) case_fallback ;;
        fallback-gate) case_fallback_gate ;;
        breaker) case_breaker ;;
        quota) case_quota ;;
        rotate) case_rotate ;;
        stream) case_stream ;;
        usage) case_usage ;;
        usage-report) case_usage_report ;;
        ratelimit)
            case_ratelimit_min
            case_ratelimit_429
            ;;
        logging) case_logging ;;
        overhead) case_overhead ;;
        responses) case_responses ;;
        hup) case_hup ;;
        probe) case_probe ;;
        all) case_all ;;
        *)
            say "unknown case: $1"
            exit 2
            ;;
    esac
    say ""
    say "结果: $PASS 通过, $FAIL 失败"
    [ "$FAIL" = 0 ]
}

main "$@"
