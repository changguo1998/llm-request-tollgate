# llm-request-tollgate 部署与运维

部署、日常操作、升级、备份与排障的操作手册. 接口语义见 [api.md](api.md), 设计见
[design.md](design.md), 配置结构见 [README](../README.md) §配置.

## 1. 快速上手

```bash
go build -o llm-request-tollgate .
# 从模板复制并填好配置 (密钥走环境变量, 见 §3):
cp config.example.json config.local.json
./llm-request-tollgate -config config.local.json
```

前置: 无第三方依赖 (标准库单文件 Go); 需要 Go 工具链构建.

## 2. 常驻部署 (systemd 用户服务)

### 2.1 安装

前置条件: 仓库根目录已有 `llm-request-tollgate` 二进制与填好的 `config.local.json`
(install 脚本会检查, 缺任一都拒绝装); 还要有 `deploy/tollgate.service` 模板 —— 公开
仓库只跟踪示例版, 需先 `cp deploy/tollgate.service.example deploy/tollgate.service`.

```bash
./deploy/tollgate.sh install      # 等价于 bash deploy/install_service.sh
```

install 脚本做四件事: 把 `deploy/tollgate.service` 模板里的 `__ROOT__` 替换为仓库绝对
路径, 写入 `~/.config/systemd/user/tollgate.service`; `daemon-reload`; `enable --now`
(开机自启 + 立即启动); 打印状态.

### 2.2 服务特性

- 用户服务, `WantedBy=default.target`, 开机自启.
- `Restart=on-failure`, `RestartSec=3`: 崩了自动拉起.
- `TimeoutStopSec=10`: 停止上限 10s.
- 工作目录固定为仓库根 (日志等相对路径相对它解析).

### 2.3 卸载

```bash
./deploy/tollgate.sh uninstall    # 停用 + 删 unit + daemon-reload
```

不删除二进制与数据 (var/ 等), 只摘掉服务.

## 3. 密钥管理

原则: 真 key 只存在于进程环境变量 (`key_env`), 不落任何盘; 服务不读配置文件里的明文
key (`key` 字段仅在临时/测试配置里用).

### 3.1 systemd 服务怎么拿到密钥

本机 dotfiles 维护的 unit (`deploy/tollgate.service`, 不进公开仓库; 公开示例见
`deploy/tollgate.service.example`) 的 `ExecStart` 包了一层 shell, 顺序:

1. `source ~/fff/config/shellrc.sh` 拿 FFF_HOME / PATH 等;
2. `source` fff 的 `aliases.d/25-env.sh` 与 `aliases.d/10-ai.sh`, 拿到 `fffenv` 与
   `aikey` 两个函数 (不 source ~/.bashrc: 非交互 shell 里它提前 return, 不会定义
   aikey);
3. `aikey all` 遍历 `~/.fff/keys/*.ai` 逐个 fffenv, 把密钥加载进本进程环境;
4. `exec` 真正的代理进程.

所以重启后不需要任何手工导入. 加载结果 (`Loaded xxx.ai` 之类) 进 journal:
`./deploy/tollgate.sh logs`.

### 3.2 交互 / 手动运行时

```bash
export USTC_API_KEY=...     # 名字与 ~/.fff/keys/*.ai 一致
export GROUP_API_KEY=...
./llm-request-tollgate -config config.local.json
```

systemd 场景如不想走 fff 加载, 也可以手动导入后重启:

```bash
systemctl --user import-environment USTC_API_KEY GROUP_API_KEY ARKCP_API_KEY ARKAP_API_KEY DEEPSEEK_API_KEY
# 或 systemctl --user set-environment USTC_API_KEY=...
./deploy/tollgate.sh restart
```

### 3.3 排查

```bash
./deploy/tollgate.sh logs     # 看 Loaded xxx.ai
ps -o cmd= -p "$(systemctl --user show -p MainPID --value tollgate.service)"
```

`-check` 会校验 `key_env` 对应环境变量是否存在 (缺了报错), 可用来尽早发现加载问题.

## 4. 日常操作

`deploy/tollgate.sh <cmd>` 包装 systemctl 用户服务; 没连上 user bus (定时任务 / 沙箱)
时 `status` 自动退回进程视角 (`pgrep`), 避免误报故障.

| 命令 | 作用 |
|---|---|
| `status` | 运行状态 (systemctl, 拿不到 bus 时退进程检查: pid / 二进制构建时间) |
| `logs` | `journalctl -f` 跟看服务日志 (Ctrl-C 退出) |
| `restart` | 重启 (改代码重建后用它) |
| `stop` / `start` | 停止 / 启动 |
| `hup` | `SIGHUP`, 热加载配置 (别名 / 上游 / gate), 不用重启 |
| `uninstall` | 停止并删除服务 |

### 4.1 热加载 (SIGHUP)

`config.local.json` 改别名 / 上游 / `gate` 参数后 `./deploy/tollgate.sh hup` 即可, 不
重启. 热加载时调度状态按上游 id 保留: 队列里排着的请求、爬到的并发上限、退避与锁都不
清.

注意: 超时参数 (`timeouts`) 与 `probe` 参数是启动时固化的 (Transport / 探测参数启动时
一次性确定), 改它们要重启.

## 5. 升级 / 回滚

### 5.1 升级

```bash
git pull                    # 或手动更新到目标版本
go build -o llm-request-tollgate .
./deploy/tollgate.sh restart   # 二进制被替换不会自动重启, 需手动 restart
```

重启后并发上限从 1 重新爬 (不落盘), 锁与退避状态从 `var/gate-state.json` 恢复.

### 5.2 回滚

```bash
# 方法一: git 回旧版本重建
git checkout <旧版本> && go build -o llm-request-tollgate . && ./deploy/tollgate.sh restart
# 方法二: 升级前先备份旧二进制
cp llm-request-tollgate llm-request-tollgate.bak
# 出问题时:
mv llm-request-tollgate.bak llm-request-tollgate && ./deploy/tollgate.sh restart
```

配置向后兼容; 锁状态文件跨版本以时间戳为准, 通常无需清理.

## 6. 配置生成与权重

### 6.1 由 fff 模型列表生成配置

本机版 `scripts/gen_tollgate_config.py` (在 dotfiles, 不进公开仓库) 读
`~/fff/config/llm-models.json` 与黑名单 `llm-models-droplist.json`, 生成
`upstreams` 与 `aliases` 写回 `config.local.json` (即 `~/fff/config/tollgate.json`).
公开仓库提供不依赖 fff 的示例: `scripts/gen_tollgate_config.example.py` +
模型列表示例 `scripts/llm-models.example.json`. 要点:

- 逻辑名取条目的 `tollgate_group`, 没有就是模型 id; 同 id 跨上游 / 显式同组归并成一个
  别名.
- 候选顺序 = `tollgate_priority` 降序 (同值看 provider 的, 再同值按 `--providers` 顺
  序); 0 最小, 缺省即 0.
- 代理对外模型列表继承候选的 `thinkingLevelMap` (取交集) 与最小的 `contextWindow` /
  `maxTokens`; 缺等级表时 agent 汇报 `UNSUPPORTED_REASONING_EFFORT` 拒发.
- `key_env` 取 store 里 `apiKey` 的占位符名; `auth_header` 等从现有配置继承.
- `--dry-run` 只打印; `--providers` 改上游与优先级. 不传时缺省是 store 里除 `tollgate`
  (代理自身) 与 `arkap` 外的全部 provider; arkap 要带就显式写进 `--providers`.
- 额度组取模型的 `tollgate_billing`, 生成到上游的 `models` (字段 `billing_group`); 同家同名
  一组, 不写归默认组, 整家都没写时不输出 `models`.
- 不在 store 里的手加上游在重生成时会丢 (`build_upstreams` 只认 store 里的 provider);
  要先在 store 里补 provider, 或重生成后手工补回.

```bash
# 本机版 (dotfiles 维护, 依赖本机 fff store):
python3 scripts/gen_tollgate_config.py --dry-run
python3 scripts/gen_tollgate_config.py
# 公开仓库示例 (不依赖 fff, 读 scripts/llm-models.example.json):
python3 scripts/gen_tollgate_config.example.py
```

### 6.2 用 TUI settings 页调权重与调度参数

状态台第 4 个页签 (settings) 上半是 `gate` 的 7 项调度参数 (写回 `config.local.json` 的
`gate` 段, 其余字段与格式原样保留), 下半是 store (`~/fff/config/llm-models.json` 里的
`tollgate_priority`), 右栏实时预览候选顺序. `m` / `p` / `g` 聚焦模型 / provider / 调度参数,
`+/-` 调值, `=` 输数值, `/` 过滤, `s` 保存并热加载, `r` 重生成配置, `h` 热加载, `q` 退出.

```bash
# 同进程起代理 + 状态台 (q 退出):
./llm-request-tollgate -tui
# 只看已常驻的服务 (settings 页改完 s/h = 发 SIGHUP):
./llm-request-tollgate -tui -tui-state http://127.0.0.1:8787
```

## 7. 日志 / 状态文件 / 离线分析

### 7.1 请求日志

- 当前日志: `log_path` (缺省 `var/requests.jsonl`), JSONL 追加写, 每行一笔 (字段见
  [api.md](api.md) §7).
- 轮转: 跨本地日期 (或隔天启动) 把上一段归档为 `<base>-YYYY-MM-DD.jsonl`; 同名不覆盖,
  依次加 `-1` / `-2`; 归档**不自动删除不压缩**, 留存交给外部 (logrotate 或脚本).
- `-summary` 与 `-usage` 都跨归档读; 传目录则扫目录下所有日志.

### 7.2 状态文件 (日志同目录, 均 gitignored)

| 文件 | 内容 | 生命周期 |
|---|---|---|
| `requests.jsonl` 与归档 | 每笔请求明细 | 永久保留, 外部清理 |
| `gate-state.json` | 两把锁 (额度 / 熔断) 的到期时间与原因 | 锁内存在, 重启恢复 |
| `probe.json` | 探测 / 被动延迟采样 | 启用 probe 时写, 重启恢复 |
| `tui-pulse.json` | 每秒观测快照 (同进程 `-tui` 时) | 仅 `-tui` 时 |

`/v1/state` 与 `/v1/probe` 的 HTTP 面是同一份数据的只读输出, 见 [api.md](api.md) §6.

### 7.3 离线子命令

```bash
./llm-request-tollgate -config config.local.json -check      # 校验配置 (缺 key_env 会报错)
./llm-request-tollgate -summary var/requests.jsonl           # 按 逻辑名×来源 汇总 (请求/429/token)
./llm-request-tollgate -summary var/                         # 传目录则跨天归档一起汇总
./llm-request-tollgate -usage var/ -windows 3h,12h,24h       # 上游+模型 × 窗口 token 分解
./llm-request-tollgate -probe config.local.json              # 离线打印探测报告 (只读)
```

## 8. 自检与自测

### 8.1 自检 (二进制 / 配置 / 端口)

```bash
./test/healthcheck.example.sh    # 公开示例版: 缺省 config.local.json; 可传路径
```

三项: 二进制存在 -> `-check` 配置可解析 -> `/v1/models` 返回 200. 退出码 0 = 全通过.
密钥 (key_env) 要自己 export; 本机 dotfiles 维护的 `healthcheck.sh` 比示例版多一步
"缺密钥时按服务那套 fff 加载自动重试".

### 8.2 全量自测 (假上游, 不影响真上游)

```bash
./test/run_selftest.sh all       # 17 组用例, 每用例独立起假上游 + 代理
./test/run_selftest.sh <case>    # 跑单组: alias|keyenv|fallback|fallback-gate|breaker|quota|rotate|stream|usage|usage-report|ratelimit|logging|overhead|responses|hup|probe
```

产物在 `test/tmp/` (gitignored). 用例覆盖: 白名单 404 / 认证替换 / fallback 与闸门 /
熔断与额度锁 / 隔天轮转 / 流式透传与断流 / usage 提取与兜底 / 节拍与并发挡下 / 日志字
段 / 开销 / /v1/responses / SIGHUP 热加载 / 探测.

## 9. 排障

| 症状 | 排查方向 |
|---|---|
| 服务起不来 / 崩了重启 | `./deploy/tollgate.sh status`; systemctl 失败原因看 `journalctl --user -u tollgate.service -e` |
| 提示缺二进制 / 缺配置 | install 会检查; 先 `go build -o llm-request-tollgate .` 与备好 `config.local.json` |
| 密钥没加载 (401) | `./deploy/tollgate.sh logs` 看 `Loaded xxx.ai`; `-check` 验证 key_env |
| 一片 429 | 先分 `gate` 字段: `locked` 看 `var/gate-state.json` (锁到几点、什么原因); `sem_timeout` / `queue_timeout` 是并发 / 排队不够, 调 `gate` 参数 |
| 某模型持续 429 / 慢 | 熔断锁只锁 `上游/模型`, 换别的模型或等锁到期; 看 `/v1/state` 的 `cands[].models` |
| hup 后不生效 | 确认改的是别名 / 上游 / `gate`; `timeouts` 与 `probe` 参数要重启 |
| 上游报错种类怪 | `unclassified upstream error` 会打进服务日志, 按 docs/providers-api.md 补 `errors` 规则后 hup |
| 日志磁盘增长 | 归档不自动清理, 配 logrotate 或定时删除 |
| 端口被占 | `listen` 改别的端口, 或找占用进程 |

## 10. 数据与缓存目录 (可清理)

| 路径 | 内容 | 处置 |
|---|---|---|
| `var/` | 请求日志 + 状态文件 | 保留 (日志是事实来源); 归档清理按 §7.1 |
| `test/tmp/` | 自测产物 (假上游日志 / 二进制) | 自测每次重建, 可随时删 |
| `tmp/gocache`, `.cache/gocache` | Go 构建缓存 (自测用 GOCACHE) | 可删, 会重建 |
| `.ruff_cache/` | ruff 缓存 (生成脚本 lint 用) | 可删, 会重建 |
| `scripts/__pycache__/` | Python 字节码缓存 | 可删, 会重建 |
| `config.local.json` | 本机配置软链 -> `~/fff/config/tollgate.json` | **不可删**; 别提交进 git |
| `deploy/tollgate.service` / `scripts/gen_tollgate_config.py` / `test/healthcheck.sh` | 本机 fff 版脚本, 公开仓库只跟踪对应 `.example` | 别提交进 git (已 ignore); 删了可从 dotfiles 恢复 |
