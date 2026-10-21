#!/usr/bin/env python3
"""示例版配置生成器: 从模型列表文件生成 tollgate 的 upstreams 与 aliases.

这是公开仓库里的示例版, 不依赖 fff dotfiles 生态. 本机实际用的
scripts/gen_tollgate_config.py 与它同源, 但直接读 ~/fff/config/llm-models.json
(含 droplist 黑名单过滤、写回代理自身的 provider 条目等本机约定), 那份脚本在
本机 dotfiles 里维护, 不进公开仓库.

输入形状与 fff store 一致 (见同目录 llm-models.example.json):
    providers.<name>.baseUrl / apiKey ("$NAME" 占位符) / tollgate_priority?
    providers.<name>.models[].id / tollgate_group? / tollgate_billing? / tollgate_priority?

规则 (与 README -配置- 一节一致):
    - 逻辑名 = 条目的 tollgate_group, 没有就是模型 id; 跨上游同名归并成一个别名.
    - 候选顺序 = 模型 tollgate_priority 降序, 同值看 provider 的 tollgate_priority
      降序, 再同值按列表出现顺序. 0 最小, 缺省即 0.
    - key_env = store 里 apiKey 占位符去掉 "$" 后的名字 ($USTC_API_KEY -> USTC_API_KEY).
    - 额度组 = 条目的 tollgate_billing, 同家同名一组, 缺省归默认组; 输出为上游
      models 里的 billing_group. 整家都没声明时不写 models (与加组之前的形状一致).

用法:
    python3 scripts/gen_tollgate_config.example.py                 # 读示例列表, 打印 JSON
    python3 scripts/gen_tollgate_config.example.py --list X.json   # 用自己的模型列表
    python3 scripts/gen_tollgate_config.example.py --out out.json  # 写文件而非打印
"""
import argparse
import json
import sys
from pathlib import Path

DEFAULT_LIST = Path(__file__).with_name("llm-models.example.json")


def provider_priority(provider: dict) -> int:
    return int(provider.get("tollgate_priority", 0))


def model_priority(model: dict) -> int:
    return int(model.get("tollgate_priority", 0))


def build(list_path: Path) -> dict:
    data = json.loads(list_path.read_text(encoding="utf-8"))
    providers = data["providers"]

    # (逻辑名, 上游 id, 真实模型 id, 模型权重, provider 权重, 出现序)
    rows = []
    for up_id, prov in providers.items():
        for order, model in enumerate(prov.get("models", [])):
            logical = model.get("tollgate_group") or model["id"]
            rows.append((logical, up_id, model["id"], model_priority(model),
                         provider_priority(prov), order))

    aliases: dict[str, list[list[str]]] = {}
    for row in sorted(rows, key=lambda r: (-r[3], -r[4], r[5])):
        logical, up_id, model_id, _, _, _ = row
        aliases.setdefault(logical, []).append([up_id, model_id])

    upstreams = []
    for up_id, prov in providers.items():
        models = []
        for model in prov.get("models", []):
            item = {"id": model["id"]}
            group = model.get("tollgate_billing")
            if isinstance(group, str) and group.strip():
                item["billing_group"] = group.strip()
            models.append(item)
        if not any("billing_group" in m for m in models):
            models = []  # 整家都没声明额度组: 不写 models, 归默认组
        entry = {
            "id": up_id,
            "base_url": prov["baseUrl"],
            "auth_header": "Authorization",
            "key_env": str(prov.get("apiKey", "")).lstrip("$"),
            "headers": {},
            "include_usage": True,
        }
        if models:
            entry["models"] = models
        upstreams.append(entry)

    return {"upstreams": upstreams, "aliases": aliases}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--list", default=str(DEFAULT_LIST), help="模型列表 JSON (缺省示例文件)")
    ap.add_argument("--out", default="", help="写到文件而非 stdout")
    args = ap.parse_args()

    out = build(Path(args.list))
    text = json.dumps(out, indent=2, ensure_ascii=False) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"已写入 {args.out}", file=sys.stderr)
    else:
        print(text)


if __name__ == "__main__":
    main()
