#!/usr/bin/env python3
"""plot_r_vs_rstar.py: 散点图 x=r (实际解码速率) 对 y=r* (性能), 叠加 y=x 对角线.

r   = c/d*1000          (tokens/s, 日志派生)
r*  = r * (c/b)^(-g)     (perfref 定稿口径, g 取 speedref_params.json, 缺参数退 r)

版本: gnuplot(pngcairo) + 零第三方库; 比例 1:1 —— 图片宽高比 1:1 且图区正方形
(set size square), 两轴范围取同一值, 保证 y=x 是对角 45 度线; border 四边闭合.
每模型一种颜色 (含图例). 超出 --max 的离群点剔除 (缺省 1200).

用法::
    python3 analysis/plot_r_vs_rstar.py                     # 全部模型 -> tmp/r_rstar.png
    python3 analysis/plot_r_vs_rstar.py --model u/m --model ... --out tmp/a.png --max 600
"""

import argparse
import glob
import json
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VAR = os.path.join(ROOT, "var")
HERE = os.path.dirname(os.path.abspath(__file__))
PARAMS = os.path.join(HERE, "figs", "speedref_params.json")
TMP = os.path.join(ROOT, "tmp")
OUT_DEF = os.path.join(TMP, "r_rstar.png")

# 每模型一种颜色 (与 plot_speedref 风格相近的区分色)
COLORS = [
    "#1a7fd0", "#e05a2a", "#0a9a3a", "#9a3ab8",
    "#c9a000", "#1a9a9a", "#c04060", "#7a7a5a",
]
PS = 0.22


def load_params():
    with open(PARAMS) as fh:
        raw = json.load(fh)
    return {k: v for k, v in raw.items() if k != "_meta"}


def load_pts(params, only):
    """读日志: 返回 {key: [(r, rstar)]}; 参数表里的模型才画 (缺参数退 r*=r)."""
    out = {}
    for p in sorted(glob.glob(os.path.join(VAR, "requests*.jsonl"))):
        with open(p) as fh:
            for line in fh:
                o = json.loads(line)
                if o.get("status", 0) >= 400 or not o.get("stream"):
                    continue
                u, m = o.get("upstream"), o.get("model")
                key = f"{u}/{m}"
                if params.get(key) is None:
                    continue
                if only and key not in only and m not in only:
                    continue
                pr = params[key]
                u0 = o.get("usage") or {}
                c, d, b = u0.get("completion") or 0, o.get("decode_ms") or 0, o.get("bytes") or 0
                if c <= 0 or d <= 0 or b <= 0:
                    continue
                r = c / d * 1000.0
                rstar = r * (c / b) ** (-pr["g"])
                out.setdefault(key, []).append((r, rstar))
    return out


def render(by_model, out, maxv):
    os.makedirs(TMP, exist_ok=True)
    dats, parts, clipped = [], [], 0
    for i, (key, pts) in enumerate(by_model.items()):
        color = COLORS[i % len(COLORS)]
        keep = [(r, rs) for r, rs in pts if r <= maxv and rs <= maxv]
        clipped += len(pts) - len(keep)
        if not keep:
            continue
        dat = os.path.join(TMP, f"r_rstar_{i}.dat")
        with open(dat, "w") as fh:
            for r, rs in keep:
                fh.write(f"{r:.6f} {rs:.6f}\n")
        dats.append(dat)
        parts.append(f"'{dat}' using 1:2 with points pt 7 ps {PS} lc rgb '{color}' title '{key}'")

    n = sum(len(v) for v in by_model.values())
    items = ["f(x) title 'y=x' with lines lc rgb '#cc2222' lw 2"] + parts
    body = "\n".join([
        "set terminal pngcairo size 1600,1600",
        f"set output '{os.path.abspath(out)}'",
        "set size square",
        "set xrange [0:%g]" % maxv,
        "set yrange [0:%g]" % maxv,
        "set tics nomirror",
        "set border 15 lw 1",
        "set xlabel 'r = c/d  (tokens/s)'",
        "set ylabel 'r* = (c/d)(c/b)^(-g)  (tokens/s)'",
        "set title 'r* vs r  (n=%d, crop > %g)'" % (n, maxv),
        "set key outside top right",
        "f(x)=x",
        "plot " + ",\\\n     ".join(items),
    ])
    gp = os.path.join(TMP, "r_rstar.gp")
    with open(gp, "w") as fh:
        fh.write(body + "\n")
    r = subprocess.run(["gnuplot", gp])
    for dat in dats:
        try:
            os.remove(dat)
        except OSError:
            pass
    return r.returncode == 0, clipped


def main():
    ap = argparse.ArgumentParser(description="r* vs r 散点 (y=x 对角线, 线性 1:1, 裁剪离群)")
    ap.add_argument("--model", action="append", default=[], help="只画指定 上游/模型 (可多次)")
    ap.add_argument("--out", default=OUT_DEF)
    ap.add_argument("--max", type=float, default=1200.0, help="两轴统一上限, 超出剔除 (缺省 1200)")
    args = ap.parse_args()

    params = load_params()
    if not params:
        print(f"参数文件缺失或为空: {PARAMS}")
        return 1
    only = set(args.model)
    by_model = load_pts(params, only)
    if not by_model:
        print("无有效样本 (流式成功且 c/d/b>0, 参数表内模型)")
        return 1
    total = sum(len(v) for v in by_model.values())
    print(f"样本 {total} 笔, 模型 {len(by_model)} 个: {sorted(by_model)}")
    ok, clipped = render(by_model, args.out, args.max)
    if not ok:
        print("gnuplot 渲染失败")
        return 1
    print(f"裁剪离群 {clipped} 笔 (r 或 r* 超过 {args.max:g}), 画 {total - clipped} 笔")
    print(f"saved -> {os.path.abspath(args.out)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
