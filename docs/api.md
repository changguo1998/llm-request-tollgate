# llm-request-tollgate HTTP API 参考

本机模型请求的统一闸口对外提供的 HTTP 接口清单. 代理对客户端暴露两个 OpenAI 兼容转发
端点 (客户端照常按 LLM 接入), 外加三个只读运维端点. 调度 / 限速 / 记账的内部语义见
[design.md](design.md) 与 [README](../README.md); 部署与日常运维见
[operations.md](operations.md).

## 1. 端点总览

| 端点 | 方法 | 用途 | 给谁用 |
|---|---|---|---|
| `/v1/chat/completions` | POST | 转发 chat completions (替换凭据 / 调度 / fallback / 记账) | 客户端, 即各 agent |
| `/v1/responses` | POST | 转发 Responses API, 同一链路 | 客户端, 即各 agent |
| `/v1/models` | GET | 逻辑模型名清单 (不暴露真实上游 id) | 客户端与运维 |
| `/v1/state` | GET | 只读观测面: 进行中的请求 + 候选调度状态 + 事件 | 运维 (TUI) |
| `/v1/probe` | GET | 只读延迟 / 吞吐报告 (只观测, 不改路由) | 运维 |

其余路径一律 404 `not_found`. base URL 是配置里的 `listen` (缺省
`127.0.0.1:8787`), 客户端 base URL 指到 `http://127.0.0.1:8787/v1`.

## 2. 公共约定

### 2.1 认证: 客户端凭据一律替换

以下请求头若客户端带来会被整体删除, 换成代理配置的凭据 (上游记录的 `auth_header` +
key):

`Authorization` / `Api-Key` / `X-Api-Key` / `api-key` / `X-Goog-Api-Key` /
`Proxy-Authorization`

- 上游 key 只存环境变量 (`key_env`), 代理启动 / 热加载时从环境取, 不落盘.
- 认证头里的 scheme: 缺省对 `Authorization` 用 `Bearer `, 其他头 (api-key 这类) 放裸
  key; 可用上游配置的 `key_prefix` 覆盖.
- 客户端发什么 key 都行, 代理不校验 (就没有"客户把自己的 key 打到上游"这回事).

### 2.2 请求头处理

- 客户端其余请求头原样拷贝到出站请求, 再删除 hop-by-hop 头与 `Accept-Encoding`
  (由 Go Transport 协商并透明解压).
- 不加 `X-Forwarded-For`.
- 响应头除 hop-by-hop 外原样透传.

### 2.3 请求体改写

- `model` 字段换成当前候选上游的真实模型 id.
- 流式且该上游 `include_usage=true` (chat completions) 时, 注入
  `stream_options.include_usage: true`, 强制上游回 usage.
- `/v1/responses` 没有 `stream_options`, 不注入.

### 2.4 响应整形

- 非流式响应体与流式每个 data chunk 里的 `model` 字段改回请求的逻辑名, 对外隐藏真实
  上游 id.
- `/v1/responses` 事件里嵌在 `response` 对象下的 `model` 同样改回逻辑名.
- `/v1/models` 只列逻辑名, 不带任何真实 id.

### 2.5 错误体格式

代理自己合成的错误统一为:

```json
{
  "error": { "message": "...", "type": "code", "code": "code" }
}
```

`type` 与 `code` 相同. 看 `code` 就能区分是哪一类 (见第 5 节).

### 2.6 来源标识

请求头 `X-Tollgate-Source` (任意字符串, 如 agent 名) 原样记入日志的 `source` 字段,
便于按逻辑名 + 来源聚合.

## 3. 转发端点

### 3.1 POST /v1/chat/completions

输入/输出与 OpenAI chat completions 兼容, 但:

- `model` 必须是白名单里的逻辑名, 否则 404 `model_not_found` (D2, 不猜上游).
- 流式 (SSE) 逐块原样透传不缓冲; 每块 `model` 改回逻辑名; 以 `[DONE]` 正常结束.
- 记账: 强写 `stream_options.include_usage`, 从流末 usage chunk / 非流式响应体提取
  token 用量 (含缓存命中 token, 四种上游字段名都认); 缺 usage 按字符粗估并记
  `missing_usage`.

### 3.2 POST /v1/responses

与 `/v1/chat/completions` 共用白名单 / 闸门 / fallback / 记账的完整链路, 差异:

- 请求体不注入 `stream_options`.
- 用量按 `input_tokens` / `output_tokens` 口径归一成 `prompt` / `completion` 记账.
- 流以 `response.completed` (以及上游自己报的 `response.failed` /
  `response.incomplete`) 判正常收尾, 不要求 `[DONE]` — 否则每条正常流都会被误补 error
  事件.
- 事件里嵌在 `response` 下的 `model` 改回逻辑名.

## 4. 路由与 fallback 语义 (客户端视角)

- 别名解析出候选链 (顺序即优先级), 按序逐个尝试.
- 每个候选走一遍闸门: 等并发信号量 (`sem_wait_ms`, 缺省 5s) -> 进上游流量队列 (出队
  节拍 `interval_ms`, 缺省 500ms) -> 进该模型队列 (只在熔断锁 / 退避时等) -> 发送.
- 换候选就重新给这几段等待, **没有整请求总预算**.
- 换错时机: 只在上游返回 429 / 5xx / 传输失败 (首字节前) 时才换下一个; 其余 4xx 不重
  试, 原样透传.
- 全部候选失败: 保留最近一次错误 (状态码 / 头 / 体) 原样透传; 如果请求根本没发出去
  (被锁 / 排队超时 / 信号量超时), 就合成 429 说明原因 (见 5.2).
- 锁的粒度: 额度锁整个上游 (`quota_lock_ms`, 缺省 10min, 额度是 key 级共享的); 熔断
  只锁出错的 `上游/模型` (`breaker_lock_ms`, 缺省 5min), 同家别的模型照常发送. 锁与
  退避落盘到日志同目录 `gate-state.json`, 重启仍生效.

## 5. 代理合成的错误

### 5.1 一览

| 状态码 | code | 场景 |
|---|---|---|
| 400 | `invalid_request_error` | 请求体读不出 / 不是合法 JSON / 缺 `model` |
| 404 | `not_found` | 不支持的路径或方法 |
| 404 | `model_not_found` | 未注册的逻辑名; 或该别名没有任何可用上游 |
| 405 | `method_not_allowed` | `/v1/models` `/v1/state` `/v1/probe` 非 GET |
| 429 | `rate_limit_error` | 闸门挡下, 请求没发出去 (带 `Retry-After`) |
| 429 | `upstream_locked` | 候选全在锁里 (上游限制的传导, 带 `Retry-After`) |
| 500 | `internal_error` | `/v1/state` 载荷编码失败 |
| 502 | `upstream_error` | 全部候选失败且没有可透传的 HTTP 状态 |

### 5.2 闸门挡下 (429 `rate_limit_error`)

候选链试完仍没拿到发送资格时合成, 带 `Retry-After`. 日志里 `gate` 字段区分原因:

| gate | 含义 |
|---|---|
| `locked` | 当前候选在锁里 (走完候选链全部被锁) |
| `sem_timeout` | 等并发信号量超时 (并发不够) |
| `queue_timeout` | 排队超时 |
| `backoff_timeout` | 退避等待超过排队预算 |
| `canceled` | 客户端中途断开 |

合成 429 时会带上 `Retry-After` (取等待预算或最长锁剩余时间), 让 agent 退避而不是立刻
重试.

### 5.3 上游错误透传

- 429 / 5xx: 换下一个候选重试; 全部失败时保留最近一次错误 (状态码 / 头 / 体) 原样透
  传 — 客户端看到的是上游自己的错误, 不加工.
- 其余 4xx (模型不存在 / 参数错 / 鉴权错): 不重试, 直接透传.
- `upstream_locked` (候选全在锁里, 且没有可透传的错误) 也带 `Retry-After`, 比 502 更
  贴近"上游限制"的真实原因.

### 5.4 流式 error 事件 (F6)

流未正常收尾时 (SSE 流在 `[DONE]` / 终态事件前结束), 代理发一个 error 事件后关流,
不伪造 `[DONE]`:

```
event: error
data: {"error":{"message":"upstream stream ended unexpectedly","type":"upstream_error"}}
```

## 6. 只读观测面

### 6.1 GET /v1/models

返回逻辑名清单 (排序), 不暴露真实上游与凭据:

```json
{ "object": "list", "data": [ { "id": "deepseek-v4.1-flash", "object": "model" } ] }
```

### 6.2 GET /v1/state

只读观测面: 进行中的请求 + 每个候选的调度状态 + 事件. 纯内存, 不落盘、不参与记账与
调度, 重启即清空; 消费方是 TUI, 也可给别的工具读.

- 查询参数 `since`: 只要序号大于它的新增事件, 返回里同时给 `max_seq`, 下次轮询带上.
- 载荷: `ver` (观测面版本), `listen`, `now`, `active` (进行中请求数组), `cands`
  (每上游一条: 队列长 / 在飞 / 并发上限 / 是否在试探 / 连续错误数 / 退避剩余 / 锁剩余
  / 锁原因 / 模型级熔断锁 / 距下次节拍), `events` (事件数组), `max_seq`, `slow_ms`.
- 事件 `kind`: `start` / `lock` / `unlock` / `fallback` / `gate` / `error` / `slow` /
  `ok` / `reload`.
- 其余方法返回 405.

### 6.3 GET /v1/probe

只读延迟 / 吞吐报告 (D14, 只观测不接入: 不改变候选顺序、不动失败计数与锁). 开启
`probe` 后代理按 `interval_sec` 主动探测 fallback 顺序里的**下一个**候选
(`scope=next`), 真实请求被动记 `ttfb_ms` (流式另记 `tps`).

载荷: `generated_at` / `standards` (`target_ttfb_ms` / `target_tps` /
`switch_margin`, 仅报告判定) / `probe` (当前探测配置) / `aliases`: 每别名按 fallback
顺序列出候选行, 每行 `role` (`primary` / `next` / `candidate`), 上游 / 模型,
`ttfb_ms` / `tps` (EWMA), `samples` / `ok` / `fail`, `active` / `passive` (主动 / 被
动样本数), `meets_targets`, `advantage` (相对主候选有明显优势, 仅建议).

状态落盘到日志同目录 `var/probe.json` (与锁状态同目录). 探测参数启动时固化, 改配置要
重启; 探测结果只进报告, 不接线路由.

## 7. JSONL 请求日志 (每行一笔)

`log_path` (缺省 `var/requests.jsonl`) 追加写, 按本地日历天轮转, 跨天归档为
`<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 `-1` / `-2`).

| 字段 | 含义 |
|---|---|
| `ts` | 结束时刻 (RFC3339Nano) |
| `logical` | 请求的逻辑模型名 |
| `upstream` | 实际服务的上游 id |
| `model` | 实际服务的真实模型 id |
| `path` | 转发端点 (`/v1/chat/completions` 或 `/v1/responses`) |
| `stream` | 是否流式 |
| `status` | 最终状态码 |
| `usage` | `{prompt, completion, total, cached?}` (归一口径; `cached` 是命中缓存输入 token) |
| `missing_usage` | true = 上游没回 usage, 走了字符粗估 |
| `queued` / `queue_ms` / `sem_ms` | 是否排过队 / 排队总时长 / 等信号量总时长 |
| `client` / `ua` / `source` | 来源 (remote addr / User-Agent / `X-Tollgate-Source`) |
| `fallbacks` | 换候选次数 |
| `err` | 错误说明 |
| `gate` | 被闸门挡下未发出时的原因 (见 5.2) |
| `lock_skips` / `backoff_skips` | 候选被锁 / 在退避而被跳过的次数 |
| `ttfb_ms` / `tps` | 首字节时延 / 解码吞吐 (流式才有 tps) |

明细事实来源是日志 (观测面重启即清空, 设计 D12); `-summary` 与 `-usage` 两个离线子命
令都能跨归档读, 见 [operations.md](operations.md) §7.
