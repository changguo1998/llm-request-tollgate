#!/usr/bin/env node
/**
 * TUI editor for tollgate priorities in the shared model-list store.
 *
 * Edits ``tollgate_priority`` on provider entries and on model entries of the
 * store (``~/fff/config/llm-models.json``), previews the resulting candidate
 * order live, and can regenerate the proxy config + hot-reload the service.
 *
 * Candidate order inside an alias = model priority desc -> provider priority
 * desc -> provider order (the ``--providers`` order used for generation);
 * 0 is the lowest. Dropped ids (llm-models-droplist.json) are hidden, like the
 * generator does.
 *
 * Runs on Node >= 22.6 (type stripping), no build step and no dependencies:
 *     node scripts/set_priority.ts
 *
 * Keys:
 *     up/down PgUp/PgDn   move        g/G   first/last
 *     Tab                 switch pane +/-   priority -1/+1
 *     =                   type value  /     filter models
 *     s                   save store  r     save + regenerate config
 *     h                   hup service q     quit (asks when unsaved)
 *
 * Non-interactive:
 *     node scripts/set_priority.ts --order [alias]
 *     node scripts/set_priority.ts --set <provider|model> <value>
 */

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

type Model = {
  id: string;
  tollgate_group?: string;
  tollgate_priority?: number;
  [key: string]: unknown;
};
type Provider = {
  kind?: string;
  tollgate_priority?: number;
  models?: Model[];
  [key: string]: unknown;
};
type Store = { providers: Record<string, Provider>; [key: string]: unknown };
type Candidate = { provider: string; model: Model };

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const GEN = join(REPO_ROOT, "scripts", "gen_tollgate_config.py");
const DEFAULT_STORE = join(process.env.FFF_PRIVATE ?? join(homedir(), ".fff"), "config", "llm-models.json");
const DEFAULT_CONFIG = join(REPO_ROOT, "config.local.json");
const PRIORITY = "tollgate_priority";
const GROUP = "tollgate_group";
const PROXY_KIND = "proxy";
const FALLBACK_PROVIDERS = ["ustc", "arkcp", "arkap", "group"];
const GENERATOR_PROVIDERS = "ustc,arkcp,arkap,group";

// ---------- store ----------

function loadStore(path: string): Store {
  return JSON.parse(readFileSync(path, "utf8")) as Store;
}

function priorityOf(entry: { tollgate_priority?: unknown }): number {
  const value = entry[PRIORITY];
  return typeof value === "number" && Number.isInteger(value) ? value : 0;
}

function isProxy(entry: Provider): boolean {
  return entry.kind === PROXY_KIND;
}

function upstreams(store: Store): Record<string, Provider> {
  return Object.fromEntries(Object.entries(store.providers ?? {}).filter(([, e]) => !isProxy(e)));
}

/** Atomic write; model keys stay sorted, the priority key stays before models. */
function saveStore(path: string, store: Store): void {
  for (const [name, entry] of Object.entries(store.providers ?? {})) {
    if (isProxy(entry)) continue;
    entry.models = (entry.models ?? []).map((model) =>
      Object.fromEntries(Object.keys(model).sort().map((k) => [k, model[k]])),
    ) as Model[];
    if (PRIORITY in entry) {
      const ordered: Provider = {};
      for (const [k, v] of Object.entries(entry)) if (k !== PRIORITY && k !== "models") ordered[k] = v;
      ordered[PRIORITY] = entry[PRIORITY];
      ordered.models = entry.models;
      store.providers[name] = ordered;
    }
  }
  const tmp = `${path}.tmp`;
  writeFileSync(tmp, `${JSON.stringify(store, null, 4)}\n`, "utf8");
  renameSync(tmp, path);
}

/** Tie-break order: the config's upstream order, else the generator default. */
function providerOrder(store: Store, config: string): string[] {
  if (existsSync(config)) {
    try {
      const cfg = JSON.parse(readFileSync(config, "utf8")) as { upstreams?: { id: string }[] };
      const known = (cfg.upstreams ?? []).map((u) => u.id).filter((id) => id in (store.providers ?? {}));
      if (known.length) return known;
    } catch {
      /* fall through to the default order */
    }
  }
  return FALLBACK_PROVIDERS.filter((p) => p in (store.providers ?? {}));
}

function loadDroplist(storePath: string, config: string): Set<string> {
  for (const base of [dirname(storePath), dirname(resolve(config))]) {
    try {
      const list = JSON.parse(readFileSync(join(base, "llm-models-droplist.json"), "utf8")) as unknown[];
      return new Set(list.filter((i): i is string => typeof i === "string"));
    } catch {
      /* try the next location */
    }
  }
  return new Set();
}

function aliasOf(model: Model): string {
  const group = model[GROUP];
  return typeof group === "string" && group.trim() ? group.trim() : model.id;
}

/** Replicate the generator's grouping + ordering, so the preview matches it. */
function buildAliases(store: Store, config: string, storePath: string): Map<string, Candidate[]> {
  const dropped = loadDroplist(storePath, config);
  const ranked = new Map<string, { key: number[]; provider: string; model: Model }[]>();
  providerOrder(store, config).forEach((name, rank) => {
    const entry = store.providers[name];
    for (const model of entry.models ?? []) {
      if (dropped.has(model.id)) continue;
      const key = [-priorityOf(model), -priorityOf(entry), rank];
      const alias = aliasOf(model);
      if (!ranked.has(alias)) ranked.set(alias, []);
      ranked.get(alias)!.push({ key, provider: name, model });
    }
  });
  const sorted = [...ranked.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return new Map(
    sorted.map(([alias, items]) => [
      alias,
      [...items]
        .sort((a, b) => {
          const diff = a.key.findIndex((v, i) => v !== b.key[i]);
          return diff === -1 ? a.model.id.localeCompare(b.model.id) : a.key[diff] - b.key[diff];
        })
        .map(({ provider, model }) => ({ provider, model })),
    ]),
  );
}

function allModels(store: Store, config: string, storePath: string): { alias: string; provider: string; model: Model; entry: Provider }[] {
  const rows: { alias: string; provider: string; model: Model; entry: Provider }[] = [];
  for (const [alias, cands] of buildAliases(store, config, storePath)) {
    for (const { provider, model } of cands) {
      rows.push({ alias, provider, model, entry: store.providers[provider] });
    }
  }
  return rows;
}

// ---------- generator / service ----------

function runGenerator(storeDir: string, config: string): { ok: boolean; out: string } {
  try {
    const out = execFileSync(
      "python3",
      [GEN, "--fff-dir", storeDir, "--out", config, "--providers", GENERATOR_PROVIDERS],
      { cwd: REPO_ROOT, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] },
    );
    return { ok: true, out };
  } catch (err) {
    const e = err as { stderr?: string; stdout?: string; message?: string };
    return { ok: false, out: (e.stderr || e.stdout || e.message || "").trim() };
  }
}

function runHup(): { ok: boolean; out: string } {
  try {
    const out = execFileSync(join(REPO_ROOT, "deploy", "tollgate.sh"), ["hup"], { cwd: REPO_ROOT, encoding: "utf8" });
    return { ok: true, out };
  } catch (err) {
    const e = err as { stderr?: string; stdout?: string; message?: string };
    return { ok: false, out: (e.stderr || e.stdout || e.message || "").trim() };
  }
}

// ---------- terminal helpers ----------

const keys = [
  ["↑/↓", "移动"], ["PgUp/PgDn", "翻页"], ["g/G", "首末"], ["Tab", "换面板"],
  ["+/-", "优先级"], ["=", "输入值"], ["/", "过滤"], ["s", "保存"],
  ["r", "重生成"], ["h", "hup"], ["q", "退出"],
] as const;
const FOOTER_ROWS = 3; // 1 message line + 2 hint lines

function charWidth(cp: number): number {
  const wide =
    (cp >= 0x1100 && cp <= 0x115f) || cp === 0x2329 || cp === 0x232a ||
    (cp >= 0x2e80 && cp <= 0xa4cf && cp !== 0x303f) || (cp >= 0xac00 && cp <= 0xd7a3) ||
    (cp >= 0xf900 && cp <= 0xfaff) || (cp >= 0xfe30 && cp <= 0xfe6f) ||
    (cp >= 0xff00 && cp <= 0xff60) || (cp >= 0xffe0 && cp <= 0xffe6) ||
    (cp >= 0x1f300 && cp <= 0x1f64f) || (cp >= 0x1f900 && cp <= 0x1f9ff) ||
    (cp >= 0x20000 && cp <= 0x3fffd);
  return wide ? 2 : 1;
}

function displayWidth(text: string): number {
  let width = 0;
  for (const ch of text) width += charWidth(ch.codePointAt(0) ?? 0);
  return width;
}

function truncate(text: string, limit: number): string {
  let out = "";
  let width = 0;
  for (const ch of text) {
    const w = charWidth(ch.codePointAt(0) ?? 0);
    if (width + w > limit) return `${out}…`;
    out += ch;
    width += w;
  }
  return out;
}

function pad(text: string, limit: number): string {
  const cut = truncate(text, limit);
  return cut + " ".repeat(Math.max(0, limit - displayWidth(cut)));
}

/** Wrap the key hints into at most `rows` lines for the given width. */
function layoutKeys(width: number, rows = FOOTER_ROWS - 1): (readonly [string, string])[][] {
  const lines: (readonly [string, string])[][] = [];
  let current: (readonly [string, string])[] = [];
  let used = 0;
  for (const pair of keys) {
    const need = displayWidth(pair[0]) + displayWidth(pair[1]) + 3;
    if (current.length && used + need > width - 1) {
      lines.push(current);
      current = [];
      used = 0;
      if (lines.length >= rows) return lines;
    }
    current.push(pair);
    used += need;
  }
  if (current.length && lines.length < rows) lines.push(current);
  return lines;
}

const ATTR = { reset: "\x1b[0m", reverse: "\x1b[7m", bold: "\x1b[1m", dim: "\x1b[2m" };

/** Terminal size; a pty may report 0, so fall back and clamp to a usable frame. */
function termSize(): { cols: number; rows: number } {
  const cols = process.stdout.columns || 100;
  const rows = process.stdout.rows || 30;
  return { cols: Math.max(cols, 60), rows: Math.max(rows, 14) };
}

// ---------- TUI ----------

type Prompt = { label: string; buffer: string; commit: (value: string) => void };

class Tui {
  storePath: string;
  config: string;
  store: Store;
  dirty: boolean | "asked" = false;
  focus: "provider" | "model" = "model";
  provSel = 0;
  modelSel = 0;
  top = 0;
  filter = "";
  message = "";
  prompt: Prompt | null = null;

  constructor(storePath: string, config: string, store: Store) {
    this.storePath = storePath;
    this.config = config;
    this.store = store;
    this.message = `${storePath} | ${providerOrder(store, config).length} 个 provider`;
  }

  providers(): string[] {
    return providerOrder(this.store, this.config);
  }

  rows() {
    const rows = allModels(this.store, this.config, this.storePath);
    if (!this.filter) return rows;
    return rows.filter(
      (r) => r.alias.includes(this.filter) || r.provider.includes(this.filter) || r.model.id.includes(this.filter),
    );
  }

  selected() {
    const rows = this.rows();
    if (!rows.length) return null;
    this.modelSel = Math.max(0, Math.min(this.modelSel, rows.length - 1));
    return rows[this.modelSel];
  }

  adjust(delta: number): void {
    if (this.focus === "provider") {
      const names = this.providers();
      if (!names.length) return;
      this.provSel = Math.max(0, Math.min(this.provSel, names.length - 1));
      const entry = this.store.providers[names[this.provSel]];
      entry[PRIORITY] = Math.max(0, priorityOf(entry) + delta);
      this.message = `${names[this.provSel]}: provider 优先级 = ${entry[PRIORITY]}`;
    } else {
      const row = this.selected();
      if (!row) return;
      row.model[PRIORITY] = Math.max(0, priorityOf(row.model) + delta);
      this.message = `${row.provider}/${row.model.id}: 模型优先级 = ${row.model[PRIORITY]}`;
    }
    this.dirty = true;
  }

  setValue(value: number): void {
    if (this.focus === "provider") {
      const entry = this.store.providers[this.providers()[this.provSel]];
      entry[PRIORITY] = Math.max(0, value);
      this.message = `provider 优先级 = ${entry[PRIORITY]}`;
    } else {
      const row = this.selected();
      if (!row) return;
      row.model[PRIORITY] = Math.max(0, value);
      this.message = `${row.provider}/${row.model.id}: 模型优先级 = ${row.model[PRIORITY]}`;
    }
    this.dirty = true;
  }

  save(): void {
    saveStore(this.storePath, this.store);
    this.dirty = false;
    this.message = `已保存 ${this.storePath}`;
  }

  regenerate(): void {
    this.save();
    const { ok, out } = runGenerator(dirname(this.storePath), this.config);
    this.message = ok ? "已重新生成配置" : `生成失败: ${out.split("\n").pop() ?? "?"}`;
  }

  hup(): void {
    const { ok, out } = runHup();
    this.message = ok ? "已 hup, 代理生效" : `hup 失败: ${out.split("\n").pop() ?? "?"}`;
  }

  draw(cols: number, rows: number): string {
    const leftW = Math.max(38, Math.floor(cols / 2));
    const body = Math.max(6, rows - FOOTER_ROWS);
    const lines: string[] = [];
    const provH = Math.min(this.providers().length + 3, Math.max(4, Math.floor(body / 3)));

    lines.push(`${ATTR.bold}${pad(" Providers" + (this.focus === "provider" ? " (焦点)" : ""), leftW - 1)}${ATTR.reset}`);
    this.providers().forEach((name, i) => {
      if (i + 1 >= provH) return;
      const attr = this.focus === "provider" && i === this.provSel ? ATTR.reverse : "";
      lines.push(`${attr}${pad(` ${name}  priority=${priorityOf(this.store.providers[name])}`, leftW - 1)}${ATTR.reset}`);
    });
    while (lines.length < provH) lines.push(" ".repeat(leftW - 1));

    const modelRows = this.rows();
    const visible = Math.max(1, body - provH - 1);
    if (this.focus === "model") {
      this.modelSel = Math.max(0, Math.min(this.modelSel, Math.max(0, modelRows.length - 1)));
      if (this.modelSel < this.top) this.top = this.modelSel;
      if (this.modelSel >= this.top + visible) this.top = this.modelSel - visible + 1;
    }
    const hidden = loadDroplist(this.storePath, this.config).size;
    lines.push(
      `${ATTR.bold}${pad(` 模型 (过滤: ${this.filter || "-"}, ${modelRows.length} 个, 黑名单剔除 ${hidden})${this.focus === "model" ? " (焦点)" : ""}`, leftW - 1)}${ATTR.reset}`,
    );
    for (let i = this.top; i < Math.min(modelRows.length, this.top + visible); i++) {
      const r = modelRows[i];
      const attr = this.focus === "model" && i === this.modelSel ? ATTR.reverse : "";
      lines.push(`${attr}${pad(` M${priorityOf(r.model)} P${priorityOf(r.entry)} ${r.provider}/${r.model.id}`, leftW - 1)}${ATTR.reset}`);
    }
    while (lines.length < body) lines.push(" ".repeat(leftW - 1));

    // right pane: candidate order preview
    const row = this.selected();
    const preview: string[] = [`${ATTR.bold}${pad(" 候选顺序预览", cols - leftW - 1)}${ATTR.reset}`];
    if (row) {
      const cands = buildAliases(this.store, this.config, this.storePath).get(row.alias) ?? [];
      preview.push(`${ATTR.bold}${pad(` ${row.alias} (${cands.length} 候选)`, cols - leftW - 1)}${ATTR.reset}`);
      cands.forEach((c, i) => {
        const attr = c.model === row.model ? ATTR.reverse : "";
        preview.push(`${attr}${pad(` ${i + 1}. ${c.provider}/${c.model.id}  M${priorityOf(c.model)} P${priorityOf(this.store.providers[c.provider])}`, cols - leftW - 1)}${ATTR.reset}`);
      });
    }
    for (let i = 0; i < body; i++) {
      lines[i] = (lines[i] ?? " ".repeat(leftW - 1)) + (preview[i] ?? "");
    }

    // footer: message line + key hints
    const message = ` ${this.prompt ? `${this.prompt.label} ${this.prompt.buffer}` : this.message}${this.dirty === true ? " [未保存]" : ""} `;
    lines.push(`${ATTR.reverse}${pad(message, cols - 1)}${ATTR.reset}`);
    const hintLines = layoutKeys(cols);
    for (let i = 0; i < FOOTER_ROWS - 1; i++) {
      const items = hintLines[i] ?? [];
      lines.push(items.map(([k, d]) => `${ATTR.bold} ${k} ${ATTR.reset}${ATTR.dim}${d} ${ATTR.reset}`).join(""));
    }
    return `${ATTR.reset}\x1b[H${lines.map((l) => `${l}\x1b[K`).join("\n")}`;
  }

  handle(input: string, quit: () => void): void {
    if (this.prompt) {
      const p = this.prompt;
      for (const ch of input) {
        if (ch === "\r" || ch === "\n") {
          this.prompt = null;
          p.commit(p.buffer.trim());
          return;
        }
        if (ch === "\x1b" || ch === "\x03") {
          this.prompt = null;
          return;
        }
        if (ch === "\x7f") p.buffer = p.buffer.slice(0, -1);
        else if (ch >= " ") p.buffer += ch;
      }
      return;
    }
    // 转义序列整块匹配; 其余按键逐字符处理 (成块到达的 "q\n" 也要能退出)。
    const sequences = ["\x1b[A", "\x1b[B", "\x1b[5~", "\x1b[6~", "\x1b[C", "\x1b[D"];
    const sequence = sequences.find((seq) => input.includes(seq));
    if (sequence) {
      this.onKey(sequence, quit);
      return;
    }
    for (const ch of input) this.onKey(ch, quit);
  }

  onKey(key: string, quit: () => void): void {
    switch (key) {
      case "\x03": // Ctrl-C
      case "q":
        if (this.dirty === true) {
          this.message = "有未保存改动: 再按 q 放弃退出, 或按 s 保存";
          this.dirty = "asked";
          return;
        }
        quit();
        return;
      case "\t":
        this.focus = this.focus === "model" ? "provider" : "model";
        this.provSel = Math.max(0, Math.min(this.provSel, this.providers().length - 1));
        return;
      case "\x1b[A": case "k": this.focus === "provider" ? (this.provSel -= 1) : (this.modelSel -= 1); break;
      case "\x1b[B": case "j": this.focus === "provider" ? (this.provSel += 1) : (this.modelSel += 1); break;
      case "\x1b[5~": this.modelSel -= 10; break;
      case "\x1b[6~": this.modelSel += 10; break;
      case "g": this.modelSel = 0; break;
      case "G": this.modelSel = 10 ** 6; break;
      case "+": case "\x1b[C": this.adjust(1); break;
      case "-": case "\x1b[D": this.adjust(-1); break;
      case "=":
        this.prompt = { label: "新的优先级数值:", buffer: "", commit: (v) => Number.isInteger(Number(v)) && v !== "" && this.setValue(Number(v)) };
        break;
      case "/":
        this.prompt = { label: "过滤 (空 = 清除):", buffer: "", commit: (v) => { this.filter = v; this.modelSel = 0; this.top = 0; } };
        break;
      case "s": this.save(); break;
      case "r": this.regenerate(); break;
      case "h": this.hup(); break;
      default: return;
    }
    if (this.dirty === "asked") this.dirty = true;
  }
}

function runTui(tui: Tui): void {
  const stdin = process.stdin;
  stdin.setRawMode(true);
  stdin.resume();
  stdin.setEncoding("utf8");
  process.stdout.write("\x1b[?25l\x1b[2J");
  const render = () => {
    const { cols, rows } = termSize();
    process.stdout.write(tui.draw(cols, rows));
  };
  const cleanup = (code = 0) => {
    process.stdout.write(`${ATTR.reset}\x1b[?25h\x1b[2J\x1b[H`);
    if (stdin.isTTY) stdin.setRawMode(false);
    stdin.pause();
    process.exit(code);
  };
  stdin.on("data", (chunk: string) => {
    tui.handle(chunk, () => cleanup(0));
    render();
  });
  process.stdout.on("resize", render);
  process.on("SIGINT", () => cleanup(130));
  render();
}

// ---------- headless ----------

function printOrder(store: Store, config: string, storePath: string, alias?: string): void {
  for (const [name, cands] of buildAliases(store, config, storePath)) {
    if (alias && !name.includes(alias)) continue;
    if (!alias && cands.length < 2) continue;
    console.log(`${name}:`);
    cands.forEach((c, i) => {
      console.log(`  ${i + 1}. ${c.provider}/${c.model.id}  M${priorityOf(c.model)} P${priorityOf(store.providers[c.provider])}`);
    });
  }
}

function headlessSet(storePath: string, token: string, value: number): number {
  const store = loadStore(storePath);
  const ups = upstreams(store);
  if (token in ups) {
    ups[token][PRIORITY] = value;
    saveStore(storePath, store);
    console.log(`${token}: provider 优先级 = ${value}`);
    return 0;
  }
  const hits: [string, Model][] = [];
  for (const [name, entry] of Object.entries(ups)) {
    for (const model of entry.models ?? []) if (model.id === token || model.id.includes(token)) hits.push([name, model]);
  }
  if (hits.length !== 1) {
    console.log(`需要唯一命中, 实际 ${hits.length}: ` + hits.slice(0, 8).map(([n, m]) => `${n}/${m.id}`).join(", "));
    return 1;
  }
  hits[0][1][PRIORITY] = value;
  saveStore(storePath, store);
  console.log(`${hits[0][0]}/${hits[0][1].id}: 模型优先级 = ${value}`);
  return 0;
}

function main(): number {
  const argv = process.argv.slice(2);
  const value = (flag: string, fallback: string) => {
    const i = argv.indexOf(flag);
    return i >= 0 && argv[i + 1] ? argv[i + 1] : fallback;
  };
  const storePath = resolve(value("--store", DEFAULT_STORE));
  const config = resolve(value("--config", DEFAULT_CONFIG));

  const orderIdx = argv.indexOf("--order");
  if (orderIdx >= 0) {
    const next = argv[orderIdx + 1];
    printOrder(loadStore(storePath), config, storePath, next && !next.startsWith("--") ? next : undefined);
    return 0;
  }
  const setIdx = argv.indexOf("--set");
  if (setIdx >= 0) {
    const [token, raw] = argv.slice(setIdx + 1);
    if (!token || !raw || !Number.isInteger(Number(raw))) {
      console.error("用法: --set <provider|模型> <值>");
      return 2;
    }
    return headlessSet(storePath, token, Number(raw));
  }
  if (!process.stdout.isTTY || !process.stdin.isTTY) {
    console.error("TUI 需要终端; 无终端时用 --order / --set");
    return 1;
  }
  runTui(new Tui(storePath, config, loadStore(storePath)));
  return -1; // TUI 靠事件循环活着, 不要在这里退出进程
}

const code = main();
if (code >= 0) process.exit(code);
