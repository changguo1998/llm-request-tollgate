# analysis

历史请求日志的分析工具 (读 `var/requests*.jsonl` 及其跨天归档, 只读, 不随服务启动)。
用于评估各锁触发阈值与排查请求性能。纯标准库 + 系统 gnuplot。

## 组成

- `loglib.py` — 读取库 + `dump` CLI。`iter_read/read` 按时间窗、字段子集、等值过滤
  (宽松类型匹配) 取记录; `select(o, path)` 取点号路径 (如 `usage.prompt`) 与派生列
  (`ts_epoch` / `ts_day` / `ts_hour`)。
- `gnuplot.py` — gnuplot 渲染助手 `scatter(pts, out, ...)`: 把 (x,y) 数据落盘并调系统
  gnuplot 存成 PNG (可 log-log 或线性轴)。
- `plot_pair.py` — 泛用散点脚本 `--x FIELDA --y FIELDB --out xxx.png`, 画任意两字段的关系。

## 用法

```bash
# 读取库 CLI: 输出选定列为 TSV, 可直接管道给 gnuplot
python3 analysis/loglib.py var dump \
  --fields=ts_epoch,ttfb_ms,decode_ms,duration_ms,usage.prompt \
  --start=2026-11-01 --filter=status=200 --header

# 泛用画图: 任意 x/y 字段对
python3 analysis/plot_pair.py --x usage.prompt --y ttfb_ms --out tmp/ttfb_prompt.png
```

常用字段见 `src/log.go` (logRec) 与 `src/usage.go` (Usage): `ttfb_ms`(首字节时延)、
`decode_ms`(解码段时长)、`duration_ms`(总耗时)、`tps`(解码吞吐)、`bytes`(响应字节)、
以及 `usage.prompt/completion/total/cached`。
