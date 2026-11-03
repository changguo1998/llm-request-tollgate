"""loglib: 统一读取管道请求日志的分析库 (只读, 不随服务启动)。

日志是 JSONL: 当前日志 {} requests.jsonl 或 {dir}/requests.jsonl + 跨天归档
{dir}/requests-YYYY-MM-DD.jsonl。单条记录结构见 src/log.go 的 logRec 与
src/usage.go 的 Usage (键与 json tag 一致)。

用法::

    import loglib
    recs = loglib.read("var", start="2026-11-01T00:00:00+08:00",
                        end="2026-11-02T00:00:00+08:00",
                        fields=["ts","upstream","model","status","ttfb_ms","usage"],
                        filters={"status":200})
    # 或流式:
    for o in loglib.iter_read("var", upstream="ustc"):
        ...

时间窗为闭区间 [start, end], start/end 传 ISO 时间串或 datetime; 省略一边即不设界。
fields 只返回指定键 (None = 全部)。filters 是等值过滤 (键 -> 取值), 多键 AND。
为省 IO 会把整日都在窗外的归档直接跳过 (只按天粒度, 不含当前日志)。
"""

from __future__ import annotations

import datetime as _dt
import json
import glob
import os
import re

# 归档文件名里嵌的日期: requests-2026-11-01.jsonl
_ARCHIVE_DAY = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def repo_join(*parts: str) -> str:
    """相对仓库根的路径 (库在 analysis/ 下, 仓库根 = 本文件上两级)。"""
    here = os.path.dirname(os.path.abspath(__file__))
    return os.path.join(os.path.dirname(here), *parts)


def parse_ts(ts: str) -> _dt.datetime:
    """解析 RFC3339Nano (含时区偏移)。解析失败返回 None。"""
    if not ts:
        return None
    try:
        return _dt.datetime.fromisoformat(ts)
    except ValueError:
        return None


def find_log_files(dir_path: str, start=None, end=None) -> list[str]:
    """返回要读取的日志文件路径, 排序。

    start/end 为 datetime 或 ISO 串。整日落在窗外 (start 的日 > 该归档日, 或
    end 的日 < 该归档日) 的归档直接跳过; 当前日志 (无日期) 恒参与。
    """
    start = _to_dt(start)
    end = _to_dt(end)
    fs = []
    pat = os.path.join(dir_path, "requests*.jsonl")
    for p in sorted(glob.glob(pat)):
        base = os.path.basename(p)
        if base.startswith("requests-"):
            day = base[len("requests-"):-len(".jsonl")]
            if _ARCHIVE_DAY.match(day):
                d = _dt.date.fromisoformat(day)
                if start is not None and start.date() > d:
                    continue  # 归档整日在窗口开始之前
                if end is not None and end.date() < d:
                    continue  # 归档整日在窗口结束之后
        fs.append(p)
    return fs


def _to_dt(x):
    if x is None:
        return None
    if isinstance(x, _dt.datetime):
        return x
    return parse_ts(x)  # str


def _same(a, b) -> bool:
    """宽松判等: 数字可对字符串比较 (status=200 匹配 int 200; stream=true 匹配 bool True)。"""
    if a == b:
        return True
    if isinstance(b, bool) and isinstance(a, str):
        return (a.strip().lower() in ("true", "1")) == b
    if isinstance(a, bool) and isinstance(b, str):
        return b.strip().lower() in ("true", "1") if a else b.strip().lower() in ("false", "0")
    # 字符串 vs 数字: 两端都试着转一下
    try:
        return float(a) == float(b)
    except (TypeError, ValueError):
        pass
    return str(a) == str(b)


def iter_read(dir_path: str, start=None, end=None, fields=None, filters=None):
    """流式生成满足条件的记录 dict (一条一行)。

    fields: list[str] | None —— 只保留这些键; None = 保留全部原键。
    filters: dict —— 等值过滤 {键: 取值}; 多条 AND; 也支持 {键: (a, b)} 合集。
    匹配是宽松的 (数字/字符串/布尔可互转, 见 _same)。
    """
    start = _to_dt(start)
    end = _to_dt(end)
    for path in find_log_files(dir_path, start, end):
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    o = json.loads(line)
                except json.JSONDecodeError:
                    continue
                ts = parse_ts(o.get("ts", ""))
                if start is not None and (ts is None or ts < start):
                    continue
                if end is not None and (ts is None or ts > end):
                    continue
                if filters:
                    skip = False
                    for k, want in filters.items():
                        got = o.get(k)
                        if isinstance(want, (tuple, list)):
                            if not any(_same(got, w) for w in want):
                                skip = True
                                break
                        elif not _same(got, want):
                            skip = True
                            break
                    if skip:
                        continue
                if fields is not None:
                    yield {k: o[k] for k in fields if k in o}
                else:
                    yield o


def read(dir_path: str, start=None, end=None, fields=None, filters=None) -> list[dict]:
    """同 iter_read, 全量读回 list。"""
    return list(iter_read(dir_path, start=start, end=end, fields=fields, filters=filters))


def group(records: list[dict], keys: list[str]):
    """按 keys 分组 -> {tuple(值): [记录...]} (缺失键记 None)。"""
    out = {}
    for o in records:
        k = tuple(o.get(key) for key in keys)
        out.setdefault(k, []).append(o)
    return out


# ---- 字段解析 (点号路径 + 派生列), 供 CLI dump 用 ----

def select(o: dict, spec: str):
    """按字段描述取值: 点号路径 (usage.prompt) 取嵌套; 前导无 o.get 顶层键。

    特殊派生列:
      ts_epoch   -> ts 的 Unix 秒 (float; 用于时间序列)
      ts_day     -> ts 的本地日期 'YYYY-MM-DD'
      ts_hour    -> ts 的本地小时 0-23
    取不到返回 None。
    """
    if spec == "ts_epoch":
        dt = parse_ts(o.get("ts", ""))
        return (dt.timestamp() if dt else None)
    if spec == "ts_day":
        dt = parse_ts(o.get("ts", ""))
        return (dt.date().isoformat() if dt else None)
    if spec == "ts_hour":
        dt = parse_ts(o.get("ts", ""))
        return (dt.hour if dt else None)
    v = o
    for part in spec.split("."):
        if not isinstance(v, dict) or part not in v:
            return None
        v = v[part]
    return v


def dump_cli(dir_path: str, argv=None) -> int:
    """CLI: 输出选定列为 TSV, 便于管道给 gnuplot。

    用法::

        python3 analysis/loglib.py <dir> dump \\
            --fields=ts_epoch,upstream,ttfb_ms,usage.prompt \\
            --start=ISO --end=ISO --filter=status=200 --header

    --fields 逗号分隔 (支持 point 路径与 ts_epoch/ts_day/ts_hour);
    --filter 可多次 (键=值; 值里用逗号表"属于集合"); 全部 AND。
    """
    import argparse
    ap = argparse.ArgumentParser(prog="loglib dump")
    ap.add_argument("--fields", required=True, help="逗号分隔的输出列")
    ap.add_argument("--start")
    ap.add_argument("--end")
    ap.add_argument("--filter", action="append", default=[])
    ap.add_argument("--header", action="store_true", help="首行输出 # 列名")
    args = ap.parse_args(argv)

    specs = [s.strip() for s in args.fields.split(",") if s.strip()]
    filters = {}
    for f in args.filter:
        k, _, v = f.partition("=")
        vals = [x.strip() for x in v.split(",") if x.strip() != ""]
        filters[k.strip()] = vals if len(vals) > 1 else vals[0]

    if args.header:
        print("# " + "\t".join(specs))
    for o in iter_read(dir_path, start=args.start, end=args.end, filters=filters):
        row = [select(o, s) for s in specs]
        print("\t".join("" if v is None else str(v) for v in row))
    return 0


if __name__ == "__main__":
    import sys
    argv = sys.argv[1:]
    if len(argv) >= 2 and argv[1] == "dump":
        sys.exit(dump_cli(argv[0], argv[2:]))
    d = argv[0] if argv else repo_join("var")
    n = len(list(iter_read(d)))
    print(f"{d}: {n} 条记录")
