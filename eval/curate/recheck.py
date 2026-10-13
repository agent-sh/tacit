#!/usr/bin/env python3
"""Offline contradiction pass over an existing curated store (report only).

For every curated fact, pair it with its closest other curated fact by
description-term overlap within the same scope family, then judge pairs in
batches of 25 with Haiku 5.5 (--bare, tools off). Writes
~/.cache/tacit-recheck/<stamp>.jsonl and prints counts. Nothing in the
store is modified; the curator applies verdicts in a later step.

  python3 eval/curate/recheck.py [--store ~/.local/share/tacit/store] [--limit N] [--out-dir D]

TACIT_JUDGE_MODEL names the judge (default: the CLI alias haiku).
"""
import argparse
import json
import math
import os
import re
import subprocess
import sys
import time
from collections import Counter, defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "eval/delivery"))
import delivery  # noqa: E402

JUDGE = os.environ.get("TACIT_JUDGE_MODEL", "haiku")
PROMPT = """For each numbered pair of facts from a shared store, say whether fact B contradicts fact A (an agent following both would have to act in two incompatible ways), supersedes it (same subject, B is the current state and A is outdated), or neither (different subjects or compatible details).
Output ONLY a JSON array of strings, one per pair in order, each exactly "contradicts", "supersedes" or "no".

{pairs}"""


def claude(prompt):
    p = subprocess.run(["claude", "-p", prompt, "--bare", "--model", JUDGE, "--output-format", "json", "--tools", "", "--max-turns", "1"], capture_output=True, text=True, timeout=600)
    try:
        res = json.loads(p.stdout)
    except json.JSONDecodeError:
        return [], 0.0
    m = re.search(r"\[.*\]", (res.get("result") or "").strip(), re.S)
    try:
        return json.loads(m.group(0)) if m else [], res.get("total_cost_usd") or 0.0
    except json.JSONDecodeError:
        return [], res.get("total_cost_usd") or 0.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--store", default=str(Path.home() / ".local/share/tacit/store"))
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--out-dir", default=str(Path.home() / ".cache/tacit-recheck"))
    args = ap.parse_args()
    facts = [f for f in delivery.load_store(os.path.expanduser(args.store)).values() if not f["path"].startswith("inbox/")]
    if args.limit:
        facts = facts[: args.limit]
    terms = {f["path"]: set(delivery.terms(f["name"] + " " + f["description"])) for f in facts}
    df = Counter()
    for t in terms.values():
        df.update(t)
    n = len(facts)
    idf = {t: math.log((n + 1) / (c + 1)) + 1 for t, c in df.items()}
    by_family = defaultdict(list)
    for f in facts:
        by_family[f["scope"].split("/")[0]].append(f)

    pairs = []
    seen = set()
    for f in facts:
        best, best_s = None, 0.0
        tf = terms[f["path"]]
        for g in by_family[f["scope"].split("/")[0]]:
            if g["path"] == f["path"]:
                continue
            common = tf & terms[g["path"]]
            s = sum(idf[t] for t in common)
            if s > best_s:
                best, best_s = g, s
        if best is None or best_s < 8 or len(tf & terms[best["path"]]) < 3:
            continue
        key = tuple(sorted((f["path"], best["path"])))
        if key in seen:
            continue
        seen.add(key)
        pairs.append((f, best, best_s))
    pairs.sort(key=lambda x: -x[2])
    print(f"facts {n}, candidate pairs {len(pairs)}")

    out_dir = Path(os.path.expanduser(args.out_dir))
    out_dir.mkdir(parents=True, exist_ok=True)
    out = open(out_dir / f"{time.strftime('%Y%m%dT%H%M%S')}.jsonl", "w")
    tally = Counter()
    cost = 0.0
    for i in range(0, len(pairs), 25):
        batch = pairs[i : i + 25]
        text = "\n".join(f"{k + 1}.\nA ({a['name']}): {a['description']}\nB ({b['name']}): {b['description']}\n" for k, (a, b, _) in enumerate(batch))
        verdicts, c = claude(PROMPT.format(pairs=text))
        cost += c
        for k, (a, b, s) in enumerate(batch):
            v = str(verdicts[k]).lower() if k < len(verdicts) else "no"
            v = "contradicts" if v.startswith("contra") else "supersedes" if v.startswith("super") else "no"
            tally[v] += 1
            if v != "no":
                out.write(json.dumps({"verdict": v, "a": a["name"], "a_scope": a["scope"], "a_desc": a["description"], "b": b["name"], "b_scope": b["scope"], "b_desc": b["description"], "overlap": round(s, 1)}, ensure_ascii=False) + "\n")
        print(f"batch {i // 25 + 1}: {dict(tally)} ${cost:.2f}", flush=True)
    print(f"\ncontradictions {tally['contradicts']}, supersedes {tally['supersedes']}, no {tally['no']}; cost ${cost:.2f}; details in {out.name}")


if __name__ == "__main__":
    main()
