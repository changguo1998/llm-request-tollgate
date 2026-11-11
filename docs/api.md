# llm-request-tollgate HTTP API 参考

本机模型请求的统一闸口对外提供的 HTTP 接口清单. 代理对客户端暴露两个 OpenAI 兼容转发
端点 (客户端照常按 LLM 接入), 外加运维端点: `/state` 只读, `/unlock` 与 `/sticky` 供
人工干预, 都不带 `/v1` 前缀. 调度 / 限速 / 记账的内部语义见
[design.md](design.md) 与 [README](../README.md); 部署与日常运维见
[operations.md](operations.md).

## 1. 端点总览

| 端点 | 方法 | 用途 | 给谁用 |
|---|---|---|---|
| `/v1/chat/completions` | POST | 转发 chat completions (替换凭据 / 调度 / fallback / 记账) | 客户端, 即各 agent |
| `/v1/responses` | POST | 转发 Responses API, 同一链路 | 客户端, 即各 agent |
| `/v1/models` | GET | 逻辑模型名清单 + 聚合后的模型参数 (不暴露真实上游 id) | 客户端与运维 |
| `/state` | GET | 只读观测面: 进行中的请求 + 候选调度状态 + 事件 | 运维 (TUI) |
| `/unlock` | POST | 调试用强制解锁: 清掉内存里的锁与失败计数 (query 可过滤 `upstream` / `model` / `group`) | 运维 (调试) |
| `/sticky` | POST | 人工改粘性偏好: query `logical` + `upstream` (+ `model`, 省略取该上游首个) 置一次; 不给 `upstream` 则清除 (`logical` 省略清全部) | 运维 |

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
- `/v1/models` 只列逻辑名 (附聚合后的模型参数), 不带任何真实 id.

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
  `missing_usage`. 粗估口径: 非 ASCII 1 token/字符, 其余 4 字符 1 token; 流式与
  非流式同一口径 (结构字段 `role` / `finish_reason` / `id` / `type` 等不计):
  - 文字: `content` / `reasoning_content` / `output[].content[].text` /
    `output[].summary[].text`, 以及 responses 里所有 `*.delta` 事件;
  - 工具调用: `tool_calls[].function.name` 与 `arguments`、`function_call_arguments.delta`;
  - 多模态: 文本伴随字段按字符计, base64 / data URI / URL 段每段记 1 token.
  纯思考模型与纯工具调用流的 `completion` 都不会为 0.

### 3.2 POST /v1/responses

与 `/v1/chat/completions` 共用白名单 / 闸门 / fallback / 记账的完整链路, 差异:

- 请求体不注入 `stream_options`.
- 用量按 `input_tokens` / `output_tokens` 口径归一成 `prompt` / `completion` 记账.
- 流以 `response.completed` (以及上游自己报的 `response.failed` /
  `response.incomplete`) 判正常收尾, 不要求 `[DONE]` — 否则每条正常流都会被误补 error
  事件.
- 事件里嵌在 `response` 下的 `model` 改回逻辑名.

## 4. 路由与 fallback 语义 (客户端视角)

- 白名单有两层: **别名** (逻辑名 -> 候选链) 与 **组** (`groups`, 组名 -> 已注册逻辑名
  列表, 别名的别名)。请求写组名时, 候选链 = 组成员候选链按序拼接 (跨成员去重),
  组名对客户端与逻辑名无差别 —— 响应 `model` 改回组名, 日志 `logical` 记组名。
  组不能嵌套 (组成员只能是逻辑名)。
- 别名解析出候选链 (顺序即优先级), 按序逐个尝试. 每逻辑名有**粘性偏好**: 最近成功服务的
  候选被提到候选链首位, 它可用就走它, 不可用才按顺序 fallback; 偏好只在被长期锁 (额度 /
  低性能 / 熔断) 挡下才迁移 (熔断是连续失败到阈值自动上锁, 一上锁即迁走偏好), 真实失败不数次数,
  锁 / 退避 / 排队超时的临时顶班不算 (候选恢复后流量自动回流); 空闲超过 `sticky_ttl_sec`
  (缺省 300s) 偏好作废, 下笔按配置优先级重选并重新建立. 缓存不因每笔都从配置第一候选开始
  而在候选间反复横跳. 组名同样有自己的粘性偏好
  (记忆的是展开后候选链里的落点).
- 每个候选走一遍闸门: 等并发信号量 (`sem_wait_ms`, 缺省 5s) -> 进上游流量队列 (出队
  节拍 `interval_ms`, 缺省 500ms) -> 进该模型队列 (只在熔断锁 / 退避时等) -> 发送.
- 换候选就重新给这几段等待, **没有整请求总预算**.
- 换候选时机: 只有上游返回 200 算成功; 其余状态码 (3xx / 4xx 含 429 / 5xx) 与首字节前的
  传输失败都进候选分支, 由错误规则判类后决定加锁、退避还是换下一个.
- 全部候选失败: 保留最近一次错误 (状态码 / 头 / 体) 原样透传; 如果请求根本没发出去
  (被锁 / 排队超时 / 信号量超时), 就合成 429 说明原因 (见 5.2).
- 锁的粒度: 额度锁该模型所属的记账组 (`quota_lock_ms`, 缺省 10min, 额度按组共享,
  一个上游可以有多组); 熔断只锁出错的 `上游/模型` (`breaker_lock_ms`, 缺省 5min);
  低性能锁 (`lowperf_lock_ms`, 缺省 5min) 同样只锁 `上游/模型` —— 最近
  `lowperf_window_ms` (缺省 10min) 内, 低吞吐 或 高延迟 任一命中即触发:
  低吞吐 = 解码吞吐的峰值低于 `lowperf_peak_max` (缺省 5 t/s) 且平均低于
  `lowperf_avg_max` (缺省 1 t/s) (平均 = 输出 token 总量 / 解码段总时长, 峰值 = 单笔最高,
  样本只取能拆出解码段的流式成功请求, 窗口内不足 2 笔不判); 高延迟 = 窗口内平均首字节
  时延超过 `lowperf_ttfb_max_ms` (ms; 缺省 0 = 关闭该项, 同样不足 2 笔不判). 豁免名单
  `lowperf_exempt` (真实模型 id 数组) 内的模型不参与判定, 永不触发低性能锁. 同家别的
  模型照常发送. 锁与退避落盘到日志同目录 `gate-state.json`, 重启仍生效.
   锁到期后默认**不直接放行**: 先由恢复探测 (内置 `max_tokens=1` 最小请求) 验证可达性,
   探测通过才解锁, 失败按原锁时长重锁 (`probe_enabled`, 缺省开; 命中恢复态的真实请求也会
   补触发一笔探测, 但请求本身仍按锁定进下一候选). 探测沿用触发锁的那笔请求的接口
   (`chat/completions` 或 `/responses`), 不计入吞吐 / 性能样本. 低性能锁的探测还要过
   `lowperf_ttfb_max_ms` 时延门槛 (超限视为失败重锁), 且解锁后一个观察窗口内再次变慢时,
   重锁时长逐次翻倍 (基础 ×2/×4/×8 封顶) —— 免得「放行一笔又锁上」的周期抖动.

## 5. 代理合成的错误

### 5.1 一览

| 状态码 | code | 场景 |
|---|---|---|
| 400 | `invalid_request_error` | 请求体读不出 / 不是合法 JSON / 缺 `model`; `/sticky` 给了 `upstream` 没给 `logical` |
| 404 | `not_found` | 不支持的路径或方法; `/sticky` 指定的候选不在别名链里 |
| 404 | `model_not_found` | 未注册的逻辑名; 或该别名没有任何可用上游 |
| 405 | `method_not_allowed` | `/v1/models` `/state` 非 GET; `/unlock` `/sticky` 非 POST |
| 429 | `rate_limit_error` | 闸门挡下, 请求没发出去 (带 `Retry-After`) |
| 429 | `upstream_locked` | 候选全在锁里 (上游限制的传导, 带 `Retry-After`) |
| 500 | `internal_error` | `/state` 载荷编码失败 |
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

- 非 200 响应: 先按该上游的错误规则分类 (先搜关键词, 关键词判不出来才按纯状态码规则
  兜底, 见 providers-api.md §1.2), 额度类锁组, 并发类锁超发试探, 其余按退避序列重试同一
  候选, 超过序列长度熔断该 `上游/模型`; 全部失败时保留最近一次错误 (状态码 / 头 / 体)
  原样透传 — 客户端看到的是上游自己的错误, 不加工.
- 3xx 同样进候选分支: 它只可能来自 `base_url` 配错或鉴权跳转, 不是上游正常行为.
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

返回逻辑名清单 (排序), 不暴露真实上游与凭据. 组 (别名的别名, `groups`) 展开成普通
逻辑名, 一并列出. 每个条目先是 OpenAI 标准字段: `id` / `object` (`model`) /
`created` (恒 `0`) / `owned_by` (恒 `tollgate`); 之后附按逻辑名聚合的模型参数, 来源是
配置 `upstreams[].models[]` 的 `reasoning` / `thinking_level_map` / `context_window` /
`max_tokens`. 官方 OpenAI 列表不含推理等级, 这些扩展键是本地约定, 客户端通常会忽略:

- `reasoning`: 任一候选声明支持思考即为 `true`.
- `thinkingLevelMap`: 推理等级 -> 上游 effort 名, 按候选取交集 —— 某等级只有所有声明了
  等级表的候选都支持才保留, 否则给 `null`.
- `contextWindow` / `maxTokens`: 候选里的最小值 (请求可能落到任一候选, 只有保守值才
  不会被上游拒绝).

```json
{ "object": "list", "data": [ { "id": "deepseek-v4.1-flash", "object": "model", "created": 0, "owned_by": "tollgate", "reasoning": true, "thinkingLevelMap": { "low": "low", "high": "high", "off": "none" }, "contextWindow": 131072, "maxTokens": 65536 } ] }
```

### 6.2 GET /state

只读观测面: 进行中的请求 + 每个候选的调度状态 + 事件. 纯内存, 不落盘、不参与记账与
调度, 重启即清空; 消费方是 TUI, 也可给别的工具读.

- 查询参数 `since`: 只要序号大于它的新增事件, 返回里同时给 `max_seq`, 下次轮询带上.
- 载荷: `ver` (观测面版本), `listen`, `now`, `active` (进行中请求数组), `cands`
  (每上游一条: 队列长 / 在飞 / 并发上限 / 是否在试探 / 连续错误数 / 退避剩余 / 锁剩余
  / 锁原因 / 各记账组的额度锁 (`groups`: 组名 + 锁剩余, 按配置顺序) / 模型级定时锁
  (`models[].why`: `breaker` 熔断 / `lowperf` 低性能) / 距下次节拍), `events` (事件数组),
  `sticky` (各逻辑名当前的粘性偏好候选: `{逻辑名: {upstream, model, remaining_ms}}`,
  无偏好或缺省不出现; `remaining_ms` 是该偏好还能保持多久 (失效窗口剩余), 已过窗
  的偏好直接不列出 (无倒计时, 也无 `*`); 有请求进行中的逻辑名倒计时暂停并按设定值
  (TTL 满值) 满显, 请求结束才从满值开始倒,
  TUI 用它给第 1 页模型/路由聚合表的偏好候选行标 `*`、粘性页画倒计时列),
  `max_seq`.
- `active` 每笔: `id` / `logical` / `source` / `path` / `stream` / `start` / `upstream` /
  `backend` / `stage` / `attempt` / `gate`; 接收中的流式请求另带 `recv_tok` (已收到的
  输出 token 估计数)、`recv_bytes` (已转发的响应字节数) 与 `recv_start` (首字节到达时刻,
  Unix 毫秒), 后三个用来算这笔的实时吞吐 (解码 / 总 / 下载三种, 不加前缀: 解码与下载从
  首字节起算, 总用请求开始时刻 `start`); `tries` 是这笔请求的每一次尝试 (换候选 / 同候选
  重试就多一段), 每段带 `upstream` / `backend` / `segs` (该段各阶段已结算耗时, 秒, 下标 =
  `stage`) / `cur` (当前阶段, `-1` = 该段已结束) / `cur_at` (当前阶段进入时刻, Unix 毫秒,
  TUI 用它现算正在走的那一格) / `stop` (失败停在哪个阶段) / `fail` (结局词
  `fail(502)` / `reject(quota 05:00)`); 观测面版本 `tollgate-state/2`.
"- `events` 每条: `seq` / `ts` / `kind` / `req` (跨重启唯一的请求编号, 省略 = 不挂某笔请求) /
  `logical` / `upstream` / `model` / `msg`. `kind`: `stage` (请求阶段战报, 短词链
  `from(参数) → to(参数)`: 进 sem 是 `new(逻辑名) / switch(新候选) / retry(候选) → sem(供应商)`, 之后
  `sem → freq → queue(候选) → send → recv`) / `ok` (成功收尾 `recv → ok(200)`) /
  `error` (每段候选失败的结局: `send → fail(归类词|状态码|connection)` 或
  `sem → reject(原因)`, 请求整体结局 = 链上最后一条) / `gate` (进入退避 `backoff`) /
  `lock` (`lock(quota 组名 05:00)` / `lock(breaker 05:00)` /
  `lock(lowperf peak 2.0 / avg 1.0 t/s 05:00 | ttfb 45s 05:00)`) / `unlock` (到期恢复
  `unlock`, 人工解锁 `unlock(manual N)`) / `probe` (开始 `probe`, 通过 `probe → ok`, 失败
  `probe → fail(request|transport|http 502|ttfb 45s) → lock(原因 05:00)`) / `start` / `reload`.
  阶段战报与收尾带 `req` + `logical`; 锁 / 探测 / 启动没有 `req`, 消费方按 `upstream` /
  `model` 显示. 仅仅首字节慢、没有触发锁的请求不发事件. TUI 显示时把同一秒内同一请求的
  `stage` / `ok` / `error` 事件折叠成一条链 (桥接阶段去重, 取信息量更多的表达), 跨秒拆条;
  正文用终端前景色, `fail(...)` 红字, `switch` / `retry` 黄字, 收尾词 `ok(200)` 绿字, 供应商名
  带上游专属色, `events` 标识列的逻辑名带专属色 (加粗).
- 其余方法返回 405.

## 7. JSONL 请求日志 (每行一笔)

`log_path` (缺省 `var/requests.jsonl`) 追加写, 按本地日历天轮转, 跨天归档为
`<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 `-1` / `-2`).

| 字段 | 含义 |
|---|---|
| `ts` | 结束时刻 (RFC3339Nano) |
| `req` | 跨重启唯一的请求编号 = (自最近一次某月 1 日 03:00 本地起的分钟数 + 1) * 100 + 该分钟内的请求序号, 进程重启后从更大的分钟数继续, 不回落; 跨月回绕时以 `ts` 里的月份区分; 可省略 |
| `logical` | 请求的逻辑模型名 |
| `upstream` | 实际服务的上游 id |
| `model` | 实际服务的真实模型 id |
| `path` | 转发端点 (`/v1/chat/completions` 或 `/v1/responses`) |
| `stream` | 是否流式 |
| `status` | 最终状态码; 上游错误行 = 该上游返回的状态码 |
| `usage` | `{prompt, completion, total, cached?}` (归一口径; `cached` 是命中缓存输入 token) |
| `missing_usage` | true = 上游没回 usage, 走了字符粗估 |
| `queued` / `queue_ms` / `sem_ms` | 是否排过队 / 排队总时长 / 等信号量总时长 |
| `client` / `ua` / `source` | 来源 (remote addr / User-Agent / `X-Tollgate-Source`) |
| `fallbacks` | 换候选次数 |
| `err` | 错误说明 |
| `err_body` | 上游真实错误 (有响应内容) 时的响应体原文 (截断 1KB); 锁内跳过 / 传输失败无此字段 |
| `err_headers` | 上游真实错误时的全部响应头 (如 `Retry-After` / `x-ratelimit-reset-*`, 供查额度重置与到期时间, 也可用于设定额度锁倒计时); 锁内跳过 / 传输失败无此字段 |
| `class` | 上游错误的归类: `quota` / `concurrency` / `other` (按该上游的 `errors` 规则表); 传输失败记 `transport` |
| `gate` | 被闸门挡下未发出时的原因 (见 5.2) |
| `lock_skips` / `backoff_skips` | 候选被锁 / 在退避而被跳过的次数 |
| `ttfb_ms` / `decode_ms` / `tps` | 首字节时延 / 解码段时长(首字节到流末, 仅流式) / 解码吞吐 tokens/s (流式才有) |
| `bytes` / `bps` | 转发给客户端的响应体字节数 / 接收的字节吞吐 bytes/s (流式才有 bps) |
| `duration_ms` | 请求耗时 (收到请求到写完响应), 供不把空闲计入分母的吞吐 |

一笔请求通常落一行; 若候选链里有上游真实返回了 HTTP 错误, 每个这样的错误都会
独立多落一行 (即使后续候选兜底成功), `status` = 该上游返回的状态码, 并带
`err` / `err_body` / `err_headers` / `class`, 便于查额度限制与到期时间; 额度类错误
解析出的重置时间还会直接用于设定额度锁的倒计时 (设计 §5.5.3); 随后整体
结局 (成功或最终失败) 再落一行正常记录。传输层失败 (没拿到响应) 也留一行,
只有 `err` 与 `class=transport`。请求级累计字段 (`fallbacks` / `queued` 等)
逐行保留当前值。

明细事实来源是日志 (观测面重启即清空, 设计 D12); `-summary` 与 `-usage` 两个离线子命
令都能跨归档读, 见 [operations.md](operations.md) §7.
