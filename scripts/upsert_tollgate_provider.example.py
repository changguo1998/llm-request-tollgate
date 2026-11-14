#!/usr/bin/env python3
"""示例版 tg 条目生成器: 算本代理自己的 store 条目, 打印到标准输出.

公开仓库里的示例版, 不依赖 fff dotfiles 生态: 默认读同目录的示例 store
(``tg-store.example.json``) 与示例配置 (``tg-config.example.json``), 算出
``providers.tg`` 那条记录打印出来, 不写任何文件. 本机实际用的
``scripts/upsert_tollgate_provider.py`` 与它同源, 只多一步把条目 upsert 进
``~/fff/config/llm-models.json`` (那份脚本在 dotfiles 里维护, 不进公开仓库).

两步 (与 README "从 fff 模型列表生成配置" 一节一致):
    1. 只读校验: 组的成员与嵌套 (组名不得与逻辑名重名, 成员必须是已注册
       逻辑名, 禁止嵌套组), 以及同一逻辑名的候选四项参数 (reasoning /
       contextWindow / maxTokens / thinkingLevelMap) 是否完全一致 (缺失也算
       不一致); 有问题逐条打到 stderr 并以退出码 1 结束.
    2. 校验通过后构建条目并打印: contextWindow 先归档位 (16K/32K/64K/128K/
       200K/256K/512K/1M/2M, 取最近档, 居中取小档), 比较与写入都用档位统一
       值 (示例 store 里的 1000000 在条目里是 1048576); 其余三项原样.

用法:
    python3 scripts/upsert_tollgate_provider.example.py             # 打印条目
    python3 scripts/upsert_tollgate_provider.example.py --store X.json --cfg Y.json
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

DEFAULT_STORE = Path(__file__).with_name("tg-store.example.json")
DEFAULT_CONFIG = Path(__file__).with_name("tg-config.example.json")
DEFAULT_PROXY_KEY_ENV = "TOLLGATE_API_KEY"

PROXY_PROVIDER = "tg"  # store 里代表本代理自己的 provider 名
PARAM_FIELDS = ("reasoning", "contextWindow", "maxTokens", "thinkingLevelMap")
_MISSING = object()  # 参数缺失的哨兵: 与任何已声明值都算不一致

# 上下文档位: 16K / 32K / 64K / 128K / 200K / 256K / 512K / 1M / 2M;
# 统一值 = 数字 × 1024 (K) 或 × 1048576 (M), 写入时用统一值。
CONTEXT_TIERS = (
    16384,
    32768,
    65536,
    131072,
    204800,
    262144,
    524288,
    1048576,
    2097152,
)


def load_json(path: Path) -> dict:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def upstream_ids(config: dict) -> list[str]:
    """config.upstreams 的上游 id (排除代理自身), 用来收集候选模型参数。"""
    return [
        u["id"]
        for u in config.get("upstreams", [])
        if isinstance(u.get("id"), str) and u["id"] != PROXY_PROVIDER
    ]


class ParamMismatch(Exception):
    """同一逻辑名的候选模型参数不一致 (由 check_params 拦下)。"""


def model_entries(config: dict) -> dict[str, list[list[str]]]:
    """候选表: 逻辑名 -> [(上游 id, 真实模型 id), ...]; 组展开的基底。

    顺序 = 配置里按模型 priority 排好的 models 顺序 + 上游 priority + 配置顺序,
    与代理的 rebuildAliases 同一条规则 (顶层 aliases 键已废弃)。
    """
    entries = {}
    for up in config.get("upstreams", []):
        up_prio = up.get("priority", 0) or 0
        if up_prio < 0:
            continue  # provider 负优先级: 整家停用
        for model in up.get("models", []):
            mp = model.get("priority", 0) or 0
            if mp < 0:
                continue  # 模型负优先级: 停用
            mid = model.get("id")
            if not mid:
                continue
            name = model.get("alias")
            if name is None:
                name = model.get("group")  # 旧配置兼容
            if not name or not isinstance(name, str) or not name.strip():
                name = mid
            entries.setdefault(name.strip(), []).append([up["id"], mid])
    return entries


def build_entries(config: dict) -> dict[str, list[list[str]]]:
    """候选表: 逻辑名 / 组名 -> [(上游 id, 真实模型 id), ...]。

    组按 members 顺序展开并跨成员去重, 与代理的 expandedAliases 同一条规则;
    成员不存在 / 嵌套组 / 组名重名由 check_groups 在第一步拦下。
    """
    entries = model_entries(config)
    for gname, members in config.get("groups", {}).items():
        chain, seen = [], set()
        for member in members:
            if not isinstance(member, str):
                continue
            for pair in entries.get(member.strip(), []):
                key = tuple(pair)
                if key in seen:
                    continue
                seen.add(key)
                chain.append(pair)
        entries[gname.strip()] = chain
    return entries


def check_groups(config: dict) -> list[str]:
    """第一步: 校验组的成员与嵌套, 返回全部问题描述。

    与代理的 normalizeGroups 同口径: 组名不能与逻辑名重名 (展开后互相覆盖);
    成员必须是非空的已注册逻辑名, 组名作成员即嵌套, 代理加载时会拒绝; 空组
    展开后候选链为空, 写进 store 只会多一个不可用的逻辑名。
    """
    groups = config.get("groups", {})
    names = model_entries(config)
    group_names = {str(gname).strip() for gname in groups}
    problems = []
    for raw_name, members in groups.items():
        gname = str(raw_name).strip()
        if not gname:
            problems.append("组名不能为空")
            continue
        if gname in names:
            problems.append(f"组 {gname}: 组名与逻辑名重名")
        if not isinstance(members, list) or not members:
            problems.append(f"组 {gname}: 组内没有成员")
            continue
        for raw_member in members:
            member = raw_member.strip() if isinstance(raw_member, str) else ""
            if not member:
                problems.append(f"组 {gname}: 成员名为空")
            elif member in group_names:
                problems.append(f"组 {gname}: 成员 {member} 是组名, 不允许嵌套")
            elif member not in names:
                problems.append(f"组 {gname}: 成员 {member} 不是已注册的逻辑名")
    return problems


def collect_sources(store: dict, providers: list[str]) -> dict[str, dict]:
    """真实模型 id -> store 里的模型条目 (同一 id 以最后出现的 provider 为准)。"""
    sources = {}
    for provider in providers:
        for model in store[provider].get("models", []):
            sources[model["id"]] = model
    return sources


def context_tier(value: int) -> int:
    """把 contextWindow 归到最近的档位; 恰好居中时取较小档 (不夸大)。"""
    return min(CONTEXT_TIERS, key=lambda tier: (abs(tier - value), tier))


def candidate_params(source: dict) -> dict:
    """候选模型的四项参数; contextWindow 先归档位, 未声明的填 ``_MISSING``。"""
    params = {field: source.get(field, _MISSING) for field in PARAM_FIELDS}
    window = params["contextWindow"]
    if isinstance(window, int) and not isinstance(window, bool):
        params["contextWindow"] = context_tier(window)
    return params


def _same(left, right) -> bool:
    """两项参数是否视为相同; 缺失只与缺失相同。"""
    if left is _MISSING or right is _MISSING:
        return left is right
    return left == right


def _shown(value, raw=None) -> str:
    """冲突报告里的取值展示; 归过档的值附上原值。"""
    if value is _MISSING:
        return "<缺失>"
    text = json.dumps(value, ensure_ascii=False, sort_keys=True)
    if raw is not None and raw is not _MISSING and raw != value:
        text += f" (原 {json.dumps(raw, ensure_ascii=False)})"
    return text


def check_params(entries: dict, sources: dict) -> list[str]:
    """第一步: 校验每个逻辑名的候选参数是否完全一致, 返回全部冲突描述。"""
    conflicts = []
    for name in sorted(entries):
        pairs = entries[name]
        if not pairs:
            continue
        base_up, base_id = pairs[0]
        base_source = sources.get(base_id, {})
        base = candidate_params(base_source)
        for up_id, model_id in pairs[1:]:
            source = sources.get(model_id, {})
            params = candidate_params(source)
            for field in PARAM_FIELDS:
                if _same(base[field], params[field]):
                    continue
                left = _shown(base[field], base_source.get(field, _MISSING))
                right = _shown(params[field], source.get(field, _MISSING))
                conflicts.append(
                    f"{name}: {base_up}/{base_id} {field}={left}"
                    f" != {up_id}/{model_id} {field}={right}"
                )
    return conflicts


def agreed_params(name: str, pairs: list, sources: dict) -> dict:
    """四项参数全部候选一致时才收录 (contextWindow 已归档), 否则抛错。"""
    result = {}
    for field in PARAM_FIELDS:
        values = [candidate_params(sources.get(mid, {}))[field] for _, mid in pairs]
        if not values:
            continue
        if not all(_same(values[0], value) for value in values[1:]):
            raise ParamMismatch(f"{name}: 候选模型的 {field} 不一致")
        value = values[0]
        if value is _MISSING:
            continue
        if field == "reasoning" and not value:
            continue  # 沿用旧输出: 只在声明为 true 时写
        result[field] = value
    return result


def build_proxy_provider(
    config: dict, store: dict, providers: list[str], proxy_url: str, key_env: str
) -> dict:
    """Build the store entry representing this proxy: one model per logical name.

    Groups (``config.groups``, the "alias of alias" layer) are logical names
    too, so each group becomes a model of its own. The four params are inherited
    only when every candidate agrees; ``main`` validates them in a separate
    first step, so a mismatch reaching here raises ``ParamMismatch``.
    """
    entries = build_entries(config)
    sources = collect_sources(store, providers)
    models = []
    for name in sorted(entries):
        entry = {"id": name}
        entry.update(agreed_params(name, entries[name], sources))
        models.append(entry)
    return {
        "kind": "proxy",
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


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument(
        "--store", default=str(DEFAULT_STORE), help="模型列表 store (缺省示例文件)"
    )
    parser.add_argument(
        "--cfg", default=str(DEFAULT_CONFIG), help="代理配置 (缺省示例文件)"
    )
    parser.add_argument(
        "--proxy-url",
        default="",
        help="base URL agents use (default: derived from the config's listen)",
    )
    parser.add_argument(
        "--proxy-key-env",
        default=DEFAULT_PROXY_KEY_ENV,
        help="placeholder key env name for the proxy provider entry",
    )
    args = parser.parse_args()

    store_path = Path(args.store)
    config_path = Path(args.cfg)
    for label, path in (("store", store_path), ("config", config_path)):
        if not path.exists():
            print(f"{label} 文件不存在: {path}", file=sys.stderr)
            return 2
    config = load_json(config_path)
    store = load_json(store_path).get("providers", {})
    providers = upstream_ids(config)
    missing = [provider for provider in providers if provider not in store]
    if missing:
        print(f"store 里没有配置的上游: {', '.join(missing)}", file=sys.stderr)
        return 2

    # 第一步: 只读校验; 有问题就逐条列出并退出 1, 不输出条目。
    problems = check_groups(config)
    entries = build_entries(config)
    sources = collect_sources(store, providers)
    problems += check_params(entries, sources)
    if problems:
        print("配置校验未通过, 未生成条目:", file=sys.stderr)
        for problem in problems:
            print(f"  {problem}", file=sys.stderr)
        return 1

    # 第二步: 校验通过后才构建条目并打印 (不写任何文件)。
    proxy_url = args.proxy_url or derive_proxy_url(
        config.get("listen", "127.0.0.1:8787")
    )
    entry = build_proxy_provider(
        config, store, providers, proxy_url, args.proxy_key_env
    )
    print(json.dumps(entry, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
