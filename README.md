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
5. 预算判定点: 每上游配滚动窗口限额, 定时从日志重建累计, 满额拒服务 (D12)。
6. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
   对外隐藏真实上游 id。
7. 运维面: `GET /v1/models` 返回逻辑名清单; 请求头 `X-Tollgate-Source`
   记入日志 `source` 字段; `-summary` 汇总请求日志; `deploy/` + `test/healthcheck.sh`
   常驻与自检。
8. 集中 fallback: 候选按序尝试, 仅首字节前 (传输失败 / 上游 429 / 5xx) 自动切换
   下一个; 全部失败保留最近一次错误 (状态码/头/体) 原样透传; 其余 4xx 不重试。
9. 熔断: 每上游连续失败 (默认 5 次) 打开, 冷却期 (默认 30s) 内跳过该候选不再真发,
   冷却后自动恢复; 参数可配 (`breaker_fail` / `breaker_cooldown_ms`)。
10. 预算闸门 (D12): 每上游可配滚动窗口限额 (`budget.windows`, 如 3h/12h/24h),
    代理定时从请求日志重建各窗口 token 累计, 任一窗口满额即返 429 `budget_exceeded`;
    重启后从日志重建不丢消耗 (账本永不落盘, 日志即账本)。
11. 日志按天轮转: 跨本地日期 (或隔天启动) 时把上一段日志归档为
    `<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 -1/-2); 预算聚合与 `-summary`
    都跨归档读, 归档不自动删除 (留存交给外部)。

## 快速开始

```bash
go build ./...
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (77 项断言):
./test/run_selftest.sh all
# 常驻 (systemd 用户服务, 崩了自动拉起):
./deploy/install_service.sh
# 自检 (二进制 / 配置 / 端口):
./test/healthcheck.sh
# 汇总请求日志 (按 逻辑名+来源, 含 token 用量; 传目录则跨天归档一起汇总):
./llm-request-tollgate -summary var/requests.jsonl
./llm-request-tollgate -summary var/
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 常驻与升级

- 安装常驻: `./deploy/tollgate.sh install` (systemd 用户服务, 崩了自动拉起)。
- 日常操作: `./deploy/tollgate.sh <status|logs|restart|stop|start|hup|uninstall>`
  - `restart` 更新代码后用它: 先 `go build -o llm-request-tollgate .`
    再 `./deploy/tollgate.sh restart` (二进制被替换不会自动重启, 需手动 restart)。
  - `hup` 改 `config.local.json` 别名/上游后热加载, 不用重启。
- 自检: `./test/healthcheck.sh`。
- 密钥注入 (`key_env`): 真 key 只存环境变量, 不落任何盘。
  - 交互用: `export TOLLGATE_USTC_KEY=...` (建议写进 shell 启动文件)。
  - systemd 服务用: `systemctl --user import-environment TOLLGATE_USTC_KEY TOLLGATE_GROUP_KEY`
    或 `systemctl --user set-environment TOLLGATE_USTC_KEY=...`, 然后 `./deploy/tollgate.sh restart`。

## 配置

实际配置 `config.local.json` 是软链接 → `~/fff/config/tollgate.json` (gitignored,
密钥不入库, 真配置在 ~/fff dotfiles 仓追踪); 仓库内只跟踪模板 `config.example.json`,
结构见下:

- `listen` 监听地址; `log_path` JSONL 日志路径 (当前日志; 跨天自动归档到同目录
  `<base>-YYYY-MM-DD.jsonl`, 见「做什么」第 11 条)。
- `timeouts`: 建连 / 等响应头 / 流空闲 / 排队上限。
- `upstreams`: 每个上游一条记录 (id / base_url / auth_header / key 或 key_env / headers /
  include_usage / limits / budget), 限速参数挂在记录上, 即「上游 + 凭据」分组。
  `key_env` 只存环境变量名, 真 key 启动时从环境取 (密钥不落盘, 见「常驻与升级」)。
  `budget`: `{ windows: {"3h":30,"12h":70,"24h":100}, unit:1, refresh_sec:60 }` —
  滚动窗口限额; `unit` 是 token 折算平台额度的系数 (默认 1, 按网页已用对照后调整);
  `refresh_sec` 是定时扫日志的间隔。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。

别名与上游支持 `SIGHUP` 热加载, 限速器状态按上游 id 保留不被重置。
预算累计每次定时扫描从日志重建, 不设独立状态 (日志即账本)。

## v1 边界

- 转发只做 `POST /v1/chat/completions` (D4); `GET /v1/models` 返回逻辑名清单 (O5);
  `/responses` 一律 404。
- 集中 fallback + 熔断只覆盖首字节前 (传输失败/429/5xx); 首字节之后不切后端。
- 预算闸门按 token 窗口累计, 折算系数 `unit` 默认 1 (平台额度口径待实测标定)。
- 归档日志不自动清理, 也不压缩; 留存与清理交给外部 (logrotate 或手动)。
- 接真上游前的清单见设计文档 §5.4。

## 目录

- `main.go` 单文件代理 (含假上游自测所需的全部实现)。
- `test/fakeupstream/` 假上游: 回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流 (验证 F6)。
- `test/run_selftest.sh` 自测: 12 组用例 77 项断言, 对本地假上游跑代理。
- `test/healthcheck.sh` 自检: 二进制 / 配置 (-check) / 端口三项, 退出码 0 = 全通过。
- `deploy/` 常驻: `tollgate.service` 模板 + `install_service.sh` (systemd 用户服务)。
- `tmp/` 一键验收脚本 (临时, 输出进 `tmp/*.log`, 不提交)。
