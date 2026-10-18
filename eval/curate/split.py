#!/usr/bin/env python3
"""Curator split pass: turn bundled memory notes into one-fact entries.

Imported memory files hold many facts behind one description line, so a
block of description lines cannot deliver them. This pass asks the model to
rewrite each inbox note as one or more facts in SPEC.md shape (name,
description that states the fact, type, scope, applies_to, short body) and
writes them to a curated scratch store for measurement.

  python3 eval/curate/split.py --out ~/.cache/tacit-curated [--store S] [--host H] [--limit N] [--model opus]

Each entry keeps `origin: <inbox fact name>` so the delivery check can map a
truth situation's stored fact to its curated children. Cost is printed per
batch from the CLI's JSON result.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "eval/delivery"))
import delivery  # noqa: E402

SPEC_TYPES = "user_standard, design_decision, past_mistake, machine_fact, code_change, hot_spot, structure, history, user, reference"

PROMPT = """You are the curator of tacit, a fact store shared by coding agents on one owner's machines.
Below are memory notes written by agents. Each note bundles several facts. Rewrite them as ONE FACT PER ENTRY.

Rules:
- A fact is a conclusion another agent can act on without reading anything else. Never a transcript or a narrative.
- `description` (max 200 chars) must STATE the fact itself, including the key path, command, host or number. Not a topic label.
- `body`: one to three sentences, the fact plus the why if known. Keep dates absolute (YYYY-MM-DD).
- `type` is one of: {types}.
- `scope` is `user`, `system`, `projects/<repo>`, or `machines/<host>` (the owner's main host is `{host}`; use machines/{host} for facts about this machine: paths, tools, shell, hardware).
- `applies_to`: lists of repo-relative path globs, repo names, host names and command prefixes that should trigger this fact. Empty lists when nothing specific applies.
- Drop anything derivable from the code or from git log, anything that is only a session log, and anything purely about the past with no present consequence. Facts that mark a thing as obsolete, retired, renamed or dead ARE valuable (keep them).
- If a note states the same fact twice, emit it once. Do not invent facts.
- Output ONLY a JSON array, no prose, no code fence. Each item:
  {{"origin": "<note name>", "name": "<slug>", "description": "...", "type": "...", "scope": "...", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}, "body": "..."}}

NOTES:
{notes}
"""


def batches(facts, max_chars=45_000):
    cur, size = [], 0
    for f in facts:
        n = len(f["body"]) + len(f["description"]) + 100
        if cur and size + n > max_chars:
            yield cur
            cur, size = [], 0
        cur.append(f)
        size += n
    if cur:
        yield cur


def render(batch):
    out = []
    for f in batch:
        out.append(f"### note name={f['name']} scope={f['scope']}\ndescription: {f['description']}\n{f['body'].strip()[:12000]}\n")
    return "\n".join(out)


def call(prompt, model):
    cmd = ["claude", "-p", prompt, "--model", model, "--output-format", "json", "--tools", "", "--max-turns", "1"]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=900)
    try:
        res = json.loads(p.stdout)
    except json.JSONDecodeError:
        return None, 0.0, p.stdout[-500:] + p.stderr[-500:]
    text = res.get("result") or ""
    text = re.sub(r"^```(?:json)?\s*|\s*```$", "", text.strip())
    start, end = text.find("["), text.rfind("]")
    if start < 0 or end < 0:
        return None, res.get("total_cost_usd") or 0.0, text[:500]
    try:
        return json.loads(text[start : end + 1]), res.get("total_cost_usd") or 0.0, ""
    except json.JSONDecodeError as e:
        return None, res.get("total_cost_usd") or 0.0, f"json: {e}"


def slug(s):
    return delivery.slug(s, 80)


def write_entry(out, e, taken, host):
    scope = e.get("scope") or "user"
    if not re.match(r"^(user|system|projects/[a-z0-9-]+|machines/[a-z0-9-]+)$", scope):
        scope = "user"
    name = slug(e.get("name") or e.get("description", "fact")[:60])
    if name in taken:
        name = f"{name}-{abs(hash(e.get('description',''))) % 10**6:06d}"
    taken.add(name)
    at = e.get("applies_to") or {}

    def lst(k):
        v = at.get(k) or []
        return [str(x) for x in v if isinstance(x, (str, int))][:12]

    fm = [
        "---",
        f"name: {name}",
        "description: " + json.dumps(" ".join(str(e.get("description", "")).split())[:200]),
        f"type: {e.get('type') if e.get('type') in SPEC_TYPES.split(', ') else 'unclassified'}",
        f"scope: {scope}",
        "applies_to:",
    ]
    for k in ("paths", "repos", "hosts", "commands"):
        fm.append(f"  {k}: " + json.dumps(lst(k)))
    fm += [
        "source:",
        "  harness: curator",
        f"  host: {host}",
        "  kind: observed",
        f"origin: {e.get('origin', '')}",
        f"created: {time.strftime('%Y-%m-%d')}",
        "---",
        "",
        " ".join(str(e.get("body", "")).split()),
        "",
    ]
    p = out / scope / f"{name}.md"
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text("\n".join(fm))
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(Path.home() / ".cache/tacit-curated"))
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--model", default=os.environ.get("TACIT_CURATE_MODEL", "opus"))
    ap.add_argument("--resume", action="store_true", help="skip origins already present in --out")
    ap.add_argument("--store", default=str(delivery.STORE), help="store to split (default $TACIT_STORE or ~/.local/share/tacit/store)")
    ap.add_argument("--host", default=delivery.HOST, help="the curator's host (default $TACIT_HOST or the hostname)")
    args = ap.parse_args()
    out = Path(os.path.expanduser(args.out))
    out.mkdir(parents=True, exist_ok=True)
    facts = sorted(delivery.load_store(os.path.expanduser(args.store)).values(), key=lambda f: f["path"])
    if args.limit:
        facts = facts[: args.limit]
    done = set()
    if args.resume:
        for p in out.rglob("*.md"):
            m = re.search(r"^origin: (.+)$", p.read_text(), re.M)
            if m:
                done.add(m.group(1).strip())
        facts = [f for f in facts if f["name"] not in done]
    taken = {p.stem for p in out.rglob("*.md")}
    total_cost, written, failed = 0.0, 0, []
    log = open(out / "split.log", "a")
    for i, batch in enumerate(batches(facts)):
        entries, cost, err = call(PROMPT.format(types=SPEC_TYPES, host=args.host, notes=render(batch)), args.model)
        total_cost += cost
        if entries is None:
            failed.append([f["name"] for f in batch])
            log.write(f"batch {i} FAILED ({len(batch)} notes): {err}\n")
            print(f"batch {i}: FAILED {err[:120]}", flush=True)
            continue
        n = 0
        for e in entries:
            if isinstance(e, dict) and e.get("description"):
                write_entry(out, e, taken, args.host)
                n += 1
        written += n
        log.write(f"batch {i}: {len(batch)} notes -> {n} facts, ${cost:.2f}\n")
        print(f"batch {i}: {len(batch)} notes -> {n} facts  ${cost:.2f}  (total ${total_cost:.2f})", flush=True)
    print(f"done: {written} facts from {len(facts)} notes, {len(failed)} failed batches, ${total_cost:.2f}")
    if failed:
        json.dump(failed, open(out / "failed.json", "w"))


if __name__ == "__main__":
    main()
