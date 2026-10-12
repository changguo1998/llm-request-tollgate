#!/usr/bin/env python3
"""Generate the tollgate proxy config from the fff model-list store.

Reads ``config/llm-models.json`` (provider -> baseUrl + models, written by
``fff-update-*-llm-list.py``) and ``config/llm-models-droplist.json``
(exact-match blacklist) from the fff config directory, groups the surviving
models into tollgate aliases, and writes the proxy config (default: this
repository's ``config.local.json``, which is a symlink to
``~/fff/config/tollgate.json``).

Grouping: a model's alias name is its ``model_group`` field when the store
entry carries one, otherwise the model id itself. Models sharing an alias name
(same id served by several providers, or an explicit ``model_group``) collapse
into one alias whose candidates are ordered by ``--providers`` order, which is
the failover priority. ``model_group`` survives the store updaters because
``merge_models_by_id`` inherits fields the provider API does not report.

Rewritten: the ``upstreams`` list (ids + base_url) and ``aliases``.
Preserved from the existing config: service-level fields (listen, log_path,
timeouts) and per-upstream settings (auth_header, key_env, headers,
include_usage, limits, budget, breaker_*). New upstreams get
Every upstream carries ``budget.cooldown_ms`` (429 cooldown, default 5 min).
``key_env`` taken from the store's ``apiKey`` placeholder (``$USTC_API_KEY`` ->
``USTC_API_KEY``), else ``TOLLGATE_<ID>_KEY``, and the limits of an existing
upstream, falling
back to the config.example.json values -- tune rates per provider by hand
(design §5.4), the store does not carry them.

Usage:
    scripts/gen_tollgate_config.py --dry-run
    scripts/gen_tollgate_config.py
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_FFF_CONFIG = Path.home() / "fff" / "config"
DEFAULT_OUT = REPO_ROOT / "config.local.json"
DEFAULT_PROVIDERS = "ustc,group,arkcp,arkap"

# Upstream fields carried over from an existing config entry.
PRESERVED_UPSTREAM_KEYS = (
    "auth_header",
    "key_env",
    "headers",
    "include_usage",
    "limits",
    "budget",
    "breaker_fail",
    "breaker_cooldown_ms",
)

# Fallback limits for a provider the config does not know yet (config.example.json).
DEFAULT_LIMITS = {"rate_per_sec": 0.34, "burst": 1, "concurrency": 1}

# 429 冷却对每个上游都开 (F11); windows/units 仍由人工按平台额度补。
DEFAULT_COOLDOWN_MS = 300000


def load_json(path: Path):
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def alias_name(model: dict) -> str:
    """Alias a model belongs to: explicit model_group, else the model id."""
    group = model.get("model_group")
    return group.strip() if isinstance(group, str) and group.strip() else model["id"]


def build_aliases(store: dict, providers: list[str], dropped: set[str]):
    """Return (aliases, kept counts, dropped counts) with candidates in provider order."""
    aliases: dict[str, list[list[str]]] = {}
    kept: dict[str, int] = {}
    removed: dict[str, int] = {}
    warnings: list[str] = []
    for provider in providers:
        models = store[provider].get("models", [])
        kept[provider] = 0
        removed[provider] = 0
        for model in sorted(models, key=lambda m: m["id"]):
            model_id = model["id"]
            if model_id in dropped:
                removed[provider] += 1
                continue
            name = alias_name(model)
            candidates = aliases.setdefault(name, [])
            pair = [provider, model_id]
            if pair not in candidates:
                candidates.append(pair)
            kept[provider] += 1
        if not kept[provider]:
            warnings.append(f"provider {provider}: no model left after the droplist")
    return aliases, kept, removed, warnings


def build_upstreams(store: dict, providers: list[str], existing: dict, cooldown_ms: int):
    """Merge store connection data into existing upstream entries, keeping settings."""
    by_id = {u["id"]: u for u in existing.get("upstreams", [])}
    sample = next((u for u in existing.get("upstreams", []) if u.get("limits")), None)
    upstreams = []
    for provider in providers:
        entry = {"id": provider}
        current = by_id.get(provider)
        if current:
            for key in PRESERVED_UPSTREAM_KEYS:
                if key in current:
                    entry[key] = current[key]
        entry.setdefault("auth_header", "Authorization")
        placeholder = str(store[provider].get("apiKey") or "").lstrip("$").strip()
        if placeholder:
            entry["key_env"] = placeholder  # store 的 apiKey 占位符是唯一口径
        else:
            entry.setdefault("key_env", f"TOLLGATE_{provider.upper()}_KEY")
        entry.setdefault("headers", {})
        entry.setdefault("include_usage", True)
        if "limits" not in entry:
            entry["limits"] = dict(sample["limits"]) if sample else dict(DEFAULT_LIMITS)
        budget = dict(entry.get("budget") or {})
        budget.setdefault("cooldown_ms", cooldown_ms)
        entry["budget"] = budget
        entry["base_url"] = store[provider]["baseUrl"]
        upstreams.append(entry)
    return upstreams


def config_path_label(path: Path) -> str:
    if path.is_symlink():
        return f"{path} -> {os.path.realpath(path)}"
    return str(path)


def build_config(args) -> tuple[dict, list[str]]:
    fff_config = Path(os.path.expanduser(args.fff_dir))
    store = load_json(fff_config / "llm-models.json")["providers"]
    drop_path = fff_config / "llm-models-droplist.json"
    dropped = set(load_json(drop_path)) if drop_path.exists() else set()

    providers = [p.strip() for p in args.providers.split(",") if p.strip()]
    unknown = [p for p in providers if p not in store]
    if unknown:
        raise SystemExit(f"providers not in the store: {', '.join(unknown)}")

    out = Path(os.path.expanduser(args.out))
    existing = load_json(out) if out.exists() else {}
    if any(u.get("key") for u in existing.get("upstreams", [])):
        raise SystemExit("existing config carries a literal key; use key_env instead")

    aliases, kept, removed, warnings = build_aliases(store, providers, dropped)
    config = {
        "listen": existing.get("listen", "127.0.0.1:8787"),
        "log_path": existing.get("log_path", "var/requests.jsonl"),
        "timeouts": existing.get(
            "timeouts",
            {
                "connect_ms": 10000,
                "response_header_ms": 120000,
                "stream_idle_ms": 240000,
                "queue_wait_ms": 60000,
            },
        ),
        "upstreams": build_upstreams(store, providers, existing, args.cooldown_ms),
        "aliases": {name: aliases[name] for name in sorted(aliases)},
    }

    lines = []
    for provider in providers:
        env_name = next(u["key_env"] for u in config["upstreams"] if u["id"] == provider)
        missing = "" if os.environ.get(env_name) else "  (环境变量未设置)"
        lines.append(
            f"  {provider:6s} {config['upstreams'][providers.index(provider)]['base_url']:42s}"
            f" 模型 {kept[provider]:2d} (黑名单剔除 {removed[provider]:2d})  {env_name}{missing}"
        )
    multi = {n: c for n, c in config["aliases"].items() if len(c) > 1}
    lines.append(f"  别名 {len(config['aliases'])} 个, 其中多候选 {len(multi)} 个:")
    for name, candidates in list(multi.items())[:12]:
        lines.append("    " + name + " <- " + ", ".join(f"{p}/{m}" for p, m in candidates))
    if len(multi) > 12:
        lines.append(f"    ... 其余 {len(multi) - 12} 个多候选别名见配置文件")
    lines.extend(f"  注意: {w}" for w in warnings)
    return config, lines


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--fff-dir", default=str(DEFAULT_FFF_CONFIG), help="fff config directory")
    parser.add_argument("--out", default=str(DEFAULT_OUT), help="config to write")
    parser.add_argument("--providers", default=DEFAULT_PROVIDERS, help="provider order = priority")
    parser.add_argument(
        "--cooldown-ms", type=int, default=DEFAULT_COOLDOWN_MS,
        help=f"429 cooldown per upstream in ms (default {DEFAULT_COOLDOWN_MS})",
    )
    parser.add_argument("--dry-run", action="store_true", help="print the config instead of writing")
    args = parser.parse_args()

    config, lines = build_config(args)
    text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"

    if args.dry_run:
        print(text, end="")
        print("\n".join(lines), file=sys.stderr)
        return 0

    out = Path(os.path.expanduser(args.out))
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(text, encoding="utf-8")
    print("\n".join(lines))
    print(f"已写入 {config_path_label(out)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
