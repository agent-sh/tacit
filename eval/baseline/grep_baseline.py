#!/usr/bin/env python3
"""Grep baseline for the truth set.

For every truth item that already has a stored fact (`existing_memory` set),
rank fact files by how many query terms ripgrep finds in them and report
hit@k. Items without a stored fact are reported as coverage gaps; nothing can
retrieve a fact that was never written.

Pull items use `query`. Push items use the situation: paths, command, repo and
intent (a hook has the paths and command; the intent stands in for the user's
last prompt, which a hook can also read).

Ties: many files share a score. The target's rank is reported as a range,
`opt` (target first among its tie group) and `pess` (target last), and hit@k is
given for both bounds. Nothing depends on the order ripgrep prints files.

Corpus pinning: `--write-manifest FILE` records the corpus file list with
sha256; `--manifest FILE` restricts scoring to that list and reports drift, so
a number in BASELINE.md can be reproduced after the live memory dirs change.

  python3 -I eval/baseline/grep_baseline.py --weight idf --truth eval/truth.jsonl
  python3 -I eval/baseline/grep_baseline.py --truth eval/examples/truth.sample.jsonl \
      --home eval/examples/home        # the synthetic sample, end to end

`--home` stands in for `~` when expanding `existing_memory` and the default
roots, so a corpus copied off another machine can be scored. Needs ripgrep.
"""
import argparse
import hashlib
import json
import math
import os
import subprocess
from collections import Counter, defaultdict
from pathlib import Path

STOP = set("the a an of to in on for and or is are was were be with by at from that this it as not no do does did how what when where which who why use using used should can could would will into over under about than then there here".split())

# Fact files only: the per-project auto-memory dirs, the shared rules file and
# Codex's distilled summary. Index files, raw transcripts, rollout summaries and
# Codex's mirrored copies of Claude notes are excluded; they are not facts and
# an index file that mentions every keyword would top every ranking.
GLOBS = ["-g", "*.md", "-g", "!MEMORY.md", "-g", "!raw_memories.md", "-g", "!rollout_summaries/**", "-g", "!extensions/**"]
KS = (1, 3, 5, 10)


HOME = Path.home()


def expand(p):
    """Expand a leading ~ against --home (default: the real home)."""
    p = str(p)
    if p == "~" or p.startswith("~/"):
        return HOME / p[2:]
    return Path(p)


def default_roots():
    roots = sorted(str(p) for p in (HOME / ".claude/projects").glob("*/memory") if p.is_dir())
    shared = HOME / ".config/agents"
    if shared.exists():
        roots.append(str(shared))
    summary = HOME / ".codex/memories/memory_summary.md"
    if summary.exists():
        roots.append(str(summary))
    return roots


def terms_for(item):
    sit = item.get("situation") or {}
    if item.get("mode") == "pull" and item.get("query"):
        text = item["query"]
    else:
        parts = list(sit.get("paths") or [])
        parts += [sit.get("command") or "", sit.get("repo") or "", sit.get("intent") or ""]
        text = " ".join(parts)
    import re
    toks = re.findall(r"[A-Za-z0-9_./-]{3,}", text.lower())
    out = []
    for t in toks:
        for piece in re.split(r"[./-]", t):
            if len(piece) >= 3 and piece not in STOP and piece not in out:
                out.append(piece)
    return out[:12]


def rg_files(term, roots):
    cmd = ["rg", "-il", "--sort", "path", "--fixed-strings", *GLOBS, term, *roots]
    r = subprocess.run(cmd, capture_output=True, text=True)
    return [Path(p).resolve() for p in r.stdout.split("\n") if p]


def corpus_files(roots):
    r = subprocess.run(["rg", "--files", "--sort", "path", *GLOBS, *roots], capture_output=True, text=True)
    return sorted({Path(p).resolve() for p in r.stdout.split("\n") if p})


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        h.update(f.read())
    return h.hexdigest()


def scores(terms, roots, weight, corpus):
    """`count`: one point per matched term (naive grep). `idf`: rare terms
    weigh more, the way an agent picks a distinctive keyword to grep for."""
    n_docs = max(1, len(corpus))
    allowed = set(corpus)
    score = Counter()
    for t in terms:
        files = [f for f in rg_files(t, roots) if f in allowed]
        w = 1.0 if weight == "count" else math.log((n_docs + 1) / (len(files) + 1)) + 1
        for f in files:
            score[f] += w
    return score


def rank_bounds(score, target):
    """0-indexed rank range of target: (opt, pess). None if unmatched."""
    s = score.get(target)
    if s is None:
        return None, None
    above = sum(1 for v in score.values() if v > s)
    tied = sum(1 for v in score.values() if v == s)
    return above, above + tied - 1


def default_truth():
    eval_dir = Path(__file__).resolve().parents[1]
    env = os.environ.get("TACIT_TRUTH")
    if env:
        return os.path.expanduser(env)
    own = eval_dir / "truth.jsonl"
    return str(own if own.exists() else eval_dir / "examples" / "truth.sample.jsonl")


def main():
    global HOME
    ap = argparse.ArgumentParser()
    ap.add_argument("--truth", default=default_truth(), help="truth set (default: $TACIT_TRUTH, eval/truth.jsonl, else the sample)")
    ap.add_argument("--home", default=str(Path.home()), help="directory that stands in for ~ (default: your home)")
    ap.add_argument("--roots", nargs="+", default=None)
    ap.add_argument("--weight", choices=["count", "idf"], default="idf")
    ap.add_argument("--manifest", help="score only files listed in this manifest; report drift")
    ap.add_argument("--write-manifest", help="write the corpus file list with sha256 and exit")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    HOME = Path(args.home).resolve()
    roots = args.roots or default_roots()
    if not roots:
        raise SystemExit(f"no memory roots under {HOME}; pass --roots or --home")
    home = str(HOME)

    corpus = corpus_files(roots)
    if args.write_manifest:
        rows = [{"path": str(p).replace(home, "~"), "sha256": sha256(p)} for p in corpus]
        with open(args.write_manifest, "w") as f:
            json.dump({"files": rows}, f, indent=0)
        print(f"wrote {len(rows)} entries to {args.write_manifest}")
        return
    if args.manifest:
        man = json.load(open(args.manifest))["files"]
        wanted = {expand(r["path"]).resolve(): r["sha256"] for r in man}
        present = set(corpus)
        missing = [p for p in wanted if p not in present]
        changed = [p for p in wanted if p in present and sha256(p) != wanted[p]]
        extra = len(present - set(wanted))
        corpus = sorted(p for p in wanted if p in present)
        print(f"manifest: {len(wanted)} files, missing {len(missing)}, changed {len(changed)}, extra on disk ignored {extra}")

    items = [json.loads(l) for l in open(args.truth) if l.strip()]
    gaps, scored, outside = [], [], []
    hit_opt, hit_pess = defaultdict(int), defaultdict(int)
    corpus_set = set(corpus)
    for it in items:
        em = it.get("existing_memory")
        if not em:
            gaps.append(it)
            continue
        target = expand(em).resolve()
        if target not in corpus_set:
            outside.append((it["id"], em))
            continue
        opt, pess = rank_bounds(scores(terms_for(it), roots, args.weight, corpus), target)
        scored.append((it, opt, pess))
        for k in KS:
            if opt is not None and opt < k:
                hit_opt[k] += 1
            if pess is not None and pess < k:
                hit_pess[k] += 1
        if args.verbose:
            print(f"{it['id']} {it['mode']} rank {opt}..{pess} {terms_for(it)}")

    n = len(scored)
    print(f"weight={args.weight}  corpus files: {len(corpus)}")
    print(f"truth items: {len(items)}  with stored fact: {len(items) - len(gaps)}  coverage gaps: {len(gaps)}  target outside corpus: {len(outside)}")
    for oid, em in outside:
        print(f"  outside: {oid} {em}")
    if n:
        print(f"scored n={n}  (hit@k as pess..opt: target last..first within its tie group)")
        for k in KS:
            print(f"grep hit@{k}: {hit_pess[k]}..{hit_opt[k]} /{n} = {hit_pess[k]/n:.2f}..{hit_opt[k]/n:.2f}")
        by_mode = defaultdict(lambda: [0, 0, 0])
        for it, opt, pess in scored:
            m = it.get("mode", "?")
            by_mode[m][2] += 1
            if pess is not None and pess < 5:
                by_mode[m][0] += 1
            if opt is not None and opt < 5:
                by_mode[m][1] += 1
        for m, (hp, ho, t) in sorted(by_mode.items()):
            print(f"  {m}: hit@5 {hp}..{ho} /{t}")
    by_type = Counter(it.get("fact_type") for it in gaps)
    print("coverage gaps by type:", dict(by_type))


if __name__ == "__main__":
    main()
