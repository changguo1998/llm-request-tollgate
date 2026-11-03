"""gnuplot helper: 把 (x, y) 数据交给系统 gnuplot 渲染成 PNG, 供各散点脚本复用。

依赖: 系统已装 gnuplot (见 README). Python 侧零第三方库。
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import loglib


def scatter(pts, out, title="", xlab="x", ylab="y", linear=False, style="points",
            size=(900, 700)):
    """画 2 列散点并存到 out。

    pts: 可迭代的 (x, y) 或 (x, y, ...) 行; gnuplot 只取前两列。
    linear=False 时用 log-log 轴.
    style: gnuplot style 片段, 如 'points pt 7 ps 0.2 lc rgb "#1668a0"'.
    """
    tmp = loglib.repo_join("tmp")
    os.makedirs(tmp, exist_ok=True)
    dat = os.path.join(tmp, os.path.basename(out).rsplit(".", 1)[0] + ".dat")
    gp = os.path.join(tmp, os.path.basename(out).rsplit(".", 1)[0] + ".gp")
    out = os.path.abspath(out)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(dat, "w") as fh:
        for r in pts:
            fh.write(" ".join(str(v) for v in r) + "\n")
    with open(gp, "w") as fh:
        fh.write(f"set terminal pngcairo size {size[0]},{size[1]}\n")
        fh.write(f"set output '{out}'\n")
        if not linear:
            fh.write("set logscale xy\n")
        fh.write(f"set xlabel '{xlab}'\n")
        fh.write(f"set ylabel '{ylab}'\n")
        fh.write(f"set title '{title}'\n")
        fh.write("set grid\n")
        fh.write(f"plot '{dat}' using 1:2 with {style}\n")
    r = os.system(f"gnuplot '{gp}'")
    return r == 0
