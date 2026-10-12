#!/usr/bin/env python3
"""TUI editor for tollgate priorities in the shared model-list store.

Edits ``tollgate_priority`` on provider entries and on model entries of
``~/fff/config/llm-models.json``, previews the resulting candidate order live,
and can regenerate the proxy config + hot-reload the service.

Candidate order inside an alias = model priority desc -> provider priority desc
-> provider order (the ``--providers`` order used for generation); 0 is lowest.

Interactive TUI:
    python3 scripts/set_priority.py

Keys:
    up/down PgUp/PgDn 移动     Tab 切换 provider/模型面板
    +/- 优先级 ±1      =   直接输入数值
    g/G 首末行         /   过滤模型
    s 保存 store       r   保存并重新生成代理配置
    h hup 让代理生效   q   退出

Non-interactive (no terminal needed):
    python3 scripts/set_priority.py --order [别名]   打印候选顺序 (与生成器同规则)
    python3 scripts/set_priority.py --set <provider|模型> <值>
"""

from __future__ import annotations

import argparse
import curses
import json
import os
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
GEN = REPO_ROOT / "scripts" / "gen_tollgate_config.py"
DEFAULT_STORE = Path(os.environ.get("FFF_PRIVATE", str(Path.home() / ".fff"))) / "config" / "llm-models.json"
DEFAULT_CONFIG = REPO_ROOT / "config.local.json"
PRIORITY = "tollgate_priority"
GROUP = "tollgate_group"
PROXY_KIND = "proxy"
FALLBACK_PROVIDERS = ("ustc", "arkcp", "arkap", "group")
GENERATOR_PROVIDERS = "ustc,arkcp,arkap,group"


# ---------- data ----------

def load_store(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def save_store(path: Path, data: dict) -> None:
    """Atomic write; model keys stay sorted, provider keys stay ordered."""
    for name, entry in data.get("providers", {}).items():
        if entry.get("kind") == PROXY_KIND:
            continue
        for i, model in enumerate(entry.get("models", [])):
            entry["models"][i] = {k: model[k] for k in sorted(model)}
        if PRIORITY in entry:
            ordered = {k: v for k, v in entry.items() if k not in (PRIORITY, "models")}
            ordered[PRIORITY] = entry[PRIORITY]
            ordered["models"] = entry["models"]
            data["providers"][name] = ordered
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, indent=4) + "\n", encoding="utf-8")
    os.replace(tmp, path)


def priority(entry: dict) -> int:
    value = entry.get(PRIORITY)
    return value if isinstance(value, int) and not isinstance(value, bool) else 0


def upstreams(store: dict) -> dict:
    return {
        name: entry
        for name, entry in store.get("providers", {}).items()
        if entry.get("kind") != PROXY_KIND
    }


def provider_order(store: dict, config: Path) -> tuple:
    """Tie-break order: the config's upstream order, else the generator default."""
    if config.exists():
        try:
            ids = [u["id"] for u in json.loads(config.read_text(encoding="utf-8"))["upstreams"]]
            known = [i for i in ids if i in store.get("providers", {})]
            if known:
                return tuple(known)
        except (ValueError, KeyError, OSError):
            pass
    return tuple(p for p in FALLBACK_PROVIDERS if p in store.get("providers", {}))


def alias_of(model: dict) -> str:
    group = model.get(GROUP)
    return group.strip() if isinstance(group, str) and group.strip() else model["id"]


def load_droplist(store_path: Path, config: Path) -> set:
    """Dropped ids live next to the store (config may be a symlink elsewhere)."""
    for base in (store_path.parent, config.resolve().parent):
        path = base / "llm-models-droplist.json"
        try:
            return {i for i in json.loads(path.read_text(encoding="utf-8")) if isinstance(i, str)}
        except (OSError, ValueError):
            continue
    return set()


def build_aliases(store: dict, config: Path, store_path: Path, dropped: set | None = None) -> dict:
    """Replicate the generator's grouping + ordering, so the preview matches it."""
    dropped = load_droplist(store_path, config) if dropped is None else dropped
    order = provider_order(store, config)
    ranked = {}
    for rank, name in enumerate(order):
        entry = store["providers"][name]
        for model in entry.get("models", []):
            if model["id"] in dropped:
                continue
            ranked.setdefault(alias_of(model), []).append(
                (-priority(model), -priority(entry), rank, model["id"], name, model)
            )
    return {
        key: [(name, model) for *_, name, model in sorted(items)]
        for key, items in sorted(ranked.items())
    }


def all_models(store: dict, config: Path, store_path: Path) -> list:
    """(alias, provider, model, provider entry) in display order."""
    return [
        (alias, name, model, store["providers"][name])
        for alias, cands in build_aliases(store, config, store_path).items()
        for name, model in cands
    ]


# ---------- generator / service ----------

def run_generator(store_dir: Path, config: Path) -> tuple:
    res = subprocess.run(
        [sys.executable, str(GEN), "--fff-dir", str(store_dir), "--out", str(config),
         "--providers", GENERATOR_PROVIDERS],
        cwd=REPO_ROOT, capture_output=True, text=True,
    )
    return res.returncode == 0, (res.stderr or res.stdout).strip()


def run_hup() -> tuple:
    res = subprocess.run([str(REPO_ROOT / "deploy" / "tollgate.sh"), "hup"],
                         cwd=REPO_ROOT, capture_output=True, text=True)
    return res.returncode == 0, (res.stderr or res.stdout).strip()


# ---------- TUI ----------

HELP = "up/down 移动  Tab 换面板  +/- 调优先级  = 输入值  / 过滤  s 保存  r 重生成  h hup  q 退出"


class Tui:
    def __init__(self, store_path: Path, config: Path, store: dict):
        self.store_path = store_path
        self.config = config
        self.store = store
        self.dirty = False
        self.focus = "model"  # or "provider"
        self.prov_sel = 0
        self.model_sel = 0
        self.top = 0
        self.filter = ""
        self.message = ""

    def providers(self) -> list:
        return list(provider_order(self.store, self.config))

    def rows(self) -> list:
        rows = all_models(self.store, self.config, self.store_path)
        if self.filter:
            rows = [r for r in rows
                    if self.filter in r[0] or self.filter in r[1] or self.filter in r[2]["id"]]
        return rows

    def selected_model(self):
        rows = self.rows()
        if not rows:
            return None
        self.model_sel = max(0, min(self.model_sel, len(rows) - 1))
        return rows[self.model_sel]

    def adjust(self, delta: int) -> None:
        if self.focus == "provider":
            names = self.providers()
            if not names:
                return
            self.prov_sel = max(0, min(self.prov_sel, len(names) - 1))
            entry = self.store["providers"][names[self.prov_sel]]
            entry[PRIORITY] = max(0, priority(entry) + delta)
            self.message = f"{names[self.prov_sel]}: provider 优先级 = {entry[PRIORITY]}"
        else:
            row = self.selected_model()
            if not row:
                return
            _, name, model, _ = row
            model[PRIORITY] = max(0, priority(model) + delta)
            self.message = f"{name}/{model['id']}: 模型优先级 = {model[PRIORITY]}"
        self.dirty = True

    def set_value(self, value: int) -> None:
        if self.focus == "provider":
            names = self.providers()
            entry = self.store["providers"][names[self.prov_sel]]
            entry[PRIORITY] = max(0, value)
            self.message = f"{names[self.prov_sel]}: provider 优先级 = {entry[PRIORITY]}"
        else:
            row = self.selected_model()
            if not row:
                return
            _, name, model, _ = row
            model[PRIORITY] = max(0, value)
            self.message = f"{name}/{model['id']}: 模型优先级 = {model[PRIORITY]}"
        self.dirty = True

    def draw(self, stdscr) -> None:
        stdscr.erase()
        height, width = stdscr.getmaxyx()
        left_w = max(38, width // 2)
        prov_h = min(len(self.providers()) + 3, max(4, height // 3))
        self.draw_providers(stdscr, prov_h, left_w)
        self.draw_models(stdscr, prov_h, height, left_w)
        self.draw_preview(stdscr, height, width, left_w + 1)
        msg = f" {self.message}" + (" [未保存]" if self.dirty is True else "") + " "
        try:
            stdscr.addnstr(height - 1, 0, msg, width - 1, curses.A_REVERSE)
            if len(msg) < width - 1:
                stdscr.addnstr(height - 1, len(msg), " " + HELP, width - 2 - len(msg), curses.A_DIM)
        except curses.error:
            pass
        stdscr.refresh()

    def draw_providers(self, stdscr, prov_h: int, left_w: int) -> None:
        head = " Providers " + ("(焦点)" if self.focus == "provider" else "")
        self.safe_add(stdscr, 0, 0, head.ljust(left_w - 1), curses.A_BOLD)
        for i, name in enumerate(self.providers()):
            if i + 1 >= prov_h:
                break
            entry = self.store["providers"][name]
            attr = curses.A_REVERSE if (self.focus == "provider" and i == self.prov_sel) else curses.A_NORMAL
            self.safe_add(stdscr, i + 1, 0,
                          f" {name:8s} priority={priority(entry):<3d}".ljust(left_w - 1), attr)

    def draw_models(self, stdscr, prov_h: int, height: int, left_w: int) -> None:
        rows = self.rows()
        visible = max(1, height - prov_h - 2)
        if self.focus == "model":
            self.model_sel = max(0, min(self.model_sel, max(0, len(rows) - 1)))
            if self.model_sel < self.top:
                self.top = self.model_sel
            if self.model_sel >= self.top + visible:
                self.top = self.model_sel - visible + 1
        hidden = len(load_droplist(self.store_path, self.config))
        head = (f" 模型 (过滤: {self.filter or '-'}, {len(rows)} 个, 黑名单剔除 {hidden}) "
                + ("(焦点)" if self.focus == "model" else ""))
        self.safe_add(stdscr, prov_h, 0, head.ljust(left_w - 1), curses.A_BOLD)
        for i in range(self.top, min(len(rows), self.top + visible)):
            alias, name, model, entry = rows[i]
            text = f" M{priority(model):<3d} P{priority(entry):<3d} {name}/{model['id']}"
            if len(text) > left_w - 3:
                text = text[: left_w - 4] + "..."
            attr = curses.A_REVERSE if (self.focus == "model" and i == self.model_sel) else curses.A_NORMAL
            self.safe_add(stdscr, prov_h + 1 + i - self.top, 0, text.ljust(left_w - 1), attr)

    def draw_preview(self, stdscr, height: int, width: int, x: int) -> None:
        if x >= width - 4:
            return
        row = self.selected_model()
        self.safe_add(stdscr, 0, x, " 候选顺序预览 ".ljust(max(0, width - x - 1)), curses.A_BOLD)
        if not row:
            return
        alias = row[0]
        cands = build_aliases(self.store, self.config, self.store_path).get(alias, [])
        self.safe_add(stdscr, 1, x, f" {alias} ({len(cands)} 候选)", curses.A_BOLD)
        for i, (name, model) in enumerate(cands):
            if i + 2 >= height - 1:
                break
            entry = self.store["providers"][name]
            text = f" {i + 1}. {name}/{model['id']}  M{priority(model)} P{priority(entry)}"
            attr = curses.A_REVERSE if model is row[2] else curses.A_NORMAL
            self.safe_add(stdscr, i + 2, x, text[: max(0, width - x - 1)], attr)

    @staticmethod
    def safe_add(stdscr, y: int, x: int, text: str, attr: int = 0) -> None:
        try:
            stdscr.addnstr(y, x, text, max(0, len(text)), attr)
        except curses.error:
            pass

    def save(self) -> None:
        save_store(self.store_path, self.store)
        self.dirty = False
        self.message = f"已保存 {self.store_path.name}"

    def regenerate(self) -> None:
        self.save()
        ok, out = run_generator(self.store_path.parent, self.config)
        self.message = "已重新生成配置" if ok else f"生成失败: {out.splitlines()[-1] if out else '?'}"

    def hup(self) -> None:
        ok, out = run_hup()
        self.message = "已 hup, 代理生效" if ok else f"hup 失败: {out.splitlines()[-1] if out else '?'}"


def prompt(stdscr, tui: Tui, text: str) -> str:
    tui.message = text
    tui.draw(stdscr)
    curses.echo()
    try:
        return stdscr.getstr(len(text) + 1, 0, 40).decode().strip()
    except curses.error:
        return ""
    finally:
        curses.noecho()


def run_tui(stdscr, tui: Tui) -> None:
    curses.curs_set(0)
    stdscr.keypad(True)
    while True:
        tui.draw(stdscr)
        key = stdscr.getch()
        if key in (ord("q"), 27):
            if tui.dirty is True:
                tui.message = "有未保存改动: 再按 q 放弃退出, 或按 s 保存"
                tui.dirty = "asked"
                continue
            return
        if tui.dirty == "asked":
            tui.dirty = True
        if key in (curses.KEY_UP, ord("k")):
            if tui.focus == "provider":
                tui.prov_sel -= 1
            else:
                tui.model_sel -= 1
        elif key in (curses.KEY_DOWN, ord("j")):
            if tui.focus == "provider":
                tui.prov_sel += 1
            else:
                tui.model_sel += 1
        elif key == curses.KEY_PPAGE:
            tui.model_sel -= 10
        elif key == curses.KEY_NPAGE:
            tui.model_sel += 10
        elif key == ord("g"):
            tui.model_sel = 0
        elif key == ord("G"):
            tui.model_sel = 10 ** 6
        elif key == 9:  # Tab
            tui.focus = "provider" if tui.focus == "model" else "model"
            tui.prov_sel = max(0, min(tui.prov_sel, len(tui.providers()) - 1))
        elif key in (ord("+"), ord("="), curses.KEY_RIGHT):
            if key == ord("="):
                raw = prompt(stdscr, tui, "新的优先级数值:")
                if raw.isdigit():
                    tui.set_value(int(raw))
            else:
                tui.adjust(1)
        elif key in (ord("-"), curses.KEY_LEFT):
            tui.adjust(-1)
        elif key == ord("/"):
            tui.filter = prompt(stdscr, tui, "过滤 (空 = 清除):")
            tui.model_sel = 0
            tui.top = 0
        elif key == ord("s"):
            tui.save()
        elif key == ord("r"):
            tui.regenerate()
        elif key == ord("h"):
            tui.hup()


# ---------- headless ----------

def print_order(store: dict, config: Path, store_path: Path, alias) -> None:
    for key, cands in build_aliases(store, config, store_path).items():
        if alias and alias not in key:
            continue
        if not alias and len(cands) < 2:
            continue
        print(f"{key}:")
        for i, (name, model) in enumerate(cands, 1):
            print(f"  {i}. {name}/{model['id']}  M{priority(model)} P{priority(store['providers'][name])}")


def headless_set(store_path: Path, token: str, value: int) -> int:
    store = load_store(store_path)
    ups = upstreams(store)
    if token in ups:
        ups[token][PRIORITY] = value
        save_store(store_path, store)
        print(f"{token}: provider 优先级 = {value}")
        return 0
    hits = [(n, m) for n, e in ups.items() for m in e.get("models", [])
            if m["id"] == token or token in m["id"]]
    if len(hits) != 1:
        print(f"需要唯一命中, 实际 {len(hits)}: " + ", ".join(f"{n}/{m['id']}" for n, m in hits[:8]))
        return 1
    hits[0][1][PRIORITY] = value
    save_store(store_path, store)
    print(f"{hits[0][0]}/{hits[0][1]['id']}: 模型优先级 = {value}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--store", default=str(DEFAULT_STORE), help="model-list store path")
    ap.add_argument("--config", default=str(DEFAULT_CONFIG), help="proxy config path")
    ap.add_argument("--order", nargs="?", const="", metavar="别名", help="print candidate order and exit")
    ap.add_argument("--set", nargs=2, metavar=("provider|模型", "值"), help="set a priority and exit")
    args = ap.parse_args()

    store_path = Path(os.path.expanduser(args.store))
    config = Path(os.path.expanduser(args.config))
    if args.order is not None:
        print_order(load_store(store_path), config, store_path, args.order or None)
        return 0
    if args.set:
        return headless_set(store_path, args.set[0], int(args.set[1]))

    tui = Tui(store_path, config, load_store(store_path))
    try:
        curses.wrapper(run_tui, tui)
    except curses.error as exc:
        print(f"无法初始化 TUI ({exc}); 无终端时用 --order / --set", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
