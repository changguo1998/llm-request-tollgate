# llm-request-tollgate

本机模型请求的统一闸口: 自研 Go 代理 (标准库, 无第三方依赖), 各 agent 把 base URL
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
    提取 token 用量 (含缓存命中 token, 四种上游字段名都认); 缺 usage 按字符粗估 (F7,
    非 ASCII 1 token/字符, 其余 4 字符 1 token; 流式逐块累加与非流式读响应体同一口径,
    思考、工具调用的 name 与 arguments、多模态段都算输出, base64 / URL 段每段记 1)
    并记 `missing_usage`。`-usage` 按 上游/模型 × 窗口 列出 token 分解。
05. 限制反应 (设计 §5.5.2/§5.5.3): 每个上游可用 `errors` 规则 (状态码 + body 关键词)
    把报错分成并发 / 额度 / 其他三类; 命中额度 -> 只锁该模型所属的**记账组**
    `gate.quota_lock_ms` (10min, 一个上游可以有多组, 不锁整家), 其他 -> 按
    `gate.retry_backoff_ms` (10/20/30s) 退避、连续 4 次熔断锁 `gate.breaker_lock_ms`
    (5min)。**熔断按 `上游/模型` 粒度**: 只锁出错的那个真实模型, 同家别的模型照常发送
    (退避与连续错误计数也一样按模型分开记; 选候选时看到某模型正在退避就直接跳过, 不让
    请求在它的队列里白等)。另有一把**低性能锁** (同样按 `上游/模型`): 最近
    `gate.lowperf_window_ms` (10min) 内, **低吞吐** 或 **高延迟** 任一命中即锁
    `gate.lowperf_lock_ms` (5min) ——
    低吞吐 = 解码吞吐的峰值低于 `gate.lowperf_peak_max` (5 t/s) **且**平均低于
    `gate.lowperf_avg_max` (1 t/s) (平均 = 输出 token 总量 / 解码段总时长, 空闲不计;
    峰值 = 单笔最高的那笔; 样本只取能拆出解码段的流式成功请求, 不足 2 笔不判);
    高延迟 = 窗口内平均首字节时延超过 `gate.lowperf_ttfb_max_ms` (缺省 0 = 关闭该项,
    开用时同样不足 2 笔不判)。模型慢到没有实用价值就暂时不用它。豁免名单
    `lowperf_exempt` 里的模型不参与判定, 永不触发低性能锁。锁内不真发,
    排队的请求整队迁移到下一个候选; 锁落 `var/gate-state.json` (重启仍生效)。服务不记账
    也不提前拦截, 只写日志. 低性能锁的恢复有收敛机制 (D26): 恢复探测带首字节时延门槛
     (超 `lowperf_ttfb_max_ms` 视为探测失败继续锁), 解锁后一个观察窗口内再次变慢则锁持时长
     逐次翻倍 (基础 ×2/×4/×8 封顶), 免得「放行一笔又锁上」的周期抖动.
06. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
    对外隐藏真实上游 id。
07. 运维面: `GET /v1/models` 返回逻辑名清单; 请求头 `X-Tollgate-Source`
    记入日志 `source` 字段; `-summary` 汇总请求日志; `deploy/` + `test/healthcheck.example.sh`
    常驻与自检。
08. 集中 fallback: 候选按序尝试, 只有 200 算成功; 首字节前其余状态码与传输失败都进候选
    分支 (按错误规则上锁 / 退避), 全部失败保留最近一次错误 (状态码/头/体) 原样透传。
    为保缓存热度, 每逻辑名有**粘性偏好** (D25): 记住最近成功服务的候选, 新请求优先它,
    它可用就走它, 只有它持续坏 (真实失败连续 2 次) 或被长期锁 (额度 / 低性能) 挡下才迁移;
    锁 / 退避 / 排队超时的临时顶班不算, 候选恢复后流量自动回流; 也可用 `POST /sticky`
    人工置一次 (立即生效, 自动迁移规则照旧).
09. 定时锁: 锁内跳过该候选/该模型不再真发 (日志 `lock_skips`), 候选全在锁里时返 429
    `upstream_locked` + `Retry-After`。锁到期后默认不直接放行, 先由恢复探测 (内置
    `max_tokens=1` 请求, 沿用触发锁的接口路径) 验证可达性, 探测通过才解锁, 失败按原锁时长
    重锁 (`probe_enabled` 可关). 低性能锁的探测还要过 `lowperf_ttfb_max_ms` 时延门槛.
    额度锁只锁 `上游/记账组`, 熔断锁与低性能锁只锁对应的那个 `上游/模型`。
10. 日志按天轮转: 跨本地日期 (或隔天启动) 时把上一段日志归档为
    `<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 -1/-2); `-summary` 与 `-usage`
    都跨归档读, 归档不自动删除 (留存交给外部)。
11. 双接口转发: `POST /v1/chat/completions` 与 `POST /v1/responses` (O5) 共用同一条链路
    (白名单 / 闸门 / fallback / 记账)。`/responses` 没有 `stream_options`, 不做
    include_usage 注入; 用量按 `input/output_tokens` 口径归一, 流以 `response.completed`
    判正常收尾 (该接口不保证发 `[DONE]`), 事件里嵌在 `response` 下的 `model` 也改回逻辑名。
12. 只读观测面与状态台: `GET /state` 给出此刻进行中的请求, 以及每个上游的队列/并发上限/锁
    (纯内存, 不落盘、不参与记账); `-tui` 把它连同日志聚合成四个页的实时状态台。

## 快速开始

```bash
make build   # 源码在 src/, 二进制产出到根目录 (等价 cd src && go build -o ../llm-request-tollgate .)
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (17 组用例 128 项断言):
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

# 按 上游/模型 × 窗口 (3h/12h/24h/全部) 看 token 分解:
./llm-request-tollgate -usage var/

# 实时状态台 (TUI: active 进行中的请求 + 上游状态 + 模型与路由聚合 / events 事件 / settings 调度参数与优先级 / sticky 粘性偏好):
./llm-request-tollgate -tui                     # 同进程起代理 + 状态台 (q 退出)
./llm-request-tollgate -tui-state               # 只看已常驻的服务 (默认 http://127.0.0.1:8787)
./llm-request-tollgate -tui-state <base>        # 只看指定地址上的常驻服务
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 状态台 (TUI)

四个页, `1`-`4` 或 `Tab` 切, `up/down`/`PgUp/PgDn` 滚动, `r` 重读日志, `q` 退出。
`sticky` 页用 `up/down` 选候选, `Enter` 置偏好, `c` 清除该逻辑名的偏好。
界面按终端尺寸铺开: 分块与表格之间留空行, 键位提示贴最后一行, 列宽按内容自适应、右栏顶到右边界。

表格统一是加粗表头 + 行 + 超出窗口时的 `N 行, 当前 a-b`; 列宽按内容自适应 (长的列自己撑开,
一路顶到终端右边界, 顶满之后才截断, 代码里没有写死的列宽上限),
一整列都没内容的列自动收起 (没有进行中的请求时列名照常显示)。第 1 页的活动分块各有灰字标题行,
分块下面的上游状态表与 模型/路由聚合表不画标题 (列名行保留), 表与表之间空一行。
第 1 页的列名固定在页签下方 (不随内容滚走), 分块之间不空行 (标题自带分隔), 每块的数据行区
按最近一段的采样留 1 至 6 行: 每个采样先算各块笔数占当时在飞笔数的比例, 再按高斯权重对各采样
的比例取加权平均 (越远越轻, 没有硬截断; 某刻挂着很多笔也不独占统计), 加权平均乘同时在飞笔数
的峰值得到参考行数; 权重的时间尺度是实测平均耗时 t0 (从开始请求信号量到请求结束, 按日志里全部
能读到的请求算, 不看回看窗口), 停留 t0 那么久的采样权重 0.8 (`0.8^((age/t0)²)`), 权重低于 0.001
的采样 (约 5.6 倍 t0 之前) 不再参与;
参看时刻是当前时间 (此刻有进行中的请求) 或最后一次请求结束的时刻 (空闲时), 于是空闲一阵版式也
不塌; 最后按 `max(此刻笔数, 参考行数, 1)` 校准 (笔数多按实际留, 空块最少 1 行), 请求换阶段时
下面的块不会上下跳; 还没有样本时退回对端的并发水平 (10min 分片峰值按年龄指数衰减) 与停留时长
占比, 再没有就用缺省档;
分块与下面两张表一起滚动。界面第 2 行是实时状态 (状态 live / no-state, 进行中请求数,
路由数, 首字节 平均/谷值, 进行中请求的字节流量, 运行时长, 窗口时长), 第 3 行是统计状态
(已读日志条数, 窗口内请求数 / 错误数 / 换候选次数 / 总 token 含缓存占比, 最近报错放行末);
两行各固定一行, 超宽截断, 内容区高度随之缩小, 键位行仍贴底。

颜色全台一套: 绿 = 健康 (live / ok / 解锁 / 错误数 0), 红 = 错误与熔断 (错误 / 熔断锁 /
连续错误 > 0), 浅红 = 额度锁 (与正红同属错误类, 用明度区分「钱」与「坏」), 黄 = 等待与
降级 (退避 / 低性能锁 / 换候选 / 排队 / 超发试探 / 顶部
错误提示), 灰 = 说明 (标题 / 位置行 / 空), 状态色用正色 (31 / 32 / 33, 浅红 91); 另外每个
上游一种专属色 (青 / 品红 / 蓝系, 外加亮红 / 亮绿 / 亮黄, 与状态的正色区分), 只画在
上游名上 (各页的 `上游/模型` 单元格同色)。表头加粗, 当前选中行反显。

| 页 | 看什么 | 数据来源 |
|---|---|---|
| `active` | 进行中的请求**按流程阶段分块**: 并发池 / 频率队列 / 状态锁 / 已发送 / 接收中 / 已失败。每行列序是 编号 / 逻辑名 / 流式 / 上游/模型 / 耗时 / 状态 (不画路径与来源), 耗时列写总耗时, 括号里是从第一段到手边这一段的各段耗时 (以 + 相连, 没到过的阶段不补 0, 当前段实时走): 并发池 + 频率队列 + 状态锁 合计不超过 4s 时三段并成一段, 只写 发送前 / 等待中 / 接收中 (如 `3s(0.5s+1s+1.5s)`); 超过 4s 才按 并发池 / 频率队列 / 状态锁 / 发送 / 接收 摊开 (如 `4.7s(0.2s+0s+4.3s+0s)`), 块标题带当前笔数, 状态列只写卡住的原因 (退避 / 熔断锁 / 额度锁倒计时), 接收中的行则按模型/路由表的同三个速度写这笔的实时吞吐 (不加前缀): 解码 = 已收到的输出 token 除以首字节到达至今 (如 `48.7 t/s`), 总 = 同一份输出 token 除以请求总耗时 (如 `30.0 t/s`), 下载 = 已转发的响应字节除以首字节到达至今 (如 `12 KB/s`), 没有 token 可计时只写下载吞吐; 各块的数据行区高度按最近 5min 的采样定参考: 每个采样先算各块笔数占当时在飞笔数的比例, 再对各采样的比例取平均, 平均值乘窗口内同时在飞笔数的峰值; 最后按 `max(此刻笔数, 参考行数, 1)` 校准 (笔数多按实际留, 空块最少 1 行, 有空行时 6 行封顶); 最近 5min 没请求时窗口前推, 无样本时用缺省档。分块下面是两张不画标题的表 (表间空一行): 上游状态 (按 provider 优先级降序, 同优先级保持配置顺序, 与 `settings` 页同口径): 列是 上游 / 并发 (`占用/上限`, 有超发试探时占用后带 `+1`) / 排队 (有排队时附 `[距下次出队]`) / 错误 (连续错误) / 记账 (默认组, 未锁 `-`), 之后只在该行后面铺开它自己声明的记账组 (列标题留空, 未锁显示绿色组名, 锁中换成红色倒计时占位; 组少的行后面留空); 与**有过请求的** `上游/真实模型` 的队列 + 路由聚合, 所有数据都是 `0` / `-` 的行不显示, 列按类别分组: 调度状态 (排队 / 退避 / 熔断)、流量 (Token = cached/total / 首字节 = 平均/谷值 / 三种速度各一列, 都写作 `平均/峰值`: 解码 = completion/decode, 总 = (prompt+completion)/duration, 字节 = bytes/duration; 平均 = `rate_window_sec` 窗口内各样本的算术平均, 峰值/谷值 = 回看窗口 (3h) 内单笔的最高速度 / 最低时延; 缺字段的格子显示 `-`, 整列全 `0`/`-` 的列自动收掉)、时间 (最近); 统计按**逻辑名分组** (同一 `上游/模型` 被多个逻辑名引用时各归各的组, 组与组之间空一行; 观测面的排队/退避/熔断等实时状态按配置候选链归回引用它的逻辑名组 (窗口内没日志也算), 配置里也没有归属的才排在末尾组; 只有连错计数而无任何其它数据的行不显示), 表里不出现逻辑名, 队列空了也保留到进程重启; 当前粘性偏好的候选在该组的 `上游/模型` 前标 `*` | 观测面 (同进程读内存, 另进程 `GET /state`); 聚合读 JSONL 日志 (含跨天归档, 窗口 3h); 上游顺序读 store 的 `tollgate_priority` |
| `events` | 三列: 时间 / 编号/逻辑名 / 事件。标识列一套格式对应三类主体: 请求写 `编号/逻辑名`, 模型写 `供应商/模型`, 供应商写 `供应商` (锁 / 探测这类不挂请求的事件不再显示逻辑名)。只记会改变程序状态的事件: 请求逐阶段 `收到请求, 等待信号量` → `获得信号量, 加入频率控制队列` → `出频率控制队列, 加入 供应商/模型 模型队列` → `发送请求` → `收到响应头, 开始接收` → `接收结束, 状态码 200`; 换候选写 `旧结果, 换到 新候选, 等待信号量` (失败回退不单独成行), 收尾写 `候选链结束, 状态码 N`; 锁写 `连错到阈值, 触发 熔断锁, 05:00` / `额度限制, 触发 额度锁 (组 g1), 05:00` / `吞吐 ... , 触发 低性能锁, 05:00`, 另有 退避 / 探测 / 解锁 / 启动 / 热载。仅仅首字节慢、没有触发任何锁的请求不留事件。最新的在最上面, 连续相同事件折成 `xN` | 服务事件流 + 日志还原 |
| `settings` | 调度参数 + 权重编辑, 三列并排: 第 1 列 **全局参数** (按同类归组: 调度等待 `出队间隔/等并发槽位/排队上限/退避序列` → 锁时长 `并发锁/额度锁/熔断锁/低性能锁` → 低性能判定 `窗口/低吞吐峰值/低吞吐平均/高延迟阈值` → 显示窗口 `速度窗口/采样尺度` → 恢复探测开关, 字段中文名+冒号+值+单位, 时间按秒可小数) 加 **供应商** 面板 (按优先级降序), 第 2 列 **模型** 列表 (行首 `M` 模型优先级 / `P` provider 优先级), 第 3 列 **预览** (所选别名的候选顺序实时预览)。区标题一律中文; 焦点区标题加 `[ 标题 ]` 括起并单独着色。`m` / `p` / `g` 分别聚焦模型 / 供应商 / 全局参数, `+` / `-` 调值, `=` 输入 (时间字段为秒), `s` 保存并热加载 | 配置 `config.local.json` 的扁平全局键 + store `~/fff/config/llm-models.json` (两个都可写) |
| `sticky` | 粘性偏好选择: 按逻辑名列出配置里的候选链 (配置顺序 = fallback 顺序), 当前偏好标 `*`, 光标行反显; `up/down` 选候选, `Enter` 置一次 (立即生效, 自动迁移规则照旧), `c` 清除该逻辑名偏好 (回落配置首候选), 失败原因写红色消息行 | 候选链读配置 `aliases`; 当前偏好读观测面 `sticky`; 写入走 `POST /sticky` |

- 观测面是**只读且纯内存**的: 不落盘、不参与记账与调度, 服务重启即清空; 明细的事实来源
  仍是日志 (设计 D12)。同进程模式顺手把它写成 `var/tui-pulse.json`, 也便于别的工具读。
- 两个入口二选一: `-tui` 适合"本地起一份看" (同进程), `-tui-state` 适合看 systemd 常驻那份
  (`./deploy/tollgate.sh` 起的服务不变)。`settings` 页两种模式都能改: 写本机的 store 与
  配置的扁平全局键, 同进程模式热加载直接生效, 观察常驻服务时发 `SIGHUP`。`s` 只写全局
  键与优先级; 全局键与 `timeouts` 都可热加载 (改配置后 SIGHUP 即生效)。
- `sticky` 页两种模式都能改: 写入统一走 `POST /sticky` (同进程发给自己,
  `-tui-state` 发给被观测服务), 不写文件; 置一次后自动迁移规则照旧。
- 列宽按内容分配 (长的列自己撑开, 顶到终端右边界为止), 余量先补齐差得少的列; 只有连各列
  最小宽都放不下时才按优先级丢列 (先丢流式、编号这类), 至少保留逻辑名、耗时、上游与真实
  模型、状态。
- 时间段写成 `12s` / `02:03` / `01:02:03` (前导的零段省掉), 时刻仍是 `hh:mm:ss`。

## 常驻与升级

- 安装常驻: `./deploy/tollgate.sh install` (systemd 用户服务, 崩了自动拉起)。
- 日常操作: `./deploy/tollgate.sh <status|logs|restart|stop|start|hup|uninstall>`
  - `restart` 更新代码后用它: 先 `make build` (源码在 `src/`, 二进制产出到根目录)
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
- 时间键全部平铺在**顶层**、以秒为单位 (`*_sec`, 允许小数); 代码内部按毫秒处理:
  `connect_sec` / `response_header_sec` / `stream_idle_sec` 建连 / 等响应头 / 流空闲。
- 门控参数同样平铺顶层 (`*_sec`): `interval_sec` 出队节拍 (缺省 0.5s), `sem_wait_sec`
  等信号量上限 (5s), `queue_wait_sec` 排队上限 (10s), `concurrency_lock_sec` /
  `quota_lock_sec` / `breaker_lock_sec` / `lowperf_lock_sec` 各把锁的时长
  (1h / 10min / 5min / 5min), `lowperf_window_sec` 低性能锁的判定窗口 (10min) 与
  低吞吐两道门槛 `lowperf_peak_max` (5) / `lowperf_avg_max` (1) —— 单位都是 tokens/s,
  峰值与平均都在这两条线以下才算低吞吐; `lowperf_ttfb_max_sec` 高延迟阈值 (平均首字节
  时延秒, 缺省 0 = 关闭该项, 低吞吐与高延迟任一命中即触发低性能锁);
  `lowperf_exempt` 低性能锁豁免名单 (真实模型 id 数组, 缺省空): 名单内的模型禁止触发
  低性能锁; `retry_backoff_sec` 通用兜底的退避序列 ([10,20,30])。并发上限不在配置里
  (起点固定 1, 每次重启重新爬)。
- `rate_window_sec` (缺省 3600) 与 `block_window_sec` (缺省 35): TUI 显示窗口 ——
  速度平均/峰值统计窗, 与 active 页分块采样的时间尺度 (settings 页的全局参数区可改)。
- `probe_enabled` (缺省 `true`): 锁恢复探测开关。额度 / 熔断 / 低性能锁到期后不直接解锁,
  先发一笔内置的最小请求 (`max_tokens=1`) 验证可达性, 探测通过才真正解锁, 失败按原锁时长
  重新加锁。探测在锁到期时由锁自身的定时器触发, 命中恢复态的真实请求也会补触发一笔 (但
  请求本身仍按锁定进下一个候选, 不等待不消费); 探测改走 `chat/completions`, 不经主链路,
  不计入吞吐 / 性能样本。`probe_enabled` 可在 settings 第 3 页的全局参数区编辑并热加载;
  设为 `false` 恢复旧行为 (锁到期即放行)。
- `upstreams`: 每个上游一条记录 (id / base_url / auth_header / key 或 key_env / headers /
  include_usage / errors), 调度状态按 id 分组 (一个上游一把 key, 即「上游 + 凭据」分组)。
  `errors` 是错误分类规则 (逐家规则见 [docs/providers-api.md](docs/providers-api.md),
  缺省空表 = 全部走通用兜底): 每类一个规则数组, 单条规则
  `{ "status": [429], "contains": ["quota exceeded"] }` —— 先看 `contains` 任一关键词是否
  出现在匹配文本 (`error.type` + `error.code` + `error.message`) 里, 命中即定类 (与状态码
  无关); 关键词都判不出来时, 才用只写了 `status` 的规则按状态码兜底。
  判定顺序 `quota → concurrency → 其他`。
  `key_env` 只存环境变量名, 真 key 启动时从环境取 (密钥不落盘, 见「常驻与升级」)。
  `key_prefix` 是认证头里的 scheme: 缺省对 `Authorization` 用 `Bearer `, 其他头不带
  (ustc 与火山 ARK 实测都要 `Bearer `; 给 `""` 可强制裸 key)。
  `models` (可选) 是该上游提供的真实模型清单, 每条 `{ "id": "...", "billing_group": "..." }`:
  `billing_group` 是**记账组**, 同家同名一组, 额度锁只锁组; 不写组名 = 该上游的默认组,
  整个上游不写 `models` = 所有模型都在默认组 (与加组之前的配置完全等价)。声明了 `models`
  的上游, 别名引用的模型必须在表内, 否则启动 / 热加载直接报错。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。

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
- 模型的 `tollgate_priority` 为负时该模型停用: 生成配置时跳过, 不进候选
  (aliases), 也不进上游的 `models` 声明/额度组; 想临时下掉某个上游模型就把它
  的优先级改成负数, 恢复时改回非负即可。
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
`m` / `p` / `g` 切模型 / provider / 调度参数, `+/-` 调值 (模型优先级减到 -1 即停用,
再按 `-` 不再减小; 该行仍显示、预览里排最后、可调回, 但生成配置时被跳过,
不会出现在 tollgate 模型目录), `=` 输入数值, `/` 过滤,
`s` 保存并生成: 优先级写 store, 调度参数写配置, store 有改动时顺带重生成
`tollgate.json` (aliases/upstreams 与 store 保持一致), 最后热加载一次到位;
`h` 热加载, `q` 退出。

```bash
./llm-request-tollgate -tui-state   # 看常驻服务, 按 3 进 settings 页
```

### 用量分析 (计划中)

服务只写日志, 不做窗口累计、不折算额度、也没有状态文件 —— 预算与用量分析交给一个独立的
分析程序 (设计文档 §5.3 O10, 语言不限, 只读日志, 不随服务启动), **尚未实现**。
现在能用的只有代理自带的两个离线子命令: `-summary` (请求数 / 429 / 等待 / 缺 usage /
token) 与 `-usage` (上游 + 模型 × 窗口的 token 分解)。

## 当前边界

- 转发覆盖 `POST /v1/chat/completions` 与 `POST /v1/responses` (O5); `GET /v1/models`
  返回逻辑名清单 (O5)。只读观测面: `GET /state`。调试用 `POST /unlock` 强制解锁
  (清掉运行中代理内存里的锁与失败计数, 可用 `?upstream=&model=&group=` 过滤);
  `POST /sticky` 人工置一次偏好 (`?logical=&upstream=&model=`, 不给 upstream 即清除,
  纯内存, 之后自动迁移规则照旧)。其余路径一律 404。
- 被闸门挡下的 429 在日志里带 `gate` (`locked` 候选在锁里 / `sem_timeout` 等不到并发
  槽位 / `queue_timeout` 排队超时 / `canceled` 客户端断开) 与等待时长 (`sem_ms` 等信号量、
  `queue_ms` 排队, 都是各候选之和), 便于区分「并发不够」「排队超时」和「被锁」。
- 不做负载均衡 (D15): 候选按列表顺序取第一个可用的, 不按权重 / 轮询 / 延迟分流;
  闸门被拒时也不溢出到其它空闲候选。
- 集中 fallback 与定时锁只覆盖首字节前 (消耗 200 以外的响应与传输失败); 首字节之后不切后端。
- 归档日志不自动清理, 也不压缩; 留存与清理交给外部 (logrotate 或手动)。
- 真上游已接入 (设计文档 §5.4 那份清单已满足); 上线前还缺切流顺序与回滚、以及「没有进程
  绕开代理」的验证 (设计文档 §5.3 O4)。

## 目录

- `src/` 整个 Go 模块 (源码 + `go.mod`): `*.go` 代理本体按职责分文件 —— `main.go` 入口,
  `config.go` 配置, `errclass.go` 报错分类, `gate*.go` 调度与锁, `serve*.go` 转发主链路,
  `relay.go` SSE 透传, `observe*.go` 观测面, `log.go` / `usage.go` 日志与用量,
  `rate.go` 吞吐口径, `summary.go` 汇总命令 (含假上游自测所需的全部实现);
  以及 `src/test/fakeupstream/` 假上游 (回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流, 验证 F6)。
- `analysis/` 历史请求日志的分析脚本 (只读 `var/` 的 JSONL 归档, 评估锁阈值)。
- `Makefile` 根目录唯一构建入口 (build / vet / test / fmt 都在 `src/` 下跑, 二进制产出到根目录)。
- `test/run_selftest.sh` 自测: 17 组用例 128 项断言, 对本地假上游跑代理。
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
- `tui_*.go` 状态台: `tui_run.go` 主循环与按键, `tui_term.go` 终端与画布, `tui_table.go`
  表格, `tui_flow*.go` / `tui_rows.go` active 页, `tui_models.go` / `tui_events.go` 后两页,
  `tui_settings*.go` / `tui_gate.go` / `tui_store.go` / `tui_prio*.go` settings 页调 gate
  参数与权重 (写配置与 store, 候选顺序实时预览, 界面内即可保存 / 重生成配置 / 热加载)。
- `src/` 下的 `gate_test.go` / `proxy_test.go` / `tui_*_test.go` 单元与端到端测试
  (`cd src && go test .`): 调度核心 (排队节拍、试探放宽、四类锁、错误归类、退避熔断、
  锁落盘)、经代理转发的完整链路, 以及状态台版式与 settings 页存盘。
- `test/tmp/` 与 `var/` 是运行产物 (自测二进制/假上游日志; 请求日志与 `gate-state.json`
  等状态), 都在 `.gitignore` 内。
