#!/usr/bin/env python3
"""Offline delivery check: would the SessionStart block have carried the fact?

For every truth situation whose fact is already stored (existing_memory set),
build the block a SessionStart hook would inject for that situation's repo
and host from the imported store, and check whether the needed fact is in
it. No model is involved; this measures the push-mode design itself.

Selection variants:
  scope      facts whose scope is projects/<repo>, plus user-scope facts
             (today's machine facts live there; machines/<host> is empty
             until the curator runs). Uncapped: is the fact eligible at all?
  top40      the eligible set ranked by idf term overlap with the situation
             (paths, command, intent, repo), cut to 40 lines of descriptions,
             the budget the hook would spend.
  top40+q    same, but pull items also contribute their query terms (an upper
             bound: the hook has no query at SessionStart).

Facts that were never written (existing_memory null) are listed as the gap
the dreamer and curator have to close; no selection can deliver them.

  python3 eval/delivery/delivery.py --truth eval/truth.jsonl --store ~/.local/share/tacit/store
  python3 eval/delivery/delivery.py --truth eval/examples/truth.sample.jsonl \
      --store eval/examples/store --host workstation          # synthetic sample

Inputs (flag, else env, else default):
  --truth   TACIT_TRUTH   eval/truth.jsonl if present, else the synthetic sample
  --store   TACIT_STORE   ~/.local/share/tacit/store
  --host    TACIT_HOST    this machine's short hostname (the machines/<host> scope)
  --import-host TACIT_IMPORT_HOST  the host in inbox/import@<host>/ (default --host)
  --cap     TACIT_CAP     40
"""
import argparse
import json
import math
import os
import re
import socket
import sys
from collections import Counter, defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
EVAL = ROOT / "eval"
SAMPLE_TRUTH = EVAL / "examples" / "truth.sample.jsonl"
STORE = Path(os.path.expanduser(os.environ.get("TACIT_STORE", str(Path.home() / ".local/share/tacit/store"))))
HOST = os.environ.get("TACIT_HOST") or socket.gethostname().split(".")[0]
IMPORT_HOST = os.environ.get("TACIT_IMPORT_HOST") or HOST
STOP = set("the a an of to in on for and or is are was were be with by at from that this it as not no do does did how what when where which who why use using used should can could would will into over under about than then there here".split())
CAP = int(os.environ.get("TACIT_CAP", "40"))


def slug(s, mx=80):
    s = re.sub(r"[^a-z0-9]+", "-", s.lower()).strip("-")
    if len(s) > mx:
        cut = s[:mx].rfind("-")
        s = s[: cut if cut >= mx // 2 else mx].strip("-")
    return s or "fact"


def frontmatter(path):
    text = path.read_text(errors="replace")
    if not text.startswith("---\n"):
        return {}, text
    end = text.find("\n---", 4)
    fm, body = text[4:end], text[end + 4 :]
    meta = {}
    section = None
    for line in fm.split("\n"):
        k, _, v = line.partition(":")
        if k and not k.startswith(" "):
            section = k.strip()
            if v.strip():
                meta[section] = v.strip()
        elif section == "applies_to" and k.strip() in ("paths", "repos", "hosts", "commands"):
            try:
                meta.setdefault("applies_to", {})[k.strip()] = json.loads(v.strip() or "[]")
            except json.JSONDecodeError:
                pass
    return meta, body


def load_store(store=None):
    """Every fact file under the store (inbox and curated alike), keyed by path."""
    store = Path(store) if store else STORE
    facts = {}
    for p in store.rglob("*.md"):
        if p.name in ("MEMORY.md", "README.md") or ".git" in p.parts:
            continue
        meta, body = frontmatter(p)
        rel = str(p.relative_to(store))
        facts[rel] = {
            "path": rel,
            "name": meta.get("name", p.stem),
            "scope": meta.get("scope", ""),
            "description": meta.get("description", "").strip('"'),
            "body": body,
            "origin": meta.get("origin", ""),
            "applies_to": meta.get("applies_to") if isinstance(meta.get("applies_to"), dict) else {},
        }
    return facts


def default_truth():
    """$TACIT_TRUTH, else eval/truth.jsonl (your own, gitignored), else the synthetic sample."""
    env = os.environ.get("TACIT_TRUTH")
    if env:
        return Path(os.path.expanduser(env))
    own = EVAL / "truth.jsonl"
    return own if own.exists() else SAMPLE_TRUTH


def load_truth(path=None):
    path = Path(path) if path else default_truth()
    if path == SAMPLE_TRUTH:
        print(f"truth: {path} (synthetic sample; pass --truth or set TACIT_TRUTH for your own)", file=sys.stderr)
    return [json.loads(l) for l in open(path) if l.strip()]


def eligible_scopes(repo, host=None):
    """The scopes a SessionStart block draws from for a situation."""
    return [f"projects/{repo}", "user", f"machines/{host or HOST}", "system"]


def imported_path(existing_memory, import_host=None):
    """~/.claude/projects/<proj>/memory/<stem>.md -> inbox/import@<host>/<proj>/<slug>.md"""
    p = Path(os.path.expanduser(existing_memory))
    if p.parent.name != "memory":
        return None
    return f"inbox/import@{import_host or IMPORT_HOST}/{p.parent.parent.name}/{slug(p.stem)}.md"


def terms(text):
    out = []
    for t in re.findall(r"[A-Za-z0-9_./-]{3,}", text.lower()):
        for piece in re.split(r"[./-]", t):
            if len(piece) >= 3 and piece not in STOP and piece not in out:
                out.append(piece)
    return out


def situation_text(item, with_query):
    sit = item["situation"]
    parts = list(sit.get("paths") or []) + [sit.get("command") or "", sit.get("repo") or "", sit.get("intent") or ""]
    if with_query and item.get("query"):
        parts.append(item["query"])
    return " ".join(parts)


def rank(eligible, qterms, df, n):
    scored = []
    for f in eligible:
        text = (f["name"] + " " + f["description"] + " " + f["body"]).lower()
        s = 0.0
        for t in qterms:
            if t in text:
                s += math.log((n + 1) / (df[t] + 1)) + 1
        scored.append((s, f["path"]))
    scored.sort(key=lambda x: (-x[0], x[1]))
    return [p for _, p in scored]


def applies(f, sit):
    """Situation match on the fact's applies_to key: 0, or a boost per matched list."""
    at = f.get("applies_to") or {}
    boost = 0.0
    repo = (sit.get("repo") or "").lower()
    host = (sit.get("host") or "").lower()
    cmd = (sit.get("command") or "").lower()
    paths = [p.lower() for p in (sit.get("paths") or [])]
    if repo and any(repo == r.lower() for r in at.get("repos", [])):
        boost += 1.0
    if host and any(host == h.lower() for h in at.get("hosts", [])):
        boost += 0.5
    if cmd and any(c and cmd.startswith(c.lower()) or c.lower() in cmd for c in at.get("commands", []) if isinstance(c, str)):
        boost += 3.0
    for g in at.get("paths", []):
        if not isinstance(g, str):
            continue
        gl = g.lower().rstrip("/*")
        if any(gl and (p.startswith(gl) or gl in p) for p in paths):
            boost += 3.0
            break
    return boost


def rank_applies(eligible, qterms, df, n, sit, weight=4.0):
    """idf ranking plus the applies_to join: a matched command or path outranks text overlap."""
    scored = []
    for f in eligible:
        text = (f["name"] + " " + f["description"] + " " + f["body"]).lower()
        s = sum(math.log((n + 1) / (df[t] + 1)) + 1 for t in qterms if t in text)
        s += weight * applies(f, sit)
        scored.append((s, f["path"]))
    scored.sort(key=lambda x: (-x[0], x[1]))
    return [p for _, p in scored]


def main():
    global STORE, HOST, IMPORT_HOST, CAP
    ap = argparse.ArgumentParser(description="Offline delivery check (no model).")
    ap.add_argument("--truth", help="truth set JSONL (default: $TACIT_TRUTH, eval/truth.jsonl, else the sample)")
    ap.add_argument("--store", default=str(STORE), help="store clone (default $TACIT_STORE or ~/.local/share/tacit/store)")
    ap.add_argument("--host", default=HOST, help="host for the machines/<host> scope (default $TACIT_HOST or the hostname)")
    ap.add_argument("--import-host", default=None, help="host in inbox/import@<host>/ (default $TACIT_IMPORT_HOST or --host)")
    ap.add_argument("--cap", type=int, default=CAP, help="block lines (default $TACIT_CAP or 40)")
    args = ap.parse_args()
    STORE = Path(os.path.expanduser(args.store))
    HOST = args.host
    IMPORT_HOST = args.import_host or os.environ.get("TACIT_IMPORT_HOST") or HOST
    CAP = args.cap
    truth = load_truth(args.truth)
    facts = load_store()
    # Curated stores carry `origin: <inbox fact name>`; a truth item then hits
    # when ANY child of its stored note is delivered.
    by_origin = defaultdict(set)
    for f in facts.values():
        if f["origin"]:
            by_origin[f["origin"]].add(f["path"])
    curated = bool(by_origin)
    print(f"store {STORE}: {len(facts)} facts, curated={curated}")
    by_scope = defaultdict(list)
    for f in facts.values():
        by_scope[f["scope"]].append(f)
    # document frequency over all facts for idf
    df = Counter()
    for f in facts.values():
        for t in set(terms(f["name"] + " " + f["description"] + " " + f["body"])):
            df[t] += 1
    n = len(facts)

    stored, gap, outside = [], [], []
    for it in truth:
        em = it.get("existing_memory")
        if not em:
            gap.append(it)
            continue
        ip = imported_path(em)
        if curated:
            origin = Path(ip).stem if ip else None
            targets = by_origin.get(origin, set())
        else:
            targets = {ip} if ip and ip in facts else set()
        if not targets:
            outside.append(it)
            continue
        stored.append((it, targets))

    hits = Counter()
    sizes = []
    per_mode = defaultdict(Counter)
    misses_top40 = []
    for it, targets in stored:
        repo = (it["situation"].get("repo") or "").lower()
        eligible = [f for s in eligible_scopes(repo) for f in by_scope.get(s, [])]
        sizes.append(len(eligible))
        elig_paths = {f["path"] for f in eligible}
        m = it["mode"]
        if targets & elig_paths:
            hits["scope"] += 1
            per_mode[m]["scope"] += 1
        for label, with_q in (("top40", False), ("top40+q", True), ("top40+at", None)):
            if with_q is None:
                ranked = rank_applies(eligible, terms(situation_text(it, False)), df, n, it["situation"])
            else:
                ranked = rank(eligible, terms(situation_text(it, with_q)), df, n)
            positions = [i for i, p in enumerate(ranked) if p in targets]
            pos = positions[0] if positions else None
            if pos is not None and pos < CAP:
                hits[label] += 1
                per_mode[m][label] += 1
            elif label == "top40":
                misses_top40.append((it["id"], m, it["fact_type"], pos, sorted(facts[t]["scope"] for t in targets)[0], repo))
        per_mode[m]["n"] += 1

    k = len(stored)
    if not k:
        sys.exit("no truth situation has its stored fact in this store")
    print(f"truth items {len(truth)}: stored-and-imported {k}, never written {len(gap)}, stored outside the import {len(outside)}")
    print(f"eligible set per situation: median {sorted(sizes)[len(sizes)//2]} facts, max {max(sizes)}; block cap {CAP} lines")
    for label in ("scope", "top40", "top40+q", "top40+at"):
        print(f"{label:8s} hit {hits[label]:3d}/{k} = {hits[label]/k:.2f}   " + "  ".join(f"{m}: {per_mode[m][label]}/{per_mode[m]['n']}" for m in sorted(per_mode)))
    print("\nwhy top40 misses (first 25): id mode type rank scope repo")
    for row in misses_top40[:25]:
        print("  ", *row)
    print("\nnever written, by type:", dict(Counter(i["fact_type"] for i in gap)))
    print("never written, push/pull:", dict(Counter(i["mode"] for i in gap)))


if __name__ == "__main__":
    sys.exit(main())
