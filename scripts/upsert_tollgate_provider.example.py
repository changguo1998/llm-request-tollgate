#!/usr/bin/env python3
"""示例版 tg 条目生成器: 算本代理自己的 store 条目, 打印到标准输出.

公开仓库里的示例版, 不依赖 fff dotfiles 生态: 默认读同目录的示例 store
(``tg-store.example.json``) 与示例配置 (``tg-config.example.json``), 算出
``providers.tg`` 那条记录打印出来, 不写任何文件. 本机实际用的
``scripts/upsert_tollgate_provider.py`` 与它同源, 只多一步把条目 upsert 进
``~/fff/config/llm-models.json`` (那份脚本在 dotfiles 里维护, 不进公开仓库).

两步 (与 README "从 fff 模型列表生成配置" 一节一致):
    1. 只读校验: 组的成员与嵌套 (组名不得与逻辑名重名, 成员必须是已注册
       逻辑名, 禁止嵌套组); reasoning 是硬性分组条件 (同一逻辑名或组展开后
       不能既有推理模型又有非推理模型); contextWindow / maxTokens 先归各自
       档位再比, 跨档时 contextWindow 报错、maxTokens 只警告.
    2. 校验通过后构建条目并打印: contextWindow / maxTokens 取候选里最小的
       已声明原值 (档位只用于比较); thinkingLevelMap 按所有候选键的并集合并
       (每个候选先补齐, 再逐级取同名映射, 没有同名取最低 effort); reasoning
       全推理写 true.

用法:
    python3 scripts/upsert_tollgate_provider.example.py             # 打印条目
    python3 scripts/upsert_tollgate_provider.example.py --store X.json --cfg Y.json
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

PROXY_PROVIDER = "tg"  # store 里代表本代理自己的 provider 名
PROXY_KIND = "proxy"  # store 条目的 kind 标记: 生成列表/上游时一律跳过
DEFAULT_STORE = Path(__file__).with_name("tg-store.example.json")
DEFAULT_CONFIG = Path(__file__).with_name("tg-config.example.json")
DEFAULT_PROXY_URL = ""  # 空 = 从配置的 listen 推导
DEFAULT_PROXY_KEY_ENV = "TOLLGATE_API_KEY"

PARAM_FIELDS = ("reasoning", "contextWindow", "maxTokens", "thinkingLevelMap")
# 写入取最小原值的两项 (请求可能落到任一候选, 只有保守值不会被拒绝);
# 两者跨档时的处理不同: contextWindow 报错阻塞, maxTokens 只警告。
MIN_PARAM_FIELDS = ("contextWindow", "maxTokens")
_MISSING = object()  # 参数缺失的哨兵: 与任何已声明值都算不一致

# contextWindow 档位: 16K / 32K / 64K / 128K / 200K / 256K / 512K / 1M / 2M;
# 统一值 = 数字 × 1024 (K) 或 × 1048576 (M), 比较时用档位值。
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

# maxTokens 档位: 4K / 8K / 16K / 32K / 64K / 128K / 256K / 384K / 512K / 1M / 2M;
# 与 contextWindow 不是同一套 (输出上限比上下文小几档, 且要单独覆盖 384K)。
MAX_TOKENS_TIERS = (
    4096,
    8192,
    16384,
    32768,
    65536,
    131072,
    262144,
    393216,
    524288,
    1048576,
    2097152,
)

# 字段名 -> 档位表: 两个数值字段各用各的档位; 跨档时 contextWindow 报错,
# maxTokens 只警告 (输出上限差异由合并取最小值吸收, 不阻塞写入)。
PARAM_TIERS = {
    "contextWindow": CONTEXT_TIERS,
    "maxTokens": MAX_TOKENS_TIERS,
}


# 推理等级从 off (不开启, 0) 到 max (最高): 补齐时用来找"更高的最低等级"。
LEVEL_ORDER = ("off", "minimal", "low", "medium", "high", "xhigh", "max")
LEVEL_RANK = {level: rank for rank, level in enumerate(LEVEL_ORDER)}
# effort 名从低到高: 合并时没有同名映射就取这里最低的那个 (none = 不开启)。
EFFORT_ORDER = ("none", "minimal", "low", "medium", "high", "xhigh", "max")
EFFORT_RANK = {effort: rank for rank, effort in enumerate(EFFORT_ORDER)}


def load_json(path: Path):
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
    """同一逻辑名的候选模型参数不一致 (写 store 前应由 check_params 拦下)。"""


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


def param_tier(field: str, value):
    """档位校验入口: 把 contextWindow / maxTokens 归到该字段自己的最近档位。

    恰好居中时取较小档 (不夸大)。没有档位表的字段、非整数与 bool 原样返回,
    交给 ``_same`` 按值比较; 未声明 (``_MISSING``) 也原样返回。
    """
    tiers = PARAM_TIERS.get(field)
    if tiers is None or not isinstance(value, int) or isinstance(value, bool):
        return value
    return min(tiers, key=lambda tier: (abs(tier - value), tier))


def raw_params(source: dict) -> dict:
    """候选模型的四项参数原值; 未声明的填 ``_MISSING``。"""
    return {field: source.get(field, _MISSING) for field in PARAM_FIELDS}


def candidate_params(source: dict) -> dict:
    """比较用参数: contextWindow / maxTokens 归各自档位, 其余取原值。"""
    params = raw_params(source)
    for field in PARAM_TIERS:
        params[field] = param_tier(field, params[field])
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


def is_reasoning(value) -> bool:
    """reasoning 的二分口径: 只有显式 true 算推理模型; false 与缺失都算非推理。"""
    return value is True


def level_rank(level: str) -> int:
    """等级在 0..6 里的位置; 表里没有的等级排最后。"""
    return LEVEL_RANK.get(level, len(LEVEL_ORDER))


def effort_rank(effort: str) -> int:
    """effort 名从低到高的位置; 表里没有的名字按最低算 (合并取最保守)。"""
    return EFFORT_RANK.get(effort, -1)


def filled_level_map(level_map: dict, levels: list[str]) -> dict:
    """把一张等级表补齐到 ``levels``, 返回"等级 -> 发给上游的 effort 名"。

    对齐规则: 表里声明了该等级就用原值; 缺的 (缺键或 null) 指向上游提供的、比它
    高的最低等级, 没有更高等级时指向上游提供的最高等级。完全没有表 (空表或缺失)
    时按同名映射, 即等级名本身就是发给上游的名字。
    """
    declared = {
        level: effort
        for level, effort in (level_map or {}).items()
        if isinstance(effort, str) and effort
    }
    if not declared:
        return {level: level for level in levels}
    highest = max(declared, key=level_rank)
    filled = {}
    for level in levels:
        if level in declared:
            filled[level] = declared[level]
            continue
        higher = [name for name in declared if level_rank(name) > level_rank(level)]
        if higher:
            filled[level] = declared[min(higher, key=level_rank)]
        else:
            filled[level] = declared[highest]
    return filled


def merged_level_map(level_maps: list) -> dict:
    """按等级并集合并多张表, 返回"等级 -> 发给上游的 effort 名"。

    等级集合取所有候选键的并集 (等级数不会因某个候选缺键而缩水)。逐级合并: 有候选
    给出与等级同名的 effort 就用它 (不改变语义); 没有同名映射时取各候选里最低的
    effort (合并取最保守), 例如 off 在 A 上补成 low、B 上是 none, 结果取 none。
    所有候选都显式写 null 的等级保持 null —— 谁都不支持它, 补成更高等级只会把
    "不支持"说成"支持"; 缺键的不同, 属于没声明, 照常按补齐规则处理。没声明整张表的
    候选 (``_MISSING`` / 非 dict) 先滤掉, 它们按同名映射参与补齐。
    """
    maps = [
        level_map if isinstance(level_map, dict) else {} for level_map in level_maps
    ]
    levels = sorted(
        {level for level_map in maps for level in level_map}, key=level_rank
    )
    if not levels:
        return {}
    filled = [filled_level_map(level_map, levels) for level_map in maps]
    merged = {}
    for level in levels:
        if all(level in level_map and not level_map.get(level) for level_map in maps):
            merged[level] = None
            continue
        efforts = [one[level] for one in filled]
        if level in efforts:
            merged[level] = level
        else:
            merged[level] = min(efforts, key=effort_rank)
    return merged


def check_params(entries: dict, sources: dict) -> tuple[list[str], list[str]]:
    """第一步: 校验每个逻辑名 (或组) 的候选参数, 返回 (阻塞错误, 只警告)。

    reasoning 是硬性分组条件: 推理模型 (显式 true) 与非推理模型 (false 或缺失)
    落在同一候选链里直接拒绝 —— 逻辑名或组一旦展开就由代理按优先级挑候选, 客户端
    带上来的推理参数无法对两种模型都安全处理。contextWindow 跨档也拒绝 (上下文
    太小会让客户端裁掉本可处理的输入)。maxTokens 跨档只警告: 输出上限差异由合并
    取最小值吸收, 不阻塞写入。thinkingLevelMap 有确定的合并规则, 不比对。
    """
    errors = []
    warnings = []
    for name in sorted(entries):
        pairs = entries[name]
        if not pairs:
            continue
        pair_sources = [sources.get(mid, {}) for _, mid in pairs]
        reasoning = [is_reasoning(raw_params(src)["reasoning"]) for src in pair_sources]
        yes = [f"{up}/{mid}" for (up, mid), flag in zip(pairs, reasoning) if flag]
        no = [f"{up}/{mid}" for (up, mid), flag in zip(pairs, reasoning) if not flag]
        if yes and no:
            errors.append(
                f"{name}: 推理与非推理模型不能同组"
                f" (推理: {', '.join(yes)}; 非推理: {', '.join(no)})"
            )
        base_up, base_id = pairs[0]
        base = candidate_params(pair_sources[0])
        for (up_id, model_id), source in zip(pairs[1:], pair_sources[1:]):
            params = candidate_params(source)
            for field in PARAM_TIERS:
                if _same(base[field], params[field]):
                    continue
                left = _shown(base[field], pair_sources[0].get(field, _MISSING))
                right = _shown(params[field], source.get(field, _MISSING))
                message = (
                    f"{name}: {base_up}/{base_id} {field}={left}"
                    f" != {up_id}/{model_id} {field}={right}"
                )
                if field == "maxTokens":
                    warnings.append(message)
                else:
                    errors.append(message)
    return errors, warnings


def agreed_params(name: str, pairs: list, sources: dict, strict: bool = True) -> dict:
    """合并同一逻辑名 (或组) 的候选参数, 返回可写进条目的字段。

    reasoning 按二分口径合并: 候选里有推理模型就写 true, 全是非推理就不写 (false
    与缺失等价); 推理 / 非推理混排由 check_params 在第一步拦下。contextWindow /
    maxTokens 不写档位值, 取候选里最小的已声明原值 (0 及以下视为未声明): 请求可能
    落到任一候选, 只有保守值不会被上游拒绝。thinkingLevelMap 按 merged_level_map
    的并集与同名优先规则合并。所有会被拒绝的情况都由 check_params 在第一步拦下,
    ``strict`` 保留给摘要类调用方。
    """
    result = {}
    if not pairs:
        return result
    pair_sources = [sources.get(mid, {}) for _, mid in pairs]
    raws = [raw_params(source) for source in pair_sources]
    for field in PARAM_FIELDS:
        if field == "reasoning":
            if any(is_reasoning(raw[field]) for raw in raws):
                result[field] = True
            continue
        if field == "thinkingLevelMap":
            merged = merged_level_map([raw[field] for raw in raws])
            if merged:
                result[field] = merged
            continue
        if field in MIN_PARAM_FIELDS:
            declared = [
                raw[field]
                for raw in raws
                if isinstance(raw[field], int)
                and not isinstance(raw[field], bool)
                and raw[field] > 0
            ]
            if declared:
                result[field] = min(declared)
    return result


def build_proxy_provider(
    config: dict,
    store: dict,
    providers: list[str],
    proxy_url: str,
    key_env: str,
    strict: bool = True,
) -> dict:
    """Build the store entry representing this proxy: one model per alias (logical name).

    The proxy hides upstream ids, so agents only ever see alias names. Groups
    (config ``groups``, the "alias of alias" layer) are added the same way: a
    group name is a logical name servable by the proxy, so it appears as a model
    of its own.

    reasoning is a hard grouping rule: candidates of one logical name (or of a
    group, once expanded) must be all reasoning (explicit true) or all
    non-reasoning (false or missing); a mix is rejected by ``check_params``.
    thinkingLevelMap is merged over the union of all candidate keys: every
    candidate is first filled (a missing level points at the lowest *higher*
    level it supports, or at its highest level when none is higher; a candidate
    without a map maps each level to itself), then each level takes the
    same-named effort when any candidate offers it, otherwise the lowest effort
    among the candidates.
    contextWindow and maxTokens are compared as their own tier values
    (contextWindow 16K ... 2M, maxTokens 4K ... 2M with 384K), so close values
    such as 1000000 / 1048576 / 1050000 do not conflict, then written as the
    smallest declared raw value among the candidates -- the request may land on
    any of them, so only the conservative value cannot be rejected downstream.
    A contextWindow tier mismatch blocks the write; a maxTokens one is only a
    warning, because the merged minimum already absorbs the difference.
    ``main`` validates this in a separate first step and exits before writing;
    ``strict`` is kept for callers that ask for the summary variant.
    """
    entries = build_entries(config)
    sources = collect_sources(store, providers)
    models = []
    for name in sorted(entries):
        entry = {"id": name}
        entry.update(agreed_params(name, entries[name], sources, strict))
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

    # 第一步: 只读校验; 有阻塞错误就逐条列出并退出 1, 不输出条目。
    # maxTokens 跨档只警告, 不拦住输出 (合并时取最小值)。
    problems = check_groups(config)
    entries = build_entries(config)
    sources = collect_sources(store, providers)
    errors, warnings = check_params(entries, sources)
    problems += errors
    for warning in warnings:
        print(f"警告 (不阻塞): {warning}", file=sys.stderr)
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
