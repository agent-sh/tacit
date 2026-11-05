#!/usr/bin/env python3
"""Cheap outcome signals of graded deliveries, and their agreement with the grades.

For each graded line of a case file (deliveries.py's cases, with labels or a
--grades TSV), read the session's Claude Code transcript after the line's
moment and compute, without a model:

  summary_kept     a compaction summary in the window holds the fact
  summary_dropped  a compaction summary in the window does not hold it
  needed_again     after such a drop, the agent opened the fact (tacit_fact,
                   `tacit fact`) or ran a tacit search whose words the fact's
                   description holds
  carried_forward  the agent put the fact in what it wrote on its own: a
                   subagent's brief, a commit or PR body, a note it added,
                   its reply text
  opened           the agent opened the fact in the window
  owner_restated   a later turn of the owner's holds the fact (only in
                   sessions whose prompts are the owner's; a curator flag,
                   never a score)

"Holds" is the hooks' summary rule (hooks_restart.go, summaryOf/holds): the
fact's whole name, or for a fact that is no ruling one sentence holding at
least 6 in 10 of its description's terms and at least 3 of them; a ruling is
held by its name only, since a rule's words are the work's words.

The window runs from the line's moment to the session's next delivery of the
same fact, or WINDOW_HOURS after it, whichever is first: later outcomes
belong to a later delivery. A subagent's delivery (a sidechain case) is left
out and counted: its outcomes are in the subagent's own transcript, which a
case does not name.

Per signal: the lines and the distinct (session, fact) units it fires on,
the shares graded useful (R) and noise (N) there, a 95% interval by a
bootstrap over sessions (lines of one session are not independent), and the
decision rule of eval/DELIVERY.md ("Cheap outcome signals"). Cases,
transcripts, grades and the report stay private: write --out outside the
repository.

  python3 -I eval/delivery/signals.py --cases /abs/private/cases.jsonl \\
      --store /abs/store-clone [--grades /abs/private/extra.tsv] --out /abs/private/signals.json
"""
import argparse
import collections
import glob
import json
import os
import random
import re
import subprocess
import sys
from datetime import datetime, timedelta

WINDOW_HOURS = 24
MIN_UNITS = 10  # distinct (session, fact) units a signal must fire on to count
WORD = re.compile(r"[a-z0-9][a-z0-9_]{3,}")
STOP = set("this that with from have when then than they them were what will your about into been only also more most some such each over just like".split())
SENTENCE = re.compile(r"[.!?](?=\s|$)|\n")


def terms(text):
    return {w for w in WORD.findall((text or "").lower()) if w not in STOP}


def named(text, name):
    low = (text or "").lower()
    return re.search(r"(?<![a-z0-9-])" + re.escape(name.lower()) + r"(?![a-z0-9-])", low) is not None


def holds(text, name, desc_terms, ruling, share=0.6, least=3):
    if named(text, name):
        return True
    if ruling or len(desc_terms) < least:
        return False
    for part in SENTENCE.split((text or "").lower()):
        got = desc_terms & terms(part)
        if len(got) >= least and len(got) >= share * len(desc_terms):
            return True
    return False


def parse_ts(s):
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except (ValueError, AttributeError):
        return None


class Store:
    """Facts by name: path at HEAD, and description and ruling status at a
    case's commit (the clone's git), falling back to HEAD's."""

    def __init__(self, root):
        self.root, self.head, self._at = root, {}, {}
        for p in glob.glob(os.path.join(root, "**", "*.md"), recursive=True):
            try:
                with open(p, encoding="utf-8", errors="replace") as fh:
                    text = fh.read(6000)
            except OSError:
                continue
            f = self.parse(text)
            if f:
                self.head.setdefault(f[0], (os.path.relpath(p, root), f[1], f[2]))

    @staticmethod
    def parse(text):
        if not text.startswith("---"):
            return None
        name = re.search(r"^name:\s*(.+)$", text, re.M)
        desc = re.search(r"^description:\s*(.+)$", text, re.M)
        if not (name and desc):
            return None
        ruling = re.search(r"^\s+kind:\s*user\s*$", text, re.M) is not None or re.search(r"^type:\s*statement\s*$", text, re.M) is not None
        return name.group(1).strip().strip("'\""), desc.group(1).strip().strip("'\""), ruling

    def fact(self, name, commit):
        if name not in self.head:
            return None
        path, desc, ruling = self.head[name]
        key = (commit, path)
        if key not in self._at:
            r = subprocess.run(["git", "-C", self.root, "show", f"{commit}:{path}"], capture_output=True, text=True)
            f = self.parse(r.stdout) if r.returncode == 0 else None
            self._at[key] = (f[1], f[2]) if f else (desc, ruling)
        return self._at[key]


def transcript_events(path):
    """The transcript's events in order: (time, kind, text). Kinds: user (a
    string turn from a person or a program: the case's origin says which),
    agent (reply text), brief (a subagent's prompt), commit (a shell command
    that writes a commit, PR or issue body), note (a tacit note the agent
    added), open (tacit_fact), search (a tacit search), summary (a
    compaction summary). Sidechain lines are left out."""
    out = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            try:
                d = json.loads(line)
            except json.JSONDecodeError:
                continue
            ts = parse_ts(d.get("timestamp") or "")
            if ts is None or d.get("isSidechain"):
                continue
            msg = d.get("message") or {}
            content = msg.get("content")
            if d.get("isCompactSummary"):
                text = content if isinstance(content, str) else " ".join(b.get("text", "") for b in content or [] if isinstance(b, dict))
                out.append((ts, "summary", text))
                continue
            if d.get("type") == "user" and isinstance(content, str) and not d.get("isMeta") and d.get("userType") == "external":
                if not content.startswith("<"):
                    out.append((ts, "user", content))
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
                        for x in re.findall(r"(?:^|[\s;&|])tacit\s+fact\s+([a-z0-9-]+)", cmd):
                            out.append((ts, "open", x))
                        if re.search(r"git\s+commit|gh\s+pr\s+(create|edit|comment)|gh\s+issue\s+(create|comment)", cmd):
                            out.append((ts, "commit", cmd))
                        if re.search(r"(?:^|[\s;&|])tacit\s+add\b", cmd):
                            out.append((ts, "note", cmd))
                        m = re.search(r"(?:^|[\s;&|])tacit\s+search\s+(.*)", cmd)
                        if m:
                            out.append((ts, "search", m.group(1)))
    return out


def signals_for(events, start, end, name, desc, ruling, owner_session):
    dt = terms(desc)
    s = {"summary_kept": False, "summary_dropped": False, "needed_again": False,
         "carried_forward": False, "opened": False, "owner_restated": None if not owner_session else False}
    dropped_at = None
    for t, kind, text in events:
        if t <= start or t >= end:
            continue
        if kind == "summary":
            if holds(text, name, dt, ruling):
                s["summary_kept"] = True
            elif dropped_at is None:
                s["summary_dropped"], dropped_at = True, t
        elif kind == "open" and text == name:
            s["opened"] = True
            if dropped_at is not None:
                s["needed_again"] = True
        elif kind == "search" and dropped_at is not None:
            q = terms(text)
            if len(q) >= 2 and len(q & dt) >= 0.6 * len(q):
                s["needed_again"] = True
        elif kind in ("brief", "commit", "note", "agent") and holds(text, name, dt, ruling):
            s["carried_forward"] = True
        elif kind == "user" and owner_session and holds(text, "\x00", dt, False):
            s["owner_restated"] = True
    return s


def bootstrap_share(rows, good, n=2000, seed=7):
    """A 95% interval of the share of rows graded good, resampling sessions."""
    by = collections.defaultdict(list)
    for r in rows:
        by[r["session"]].append(r)
    sessions = list(by)
    if not sessions:
        return (float("nan"), float("nan"))
    rnd = random.Random(seed)
    shares = []
    for _ in range(n):
        pick = [r for _ in sessions for r in by[rnd.choice(sessions)]]
        if pick:
            shares.append(sum(r["grade"] == good for r in pick) / len(pick))
    shares.sort()
    return (shares[int(0.025 * len(shares))], shares[int(0.975 * len(shares)) - 1])


def verdict(rows, sig, good, base):
    fired = [r for r in rows if r[sig]]
    units = {(r["session"], r["fact"]) for r in fired}
    row = {"fires": len(fired), "units": len(units), "R": sum(r["grade"] == "R" for r in fired),
           "W": sum(r["grade"] == "W" for r in fired), "N": sum(r["grade"] == "N" for r in fired)}
    if good and fired:
        share = row[good] / len(fired)
        lo, hi = bootstrap_share(fired, good)
        row.update(share=round(share, 3), ci=[round(lo, 3), round(hi, 3)], base=round(base[good], 3))
        if len(units) < MIN_UNITS:
            row["verdict"] = "unmeasured"  # too few units to say
        else:
            row["verdict"] = "passes" if share >= 2 * base[good] and lo > base[good] else "fails"
    elif good:
        row["verdict"] = "unmeasured"
    return row


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--cases", required=True)
    ap.add_argument("--store", required=True, help="a store clone, for the facts' descriptions at each case's commit")
    ap.add_argument("--grades", action="append", help="extra case-id/fact/grade TSV")
    ap.add_argument("--projects", default=os.path.expanduser("~/.claude/projects"), help="where the sessions' transcripts are")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    store = Store(a.store)
    transcripts = {os.path.basename(p)[:-6]: p for p in glob.glob(os.path.join(a.projects, "*", "*.jsonl"))}
    extra = collections.defaultdict(dict)
    for g in a.grades or []:
        for line in open(g):
            f = line.rstrip("\n").split("\t")
            if len(f) >= 3 and f[2] in ("R", "W", "N"):
                extra[f[0]][f[1]] = f[2]
    cases = [json.loads(line) for line in open(a.cases)]
    # When each session's later blocks deliver a fact again: a line's window
    # ends there.
    deliveries = collections.defaultdict(list)
    for c in cases:
        t = parse_ts(c.get("ts") or "")
        for n in c.get("delivered") or []:
            if t:
                deliveries[(c.get("session"), n)].append(t)
    cache, rows = {}, []
    skipped = collections.Counter()
    for c in cases:
        labels = {**(c.get("labels") or {}), **extra.get(c["id"], {})}
        if not labels:
            continue
        if c.get("sidechain"):
            skipped["sidechain"] += len(labels)
            continue
        path = transcripts.get(c.get("session") or "")
        start = parse_ts(c.get("ts") or "")
        if not path or start is None:
            skipped["no transcript"] += len(labels)
            continue
        if path not in cache:
            cache[path] = (transcript_events(path), os.path.getsize(path))
        events, size = cache[path]
        for name, grade in labels.items():
            f = store.fact(name, c.get("commit") or "HEAD")
            if f is None:
                skipped["fact not in the store"] += 1
                continue
            desc, ruling = f
            ruling = ruling or name in (c.get("rules") or [])
            later = [t for t in deliveries[(c.get("session"), name)] if t > start]
            end = min(later + [start + timedelta(hours=WINDOW_HOURS)])
            rows.append({"case": c["id"], "session": c.get("session"), "fact": name, "grade": grade, "ruling": ruling,
                         "transcript_bytes": size, **signals_for(events, start, end, name, desc, ruling, c.get("origin") == "owner")})
    n = len(rows)
    base = {g: sum(r["grade"] == g for r in rows) / n for g in "RWN"} if n else {g: 0 for g in "RWN"}
    report = {"lines": n, "sessions": len({r["session"] for r in rows}), "skipped": dict(skipped), "base": base,
              "window_hours": WINDOW_HOURS, "min_units": MIN_UNITS, "signals": {}}
    for sig, good in (("summary_kept", "R"), ("needed_again", "R"), ("carried_forward", "R"), ("opened", "R"), ("summary_dropped", "N")):
        report["signals"][sig] = verdict(rows, sig, good, base)
    restated = [r for r in rows if r["owner_restated"]]
    report["signals"]["owner_restated"] = {"fires": len(restated), "of": sum(1 for r in rows if r["owner_restated"] is not None)}
    with open(a.out, "w") as fh:
        json.dump({"report": report, "rows": rows}, fh, indent=1, default=str)
    os.chmod(a.out, 0o600)
    print(json.dumps(report, indent=1))


if __name__ == "__main__":
    sys.exit(main())
