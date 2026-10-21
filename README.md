# llm-request-tollgate

本机模型请求的统一闸口: 自研单文件 Go 代理 (无第三方依赖), 各 agent 把 base URL
指向它, 由它统一做模型路由、调度与用量记账。设计见 [docs/design.md](docs/design.md),
接口见 [docs/api.md](docs/api.md), 运维见 [docs/operations.md](docs/operations.md)。

## 做什么

01. 别名白名单路由: 逻辑模型名 -> 一组 (上游, 真实模型 id), 顺序即优先级;
    未注册的模型名直接 404, 不猜上游。
02. 按上游分组的调度 (设计 §5.5): 每个上游一条**流量队列** (FIFO, 只保证出队节拍
    `gate.interval_ms`, 缺省 500ms) + 自适应并发上限 —— 上限起点 1, 满了先用一笔超发试探
    (不占槽位), 成功就把上限 +1; 出队后进**该模型的队列** (不等节拍, 只在熔断锁 / 退避时才等)。
    拿不到槽位最多等 `gate.sem_wait_ms` (5s), 两层队列各最多等 `gate.queue_wait_ms` (10s),
    换候选就重新给这几段 (没有整请求总预算)。闸门按**当前候选**逐个过, 换候选前先归还上一个
    候选的槽位; 候选链试完仍没发出去, 就合成错误说明原因 (被锁 / 退避带 `Retry-After`)。
03. 请求转发: 密钥集中在代理 (客户端凭据一律替换), SSE 逐块原样透传不缓冲。
04. 用量记账: 强写 `stream_options.include_usage`, 从流末 usage / 非流式响应体
    提取 token 用量 (含缓存命中 token, 四种上游字段名都认); 缺 usage 按字符粗估 (F7)
    并记 `missing_usage`。`-usage` 按 上游/模型 × 窗口 列出 token 分解。
05. 限制反应 (设计 §5.5.2/§5.5.3): 每个上游可用 `errors` 规则 (状态码 + body 关键词)
    把报错分成并发 / 额度 / 其他三类; 命中额度 -> 只锁该模型所属的**记账组**
    `gate.quota_lock_ms` (10min, 一个上游可以有多组, 不锁整家), 其他 -> 按
    `gate.retry_backoff_ms` (10/20/30s) 退避、连续 4 次熔断锁 `gate.breaker_lock_ms`
    (5min)。**熔断按 `上游/模型` 粒度**: 只锁出错的那个真实模型, 同家别的模型照常发送
    (退避与连续错误计数也一样按模型分开记; 选候选时看到某模型正在退避就直接跳过, 不让
    请求在它的队列里白等)。锁内不真发, 排队的请求整队迁移到下一个候选; 锁落
    `var/gate-state.json` (重启仍生效)。服务不记账也不提前拦截, 只写日志。
06. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
    对外隐藏真实上游 id。
07. 运维面: `GET /v1/models` 返回逻辑名清单; 请求头 `X-Tollgate-Source`
    记入日志 `source` 字段; `-summary` 汇总请求日志; `deploy/` + `test/healthcheck.example.sh`
    常驻与自检。
08. 集中 fallback: 候选按序尝试, 仅首字节前 (传输失败 / 上游 429 / 5xx) 自动切换
    下一个; 全部失败保留最近一次错误 (状态码/头/体) 原样透传; 其余 4xx 不重试。
09. 定时锁: 锁内跳过该候选/该模型不再真发 (日志 `lock_skips`), 候选全在锁里时返 429
    `upstream_locked` + `Retry-After`, 锁到期自动恢复。额度锁只锁 `上游/记账组`,
    熔断锁只锁出错的那个 `上游/模型`。
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
    (纯内存, 不落盘、不参与记账); `-tui` 把它连同日志聚合成四个页的实时状态台。

## 快速开始

```bash
go build ./...
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (17 组用例 131 项断言):
./test/run_selftest.sh all
# 常驻 (systemd 用户服务, 崩了自动拉起). 部署模板示例在
# deploy/tollgate.service.example (仓库不跟踪本机版 tollgate.service:
#   cp deploy/tollgate.service.example deploy/tollgate.service
./deploy/install_service.sh
# 自检 (二进制 / 配置 / 端口; 公开仓库是示例版, 密钥要自己 export):
./test/healthcheck.example.sh
# 汇总请求日志 (按 逻辑名+来源, 含 token 用量; 传目录则跨天归档一起汇总):
./llm-request-tollgate -summary var/requests.jsonl
./llm-request-tollgate -summary var/
# 看探测/延迟报告 (只读; 不接线路由), 或在线取 GET /v1/probe:
./llm-request-tollgate -probe config.local.json

# 按 上游/模型 × 窗口 (3h/12h/24h/全部) 看 token 分解:
./llm-request-tollgate -usage var/

# 实时状态台 (TUI: active 进行中的请求 / models 模型与路由聚合 / events 事件 / settings 调度参数与优先级):
./llm-request-tollgate -tui                     # 同进程起代理 + 状态台 (q 退出)
./llm-request-tollgate -tui -tui-state http://127.0.0.1:8787   # 只看已常驻的服务
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 状态台 (TUI)

四个页, `1`-`4` 或 `Tab` 切, `up/down`/`PgUp/PgDn` 滚动, `r` 重读日志, `q` 退出。
界面按终端尺寸铺开: 分块与表格之间留空行, 键位提示贴最后一行, 列宽按内容自适应、右栏顶到右边界。

每张表都是同一套写法: 灰字标题行 (说清这张表看什么、怎么排序) + 加粗表头 + 行 + 超出窗口时的
`N 行, 当前 a-b`; 列宽按内容自适应 (长的列自己撑开, 一路顶到终端右边界,
顶满之后才截断, 代码里没有写死的列宽上限),
一整列都没内容的列自动收起 (没有进行中的请求时列名照常显示)。第 1 页例外: 列名固定在页签下方 (不随分块滚走),
分块之间空一行, 每块的数据行区至少留 4 行 (获取信号量 / 排队 / 等首字节) 或 2 行
(其余三块), 空块也照留, 请求换阶段时下面的块不会上下跳; 全部块都空时这一页约 32 行,
窗口比这更矮就靠 `up/down` 滚动分块。

颜色全台一套: 绿 = 健康 (live / ok / 解锁 / 错误数 0), 红 = 错误与锁 (错误 / 熔断锁 /
额度锁 / 连续错误 > 0), 黄 = 等待与降级 (退避 / 换候选 / 慢 / 超发试探 / 顶部
错误提示), 灰 = 说明 (标题 / 位置行 / 空); 另外每个上游一种专属色 (青 / 品红 / 蓝系,
不用红黄绿), 只画在上游名上 (各页的 `上游/模型` 单元格同色)。表头加粗, 当前选中行反显。

| 页 | 看什么 | 数据来源 |
|---|---|---|
| `active` | 进行中的请求**按流程阶段分块**: 获取信号量 / 排队 (等出队节拍) / 等模型窗口 (退避 / 熔断锁) / 等首字节 / 接收中 / 已失败。每行仍是 编号 / 耗时 / 逻辑名 / 上游/模型 / 流式 / 状态 / 来源 / 路径, 块标题带当前笔数 | 服务的只读观测面 (同进程读内存, 另进程 `GET /v1/state`) |
| `models` | 先上游状态 (按 provider 优先级降序, 同优先级保持配置顺序, 与 `settings` 页同口径): 列是 上游 / 并发 (`占用/上限`, 有超发试探时占用后带 `+1`) / 排队 (有排队时附 `[距下次出队]`) / 错误 (连续错误) / 记账 (默认组, 未锁 `-`), 之后只在该行后面铺开它自己声明的记账组 (列标题留空, 未锁显示绿色组名, 锁中换成红色倒计时占位; 组少的行后面留空); 再把**有过请求的** `上游/真实模型` 的队列与路由聚合成一张表, 列按类别分组: 调度状态 (排队 / 退避 / 熔断 / 连错)、历史计数 (请求 / 错误 / 换候选)、流量 (token / 缓存 / 首字节 / 吞吐)、时间 (最近); 统计按模型对所有逻辑名求和, 不出现逻辑名, 队列空了也保留到进程重启 | 队列读 `/v1/state`; 聚合读 JSONL 日志 (含跨天归档, 窗口 6h); 上游顺序读 store 的 `tollgate_priority` |
| `events` | 列: 时间 / 类型 / 逻辑名 / 上游/模型 / 说明。看 加锁 / 熔断 / 退避 / 解锁 / 换候选 / 错误 / 慢请求, 最新的在最上面, 连续同类事件折成 `xN` | 服务事件流 + 日志还原 |
| `settings` | 调度参数 + 权重编辑: 上半是 `gate` 的 7 项 (interval_ms / sem_wait_ms / queue_wait_ms / 三个锁时长 / retry_backoff_ms), 下半是 provider 面板 (按优先级降序, 段间空一行) 与模型列表 (行首 `M` 模型优先级 / `P` provider 优先级), 右栏是所选别名的候选顺序实时预览。`m` / `p` / `g` 分别聚焦模型 / provider / 调度参数, `+` / `-` 调值, `=` 输入, `s` 保存并热加载 | 配置 `config.local.json` 的 `gate` 段 + store `~/fff/config/llm-models.json` (两个都可写) |

- 观测面是**只读且纯内存**的: 不落盘、不参与记账与调度, 服务重启即清空; 明细的事实来源
  仍是日志 (设计 D12)。同进程模式顺手把它写成 `var/tui-pulse.json`, 也便于别的工具读。
- 两个进程模式随便挑: `-tui` 适合"本地起一份看", `-tui-state` 适合看 systemd 常驻那份
  (`./deploy/tollgate.sh` 起的服务不变)。`settings` 页两种模式都能改: 写本机的 store 与
  配置 `gate` 段, 同进程模式热加载直接生效, 观察常驻服务时发 `SIGHUP`。`s` 只写 `gate`
  与优先级; `timeouts` 与 `probe` 仍要改配置文件后重启。
- 列宽按内容分配 (长的列自己撑开, 顶到终端右边界为止), 余量先补齐差得少的列; 只有连各列
  最小宽都放不下时才按优先级丢列 (先丢路径、来源这类), 至少保留逻辑名、上游与真实模型。
- 时间段写成 `12s` / `02:03` / `01:02:03` (前导的零段省掉), 时刻仍是 `hh:mm:ss`。

## 常驻与升级

- 安装常驻: `./deploy/tollgate.sh install` (systemd 用户服务, 崩了自动拉起)。
- 日常操作: `./deploy/tollgate.sh <status|logs|restart|stop|start|hup|uninstall>`
  - `restart` 更新代码后用它: 先 `go build -o llm-request-tollgate .`
    再 `./deploy/tollgate.sh restart` (二进制被替换不会自动重启, 需手动 restart)。
  - `hup` 改 `config.local.json` 别名/上游后热加载, 不用重启。
- 自检: `./test/healthcheck.example.sh` (公开仓库的示例版, 密钥要自己 export; 本机版
  `healthcheck.sh` 在 dotfiles, 会自动加载 fff 密钥)。
- **密钥来源**: 服务只从进程环境读 (`key_env`), 不落盘。本机 dotfiles 维护的 unit
  的 `ExecStart` 包了一层 shell, 先 source `~/fff/config/shellrc.sh`, 再加载 fff 的
  `aliases.d/{25-env,10-ai}.sh`,
  最后跑 `aikey all` (遍历 `~/.fff/keys/*.ai`) 才 exec 代理本体 — 所以重启后不需要任何手工
  导入。加载情况看 `./deploy/tollgate.sh logs` (会打印 `Loaded ustc.ai` 等)。
  排查: `ps -o cmd= -p $(systemctl --user show -p MainPID --value tollgate.service)`。
- 密钥注入 (`key_env`): 真 key 只存环境变量, 不落任何盘。
  - 名字与 fff 的 `~/.fff/keys/*.ai` 一致 (`USTC_API_KEY` / `GROUP_API_KEY` /
    `ARKCP_API_KEY` / `ARKAP_API_KEY` / `DEEPSEEK_API_KEY`), 即 store 里 `apiKey` 的占位符名
    (`deepseek` 是官方直连, 其余四家经各自网关)。
  - 交互用: `export USTC_API_KEY=...` (建议写进 shell 启动文件)。
  - systemd 服务用: `for f in ~/.fff/keys/*.ai; do set -a; . "$f"; set +a; done` 后
    `systemctl --user import-environment USTC_API_KEY GROUP_API_KEY ARKCP_API_KEY ARKAP_API_KEY DEEPSEEK_API_KEY`
    (或 `systemctl --user set-environment USTC_API_KEY=...`), 再 `./deploy/tollgate.sh restart`。

## 配置

实际配置 `config.local.json` 是软链接 → `~/fff/config/tollgate.json` (gitignored,
密钥不入库, 真配置在 ~/fff dotfiles 仓追踪); 仓库内只跟踪模板 `config.example.json`,
结构见下:

- `listen` 监听地址; `log_path` JSONL 日志路径 (当前日志; 跨天自动归档到同目录
  `<base>-YYYY-MM-DD.jsonl`, 见「做什么」第 10 条)。
- `timeouts`: 建连 / 等响应头 / 流空闲 (等待上限已经收进 `gate`)。
- `gate` (v2, 所有上游共用): `interval_ms` 出队节拍 (缺省 500ms), `sem_wait_ms` 等信号量
  上限 (5s), `queue_wait_ms` 排队上限 (10s), `concurrency_lock_ms` / `quota_lock_ms` /
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
  `models` (可选) 是该上游提供的真实模型清单, 每条 `{ "id": "...", "billing_group": "..." }`:
  `billing_group` 是**记账组**, 同家同名一组, 额度锁只锁组; 不写组名 = 该上游的默认组,
  整个上游不写 `models` = 所有模型都在默认组 (与加组之前的配置完全等价)。声明了 `models`
  的上游, 别名引用的模型必须在表内, 否则启动 / 热加载直接报错。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。
- `probe` (可选, 默认关闭): `{ enabled, scope:"next"|"all", interval_sec:900, max_tokens:8, timeout_ms:30000, window:5, targets:[别名...], target_ttfb_ms, target_tps, switch_margin }`。
  `scope=next` 只探第二个候选 (第一个候选由真实流量被动观测); `targets` 为空则探所有多候选
  别名。`target_ttfb_ms` / `target_tps` 是两条标准, `switch_margin` 是"明显优势"阈值,
  三个**只用于报告里给建议**, 不影响路由 —— 自适应路由已决策不做 (设计 D15)。
  探测参数启动时固化, 改配置要重启。

别名、上游与 `gate` 都支持 `SIGHUP` 热加载; 调度状态 (队列里排着的请求、爬到的并发上限、
退避与锁) 按上游 id 保留, 不被 hup 清掉。

### 从 fff 模型列表生成配置

本机版 `scripts/gen_tollgate_config.py` (直接读本机 fff store, 在 dotfiles 里维护,
不进公开仓库) 的行为如下; 公开仓库提供不依赖 fff 的示例:
`scripts/gen_tollgate_config.example.py` + 模型列表示例 `scripts/llm-models.example.json`
(`python3 scripts/gen_tollgate_config.example.py`), 输入形状与规则与本机版一致。

本机版读 `~/fff/config/llm-models.json` (provider -> baseUrl/models,
由 fff 的 `fff-update-*-llm-list.py` 维护) 与 `llm-models-droplist.json` (精确匹配黑名单),
生成 `upstreams` 与 `aliases`:

- 别名的逻辑名取条目里的 `tollgate_group`, 没有就是模型 id 本身; 同名 (同 id 跨上游, 或显式
  `tollgate_group`) 的归并成一个别名。
- 候选顺序 = fallback 优先级 = 模型的 `tollgate_priority` 降序, 同值看 provider 的
  `tollgate_priority` 降序, 再同值按 `--providers` 顺序; 0 最小, 缺省即 0。
- `tollgate_group` / `tollgate_priority` 手工维护, 但不会被 store 更新脚本冲掉
  (`merge_models_by_id` 继承 provider API 不提供的字段)。跨上游同族不同名的模型 (如 ustc 的
  `deepseek-flash` 与 ark 的 `deepseek-v4-1-flash*`) 就靠 `tollgate_group` 并成一个别名。
- 额度组取模型的 `tollgate_billing` (同家同名一组, 缺省归默认组), 生成到上游的 `models`
  (字段 `billing_group`); 整家都没声明 `tollgate_billing` 时不写 `models`, 配置形状与
  加记账组之前一致。
- 代理自身的模型列表 (agent 看到的) 同时继承候选的 `thinkingLevelMap` (取交集: 某等级只有所有
  候选都支持才保留), 以及最小的 `contextWindow` / `maxTokens`; 缺等级表时 agent 会拒发
  `reasoning_effort`, 报 `UNSUPPORTED_REASONING_EFFORT` (dsh 客户端校验)。
- `key_env` 取 store 里 `apiKey` 的占位符名; `auth_header` 等设置从现有配置继承, 不会被覆盖。
- 写入 `config.local.json` (即 `~/fff/config/tollgate.json`); `--dry-run` 只打印,
  `--providers` 改上游与优先级; 不传时缺省是 store 里**除 tollgate 与 arkap 外**的全部 provider
  (arkap 要带就显式写进 `--providers`).
- 不在 store 里的手加上游在重生成时会丢 (`build_upstreams` 只认 store 里的 provider);
  要先在 store 里补 provider, 或重生成后手工补回。

```bash
# 本机版 (dotfiles 维护, 依赖本机 fff store):
python3 scripts/gen_tollgate_config.py --dry-run
python3 scripts/gen_tollgate_config.py
# 公开仓库示例 (不依赖 fff, 读 scripts/llm-models.example.json):
python3 scripts/gen_tollgate_config.example.py
```

优先级用状态台的 `settings` 页调: provider 面板 + 模型列表 + 候选顺序实时预览;
`m` / `p` / `g` 切模型 / provider / 调度参数, `+/-` 调值, `=` 输入数值, `/` 过滤,
`s` 保存 (store 与 `gate`) 并热加载, `r` 重生成配置, `h` 热加载, `q` 退出。

```bash
./llm-request-tollgate -tui -tui-state http://127.0.0.1:8787   # 看常驻服务, 按 4 进 settings 页
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
- 集中 fallback 与定时锁只覆盖首字节前 (传输失败 / 429 / 5xx); 首字节之后不切后端。
- 归档日志不自动清理, 也不压缩; 留存与清理交给外部 (logrotate 或手动)。
- 真上游已接入 (设计文档 §5.4 那份清单已满足); 上线前还缺切流顺序与回滚、以及「没有进程
  绕开代理」的验证 (设计文档 §5.3 O4)。

## 目录

- `main.go` 单文件代理 (含假上游自测所需的全部实现)。
- `test/fakeupstream/` 假上游: 回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流 (验证 F6)。
- `test/run_selftest.sh` 自测: 17 组用例 131 项断言, 对本地假上游跑代理。
- `test/healthcheck.example.sh` 自检示例: 二进制 / 配置 (-check) / 端口三项, 退出码
  0 = 全通过 (本机版 `healthcheck.sh` 在 dotfiles, 多一项 fff 密钥自动加载)。
- `deploy/` 常驻: `tollgate.service.example` 模板 (公开示例, 本机版 `tollgate.service`
  在 dotfiles) + `install_service.sh` (systemd 用户服务)。
- `docs/api.md` HTTP 接口参考: 端点 / 认证替换 / 合成错误 / 只读观测面 / 日志字段。
- `docs/operations.md` 部署运维 runbook: systemd 常驻 / 密钥 / 升级回滚 / 排障 / 清理。
- `docs/providers-api.md` 上游错误分类调研: 判定流水线 + 逐家实测错误清单 + 可直接落地的
  `errors` 规则 (2026-10-13)。
- `scripts/gen_tollgate_config.example.py` 由模型列表生成本项目配置 (示例版, 不依赖
  fff; 输入与规则见上) + `llm-models.example.json` 模型列表示例 (本机版
  `gen_tollgate_config.py` 在 dotfiles)。
- `tui.go` / `tui_priority.go` 状态台: 前三个页读状态与日志, `settings` 页调 gate 参数与
  权重 (写配置与 store, 候选顺序实时预览, 界面内即可保存 / 重生成配置 / 热加载)。
- `gate_test.go` / `proxy_test.go` / `tui_*_test.go` 单元与端到端测试 (`go test .`): 调度
  核心 (排队节拍、试探放宽、两把锁、错误归类、退避熔断、锁落盘)、经代理转发的完整链路,
  以及状态台版式与 settings 页存盘。
- `test/tmp/` 与 `var/` 是运行产物 (自测二进制/假上游日志; 请求日志与 `gate-state.json`
  等状态), 都在 `.gitignore` 内。
