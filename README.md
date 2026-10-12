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
   提取 token 用量 (含缓存命中 token, 三种上游字段名都认); 缺 usage 按字符粗估 (F7)
   并记 `missing_usage`。`-usage` 按 上游/模型 × 窗口 列出 token 分解, 用于标定预算系数。
5. 预算自适应 (D12/D13): 不提前拦截请求; 上游返 429 即视为限额信号 — 该上游
   (provider+key) 进冷却 (`budget.cooldown_ms`, 默认 5 分钟), 请求自动切下一个候选, 并用
   撞墙时刻的窗口累计反推折算系数 (落 `var/budget-units.json`, 重启不丢)。
6. 响应整形: 非流 / 流式 chunk 里的 `model` 字段改回请求的逻辑名,
   对外隐藏真实上游 id。
7. 运维面: `GET /v1/models` 返回逻辑名清单; 请求头 `X-Tollgate-Source`
   记入日志 `source` 字段; `-summary` 汇总请求日志; `deploy/` + `test/healthcheck.sh`
   常驻与自检。
8. 集中 fallback: 候选按序尝试, 仅首字节前 (传输失败 / 上游 429 / 5xx) 自动切换
   下一个; 全部失败保留最近一次错误 (状态码/头/体) 原样透传; 其余 4xx 不重试。
9. 熔断: 每上游连续失败 (默认 5 次) 打开, 冷却期 (默认 30s) 内跳过该候选不再真发,
   冷却后自动恢复; 参数可配 (`breaker_fail` / `breaker_cooldown_ms`)。
10. 限额冷却与系数标定 (D13): 上游 429 后该上游冷却期内不再真发 (日志 `cooldown_skips`),
    候选全在冷却时返 429 `upstream_cooling` + `Retry-After`; 额度类 429 会用窗口累计
    反推 `unit`, 状态存 `var/budget-units.json` (配置里的 `units` 仍优先)。
11. 日志按天轮转: 跨本地日期 (或隔天启动) 时把上一段日志归档为
    `<base>-YYYY-MM-DD.jsonl` (同名不覆盖, 依次加 -1/-2); 预算聚合与 `-summary`
    都跨归档读, 归档不自动删除 (留存交给外部)。

## 快速开始

```bash
go build ./...
# 填好 config.local.json (从 config.example.json 复制) 后:
./llm-request-tollgate -config config.local.json
# 起假上游 + 全量自测 (91 项断言):
./test/run_selftest.sh all
# 常驻 (systemd 用户服务, 崩了自动拉起):
./deploy/install_service.sh
# 自检 (二进制 / 配置 / 端口):
./test/healthcheck.sh
# 汇总请求日志 (按 逻辑名+来源, 含 token 用量; 传目录则跨天归档一起汇总):
./llm-request-tollgate -summary var/requests.jsonl
./llm-request-tollgate -summary var/
# 按 上游/模型 × 窗口 (3h/12h/24h/全部) 看 token 分解, 用来标定预算系数:
./llm-request-tollgate -usage var/
```

agent 侧只需把 base URL 指向 `http://127.0.0.1:8787/v1` (端口可配)。

## 常驻与升级

- 安装常驻: `./deploy/tollgate.sh install` (systemd 用户服务, 崩了自动拉起)。
- 日常操作: `./deploy/tollgate.sh <status|logs|restart|stop|start|hup|uninstall>`
  - `restart` 更新代码后用它: 先 `go build -o llm-request-tollgate .`
    再 `./deploy/tollgate.sh restart` (二进制被替换不会自动重启, 需手动 restart)。
  - `hup` 改 `config.local.json` 别名/上游后热加载, 不用重启。
- 自检: `./test/healthcheck.sh`。
- **重启后起不来?** 多半是缺 key 环境变量 (服务只从环境读, 不落盘)。常驻做法:
  `~/fff/config/systemd/fff-keys.service` (软链到 `~/.config/systemd/user`, `enable` 后开机把
  `~/.fff/keys/*.ai` 导入用户环境), 本服务模板里已 `Wants=/After=` 它; 手工兜底:
  `for f in ~/.fff/keys/*.ai; do set -a; . "$f"; set +a; done` 后
  `systemctl --user import-environment USTC_API_KEY GROUP_API_KEY ARKCP_API_KEY ARKAP_API_KEY`。
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
  `<base>-YYYY-MM-DD.jsonl`, 见「做什么」第 11 条)。
- `timeouts`: 建连 / 等响应头 / 流空闲 / 排队上限。
- `upstreams`: 每个上游一条记录 (id / base_url / auth_header / key 或 key_env / headers /
  include_usage / limits / budget), 限速参数挂在记录上, 即「上游 + 凭据」分组。
  生成器给每个上游都写 `budget.cooldown_ms` (429 冷却, 默认 5 分钟); `windows` / `units`
  按平台额度人工补 (ustc 已填 key 记录里的 3h 30 / 12h 70 / 24h 100)。
  `key_env` 只存环境变量名, 真 key 启动时从环境取 (密钥不落盘, 见「常驻与升级」)。
  `key_prefix` 是认证头里的 scheme: 缺省对 `Authorization` 用 `Bearer `, 其他头不带
  (ustc 与火山 ARK 实测都要 `Bearer `; 给 `""` 可强制裸 key)。
  `budget`: `{ windows: {"3h":30,"12h":70,"24h":100}, unit:1, units:{}, cooldown_ms:300000,
  refresh_sec:60 }` — `windows` 是滚动窗口限额 (用于标定与诊断), `unit`/`units` 是 token
  折算平台额度的系数 (模型级 `units` 优先, 其次 `unit`, 再次撞墙标定值), `cooldown_ms`
  是 429 后的冷却时长, `refresh_sec` 是定时扫日志的间隔。
- `aliases`: 逻辑名 -> `[[上游 id, 真实模型 id], ...]`。

别名与上游支持 `SIGHUP` 热加载, 限速器状态按上游 id 保留不被重置。
预算累计每次定时扫描从日志重建, 不设独立状态 (日志即账本)。

### 从 fff 模型列表生成配置

`scripts/gen_tollgate_config.py` 读 `~/fff/config/llm-models.json` (provider -> baseUrl/models,
由 fff 的 `fff-update-*-llm-list.py` 维护) 与 `llm-models-droplist.json` (精确匹配黑名单),
生成 `upstreams` 与 `aliases`:

- 别名的逻辑名取条目里的 `model_group`, 没有就是模型 id 本身; 同名 (同 id 跨上游, 或显式
  `model_group`) 的归并成一个别名, 候选顺序 = `--providers` 顺序 = fallback 优先级。
- `model_group` 手工维护, 但不会被 store 更新脚本冲掉 (`merge_models_by_id` 继承 provider
  API 不提供的字段)。跨上游同族不同名的模型 (如 ustc 的 `deepseek-flash` 与 ark 的
  `deepseek-v4-1-flash*`) 就靠它并成一个别名。
- `key_env` 取 store 里 `apiKey` 的占位符名; `limits` / `budget` / `auth_header` 等设置从
  现有配置继承, 不会被覆盖 (缺失的 `budget.cooldown_ms` 会补默认值, `--cooldown-ms` 可改)。
- 写入 `config.local.json` (即 `~/fff/config/tollgate.json`); `--dry-run` 只打印,
  `--providers` 改上游与优先级。

```bash
python3 scripts/gen_tollgate_config.py --dry-run
python3 scripts/gen_tollgate_config.py
```

### 预算系数怎么定

`budget` 的限额与 `unit` 取决于平台额度口径, 标定一次即可:

1. `./llm-request-tollgate -usage var/` 看某模型在窗口内的 `total` (同时看 `miss`: 不为 0
   说明有估的用量, 系数会偏)。
2. 打开平台网页面板读同一窗口的"已用额度"。
3. 系数 = 已用额度 / `total`, 填进该上游的 `budget.units` (键是真实模型 id)。

参考: 平台公开发布每模型价格 (`GET /api/models/public`, 无需登录), 例
`deepseek-flash` 输入 2.0 / 缓存命中 0.2 / 输出 2.0 (每百万 token), qwen 系 1.0/0.1/1.0,
`glm-5.3-flash` 4.0/0.4/4.0。若平台额度就是这些价格单位, 系数即 价格/1e6。

## v1 边界

- 转发只做 `POST /v1/chat/completions` (D4); `GET /v1/models` 返回逻辑名清单 (O5);
  `/responses` 一律 404。
- 集中 fallback + 熔断只覆盖首字节前 (传输失败/429/5xx); 首字节之后不切后端。
- 预算不提前拦截: 靠上游 429 触发冷却与系数标定; 未撞过墙时系数默认 1 (仅影响估算)。
- 归档日志不自动清理, 也不压缩; 留存与清理交给外部 (logrotate 或手动)。
- 接真上游前的清单见设计文档 §5.4。

## 目录

- `main.go` 单文件代理 (含假上游自测所需的全部实现)。
- `test/fakeupstream/` 假上游: 回放思考块 / 文本 / 工具调用 / usage 的 SSE,
  支持中途断流 (验证 F6)。
- `test/run_selftest.sh` 自测: 13 组用例 91 项断言, 对本地假上游跑代理。
- `test/healthcheck.sh` 自检: 二进制 / 配置 (-check) / 端口三项, 退出码 0 = 全通过。
- `deploy/` 常驻: `tollgate.service` 模板 + `install_service.sh` (systemd 用户服务)。
- `scripts/gen_tollgate_config.py` 由 fff 模型列表生成本项目配置 (读
  `~/fff/config/llm-models.json` + 黑名单, 按 `model_group` 归并成别名; 见下)。
- `tmp/` 一键验收脚本 (临时, 输出进 `tmp/*.log`, 不提交)。
