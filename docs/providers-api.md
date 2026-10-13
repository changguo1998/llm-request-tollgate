# 上游错误分类：逻辑 / 调研结果 / 配置样例

> 用途：为 v2 的 `errors` 规则表提供判定逻辑、依据与可直接落地的配置样例 (design.md §5.5.2 的
> 三档分类, D13 / D17)。
> 调研日期 2026-10-13。第 2 节含对真实上游的一次性直连实测。
> 脱敏：本文不含真实密钥、账号标识与自建网关地址 (base_url 一律写占位符或公开厂商域名) ——
> `config.local.json` 是 gitignored 的「地址与密钥不入库」文件, 真实值只在那里。

---

# 1. 逻辑

## 1.1 一句话原则

**状态码只用来收敛候选集, 定类必须落到 body 的结构化字段或关键词；判不出来就走通用兜底 + 记账,
绝不默认当额度锁。**

理由：多家的 429 都是「限流与额度共用」的状态码, 且额度错误的状态码会随网关版本漂移。

| 上游 | 429 可能是 | 判定依据 |
|---|---|---|
| `ustc` (LiteLLM) | 限流 / 预算耗尽 | `error.type`: `budget_exceeded` vs `rate_limit_error` |
| `arkcp` / `arkap` | 限流 / 套餐额度 / 过载 | `error.code`: `RateLimitExceeded.*` vs `QuotaExceeded*` vs `ServerOverloaded` |
| `group` (CLIProxyAPI) | 限流 / 账号额度 | message 关键词 (无稳定 code) |
| OpenAI (直连) | 限流 / 额度 | `error.code`: `rate_limit_exceeded` vs `insufficient_quota` 系列 |
| Kimi (直连) | 限流 / 余额 | `error.type`: `rate_limit_reached_error` vs `exceeded_current_quota_error` |
| DeepSeek (直连) | 限流 (额度走 402) | 状态码即可分 |

## 1.2 判定流水线（顺序固定）

1. **前提**：只在「首字节尚未写给下游」时分类。流已开始就没有分类可言, 只能发 SSE error 事件
   (design §2 第 2 条)。
2. **取 body**：截断到 8 KB；先试 JSON 解析, 抽出 `error.type` / `error.code` / `error.message`
   (同一路径覆盖 OpenAI 形、方舟形与 Anthropic 形 `type=error` 包裹)；JSON 失败 (DeepSeek 的 401 是
   纯文本) 则用原始 body。
3. **拼匹配文本** = `type + code + message`, 统一转小写。不要只挑一个字段 —— 有的上游关键词只在
   `type` (LiteLLM), 有的只在 `code` (方舟, 其 `type` 常为空串), 有的只在 `message` (group)。
4. **按 `quota → concurrency → rate → other` 顺序匹配**, 首条命中即止。单条规则语义：
   `status ∈ 列表`（缺省 = 任意状态码）**且** `contains` 任一子串命中。
5. **命中即定动作**：
   - `quota` → 供应商锁 10min + 整队迁移, 换下一个候选（该上游不可重试）
   - `concurrency` → 并发上限锁 1h（只锁超发试探）, 本笔按普通错误继续走
   - `rate` / `other` → 通用兜底退避 10s / 20s / 30s, 第 4 次熔断 5min
   - 全不命中 → 落 `other`, 并记一条未分类样本

## 1.3 状态码使用规矩

1. **只做必要条件, 不做充分条件**：`status` 是每条规则的第一道闸, 但不允许出现「只看状态码就锁供应商」的规则。
2. **额度类要放宽状态码**：`{400, 402, 403, 422, 429}` 按上游取子集一起收, 靠关键词区分 ——
   LiteLLM 的 `budget_exceeded` 历史用过 400 / 422 / 429, 新版权威值是 422 而 USTC 实测 429。
3. **状态码列表必须与关键词配套**：`403` 在方舟是额度信号 (`AccountOverdueError`), 在 ustc 只是
   模型无权限 (`key_model_access_denied`) —— 所以 ustc 的额度规则不带 403, 方舟的带。
4. **空 body 不算命中**：`arkap` 的 `/models` 404、`group` 的部分 404 是空 body, 直接落 `other`。

## 1.4 字段可信度（按上游选判定依据）

| 上游 | 主要依据 | 次要依据（兜底） | 不能用的 |
|---|---|---|---|
| `ustc` (LiteLLM) | `error.type`：`budget_exceeded` / `rate_limit_error` / `throttling_error` | message `Budget has been exceeded!`、`ExceededBudget:`、`Max parallel request limit reached` | 状态码（跨版本漂移） |
| `arkcp` / `arkap` | `error.code` 子串：`RateLimitExceeded.*`、`QuotaExceeded*`、`InflightBatchsizeExceeded`、`InvalidSubscription` | message：`overdue balance`、`free trial quota`、`usage quota` | `error.type`（常为空串）、状态码（429 有六类原因同码） |
| `group` (CLIProxyAPI) | message 子串 + 状态码 | — | `error.code`（不稳定）、`error.type`（`error` 有时是字符串而非对象） |
| DeepSeek（未来直连） | 状态码 402 / 429 / 503 | 中英文 message：`Insufficient Balance` / `余额不足` | `error.code` / `error.type`（官方从未定义） |
| OpenAI / Kimi（未来直连） | `error.code` / `error.type` | 限流头 `retry-after` | 状态码单判（两家 429 也是混用） |

## 1.5 关键词写法

1. **子串匹配 + 小写化**, 不用正则。
2. 取**最独特的片段**：`budget_exceeded` 优于 `budget`（后者会命中无关文本）。
3. 中英文都收（DeepSeek 有 `余额不足`, Kimi 有 `insufficient balance`）。
4. 方舟的 `code` 用**前缀族**写法：`QuotaExceeded` 已覆盖 `QuotaExceeded.AgentPlanQuotaExceeded`,
   `RateLimitExceeded` 已覆盖 `.EndpointRPMExceeded` 等, 不必逐个枚举。
5. 关键词表宁可短：**分类不了 = 退避 + 记未分类样本**（upstream + status + body 前 200 字）,
   拿到真实文案再补。误判成额度的代价（锁 10min + 整队迁移）远高于多试一次。

## 1.6 易错点

1. **网关错误 vs 上游错误**：LiteLLM 会把上游错误重包成 `type=throttling_error` + message 含
   `<Provider>Exception`（如 `OpenAIException`）—— 这类是上游限流, 不是网关配额。动作同为退避, 可不区分。
2. **别用响应里的 `model` 判定或路由**：`group` 回写 `kimi-for-coding`、`arkcp` 回写
   `deepseek-v4-flash-ga-260731`, 与别名表里的 id 对不上。
3. **过载类不进三档**：503 `Server Overloaded`、429 `ServerOverloaded` / `RequestBurstTooFast` /
   `engine_overloaded_error` 既不是并发上限也不是额度 → 通用兜底退避。
4. **上游限流 ≠ 并发上限**：只有明确说「并发 / 在途数 / max parallel」的才算 `concurrency`
   (D17 只在这种情况下锁 1h)；RPM / TPM 类限流走通用兜底。

---

# 2. 调研结果列表

## 2.1 上游身份与错误体形状

| 上游 id | base_url | 网关软件 | 错误体形状 |
|---|---|---|---|
| `ustc` | `https://api.llm.ustc.edu.cn/v1` | LiteLLM Proxy (uvicorn) | OpenAI 形 `error{message,type,param,code}` |
| `arkcp` | `https://ark.cn-beijing.volces.com/api/coding/v3` | 火山方舟 Coding Plan | 方舟形 `error{code,message,param,type}` |
| `arkap` | `https://ark.cn-beijing.volces.com/api/plan/v3` | 火山方舟 Agent Plan | 同上 |
| `group` | `<group-base>/v1`（真实值见 config.local.json） | CLIProxyAPI + nginx | OpenAI 形；`/v1/messages` 上是 Anthropic 形 |

判定依据 (实测)：`ustc` 的 401 body 点名 `LiteLLM_VerificationTokenTable` + 响应头 `server: uvicorn`,
USTC 技术更新页也写明网关是 LiteLLM；`group` 的 401 响应头带 `X-CPA-TRACE-ID` / `X-CPA-VERSION`
(CPA = CLI Proxy API) 且未知模型文案 `unknown provider for model X` 是其专有字符串；`arkcp` / `arkap`
响应头 `server: istio-envoy` + `x-request-id`。

**结论：规则按上游写, 不按模型厂商写。** 同一个 `deepseek-v4-flash` 走四家, 错误体分别是
LiteLLM 形 / 方舟形 / CLIProxyAPI 形；厂商规范只在直连官方 API 时适用, 本仓当前没有直连上游。

## 2.2 实测错误清单

### `ustc`（LiteLLM）

| 场景 | 状态码 | `error.type` | body 原文（已脱敏） |
|---|---|---|---|
| 无 key | 401 | `auth_error` | `Authentication Error, No api key passed in.` |
| key 无效 | 401 | `token_not_found_in_db` | `Unable to find token in cache or \`LiteLLM_VerificationTokenTable\`` |
| **额度撞墙** | **429** | **`budget_exceeded`** | `Budget has been exceeded! Key=<key-descriptor> (sk-...<masked>) Current cost: 100.9879641999998, Max budget: 100.0` |
| 模型无权限 | 403 | `key_model_access_denied` | `key not allowed to access model. This key can only access models=[...]. Tried to access X` |

- 429 上**没有** `Retry-After`, 也没有 `x-ratelimit-*`（响应头只有 `server: uvicorn`）。
- `/v1/messages`（Anthropic 端点）存在, 错误体仍是 OpenAI 形；`/v1/models` 也会因预算耗尽直接 429
  （预算检查在路由之前）。
- 403 的 message 回显该 key 的**完整可用模型清单** —— 零成本拿白名单的办法。
- 新版权威状态码是 422（`litellm/__init__.py` 的 `budget_exceeded_status_code = 422`, 注释写明
  「set to 429 to restore the pre-422 response code」）, 历史用过 400 / 429。

### `arkcp` / `arkap`（火山方舟）

| 上游 | 场景 | 状态码 | `error.code` | body 原文（已脱敏） |
|---|---|---|---|---|
| arkcp | 模型不在 Coding Plan 内 | 404 | `UnsupportedModel` | `The requested model does not support the coding plan feature. ... Request id: <id>` |
| arkap | 模型不在 Agent Plan 内 | 404 | `UnsupportedModel` | `... does not support the agent plan feature ...` |
| arkap | 订阅缺失 / 过期 | 400 | `InvalidSubscription` | `Your account (<account-id>) does not have a valid AgentPlan subscription, or your subscription has expired. ...` |

- 错误体：`{"error":{"code":"...","message":"... Request id: <id>","param":"","type":""}}`,
  `type` 常为空串 → 关键词只能取自 `code` 与 `message`。
- `arkcp` 有 `/models` (135 个), `arkap` 的 `/models` 是 404 空 body。
- **`arkcp` 的 `/models` 不含 Coding Plan 可用 id**：别名表里 9 个 arkcp id 在 `/models` 里 0 命中,
  但逐个 1 token 实测 **9/9 全部 200 可用** → design §4.5「真实模型 id 以 `/models` 为准」对它不成立。
- 成功响应头带 `x-request-id` / `x-client-request-id`；无 `x-ratelimit-*`、无 `Retry-After`。
- 用量字段：`prompt_tokens_details.cached_tokens`、`completion_tokens_details.reasoning_tokens`。

### `group`（CLIProxyAPI）

| 场景 | 状态码 | body 原文 |
|---|---|---|
| 无 key | 401 | `{"error":"Missing API key"}` —— **error 是字符串, 不是对象** |
| 未知模型 | 400 | `{"error":{"message":"unknown provider for model X","type":"invalid_request_error","code":"model_not_found","param":"model"}}` |
| body 不合法 | 400 | `{"error":{"message":"messages must not be empty","type":"invalid_request_error"}}`（无 `code`） |
| 未知路由 | 404 | 空 body |
| `/v1/messages` 未知模型 | 400 | `{"type":"error","error":{"type":"invalid_request_error","message":"unknown provider for model X"}}` —— **Anthropic 形** |

- `/v1/models` 可用 (30 个), 别名表里 group 的 18 个 id **全部命中** ✓。
- 成功响应头带 `X-Cpa-Trace-Id`；无 `x-ratelimit-*`。
- 该网关自己会对上游额度错误做「换项目 / 换预览模型」重试 (`/v0/management/quota-exceeded/*`),
  所以**不一定把上游的额度错误透出来**。
- 它上游接的是 Codex / Claude Code 一类**账号池**, 额度与风控文案随账号方变化, 无法穷举。

## 2.3 当前可用性与别名表问题（2026-10-13 实测）

| 上游 | 状态 | 证据 | 恢复条件 |
|---|---|---|---|
| `ustc` | ✗ 全部请求 429 | `budget_exceeded`, Current cost 100.99 / Max budget 100.0（对应 24h 窗口 100） | 等窗口重置 |
| `arkap` | ✗ 全部请求 400 | `InvalidSubscription`（AgentPlan 订阅缺失或过期） | 续订 |
| `arkcp` | ✓ 9/9 别名 id 可用 | 逐个 1 token 实测 200 | — |
| `group` | ✓ `/models` 与 18/18 别名 id 对齐 | 实测 200 | — |

1. `ustc` 的 `deepseek-v4-flash-ascend` 不在该 key 可用清单里 → 该候选恒 403（白等一轮 fallback）。
2. `arkap` 的 4 个末位候选（`deepseek-v4-flash-ga-260731`、`deepseek-v4-pro-ga-260813`、
   `glm-5-3-flash-260828`、`kimi-k2-8-preview`）当前恒 400 `InvalidSubscription` ——
   规则表会把它判为额度类锁掉, 但更彻底的做法是删候选或续订。
3. 别名 id 的校验手段：`group` 用 `/models`（18/18 ✓）；`arkcp` **不能**用 `/models`；
   `ustc` 用 403 的 `key_model_access_denied` message 拿白名单；`arkap` 无 `/models`。

## 2.4 厂商官方规范（仅直连官方 API 时适用）

### DeepSeek

- `https://api.deepseek.com`（Anthropic 兼容端点 `/anthropic`）；`Authorization: Bearer sk-...`。
- **错误体形状官方从未定义**；实测未认证返回**纯文本** `Authentication Fails (governor)`, 不是 JSON。
  官方只文档化 7 个状态码, `error.code` / `error.type` 字符串全部未文档化。

| HTTP | 官方标签 | 归类 | 关键词 |
|---|---|---|---|
| 402 | Insufficient Balance | 额度 | `Insufficient Balance` / `余额不足` |
| 400 | （第三方称余额不足也可走 400） | 额度 | 同上, 另加 `in arrears` / `欠费` |
| 429 | Rate Limit Reached | 限流（官方口径混用「发送过快 / 并发超限 / TPM 达上限」） | `Rate limit reached` / `请求速率达到上限` |
| 503 | Server Overloaded | 过载（退避） | `Server Overloaded` / `服务器繁忙` |
| 500 / 401 | Server Error / Authentication Fails | 服务端 / 鉴权 | `Server Error` / `Authentication Fails` |
| 400 / 422 | Invalid Format / Invalid Parameters | 请求错误 | `Invalid Format` / `Invalid Parameters` |

- 无 `Retry-After` / `x-ratelimit-*`（官方无记载, 实测 401 也没有）；有 `x-ds-trace-id`。
- 支持 `stream_options.include_usage`, 但**不单独发 usage-only chunk**：用量挂在 `[DONE]` 前最后一个内容 chunk。
- 保活：流式发 SSE 注释 `: keep-alive`（代理解析要跳过）；10 分钟未开始推理则服务端断连。

### Kimi / Moonshot

- `https://api.moonshot.cn/v1`（国内）/ `https://api.moonshot.ai/v1`（国际, 账号与余额隔离, 混用 401）；
  Anthropic 端点 base 为 `/anthropic`。
- 错误体 `{"error":{"message":...,"type":...}}`, **实测从不带 `code`**。

| HTTP | `error.type` | 归类 | message 关键词 |
|---|---|---|---|
| 429 | `rate_limit_reached_error` | 限流 / 并发 | `We're receiving too many requests` / `reached organization TPD rate limit, current: …` / `concurrency limit` |
| 429 | `engine_overloaded_error` | 过载（按 `Retry-After` 等） | `The engine is currently overloaded` |
| 429 | `exceeded_current_quota_error` | 额度 | `insufficient balance` / `is suspended due to insufficient balance, please recharge` |
| 403 | `permission_denied_error` | 额度 / 风控 | 余额不足、账号受限；Kimi Code 订阅另有 `usage limit` / `concurrent request limit` |
| 401 / 404 | `invalid_authentication_error` / `incorrect_api_key_error` / `resource_not_found_error` | 鉴权 / 模型不存在 | `Invalid Authentication` / `Model not found` |

- 并发是**独立的并发请求数上限**（按充值分级 Tier0=1 … Tier5=100）, 不是 TPM/RPM 派生。
- 响应头：`Retry-After`、`X-RateLimit-Limit/Remaining/Reset`、`msh-request-id`、`x-msh-trace-id`。

### OpenAI

- `https://api.openai.com/v1`；错误体 `error{message,type,param,code}`。
- 429 必须看 `error.code`：限流 `rate_limit_exceeded` / `slow_down`；额度
  `credit_balance_exhausted` / `organization_spend_limit_exceeded` / `project_spend_limit_exceeded` /
  `organization_usage_limit_exceeded` / `usage_limit_exceeded`, 或 `error.type == "insufficient_quota"`。
- 过载：503 `service_unavailable_error` + `server_is_overloaded`。
- 限流头：`retry-after`(秒) 与 `x-ratelimit-limit/remaining/reset-{requests,tokens}`（reset 是 `1s` / `6m0s` 时长串）。
- 存量码**不要写死**：`invalid_api_key`、`billing_hard_limit_reached`、`insufficient_quota` 作为 code
  在现行官方文档中 0 命中。

### 火山方舟（通用, 含非套餐端点）

- `https://ark.cn-beijing.volces.com/api/v3`（注意是 `/api/v3`）；错误体 `error{code,message,param,type}`。
- 鉴权先于路由：未带合法 key 时任意路径都返回 401, **不能用 404 判断路径是否存在**。

| HTTP | `error.code` | 归类 |
|---|---|---|
| 429 | `RateLimitExceeded.Endpoint{RPM,TPM,FlexTPM}Exceeded` / `ModelAccount{Rpm,Tpm,FlexTpm}RateLimitExceeded` / `APIAccountRpmRateLimitExceeded` / `ModelAccountIpmRateLimitExceeded` / `AccountRateLimitExceeded` | 限流 (RPM / TPM / IPM) |
| 429 | `InflightBatchsizeExceeded` | **并发**（在途批大小, 文案「达到当前充值金额下的最大并发数限制」） |
| 429 | `QuotaExceeded` / `QuotaExceeded.AgentPlanQuotaExceeded` / `SetLimitExceeded` | 额度（免费额度 / 套餐 5h-周-月 限 / endpoint 用量上限） |
| 429 | `ServerOverloaded` / `RequestBurstTooFast` | 过载（退避, 非用户配额） |
| 403 | `AccountOverdueError` / `OperationDenied.ServiceOverdue` / `OperationDenied.FileQuotaExceeded` / `QuotaExceeded.DoubaoSearchFreeQuotaExceeded` | 额度 / 欠费 |
| 400 | `InvalidSubscription` | 额度（套餐未订阅或过期） |
| 404 | `ModelNotOpen` / `InvalidEndpointOrModel.NotFound` / `UnsupportedModel` | 模型不可用 |
| 401 / 400 / 500 | `AuthenticationError` / `InvalidAccountStatus` / `MissingParameter` / `InvalidParameter*` / `OutofContextError` / `InternalServiceError` | 鉴权 / 请求错误 / 服务端 |

- 响应头：`x-request-id`、`x-error-code`（如 `AuthN_AuthenticationError`）、`x-client-request-id`（可自带）；
  `Retry-After` / `x-ratelimit-*` 未确认。
- 限流维度：模型级（同主账号同模型, 不可调）+ 接入点级（RPM/TPM/IPM, 可调）；指标含 TPM / TPD / RPM / IPM / Inflight Batchsize。

## 2.5 其它核实到的事实

1. `stream_options.include_usage` 实测：`arkcp` ✓（usage chunk 的 `choices` 为空数组；方舟另有专有参数
   `chunk_include_usage` 做逐 chunk 累计）, `group` ✓；DeepSeek 支持但不单独发 usage chunk。
2. `/responses` 收尾差异（实测）：`arkcp` 会发 `data: [DONE]`（终态事件可能是 `response.incomplete`,
   `incomplete_details.reason=length`）；`group` **不发 `[DONE]`**, 只有 `response.completed`。
   → design §4.3「以 `response.completed` 判正常收尾」需放宽为「`response.completed` 或
   `response.incomplete` / `response.failed`, 且兼容有无 `[DONE]`」。
3. 模型名会被上游回写（`kimi-k2.8` → `kimi-for-coding`；`deepseek-v4-flash` → `deepseek-v4-flash-ga-260731`）。

## 2.6 未确认清单（不要写死进规则表）

1. `group` 的额度 / 限流文案（402 / 429）与并发文案 —— 未实测到。
2. `ustc` 的限流 (`rate_limit_error`) 与并发槽位超限文案 —— 该 key 被预算挡在更前面, 没打到。
3. 方舟的 `Retry-After` / `x-ratelimit-*` 是否存在。
4. DeepSeek 的 400/402/422/429/500/503 官方 body 原文与 `Retry-After`；Chat Completions 流内错误事件形态。
5. OpenAI 的 `invalid_api_key` / `billing_hard_limit_reached` / `insufficient_quota`（作为 code）。

来源：[USTC API 使用](https://llm.ustc.edu.cn/guide/api-usage/)、[USTC 技术更新](https://llm.ustc.edu.cn/guide/technical-updates/)、[USTC 协议支持](https://llm.ustc.edu.cn/guide/protocol/)、[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)、[LiteLLM Error Reference](https://docs.litellm.ai/docs/proxy/error_reference)、[方舟错误码](https://docs.volcengine.com/docs/ark/error-codes?lang=zh)、[方舟 base url 与认证](https://docs.volcengine.com/docs/ark/base-url-and-authentication?lang=zh)、[DeepSeek error_codes](https://api-docs.deepseek.com/quick_start/error_codes)、[Kimi errors](https://platform.kimi.com/docs/api/errors.md)、[OpenAI error-codes](https://developers.openai.com/api/docs/guides/error-codes.md)、[OpenAI rate-limits](https://developers.openai.com/api/docs/guides/rate-limits.md)

---

# 3. 示例配置文件

## 3.1 完整形状（v2；`errors` 为本次调研产出）

```json
{
  "listen": "127.0.0.1:8787",
  "log_path": "var/requests.jsonl",
  "timeouts": { "connect_ms": 10000, "response_header_ms": 120000, "stream_idle_ms": 240000 },
  "gate": {
    "interval_ms": 500,
    "sem_wait_ms": 20000,
    "queue_wait_ms": 10000,
    "concurrency_lock_ms": 3600000,
    "quota_lock_ms": 600000,
    "breaker_lock_ms": 300000,
    "retry_backoff_ms": [10000, 20000, 30000]
  },
  "upstreams": [
    {
      "id": "ustc",
      "base_url": "https://api.llm.ustc.edu.cn/v1",
      "auth_header": "Authorization",
      "key_env": "USTC_API_KEY",
      "headers": {},
      "include_usage": true,
      "errors": {
        "quota": [
          {
            "status": [400, 422, 429],
            "contains": [
              "budget_exceeded",
              "Budget has been exceeded!",
              "ExceededBudget:",
              "exceeded budget for model="
            ]
          }
        ],
        "concurrency": [
          {
            "status": [429],
            "contains": ["Max parallel request limit reached", "max_parallel_requests"]
          }
        ],
        "rate": [
          {
            "status": [429],
            "contains": [
              "Crossed TPM / RPM / Max Parallel Request Limit",
              "Rate Limit Handler for rate limit type",
              "Rate limit exceeded for",
              "Limit type: requests",
              "Limit type: tokens",
              "throttling_error"
            ]
          }
        ]
      }
    },
    {
      "id": "arkcp",
      "base_url": "https://ark.cn-beijing.volces.com/api/coding/v3",
      "auth_header": "Authorization",
      "key_env": "ARKCP_API_KEY",
      "headers": {},
      "include_usage": true,
      "errors": {
        "quota": [
          {
            "status": [400, 403, 429],
            "contains": [
              "QuotaExceeded",
              "AgentPlanQuotaExceeded",
              "DoubaoSearchFreeQuotaExceeded",
              "SetLimitExceeded",
              "AccountOverdueError",
              "ServiceOverdue",
              "FileQuotaExceeded",
              "InvalidSubscription"
            ]
          }
        ],
        "concurrency": [
          { "status": [429], "contains": ["InflightBatchsizeExceeded"] }
        ],
        "rate": [
          {
            "status": [429],
            "contains": [
              "RateLimitExceeded",
              "ModelAccountRpmRateLimitExceeded",
              "ModelAccountTpmRateLimitExceeded",
              "ModelAccountFlexTpmRateLimitExceeded",
              "ModelAccountIpmRateLimitExceeded",
              "APIAccountRpmRateLimitExceeded",
              "AccountRateLimitExceeded"
            ]
          }
        ]
      }
    },
    {
      "id": "arkap",
      "base_url": "https://ark.cn-beijing.volces.com/api/plan/v3",
      "auth_header": "Authorization",
      "key_env": "ARKAP_API_KEY",
      "headers": {},
      "include_usage": true,
      "errors": {
        "quota": [
          {
            "status": [400, 403, 429],
            "contains": [
              "QuotaExceeded",
              "AgentPlanQuotaExceeded",
              "SetLimitExceeded",
              "AccountOverdueError",
              "ServiceOverdue",
              "InvalidSubscription"
            ]
          }
        ],
        "concurrency": [
          { "status": [429], "contains": ["InflightBatchsizeExceeded"] }
        ],
        "rate": [
          {
            "status": [429],
            "contains": [
              "RateLimitExceeded",
              "ModelAccountRpmRateLimitExceeded",
              "ModelAccountTpmRateLimitExceeded",
              "APIAccountRpmRateLimitExceeded",
              "AccountRateLimitExceeded"
            ]
          }
        ]
      }
    },
    {
      "id": "group",
      "base_url": "<group-base>/v1",
      "auth_header": "Authorization",
      "key_env": "GROUP_API_KEY",
      "headers": {},
      "include_usage": true,
      "errors": {
        "quota": [
          {
            "status": [402, 429],
            "contains": [
              "user_credits_insufficient",
              "user_period_limit_exceeded",
              "insufficient",
              "usage limit",
              "credits"
            ]
          }
        ],
        "concurrency": [],
        "rate": [
          {
            "status": [429],
            "contains": ["rate_limit_error", "too many requests", "Rate limit reached"]
          }
        ]
      }
    }
  ],
  "aliases": {
    "example-model": [["ustc", "<ustc-real-id>"], ["group", "<group-real-id>"], ["arkcp", "<arkcp-real-id>"]]
  }
}
```

## 3.2 落地约定

1. **类间优先级固定** `quota → concurrency → rate → other`：额度类不可重试, 必须先于限流类判定。
2. **`rate` 是可选的第四档**：整段删除即走通用兜底退避, 与 design §5.5.2 的 e2 语义一致。
   建议先删掉观察日志, 确认「限流需要比通用退避更强的动作」再启用。
3. **`errors` 缺省空表 = 全部走通用兜底**（design §4.8）；`group` 的 `quota` 关键词是弱证据,
   若求稳可改成 `[]`, 同时在日志里记未分类 4xx/5xx 的 body 截断, 拿到真实文案再补。
4. 不需要进 `errors` 的关键词（不是「限制」语义）：401 `auth_error` / `token_not_found_in_db` /
   `Missing API key`；403 `key_model_access_denied`；400 `model_not_found` /
   `unknown provider for model` / `UnsupportedModel` / `Invalid model name passed in`。

## 3.3 脱敏说明

本文所有 base_url 用占位符或公开厂商域名；真实自建网关地址、账号标识与密钥片段一律不入库,
以 `config.local.json` 为准（`.gitignore` 已排除）。
