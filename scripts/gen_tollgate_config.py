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
Besides the config, it upserts a ``tollgate`` provider into the model-list store
(``--no-store`` skips it): one model per alias, baseUrl derived from the config's
``listen``, so agents synced from the store can point at this proxy instead of
the upstreams. That entry carries ``kind: "proxy"`` and is never treated as an
upstream (alias generation and ``--providers`` both refuse it), so the list can
never be generated from itself. Hand-curated fields (provider keys, per-model contextWindow /
maxTokens / thinkingLevelMap) survive the update.

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
DEFAULT_PROXY_URL = ""  # 空 = 从配置的 listen 推导
DEFAULT_PROXY_KEY_ENV = "TOLLGATE_API_KEY"
PROXY_PROVIDER = "tollgate"  # store 里代表本代理自己的 provider 名
PROXY_KIND = "proxy"  # store 条目的 kind 标记: 生成列表/上游时一律跳过, 防自转发递归

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


def build_proxy_provider(config: dict, store: dict, providers: list[str], proxy_url: str,
                         key_env: str) -> dict:
    """Build the store entry representing this proxy: one model per alias (logical name).

    The proxy hides upstream ids, so agents only ever see alias names. reasoning is
    inherited from whichever candidate declares it; contextWindow / maxTokens take
    the smallest value across candidates, since a request may be served by any of
    them (kimi drops models without a usable contextWindow).
    """
    sources = {}
    for provider in providers:
        for model in store[provider].get("models", []):
            sources[model["id"]] = model
    models = []
    for name in sorted(config["aliases"]):
        entry = {"id": name}
        windows, max_tokens, reasons = [], [], False
        for _, model_id in config["aliases"][name]:
            source = sources.get(model_id, {})
            if source.get("reasoning"):
                reasons = True
            if isinstance(source.get("contextWindow"), int):
                windows.append(source["contextWindow"])
            if isinstance(source.get("maxTokens"), int):
                max_tokens.append(source["maxTokens"])
        if reasons:
            entry["reasoning"] = True
        # 取候选里的最小值: 请求可能落到任一候选, 只有保守值才不会被上游拒绝。
        if windows:
            entry["contextWindow"] = min(windows)
        if max_tokens:
            entry["maxTokens"] = min(max_tokens)
        models.append(entry)
    return {
        "kind": PROXY_KIND,
        "api": "openai-completions",
        "apiKey": f"${key_env}",
        "baseUrl": proxy_url,
        "compat": {"supportsDeveloperRole": False},
        "models": models,
    }


def derive_proxy_url(listen: str) -> str:
    """Proxy base URL agents should use, derived from the config's listen address."""
    host, _, port = listen.rpartition(":")
    if not host:
        host, port = "127.0.0.1", listen
    if host in ("0.0.0.0", "::", "[::]", "*"):
        host = "127.0.0.1"  # 通配监听要落到回环地址
    return f"http://{host}:{port}/v1"


def write_proxy_provider(store_path: Path, entry: dict) -> None:
    """Upsert providers.tollgate in the shared model-list store.

    Other providers are untouched; keys and per-model fields we do not generate
    (hand-curated contextWindow / maxTokens / thinkingLevelMap, extra provider
    keys) are inherited from the existing entry, like merge_models_by_id does
    for the upstream providers.
    """
    data = load_json(store_path) if store_path.exists() else {"providers": {}}
    providers = data.setdefault("providers", {})
    old = providers.get(PROXY_PROVIDER, {})
    merged = {k: v for k, v in old.items() if k not in entry}
    merged.update(entry)
    curated = {m["id"]: m for m in old.get("models", [])}
    models = []
    for model in entry["models"]:
        previous = curated.get(model["id"], {})
        item = {k: v for k, v in previous.items() if k not in model}
        item.update(model)
        models.append({k: item[k] for k in sorted(item)})
    merged["models"] = models
    providers[PROXY_PROVIDER] = merged
    store_path.write_text(json.dumps(data, indent=4) + "\n", encoding="utf-8")


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
    # 代理自身 (kind=proxy) 永远不能当上游, 否则请求会转发给自己形成递归。
    marked = [p for p in providers if p == PROXY_PROVIDER or store.get(p, {}).get("kind") == PROXY_KIND]
    if marked:
        raise SystemExit(f"{', '.join(marked)} is this proxy itself (kind={PROXY_KIND}), not an upstream")
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
    proxy_url = args.proxy_url or derive_proxy_url(config["listen"])
    proxy_entry = build_proxy_provider(config, store, providers, proxy_url, args.proxy_key_env)
    lines.append(
        f"  {PROXY_PROVIDER:6s} {proxy_url:42s} 模型 {len(proxy_entry['models']):2d}"
        f" (别名即逻辑名)  {args.proxy_key_env}"
    )
    lines.append(f"  别名 {len(config['aliases'])} 个, 其中多候选 {len(multi)} 个:")
    for name, candidates in list(multi.items())[:12]:
        lines.append("    " + name + " <- " + ", ".join(f"{p}/{m}" for p, m in candidates))
    if len(multi) > 12:
        lines.append(f"    ... 其余 {len(multi) - 12} 个多候选别名见配置文件")
    lines.extend(f"  注意: {w}" for w in warnings)
    return config, proxy_entry, lines


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--fff-dir", default=str(DEFAULT_FFF_CONFIG), help="fff config directory")
    parser.add_argument("--out", default=str(DEFAULT_OUT), help="config to write")
    parser.add_argument("--providers", default=DEFAULT_PROVIDERS, help="provider order = priority")
    parser.add_argument(
        "--cooldown-ms", type=int, default=DEFAULT_COOLDOWN_MS,
        help=f"429 cooldown per upstream in ms (default {DEFAULT_COOLDOWN_MS})",
    )
    parser.add_argument(
        "--proxy-url", default=DEFAULT_PROXY_URL,
        help="base URL agents use (default: derived from the config's listen)",
    )
    parser.add_argument(
        "--proxy-key-env", default=DEFAULT_PROXY_KEY_ENV,
        help="placeholder key env name for the proxy provider entry",
    )
    parser.add_argument(
        "--no-store", action="store_true",
        help=f"do not write the {PROXY_PROVIDER} provider into the model-list store",
    )
    parser.add_argument("--dry-run", action="store_true", help="print the config instead of writing")
    args = parser.parse_args()

    config, proxy_entry, lines = build_config(args)
    text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"

    if args.dry_run:
        print(text, end="")
        print("\n".join(lines), file=sys.stderr)
        return 0

    out = Path(os.path.expanduser(args.out))
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(text, encoding="utf-8")
    if not args.no_store:
        store_path = Path(os.path.expanduser(args.fff_dir)) / "llm-models.json"
        write_proxy_provider(store_path, proxy_entry)
        lines.append(f"已更新 store 的 {PROXY_PROVIDER} provider: {store_path}")
    print("\n".join(lines))
    print(f"已写入 {config_path_label(out)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
