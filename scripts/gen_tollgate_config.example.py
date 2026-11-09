#!/usr/bin/env python3
"""示例版配置生成器: 从模型列表文件 + 已有配置的权重生成 upstreams 与 aliases.

这是公开仓库里的示例版, 不依赖 fff dotfiles 生态. 本机实际用的
scripts/gen_tollgate_config.py 与它同源, 但直接读 ~/fff/config/llm-models.json
(含 droplist 黑名单过滤、写回代理自身的 provider 条目等本机约定), 那份脚本在
本机 dotfiles 里维护, 不进公开仓库.

输入形状与 fff store 一致 (见同目录 llm-models.example.json), 权重不在 store 里:
    providers.<name>.baseUrl / apiKey ("$NAME" 占位符)
    providers.<name>.models[].id / contextWindow? / maxTokens? / thinkingLevelMap? / reasoning?

权重从已有配置 (--cfg, 形状见 config.example.json) 的 upstreams 读, 参数可选:
    upstreams[].priority                          = provider 权重 (负值停用整家)
    upstreams[].models[].priority                 = 模型权重 (负值停用该模型)
    upstreams[].models[].group                    = 逻辑名 (缺省 = 模型 id)
    upstreams[].models[].billing_group            = 额度组 (缺省 = 默认组)
    不给 --cfg 或条目缺失时用缺省 (0 / 0 / 模型 id / 无).

规则 (与 README -配置- 一节一致):
    - 逻辑名 = 条目的 group, 没有就是模型 id; 跨上游同名归并成一个别名.
    - 候选顺序 = 模型 priority 降序, 同值看 provider 的 priority 降序, 再同值按
      upstreams 顺序. 0 最小, 缺省即 0.
    - 模型 priority 为负: 该模型停用, 不进候选 (但仍留在 upstreams 的 models, 权重不丢).
    - provider 的 priority 为负: 整家停用, 不进候选 (仍留在 upstreams).
    - key_env = store 里 apiKey 占位符去掉 "$" 后的名字 ($USTC_API_KEY -> USTC_API_KEY).
    - 额度组 = 条目的 billing_group, 同家同名一组, 缺省归默认组; 输出为上游
      models 里的 billing_group. 上游的 models 现在固定写出 (它带着权重字段).
    - groups (别名的别名, 手工维护顶层键) 从已有配置原样保留; 生成后若某组成员
      不再是对应别名 (模型被删 / 停用), 打印警告 —— 代理加载时会因「成员不是已
      注册逻辑名」报错拦住.

用法:
    python3 scripts/gen_tollgate_config.example.py                 # 读示例列表, 打印 JSON
    python3 scripts/gen_tollgate_config.example.py --list X.json   # 用自己的模型列表
    python3 scripts/gen_tollgate_config.example.py --cfg Y.json    # 从已有配置继承权重
    python3 scripts/gen_tollgate_config.example.py --out out.json  # 写文件而非打印
"""

import argparse
import json
import sys
from pathlib import Path

DEFAULT_LIST = Path(__file__).with_name("llm-models.example.json")

PRIORITY_FIELD = "priority"
GROUP_FIELD = "group"
BILLING_FIELD = "billing_group"

# 组 (别名的别名): 组名 -> 已注册逻辑名列表. 手工维护, 从 --cfg 原样保留.
GROUPS_FIELD = "groups"


def weight(
    cfg: dict | None, upstream_id: str, model_id: str | None
) -> tuple[int, int, str, str]:
    """从已有配置取 (provider 优先级, 模型优先级, 逻辑名, 额度组), 缺省 (0, 0, 模型 id, 无)."""
    prov_prio, model_prio, group, billing = 0, 0, model_id or "", ""
    if cfg is None:
        return prov_prio, model_prio, group, billing
    for up in cfg.get("upstreams", []):
        if up.get("id") != upstream_id:
            continue
        prov_prio = int(up.get(PRIORITY_FIELD, 0) or 0)
        if model_id is None:
            break
        for m in up.get("models", []):
            if m.get("id") != model_id:
                continue
            model_prio = int(m.get(PRIORITY_FIELD, 0) or 0)
            if isinstance(m.get(GROUP_FIELD), str) and m.get(GROUP_FIELD).strip():
                group = m[GROUP_FIELD].strip()
            if isinstance(m.get(BILLING_FIELD), str) and m.get(BILLING_FIELD).strip():
                billing = m[BILLING_FIELD].strip()
            break
        break
    return prov_prio, model_prio, group, billing


def groups_of(cfg: dict | None) -> dict[str, list[str]]:
    """从已有配置取 groups (组名 -> 成员逻辑名列表), 原样保留; 空 / 缺省返回空表."""
    groups = (cfg or {}).get(GROUPS_FIELD, {})
    if not isinstance(groups, dict):
        return {}
    out: dict[str, list[str]] = {}
    for g, members in groups.items():
        if not isinstance(members, list):
            continue
        out[str(g)] = [str(m) for m in members]
    return out


def build(list_path: Path, cfg_path: Path) -> tuple[dict, list[str]]:
    data = json.loads(list_path.read_text(encoding="utf-8"))
    providers = data["providers"]
    cfg = (
        json.loads(cfg_path.read_text(encoding="utf-8")) if cfg_path.exists() else None
    )

    # (逻辑名, 上游 id, 真实模型 id, 模型权重, provider 权重, 出现序)
    rows = []
    for order, (up_id, prov) in enumerate(providers.items()):
        for model in prov.get("models", []):
            model_id = model["id"]
            prov_prio, model_prio, group, _ = weight(cfg, up_id, model_id)
            if prov_prio < 0 or model_prio < 0:
                continue  # 负优先级: 模型或整家停用, 不进候选
            rows.append(
                (group or model_id, up_id, model_id, model_prio, prov_prio, order)
            )

    aliases: dict[str, list[list[str]]] = {}
    for row in sorted(rows, key=lambda r: (-r[3], -r[4], r[5])):
        logical, up_id, model_id, _, _, _ = row
        aliases.setdefault(logical, []).append([up_id, model_id])

    upstreams = []
    for up_id, prov in providers.items():
        prov_prio, _, _, _ = weight(cfg, up_id, None)
        models = []
        for model in prov.get("models", []):
            _, model_prio, group, billing = weight(cfg, up_id, model["id"])
            item = {
                "id": model["id"],
                PRIORITY_FIELD: model_prio,
                GROUP_FIELD: group or model["id"],
            }
            if billing:
                item[BILLING_FIELD] = billing
            models.append(item)
        entry = {
            "id": up_id,
            "base_url": prov["baseUrl"],
            "auth_header": "Authorization",
            "key_env": str(prov.get("apiKey", "")).lstrip("$"),
            "headers": {},
            "include_usage": True,
            PRIORITY_FIELD: prov_prio,
            "models": models,
        }
        upstreams.append(entry)

    out: dict = {"upstreams": upstreams, "aliases": aliases}
    warnings: list[str] = []
    groups = groups_of(cfg)
    # 组只能在生成的别名集合里引用成员: 成员被删 / 停用后不再是别名, 警告但保留原样
    # (代理加载时会显式报错, 别在这里静默改掉手工维护的列表).
    if groups:
        out[GROUPS_FIELD] = groups
        alias_ids = set(aliases)
        for g, members in groups.items():
            missing = [m for m in members if m not in alias_ids]
            if missing:
                warnings.append(
                    f"group {g}: member(s) no longer an alias: {', '.join(missing)}"
                )
    return out, warnings


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--list", default=str(DEFAULT_LIST), help="模型列表 JSON (缺省示例文件)"
    )
    ap.add_argument("--cfg", default="", help="已有配置 JSON (缺省无, 权重用缺省)")
    ap.add_argument("--out", default="", help="写到文件而非 stdout")
    args = ap.parse_args()

    out, warnings = build(Path(args.list), Path(args.cfg))
    text = json.dumps(out, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    for w in warnings:
        print(f"注意: {w}", file=sys.stderr)
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"已写入 {args.out}", file=sys.stderr)
    else:
        print(text)


if __name__ == "__main__":
    main()
