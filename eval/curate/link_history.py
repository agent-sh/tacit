#!/usr/bin/env python3
"""What the curator's link step did to links in fact files, from git alone.

  link_history.py [--store CLONE]

For every commit whose subject starts with "curator: links:", each [[link]]
target a fact file had before the commit and not after it, classified at
the commit's parent:

- provenance: the target's superseded_by chain reaches the fact itself
  (the fact replaced it, directly or through another fact)
- superseded: the target was replaced by some other fact
- missing: no fact had that name
- live: a live fact had that name

Prints one TSV row per link (day, commit, path, target, class) and the
counts on stderr. Read-only; no model.
"""
import argparse
import collections
import os
import re
import subprocess
import sys

LINK = re.compile(r"\[\[([^\]|#]+)(?:#[^\]|]*)?(?:\|[^\]]*)?\]\]")
FIELD = re.compile(r"^(name|superseded_by|retired):\s*(.*)$")


def git(store, *args):
    return subprocess.run(["git", "-C", store, *args], capture_output=True, text=True, check=True).stdout


def body(text):
    if text.startswith("---\n"):
        _, _, rest = text[4:].partition("\n---\n")
        return rest
    return text


def facts_at(store, rev):
    """name -> (superseded_by, retired) for the store's facts at rev (inbox and review excluded)."""
    out, seen = {}, set()
    grep = subprocess.run(["git", "-C", store, "grep", "-E", "^(name|superseded_by|retired):", rev, "--", "*.md", ":!inbox", ":!review"],
                          capture_output=True, text=True).stdout
    per = collections.defaultdict(dict)
    for line in grep.splitlines():
        _, path, rest = line.split(":", 2)
        m = FIELD.match(rest)
        if m and m.group(1) not in per[path]:  # the frontmatter's line comes first
            per[path][m.group(1)] = m.group(2).strip().strip("'\"")
    for path, f in per.items():
        if f.get("name"):
            out[f["name"]] = (f.get("superseded_by", ""), f.get("retired", ""))
    return out


def reaches(facts, target, name):
    seen = set()
    while target in facts and target not in seen and len(seen) < 16:
        seen.add(target)
        target = facts[target][0]
        if not target:
            return False
        if target == name:
            return True
    return False


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--store", default=os.environ.get("TACIT_STORE", os.path.expanduser("~/.local/share/tacit/store")))
    a = ap.parse_args()
    counts = collections.Counter()
    for line in git(a.store, "log", "--format=%H %ad", "--date=short", "--grep=^curator: links:").splitlines():
        commit, day = line.split()
        paths = [p for p in git(a.store, "diff-tree", "--no-commit-id", "--name-only", "-r", commit).splitlines()
                 if p.endswith(".md") and not p.startswith(("review/", "inbox/")) and p != "MEMORY.md"]
        if not paths:
            continue
        facts = facts_at(a.store, commit + "^")
        for path in paths:
            try:
                before = git(a.store, "show", f"{commit}^:{path}")
                after = git(a.store, "show", f"{commit}:{path}")
            except subprocess.CalledProcessError:
                continue  # added or removed by the commit: not a repair
            m = re.search(r"^name:\s*(.*)$", before, re.M)
            name = m.group(1).strip() if m else ""
            gone = {x.group(1).strip() for x in LINK.finditer(body(before))} - {x.group(1).strip() for x in LINK.finditer(body(after))}
            for target in sorted(gone):
                if reaches(facts, target, name):
                    kind = "provenance"
                elif target not in facts:
                    kind = "missing"
                elif facts[target][0]:
                    kind = "superseded"
                else:
                    kind = "live"
                counts[kind] += 1
                print("\t".join((day, commit[:9], path, target, kind)))
    print(" ".join(f"{k}={v}" for k, v in sorted(counts.items())) or "no link repairs", file=sys.stderr)


if __name__ == "__main__":
    main()
