#!/usr/bin/env python3
"""Cheap outcome signals of graded deliveries, and their agreement with the grades.

For each graded line of a case file (deliveries.py's cases, with labels or a
--grades TSV), read the session's Claude Code transcript after the line's
moment and compute, without a model:

  summary_kept     a compaction after the moment, whose summary holds the fact
                   (its name, or one sentence holding most of its description)
  summary_dropped  a compaction after the moment whose summary does not hold it
  needed_again     after such a drop, the agent opened the fact (tacit_fact,
                   `tacit fact`) or searched tacit with most of its words
  carried_forward  the fact's name, or one sentence holding most of its
                   description, in what the agent wrote on its own later: a
                   subagent's brief, a commit or PR body, a note it added, or
                   its reply text
  opened           the agent opened the fact after the moment
  owner_restated   a later turn the owner typed holds most of the description
                   (relevant but ineffective: a curator flag, never a score)

Then per signal: how many graded lines it fires on, the share graded useful
(R) and noise (N) there with Wilson intervals, against the base rates, and
whether it clears the decision rule of eval/DELIVERY.md ("Cheap outcome
signals"). Cases, transcripts, grades and the report stay private: write
--out outside the repository.

  python3 -I eval/delivery/signals.py --cases /abs/private/cases.jsonl \\
      --store /abs/store-clone [--grades /abs/private/extra.tsv] --out /abs/private/signals.json
"""
import argparse
import glob
import json
import math
import os
import re
import sys

WORD = re.compile(r"[a-z0-9][a-z0-9_]{3,}")
STOP = set("this that with from have when then than they them were what will your about into been only also more most some such each over just like".split())


def terms(text):
    return {w for w in WORD.findall((text or "").lower()) if w not in STOP}


def holds(text, name, desc_terms, share=0.6, least=3):
    """Text holds a fact: its whole name, or one sentence or line holding at
    least share of its description's terms and at least least of them (the
    hooks' summary rule, summaryOf/holds)."""
    low = (text or "").lower()
    if re.search(r"(?<![a-z0-9-])" + re.escape(name.lower()) + r"(?![a-z0-9-])", low):
        return True
    if len(desc_terms) < least:
        return False
    for part in re.split(r"[.!?\n]+", low):
        got = desc_terms & terms(part)
        if len(got) >= least and len(got) >= share * len(desc_terms):
            return True
    return False


def wilson(k, n, z=1.96):
    if n == 0:
        return (float("nan"), float("nan"))
    p = k / n
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((c - h) / d, (c + h) / d)


def store_facts(store):
    """name -> description, from the store's fact files (frontmatter)."""
    out = {}
    for p in glob.glob(os.path.join(store, "**", "*.md"), recursive=True):
        try:
            with open(p, encoding="utf-8", errors="replace") as fh:
                head = fh.read(4000)
        except OSError:
            continue
        if not head.startswith("---"):
            continue
        name = re.search(r"^name:\s*(.+)$", head, re.M)
        desc = re.search(r"^description:\s*(.+)$", head, re.M)
        if name and desc:
            out.setdefault(name.group(1).strip().strip("'\""), desc.group(1).strip().strip("'\""))
    return out


def transcript_events(path):
    """The transcript's events in order: (ts, kind, text). Kinds: owner
    (a turn a person typed), agent (reply text), brief (a subagent's prompt),
    commit (a shell command that writes a commit or PR body), note (a tacit
    note the agent added), open (tacit_fact), search (tacit_search), summary
    (a compaction summary)."""
    out = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            try:
                d = json.loads(line)
            except json.JSONDecodeError:
                continue
            ts = d.get("timestamp") or ""
            msg = d.get("message") or {}
            content = msg.get("content")
            if d.get("isCompactSummary"):
                text = content if isinstance(content, str) else " ".join(b.get("text", "") for b in content or [] if isinstance(b, dict))
                out.append((ts, "summary", text))
                continue
            if d.get("type") == "user" and isinstance(content, str) and not d.get("isMeta") and d.get("userType") == "external" and not d.get("isSidechain"):
                if not content.startswith("<"):
                    out.append((ts, "owner", content))
                continue
            if d.get("type") != "assistant" or not isinstance(content, list):
                continue
            for b in content:
                if not isinstance(b, dict):
                    continue
                if b.get("type") == "text":
                    out.append((ts, "agent", b.get("text", "")))
                elif b.get("type") == "tool_use":
                    name, inp = b.get("name", ""), b.get("input") or {}
                    if name in ("Agent", "Task"):
                        out.append((ts, "brief", str(inp.get("prompt", ""))))
                    elif name.endswith("tacit_fact"):
                        out.append((ts, "open", str(inp.get("name", ""))))
                    elif name.endswith("tacit_search"):
                        out.append((ts, "search", str(inp.get("query", ""))))
                    elif name.endswith("tacit_add"):
                        out.append((ts, "note", json.dumps(inp)))
                    elif name == "Bash":
                        cmd = str(inp.get("command", ""))
                        m = re.findall(r"(?:^|[\s;&|])tacit\s+fact\s+([a-z0-9-]+)", cmd)
                        for x in m:
                            out.append((ts, "open", x))
                        if re.search(r"git\s+commit|gh\s+pr\s+(create|edit|comment)|gh\s+issue\s+(create|comment)", cmd):
                            out.append((ts, "commit", cmd))
                        if re.search(r"(?:^|[\s;&|])tacit\s+add\b", cmd):
                            out.append((ts, "note", cmd))
                        if re.search(r"(?:^|[\s;&|])tacit\s+search\b", cmd):
                            out.append((ts, "search", cmd))
    return out


def signals_for(events, ts, name, desc):
    dt = terms(desc)
    after = [e for e in events if e[0] > ts]
    s = {"summary_kept": False, "summary_dropped": False, "needed_again": False,
         "carried_forward": False, "opened": False, "owner_restated": False}
    dropped_at = None
    for t, kind, text in after:
        if kind == "summary":
            if holds(text, name, dt):
                s["summary_kept"] = True
            elif dropped_at is None:
                s["summary_dropped"], dropped_at = True, t
        elif kind == "open" and text == name:
            s["opened"] = True
            if dropped_at is not None and t > dropped_at:
                s["needed_again"] = True
        elif kind == "search" and dropped_at is not None and t > dropped_at and len(dt) >= 3 and len(dt & terms(text)) >= 0.6 * len(dt):
            s["needed_again"] = True
        elif kind in ("brief", "commit", "note", "agent") and holds(text, name, dt):
            s["carried_forward"] = True
        elif kind == "owner" and len(dt) >= 3 and holds(text, "\x00", dt):
            s["owner_restated"] = True
    return s


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--cases", required=True)
    ap.add_argument("--store", required=True, help="a store clone, for the facts' descriptions")
    ap.add_argument("--grades", action="append", help="extra case-id/fact/grade TSV")
    ap.add_argument("--projects", default=os.path.expanduser("~/.claude/projects"), help="where the sessions' transcripts are")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    facts = store_facts(a.store)
    transcripts = {os.path.basename(p)[:-6]: p for p in glob.glob(os.path.join(a.projects, "*", "*.jsonl"))}
    extra = {}
    for g in a.grades or []:
        for line in open(g):
            f = line.rstrip("\n").split("\t")
            if len(f) >= 3 and f[2] in ("R", "W", "N"):
                extra[(f[0], f[1])] = f[2]
    cache, rows = {}, []
    for line in open(a.cases):
        c = json.loads(line)
        labels = dict(c.get("labels") or {})
        for (cid, n), g in extra.items():
            if cid == c["id"]:
                labels[n] = g
        path = transcripts.get(c.get("session") or "")
        if not labels or not path:
            continue
        if path not in cache:
            cache[path] = transcript_events(path)
        for name, grade in labels.items():
            if name not in facts:
                continue
            rows.append({"case": c["id"], "fact": name, "grade": grade, **signals_for(cache[path], c["ts"], name, facts[name])})
    n = len(rows)
    base = {g: sum(r["grade"] == g for r in rows) / n for g in "RWN"} if n else {}
    report = {"lines": n, "base": base, "signals": {}}
    for sig, good in (("summary_kept", "R"), ("needed_again", "R"), ("carried_forward", "R"), ("opened", "R"), ("summary_dropped", "N"), ("owner_restated", None)):
        fired = [r for r in rows if r[sig]]
        k = len(fired)
        row = {"fires": k, "R": sum(r["grade"] == "R" for r in fired), "W": sum(r["grade"] == "W" for r in fired), "N": sum(r["grade"] == "N" for r in fired)}
        if good and k:
            share = row[good] / k
            lo, hi = wilson(row[good], k)
            row.update(share=round(share, 3), ci=[round(lo, 3), round(hi, 3)], base=round(base[good], 3),
                       passes=share >= 2 * base[good] and lo > base[good])
        report["signals"][sig] = row
    with open(a.out, "w") as fh:
        json.dump({"report": report, "rows": rows}, fh, indent=1)
    os.chmod(a.out, 0o600)
    print(json.dumps(report, indent=1))


if __name__ == "__main__":
    sys.exit(main())
