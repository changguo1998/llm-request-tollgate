"""plot_speedref.py: 为各 上游/模型 计算速度参考参数 (定稿口径).

口径 (见 docs/speed-metric.md §6.1):
  样本前置 -> 理想小时 (med r >= p65 且 med t <= p35) -> 二维密度典型区
  (100x100, 3x3 平滑, frac=0.60) -> 典型区 OLS 拟合 log(c/d) = a + g*log(c/b).

输出:
  参数 JSON  -> analysis/figs/speedref_params.json (随仓库保存, 供以后查看/复用)
  图片       -> 仅加 --plot 时渲染到 tmp/speedref/ (临时, 不入仓库)

用法:
  python3 analysis/plot_speedref.py            # 只算参数, 写 JSON
  python3 analysis/plot_speedref.py --plot      # 同时渲染各模型 PNG 到 tmp/speedref/
  python3 analysis/plot_speedref.py --no-json   # 只打印, 不写文件
"""

import argparse
import glob
import json
import math
import os
import subprocess
import sys
from collections import defaultdict
from datetime import datetime

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VAR = os.path.join(ROOT, "var")
HERE = os.path.dirname(os.path.abspath(__file__))
JSON_OUT = os.path.join(HERE, "figs", "speedref_params.json")
PLOT_TMP = os.path.join(ROOT, "tmp", "speedref")

# 定稿参数
TH_RATE, TH_TTFB = 0.65, 0.35   # 理想小时分位门槛
FRAC = 0.60                     # 典型区密度前 60%
D_MIN, C_MIN = 100, 15          # 微样本剔除
NX, NY = 100, 100               # 密度网格
PS_ALL, PS_TYP, LW = 0.22, 0.5, 3

MODELS = [
    ("group", "gpt-6-sol"),
    ("ustc", "deepseek-flash"),
    ("group", "kimi-k3.1-pro"),
    ("ustc", "deepseek-v4-flash"),
    ("arkcp", "deepseek-v4-1-flash-economy"),
    ("group", "deepseek-v4.1-flash-off"),
]


def load_rows(up, model):
    """读 var 下全部日志, 过滤到目标 上游/模型 的有效样本."""
    rows = []
    for p in sorted(glob.glob(os.path.join(VAR, "requests*.jsonl"))):
        with open(p) as fh:
            for line in fh:
                o = json.loads(line)
                u = o.get("usage") or {}
                tt = o.get("ttfb_ms") or 0
                d = o.get("decode_ms") or 0
                c = u.get("completion") or 0
                by = o.get("bytes") or 0
                ts = o.get("ts", "")
                if o.get("status", 0) >= 400 or not o.get("stream"):
                    continue
                if o.get("upstream") != up or o.get("model") != model:
                    continue
                if tt <= 0 or d <= 0 or c <= 0 or by <= 0 or not ts:
                    continue
                try:
                    hour = datetime.fromisoformat(ts).hour
                except Exception:
                    continue
                if c >= C_MIN and d >= D_MIN:
                    rows.append((hour, tt, d, c, by))
    return rows


def median(v):
    v = sorted(v)
    return v[len(v) // 2]


def pct(v, q):
    v = sorted(v)
    return v[min(len(v) - 1, int(len(v) * q))]


def ideal_hours(rows):
    """按小时 med r / med ttfb 的门槛挑理想小时 (0.65/0.35)."""
    by = defaultdict(list)
    for r in rows:
        by[r[0]].append(r)

    def rate(r):
        return r[3] / (r[2] / 1000)

    hm = {h: median([rate(r) for r in lst]) for h, lst in by.items()}
    ht = {h: median([r[1] for r in lst]) for h, lst in by.items()}
    if len(hm) < 4:
        return list(range(24))
    rq = pct(sorted(hm.values()), TH_RATE)
    tq = pct(sorted(ht.values()), TH_TTFB)
    return [h for h in hm if hm[h] >= rq and ht[h] <= tq]


def dense_mask(xall, yall, frac):
    """二维密度 (100x100 + 3x3 平滑), 返回密度前 frac 的布尔掩码与画图范围."""
    xlo, xhi = pct(xall, 0.005) - 0.2, pct(xall, 0.995) + 0.2
    ylo, yhi = pct(yall, 0.005) - 0.2, pct(yall, 0.995) + 0.2
    H, _, _ = np.histogram2d(xall, yall, bins=[NX, NY], range=[[xlo, xhi], [ylo, yhi]])
    Hs = H.copy()
    for dx in (-1, 0, 1):
        for dy in (-1, 0, 1):
            Hs += np.roll(np.roll(H, dx, 0), dy, 1)
    ii = np.clip(((xall - xlo) / (xhi - xlo) * NX).astype(int), 0, NX - 1)
    jj = np.clip(((yall - ylo) / (yhi - ylo) * NY).astype(int), 0, NY - 1)
    dens = Hs[ii, jj]
    sel = dens >= np.quantile(dens, 1 - frac)
    return sel, (xlo, xhi, ylo, yhi)


def flatness(ideal, g):
    """全体理想小时样本上, 按 c 三分位的 r* 中位 (先减全局中位归一, 应接近 1.0)."""
    n = len(ideal)
    cs = sorted(r[3] for r in ideal)
    c30, c70 = cs[n // 3], cs[2 * n // 3]

    idx = []
    for _h, _t, d, c, by in ideal:
        r = math.log((c / (d / 1000)) * (c / by) ** (-g))  # 定稿 r* = (c/d)*(c/b)^(-g)
        idx.append(r)
    m = median(idx)

    def terc(lo, hi):
        vv = []
        for v, r in zip(idx, ideal):
            if lo <= r[3] < hi:
                vv.append(v - m)
        return math.exp(median(vv)) if vv else None

    return (terc(0, c30), terc(c30, c70), terc(c70, 1e18))


def render(up, model, ideal, a, g, xlo, xhi, ylo, yhi, n_dense, out_dir):
    """渲染一版图到 out_dir (临时): 全体理想样本灰点 + 典型区蓝点 + 拟合线."""
    os.makedirs(out_dir, exist_ok=True)
    out_png = os.path.join(out_dir, f"lgrate_{model}.png")
    xall = np.array([math.log(r[4] / r[3]) for r in ideal])
    yall = np.array([math.log(r[3] / (r[2] / 1000)) for r in ideal])
    sel, _ = dense_mask(xall, yall, FRAC)
    dat_d = os.path.join(out_dir, f"_dense_{model}.dat")
    dat_s = os.path.join(out_dir, f"_sparse_{model}.dat")
    with open(dat_d, "w") as fd, open(dat_s, "w") as fs:
        for x, y, s in zip(xall, yall, sel):
            (fd if s else fs).write(f"{x} {y}\n")
    ih = sorted(set(r[0] for r in ideal))
    gp = f"""set terminal pngcairo size 1600,1200
set output '{out_png}'
set xlabel 'ln(bytes/c)'
set ylabel 'ln(c/d)'
set title '{up}/{model}  frac={FRAC}  g={g:+.3f}  (ideal hours: {ih})'
set grid
set xrange [{xlo:.2f}:{xhi:.2f}]
set yrange [{ylo:.2f}:{yhi:.2f}]
set key inside top right
f(x) = {a:.4f} + ({-g:.4f})*x
plot '{dat_s}' using 1:2 with points pt 7 ps {PS_ALL} lc rgb '#bbbbbb' notitle, \\
     '{dat_d}' using 1:2 with points pt 7 ps {PS_TYP} lc rgb '#1a7fd0' title 'typical {int(FRAC*100)}% (n={n_dense})', \\
     f(x) with lines lc rgb '#0a9a3a' lw {LW} title 'fit g={g:+.3f}'
"""
    gp_path = os.path.join(out_dir, f"_lgrate_{model}.gp")
    with open(gp_path, "w") as fh:
        fh.write(gp)
    r = subprocess.run(["gnuplot", gp_path])
    return r.returncode == 0, out_png


def main():
    ap = argparse.ArgumentParser(description="速度参考参数 (定稿口径)")
    ap.add_argument("--plot", action="store_true", help="渲染各模型 PNG 到 tmp/speedref/")
    ap.add_argument("--no-json", action="store_true", help="不写参数 JSON, 只打印")
    args = ap.parse_args()

    params = {}
    for up, model in MODELS:
        rows = load_rows(up, model)
        ih = ideal_hours(rows)
        ideal = [r for r in rows if r[0] in ih]
        if len(ideal) < 120:
            print(f"跳过 {up}/{model}: 理想样本不足 ({len(ideal)})")
            continue
        xall = np.array([math.log(r[4] / r[3]) for r in ideal])
        yall = np.array([math.log(r[3] / (r[2] / 1000)) for r in ideal])
        zall = np.array([math.log(r[3] / r[4]) for r in ideal])
        sel, (xlo, xhi, ylo, yhi) = dense_mask(xall, yall, FRAC)
        g, a = np.polyfit(zall[sel], yall[sel], 1)
        f = flatness(ideal, float(g))
        n_dense = int(sel.sum())
        # 缩放常数 m = (典型区 c/b 中位)^g: 把归一后的 r* 校准回典型解码速率量级
        m = math.exp(g * median(zall[sel]))
        params[f"{up}/{model}"] = {
            "g": round(float(g), 4), "a": round(float(a), 4), "m": round(float(m), 6),
            "ref_hours": sorted(ih), "n_ideal": len(ideal), "n_typical": n_dense,
            "frac": FRAC,
            "flatness_short_mid_long": [round(v, 3) for v in f],
            "flatness_target": "ideal-sample c-tercile r* medians, ~1.0 expected",
        }
        line = (f"{up}/{model}: ideal={sorted(ih)} n={len(ideal)} "
                f"g={g:+.3f} short/mid/long={f[0]:.2f}/{f[1]:.2f}/{f[2]:.2f}")
        if args.plot:
            ok, out_png = render(up, model, ideal, float(a), float(g),
                                 xlo, xhi, ylo, yhi, n_dense, PLOT_TMP)
            line += f" -> {out_png} (ok={ok})"
        print(line)

    if not args.no_json:
        # 顶层 _meta 记版本与生成时间 (docs/speed-metric.md: 参数文件带版本与时间戳).
        out = {"_meta": {
            "version": "2026-11",  # 参考窗标签, 与 docs/speed-metric.md 表格一致
            "generated_at": datetime.now().strftime("%Y-%m-%dT%H:%M:%S"),
        }}
        out.update(params)
        os.makedirs(os.path.dirname(JSON_OUT), exist_ok=True)
        with open(JSON_OUT, "w", encoding="utf-8") as fh:
            json.dump(out, fh, ensure_ascii=False, indent=2)
        print(f"\n参数文件: {JSON_OUT}")


if __name__ == "__main__":
    sys.exit(main())
