# llm-request-tollgate

本机模型请求的统一闸口: 自研单文件 Go 代理 (无第三方依赖), 各 agent 把 base URL
指向它, 由它统一做模型路由、限速与用量记账。设计文档见 [docs/design.md](docs/design.md)。

## 做什么

1. 别名白名单路由: 逻辑模型名 -> 一组 (上游, 真实模型 id), 顺序即优先级;
   未注册的模型名直接 404, 不猜上游。
2. 按上游分组的限速: 令牌桶 (速率 + 突发, 即严格最小间隔) + 信号量 (并发),
   桶空排队, 超 `queue_wait_ms` 返 429 带 `Retry-After`。
3. 请求转发: 密钥集中在代理 (客户端凭据一律替换), SSE 逐块原样透传不缓冲。
4. 用量记账: 强写 `stream_options.include_usage`, 从流末 usage / 非流式响应体
   提取 token 用量; 缺 usage 按字符粗估 (F7) 并记 `missing_usage`。
5. 预算判定点: 第一版恒放行只留位置 (余额接口后续接入)。
6. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
   对外隐藏真实上游 id。

## 快速开始

```bash
go build ./...
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (45 项断言):
./test/run_selftest.sh all
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 配置

`config.local.json` (gitignored, 地址与密钥不入库), 结构见 `config.example.json`:

- `listen` 监听地址; `log_path` JSONL 日志路径。
- `timeouts`: 建连 / 等响应头 / 流空闲 / 排队上限。
- `upstreams`: 每个上游一条记录 (id / base_url / auth_header / key / headers /
  include_usage / limits), 限速参数挂在记录上, 即「上游 + 凭据」分组。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。

别名与上游支持 `SIGHUP` 热加载, 限速器状态按上游 id 保留不被重置。

## v1 边界

- 只做 `POST /v1/chat/completions` (D4); `/responses`、`/v1/models` 一律 404。
- 无集中 fallback / 熔断; 首字节之后不切后端。
- 预算判定恒放行 (D5); 账本不落盘, 明细在日志。
- 接真上游前的清单见设计文档 §5.4。

## 目录

- `main.go` 单文件代理 (含假上游自测所需的全部实现)。
- `test/fakeupstream/` 假上游: 回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流 (验证 F6)。
- `test/run_selftest.sh` 自测: 7 组用例 45 项断言, 对本地假上游跑代理。
- `tmp/verify.sh` 一键验收脚本 (临时, 输出进 `tmp/verify.log`, 不提交)。
