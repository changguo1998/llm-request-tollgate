#!/usr/bin/env python3
"""plot_pair.py: 泛用散点 —— 给定 x / y 两个字段画它们的关系, 存成 PNG.

字段用 loglib 的点号路径 (usage.prompt 等) 或派生列 (ts_epoch/ts_day/ts_hour)。
用法::

    python3 analysis/plot_pair.py --x usage.prompt --y ttfb_ms --out tmp/a.png
    python3 analysis/plot_pair.py --x ts_epoch --y ttfb_ms --out tmp/t.png \\
        --filter upstream=ustc --filter status=200 --linear
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import loglib
import gnuplot


def render(x_spec, y_spec, out, start=None, end=None, filters=None,
           linear=False, title=None, xtitle=None, ytitle=None, sample_max=None):
    pts = []
    var = loglib.repo_join("var")
    for o in loglib.iter_read(var, start=start, end=end, filters=filters):
        x = loglib.select(o, x_spec)
        y = loglib.select(o, y_spec)
        if x is None or y is None:
            continue
        pts.append((x, y))
        if sample_max and len(pts) >= sample_max:
            break
    if not pts:
        print(f"无数据 (x={x_spec}, y={y_spec})")
        return False
    ok = gnuplot.scatter(pts, out,
                         title=title or f"{y_spec} vs {x_spec}  (n={len(pts)})",
                         xlab=xtitle or x_spec, ylab=ytitle or y_spec,
                         linear=linear)
    if ok:
        print(f"saved -> {os.path.abspath(out)}  (n={len(pts)})")
    return ok


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--x", required=True)
    ap.add_argument("--y", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--start")
    ap.add_argument("--end")
    ap.add_argument("--filter", action="append", default=[])
    ap.add_argument("--linear", action="store_true")
    ap.add_argument("--title")
    ap.add_argument("--sample-max", type=int)
    args = ap.parse_args()
    filters = {}
    for f in args.filter:
        k, _, v = f.partition("=")
        vals = [s.strip() for s in v.split(",") if s.strip()]
        filters[k.strip()] = vals if len(vals) > 1 else vals[0]
    return 0 if render(args.x, args.y, args.out, start=args.start, end=args.end,
                       filters=filters, linear=args.linear, title=args.title,
                       sample_max=args.sample_max) else 1


if __name__ == "__main__":
    sys.exit(main())
