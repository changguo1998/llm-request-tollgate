# llm-request-tollgate

本机模型请求的统一闸口: 自研单文件 Go 代理 (无第三方依赖), 各 agent 把 base URL
指向它, 由它统一做模型路由、调度与用量记账。设计文档见 [docs/design.md](docs/design.md)。

## 做什么

01. 别名白名单路由: 逻辑模型名 -> 一组 (上游, 真实模型 id), 顺序即优先级;
    未注册的模型名直接 404, 不猜上游。
02. 按上游分组的调度 (设计 §5.5): 每个上游一条 FIFO 队列 + 自适应并发上限 —— 上限起点 1,
    满了先用一笔超发试探 (不占槽位), 成功就把上限 +1; 出队节拍 `gate.interval_ms`
    (缺省 500ms)。拿不到槽位最多等 `gate.sem_wait_ms` (20s), 入队后最多等
    `gate.queue_wait_ms` (10s), 换候选就重新给这两段 (没有整请求总预算)。闸门按
    **当前候选**逐个过, 换候选前先归还上一个候选的槽位; 候选链试完仍没发出去, 就
    合成 429 说明原因, 带 `Retry-After`。
03. 请求转发: 密钥集中在代理 (客户端凭据一律替换), SSE 逐块原样透传不缓冲。
04. 用量记账: 强写 `stream_options.include_usage`, 从流末 usage / 非流式响应体
    提取 token 用量 (含缓存命中 token, 四种上游字段名都认); 缺 usage 按字符粗估 (F7)
    并记 `missing_usage`。`-usage` 按 上游/模型 × 窗口 列出 token 分解。
05. 限制反应 (设计 §5.5.2/§5.5.3): 每个上游可用 `errors` 规则 (状态码 + body 关键词)
    把报错分成并发 / 额度 / 其他三类; 命中额度 -> 锁该候选 `gate.quota_lock_ms` (10min),
    其他 -> 按 `gate.retry_backoff_ms` (10/20/30s) 退避、连续 4 次熔断锁
    `gate.breaker_lock_ms` (5min); 规则没覆盖到的报错会把上游 + 状态码 + body 片段写进服务
    日志, 便于按真实文案补规则。锁内不真发, 排队的请求整队迁移到下一个候选; 锁落
    `var/gate-state.json` (重启仍生效)。服务不记账也不提前拦截, 只写日志。
06. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
    对外隐藏真实上游 id。
07. 运维面: `GET /v1/models` 返回逻辑名清单; 请求头 `X-Tollgate-Source`
    记入日志 `source` 字段; `-summary` 汇总请求日志; `deploy/` + `test/healthcheck.sh`
    常驻与自检。
08. 集中 fallback: 候选按序尝试, 仅首字节前 (传输失败 / 上游 429 / 5xx) 自动切换
    下一个; 全部失败保留最近一次错误 (状态码/头/体) 原样透传; 其余 4xx 不重试。
09. 供应商锁: 锁内跳过该候选不再真发 (日志 `lock_skips`), 候选全在锁里时返 429
    `upstream_locked` + `Retry-After`, 锁到期自动恢复。
10. 日志按天轮转: 跨本地日期 (或隔天启动) 时把上一段日志归档为
    `<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 -1/-2); `-summary` 与 `-usage`
    都跨归档读, 归档不自动删除 (留存交给外部)。
11. 探测与延迟观测 (D14, 默认关闭, **只观测不接入**): 真实请求被动记 `ttfb_ms`
    (流式另记 `tps`); 开启 `probe` 后按 `interval_sec` 只主动探测 fallback 顺序里的
    **下一个**候选 (`scope=next`, 单候选别名不探)。结果看 `GET /v1/probe` 或
    `-probe <config>`, 状态落 `var/probe.json`。探测不改变候选顺序, 不触发失败计数与锁。
    自适应路由已决策不做 (设计 D15), 所以探测结果只进报告, 不接线路由。
12. 双接口转发: `POST /v1/chat/completions` 与 `POST /v1/responses` (O5) 共用同一条链路
    (白名单 / 闸门 / fallback / 记账)。`/responses` 没有 `stream_options`, 不做
    include_usage 注入; 用量按 `input/output_tokens` 口径归一, 流以 `response.completed`
    判正常收尾 (该接口不保证发 `[DONE]`), 事件里嵌在 `response` 下的 `model` 也改回逻辑名。
13. 只读观测面与状态台: `GET /v1/state` 给出此刻进行中的请求, 以及每个上游的队列/并发上限/锁
    (纯内存, 不落盘、不参与记账); `-tui` 把它连同日志聚合成三个页的实时状态台。

## 快速开始

```bash
go build ./...
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (17 组用例 131 项断言):
./test/run_selftest.sh all
# 常驻 (systemd 用户服务, 崩了自动拉起):
./deploy/install_service.sh
# 自检 (二进制 / 配置 / 端口):
./test/healthcheck.sh
# 汇总请求日志 (按 逻辑名+来源, 含 token 用量; 传目录则跨天归档一起汇总):
./llm-request-tollgate -summary var/requests.jsonl
./llm-request-tollgate -summary var/
# 看探测/延迟报告 (只读; 不接线路由), 或在线取 GET /v1/probe:
./llm-request-tollgate -probe config.local.json

# 按 上游/模型 × 窗口 (3h/12h/24h/全部) 看 token 分解:
./llm-request-tollgate -usage var/

# 实时状态台 (TUI: active 进行中的请求 / routes 路由历史 / events 事件):
./llm-request-tollgate -tui                     # 同进程起代理 + 状态台 (q 退出)
./llm-request-tollgate -tui -tui-state http://127.0.0.1:8787   # 只看已常驻的服务
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 状态台 (TUI)

三个页, `1`/`2`/`3` 或 `Tab` 切, `up/down`/`PgUp/PgDn` 滚动, `r` 重读日志, `q` 退出。

| 页 | 看什么 | 数据来源 |
|---|---|---|
| `active` | 进行中的请求 (编号 / 已耗时 / 逻辑名 / 上游 / 真实模型 / 流式与否 / 当前卡在哪) + 每个上游的 进行中/上限、排队数、锁与退避 | 服务的只读观测面 (同进程读内存, 另进程 `GET /v1/state`) |
| `routes` | 按 `逻辑名 × 上游 × 真实模型` 聚合的请求数 / 错误 / 换候选次数 / token / 缓存命中 / 首字节 / 吞吐 / 最近一次, 附每分钟请求数条 | JSONL 日志 (含跨天归档, 窗口 6h) |
| `events` | 加锁 / 熔断 / 退避 / 解锁 / 换候选 / 错误 / 慢请求, 最新的在最上面, 连续同类事件折成 `xN` | 服务事件流 + 日志还原 |

- 观测面是**只读且纯内存**的: 不落盘、不参与记账与调度, 服务重启即清空; 明细的事实来源
  仍是日志 (设计 D12)。同进程模式顺手把它写成 `var/tui-pulse.json`, 也便于别的工具读。
- 两个进程模式随便挑: `-tui` 适合"本地起一份看", `-tui-state` 适合看 systemd 常驻那份
  (`./deploy/tollgate.sh` 起的服务不变)。
- 窄终端会按优先级自动丢列 (先丢路径、来源这类), 至少保留逻辑名、上游与真实模型。

## 常驻与升级

- 安装常驻: `./deploy/tollgate.sh install` (systemd 用户服务, 崩了自动拉起)。
- 日常操作: `./deploy/tollgate.sh <status|logs|restart|stop|start|hup|uninstall>`
  - `restart` 更新代码后用它: 先 `go build -o llm-request-tollgate .`
    再 `./deploy/tollgate.sh restart` (二进制被替换不会自动重启, 需手动 restart)。
  - `hup` 改 `config.local.json` 别名/上游后热加载, 不用重启。
- 自检: `./test/healthcheck.sh`。
- **密钥来源**: 服务只从进程环境读 (`key_env`), 不落盘。unit 的 `ExecStart` 包了一层
  shell, 先 source `~/fff/config/shellrc.sh`, 再加载 fff 的 `aliases.d/{25-env,10-ai}.sh`,
  最后跑 `aikey all` (遍历 `~/.fff/keys/*.ai`) 才 exec 代理本体 — 所以重启后不需要任何手工
  导入。加载情况看 `./deploy/tollgate.sh logs` (会打印 `Loaded ustc.ai` 等)。
  排查: `ps -o cmd= -p $(systemctl --user show -p MainPID --value tollgate.service)`。
- 密钥注入 (`key_env`): 真 key 只存环境变量, 不落任何盘。
  - 名字与 fff 的 `~/.fff/keys/*.ai` 一致 (`USTC_API_KEY` / `GROUP_API_KEY` /
    `ARKCP_API_KEY` / `ARKAP_API_KEY`), 即 store 里 `apiKey` 的占位符名。
  - 交互用: `export USTC_API_KEY=...` (建议写进 shell 启动文件)。
  - systemd 服务用: `for f in ~/.fff/keys/*.ai; do set -a; . "$f"; set +a; done` 后
    `systemctl --user import-environment USTC_API_KEY GROUP_API_KEY ARKCP_API_KEY ARKAP_API_KEY`
    (或 `systemctl --user set-environment USTC_API_KEY=...`), 再 `./deploy/tollgate.sh restart`。

## 配置

实际配置 `config.local.json` 是软链接 → `~/fff/config/tollgate.json` (gitignored,
密钥不入库, 真配置在 ~/fff dotfiles 仓追踪); 仓库内只跟踪模板 `config.example.json`,
结构见下:

- `listen` 监听地址; `log_path` JSONL 日志路径 (当前日志; 跨天自动归档到同目录
  `<base>-YYYY-MM-DD.jsonl`, 见「做什么」第 10 条)。
- `timeouts`: 建连 / 等响应头 / 流空闲 (等待上限已经收进 `gate`)。
- `gate` (v2, 所有上游共用): `interval_ms` 出队节拍 (缺省 500ms), `sem_wait_ms` 等信号量
  上限 (20s), `queue_wait_ms` 排队上限 (10s), `concurrency_lock_ms` / `quota_lock_ms` /
  `breaker_lock_ms` 两把锁的时长 (1h / 10min / 5min), `retry_backoff_ms` 通用兜底的退避
  序列 ([10000,20000,30000])。并发上限不在配置里 (起点固定 1, 每次重启重新爬)。
- `upstreams`: 每个上游一条记录 (id / base_url / auth_header / key 或 key_env / headers /
  include_usage / errors), 调度状态按 id 分组 (一个上游一把 key, 即「上游 + 凭据」分组)。
  `errors` 是错误分类规则 (逐家规则见 [docs/providers-api.md](docs/providers-api.md),
  缺省空表 = 全部走通用兜底): 每类一个规则数组, 单条规则
  `{ "status": [429], "contains": ["quota exceeded"] }` —— 状态码在列表内 (缺省 = 任意状态码)
  **且** 匹配文本 (`error.type` + `error.code` + `error.message`) 里出现关键词才算命中;
  判定顺序 `quota → concurrency → 其他`。
  `key_env` 只存环境变量名, 真 key 启动时从环境取 (密钥不落盘, 见「常驻与升级」)。
  `key_prefix` 是认证头里的 scheme: 缺省对 `Authorization` 用 `Bearer `, 其他头不带
  (ustc 与火山 ARK 实测都要 `Bearer `; 给 `""` 可强制裸 key)。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。
- `probe` (可选, 默认关闭): `{ enabled, scope:"next"|"all", interval_sec:900, max_tokens:8, timeout_ms:30000, window:5, targets:[别名...], target_ttfb_ms, target_tps, switch_margin }`。
  `scope=next` 只探第二个候选 (第一个候选由真实流量被动观测); `targets` 为空则探所有多候选
  别名。`target_ttfb_ms` / `target_tps` 是两条标准, `switch_margin` 是"明显优势"阈值,
  三个**只用于报告里给建议**, 不影响路由 —— 自适应路由已决策不做 (设计 D15)。
  探测参数启动时固化, 改配置要重启。

别名、上游与 `gate` 都支持 `SIGHUP` 热加载; 调度状态 (队列里排着的请求、爬到的并发上限、
退避与锁) 按上游 id 保留, 不被 hup 清掉。

### 从 fff 模型列表生成配置

`scripts/gen_tollgate_config.py` 读 `~/fff/config/llm-models.json` (provider -> baseUrl/models,
由 fff 的 `fff-update-*-llm-list.py` 维护) 与 `llm-models-droplist.json` (精确匹配黑名单),
生成 `upstreams` 与 `aliases`:

- 别名的逻辑名取条目里的 `tollgate_group`, 没有就是模型 id 本身; 同名 (同 id 跨上游, 或显式
  `tollgate_group`) 的归并成一个别名。
- 候选顺序 = fallback 优先级 = 模型的 `tollgate_priority` 降序, 同值看 provider 的
  `tollgate_priority` 降序, 再同值按 `--providers` 顺序; 0 最小, 缺省即 0。
- `tollgate_group` / `tollgate_priority` 手工维护, 但不会被 store 更新脚本冲掉
  (`merge_models_by_id` 继承 provider API 不提供的字段)。跨上游同族不同名的模型 (如 ustc 的
  `deepseek-flash` 与 ark 的 `deepseek-v4-1-flash*`) 就靠 `tollgate_group` 并成一个别名。
- 代理自身的模型列表 (agent 看到的) 同时继承候选的 `thinkingLevelMap` (取交集: 某等级只有所有
  候选都支持才保留), 以及最小的 `contextWindow` / `maxTokens`; 缺等级表时 agent 会拒发
  `reasoning_effort`, 报 `UNSUPPORTED_REASONING_EFFORT` (dsh 客户端校验)。
- `key_env` 取 store 里 `apiKey` 的占位符名; `auth_header` 等设置从现有配置继承, 不会被覆盖。
- 写入 `config.local.json` (即 `~/fff/config/tollgate.json`); `--dry-run` 只打印,
  `--providers` 改上游与优先级。

```bash
python3 scripts/gen_tollgate_config.py --dry-run
python3 scripts/gen_tollgate_config.py
```

优先级用 TUI 调 (Node >= 22.6 直接跑 TS, 零依赖零构建, 无命令行参数):
provider 面板 + 模型列表 + 候选顺序实时预览; `+/-` 调值, `=` 输入数值, `Tab` 换面板,
`/` 过滤, `s` 存 store, `r` 重生成配置, `h` hup, `q` 退出。改完直接 `r` `h` 即可生效。

```bash
node scripts/set_priority.ts
```

### 用量分析 (计划中)

服务只写日志, 不做窗口累计、不折算额度、也没有状态文件 —— 预算与用量分析交给一个独立的
分析程序 (设计文档 §5.3 O10, 语言不限, 只读日志, 不随服务启动), **尚未实现**。
现在能用的只有代理自带的两个离线子命令: `-summary` (请求数 / 429 / 等待 / 缺 usage /
token) 与 `-usage` (上游 + 模型 × 窗口的 token 分解)。

## 当前边界

- 转发覆盖 `POST /v1/chat/completions` 与 `POST /v1/responses` (O5); `GET /v1/models`
  返回逻辑名清单 (O5)。只读观测面: `GET /v1/probe`、`GET /v1/state`。其余路径一律 404。
- 被闸门挡下的 429 在日志里带 `gate` (`locked` 候选在锁里 / `sem_timeout` 等不到并发
  槽位 / `queue_timeout` 排队超时 / `canceled` 客户端断开) 与等待时长 (`sem_ms` 等信号量、
  `queue_ms` 排队, 都是各候选之和), 便于区分「并发不够」「排队超时」和「被锁」。
- 不做负载均衡 (D15): 候选按列表顺序取第一个可用的, 不按权重 / 轮询 / 延迟分流;
  闸门被拒时也不溢出到其它空闲候选。
- 集中 fallback 与供应商锁只覆盖首字节前 (传输失败 / 429 / 5xx); 首字节之后不切后端。
- 归档日志不自动清理, 也不压缩; 留存与清理交给外部 (logrotate 或手动)。
- 真上游已接入 (设计文档 §5.4 那份清单已满足); 上线前还缺切流顺序与回滚、以及「没有进程
  绕开代理」的验证 (设计文档 §5.3 O4)。

## 目录

- `main.go` 单文件代理 (含假上游自测所需的全部实现)。
- `test/fakeupstream/` 假上游: 回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流 (验证 F6)。
- `test/run_selftest.sh` 自测: 17 组用例 131 项断言, 对本地假上游跑代理。
- `test/healthcheck.sh` 自检: 二进制 / 配置 (-check) / 端口三项, 退出码 0 = 全通过。
- `deploy/` 常驻: `tollgate.service` 模板 + `install_service.sh` (systemd 用户服务)。
- `docs/providers-api.md` 上游错误分类调研: 判定流水线 + 逐家实测错误清单 + 可直接落地的
  `errors` 规则 (2026-10-13)。
- `scripts/gen_tollgate_config.py` 由 fff 模型列表生成本项目配置 (读
  `~/fff/config/llm-models.json` + 黑名单, 按 `tollgate_group` 归并成别名; 见下)。
- `scripts/set_priority.ts` 调优先级的 TUI (Node >= 22.6 跑 TS, 无依赖无参数): 候选顺序
  实时预览, 界面内即可保存 store / 重生成配置 / hup。
- `gate_test.go` / `proxy_test.go` 单元与端到端测试 (`go test .`): 调度核心 (排队节拍、
  试探放宽、两把锁、错误归类、退避熔断、锁落盘) 与经代理转发的完整链路。
- `test/tmp/` 与 `var/` 是运行产物 (自测二进制/假上游日志; 请求日志与 `gate-state.json`
  等状态), 都在 `.gitignore` 内。
