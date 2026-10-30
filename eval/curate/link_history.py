#!/usr/bin/env python3
"""What the curator's link step did to links in fact files, from git alone.

  link_history.py [--store CLONE]

For every commit whose subject starts with "curator: links:", each [[link]]
target a fact file linked before the commit more often than after it
(links in code, as lint L6 and the step read them, do not count), classified
at the commit's parent:

- provenance: the target's superseded_by chain reaches the fact itself
  (the fact replaced it, directly or through another fact)
- superseded: the target was replaced by some other fact
- retired: the target was retired, with no successor
- inbox: only an inbox note has the name
- missing: no fact had that name
- live: a live curated fact had that name

Prints one TSV row per (fact, target) pair: day, commit, path, target,
class, links removed; and the counts on stderr. Read-only; no model.
"""
import argparse
import collections
import os
import re
import subprocess
import sys

LINK = re.compile(r"\[\[([^\]|#]+)(?:#[^\]|]*)?(?:\|[^\]]*)?\]\]")
FENCE = re.compile(r"^(```|~~~)")
INLINE = re.compile(r"`[^`\n]*`")


def git(store, *args, check=True):
    return subprocess.run(["git", "-C", store, *args], capture_output=True, text=True, check=check).stdout


def split(text):
    """(frontmatter fields, body) of a fact file."""
    if not text.startswith("---\n"):
        return {}, text
    head, _, body = text[4:].partition("\n---\n")
    fields = {}
    for line in head.split("\n"):
        m = re.match(r"^([a-z_]+):\s*(.*)$", line)
        if m and m.group(1) not in fields:
            fields[m.group(1)] = m.group(2).strip().strip("'\"")
    return fields, body


def links(body):
    """Link targets outside code, counted: fenced blocks, indented blocks
    after a blank line outside a list, and inline code spans are skipped."""
    out = collections.Counter()
    fenced, prev_blank, in_list = False, True, False
    for line in body.split("\n"):
        if FENCE.match(line.lstrip()):
            fenced = not fenced
            prev_blank = False
            continue
        if fenced:
            continue
        stripped = line.strip()
        if re.match(r"^([-*+]|\d+[.)])\s", stripped):
            in_list = True
        elif not stripped:
            in_list = False
        if (line.startswith("    ") or line.startswith("\t")) and prev_blank and not in_list:
            continue  # an indented code block
        prev_blank = not stripped
        for m in LINK.finditer(INLINE.sub("", line)):
            out[m.group(1).strip()] += 1
    return out


def is_fact(path):
    base = os.path.basename(path)
    return path.endswith(".md") and not base.startswith("_") and base != "README.md" and base != "MEMORY.md" and not path.startswith(("review/", "keys/"))


def facts_at(store, rev):
    """name -> (superseded_by, retired, inbox) for the store's facts at rev,
    read in one git process."""
    blobs = []
    for line in git(store, "ls-tree", "-r", rev).splitlines():
        meta, path = line.split("\t", 1)
        if is_fact(path) and meta.split()[1] == "blob":
            blobs.append((meta.split()[2], path))
    p = subprocess.run(["git", "-C", store, "cat-file", "--batch"], input="\n".join(sha for sha, _ in blobs).encode(), capture_output=True, check=True)
    data, pos, out = p.stdout, 0, {}
    for _, path in blobs:
        nl = data.index(b"\n", pos)
        size = int(data[pos:nl].split()[2])
        text = data[nl + 1:nl + 1 + size].decode("utf-8", "replace")
        pos = nl + 1 + size + 1
        fields, _ = split(text)
        name = fields.get("name")
        if not name:
            continue
        inbox = path.startswith("inbox/")
        if name in out and not out[name][2]:
            continue  # a curated fact's name wins over an inbox note's
        out[name] = (fields.get("superseded_by", ""), fields.get("retired", ""), inbox)
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
    pairs, removed = collections.Counter(), collections.Counter()
    for line in git(a.store, "log", "--format=%H %ad", "--date=short", "--grep=^curator: links:").splitlines():
        commit, day = line.split()
        paths = [p for p in git(a.store, "diff-tree", "--no-commit-id", "--name-only", "-r", commit).splitlines() if is_fact(p) and not p.startswith("inbox/")]
        if not paths:
            continue
        facts = facts_at(a.store, commit + "^")
        for path in paths:
            before = git(a.store, "show", f"{commit}^:{path}", check=False)
            after = git(a.store, "show", f"{commit}:{path}", check=False)
            if not before or not after:
                continue  # added or removed by the commit: not a repair
            fields, body = split(before)
            name = fields.get("name", "")
            lost = links(body) - links(split(after)[1])
            for target, n in sorted(lost.items()):
                f = facts.get(target)
                if reaches(facts, target, name):
                    kind = "provenance"
                elif f is None:
                    kind = "missing"
                elif f[2]:
                    kind = "inbox"
                elif f[0]:
                    kind = "superseded"
                elif f[1]:
                    kind = "retired"
                else:
                    kind = "live"
                pairs[kind] += 1
                removed[kind] += n
                print("\t".join((day, commit[:9], path, target, kind, str(n))))
    print(" ".join(f"{k}={pairs[k]} pairs, {removed[k]} links" for k in sorted(pairs)) or "no link repairs", file=sys.stderr)


if __name__ == "__main__":
    main()
