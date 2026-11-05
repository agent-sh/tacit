#!/usr/bin/env python3
"""The dreamer's judge against the hand grades.

For each graded line of a case file (deliveries.py's cases, with labels or a
--grades TSV; the same rows signals.py reads: a subagent's delivery, a
duplicate and a line graded two ways are left out), build the case the judge
reads in the dream pass (delivery_judge.go, momentOf): where the line came,
the request (the case's prompt; a push's tool call), the line (the fact's
name and description at the case's commit) and the first events of the
session after the moment (reply text, tool calls, tool results, the owner's
turns; each clipped to 800 characters, at most 12). `tacit delivery-judge`
redacts them, asks the judge on the same route as the dream pass (`claude
-p` on the subscription's login, never Bedrock) and prints its verdicts.

Agreement, under the rule eval/DELIVERY.md ("Scoring each delivery", step 5)
wrote before the first run:

  relevant   the judge's yes/no against on the topic (R or W) vs noise (N)
  useful     relevant and novel both yes, against useful (R) vs not (W, N)
  confused   reported only: no grade says whether a line misled

Per label: Cohen's kappa over the lines the judge decided (unsure left out
and counted), a 95% interval by a bootstrap over sessions, and the rule's
verdict. Cases, transcripts, grades, verdicts and the report stay private:
write --out outside the repository.

  python3 -I eval/delivery/judge_agreement.py --cases /abs/private/cases.jsonl \\
      --store /abs/store-clone [--grades /abs/private/extra.tsv] --out /abs/private/judge.json
"""
import argparse
import collections
import glob
import importlib.util
import json
import os
import random
import subprocess
import sys

_spec = importlib.util.spec_from_file_location("signals", os.path.join(os.path.dirname(os.path.abspath(__file__)), "signals.py"))
signals = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(signals)

AFTER, TEXT = 12, 800  # delivery_judge.go: judgeAfter, judgeTextLen
# The rule (eval/DELIVERY.md, step 5).
MIN_LINES, MIN_SESSIONS = 200, 30
KAPPA, KAPPA_LOW, MAX_UNSURE = 0.41, 0.21, 0.2
HOOKS = {"prompt": "the owner's prompt", "push": "a tool call about to run", "start": "the session's start"}


def clip(s, n=TEXT):
    s = " ".join(str(s).split())
    return s[:n]


def after_events(path, start):
    """The first AFTER events of the main chain after start, as momentOf
    words them."""
    out = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            try:
                d = json.loads(line)
            except json.JSONDecodeError:
                continue
            ts = signals.parse_ts(d.get("timestamp") or "")
            if ts is None or ts <= start or d.get("isSidechain") or d.get("isMeta"):
                continue
            content = (d.get("message") or {}).get("content")
            if d.get("type") == "user":
                if isinstance(content, str):
                    if d.get("userType") == "external" and content and not content.startswith("<"):
                        out.append("owner: " + clip(content))
                elif isinstance(content, list):
                    for b in content:
                        if isinstance(b, dict) and b.get("type") == "tool_result":
                            c = b.get("content")
                            if isinstance(c, list):
                                c = " ".join(x.get("text", "") for x in c if isinstance(x, dict))
                            out.append("tool result: " + clip(c or ""))
            elif d.get("type") == "assistant" and isinstance(content, list):
                for b in content:
                    if not isinstance(b, dict):
                        continue
                    if b.get("type") == "text" and b.get("text"):
                        out.append("agent: " + clip(b["text"]))
                    elif b.get("type") == "tool_use":
                        out.append("tool call " + str(b.get("name", "")) + ": " + clip(json.dumps(b.get("input") or {}, ensure_ascii=False)))
            if len(out) >= AFTER:
                return out[:AFTER]
    return out


def rows_of(a):
    store = signals.Store(a.store)
    transcripts = {os.path.basename(p)[:-6]: p for p in glob.glob(os.path.join(a.projects, "*", "*.jsonl"))}
    extra = collections.defaultdict(dict)
    for g in a.grades or []:
        for line in open(g):
            f = line.rstrip("\n").split("\t")
            if len(f) >= 3 and f[2] in ("R", "W", "N"):
                extra[f[0]][f[1]] = f[2]
    cases = [json.loads(line) for line in open(a.cases)]

    def labels_of(c):
        return {**(c.get("labels") or {}), **{n: g for n, g in extra.get(c["id"], {}).items() if n in (c.get("delivered") or [])}}

    seen, conflicts = {}, set()
    for c in cases:
        for n, g in labels_of(c).items():
            k = (signals.block_key(c), n)
            if k in seen and seen[k] != g:
                conflicts.add(k)
            seen.setdefault(k, g)
    rows, done, skipped = [], set(), collections.Counter()
    for c in cases:
        labels = labels_of(c)
        if not labels:
            continue
        if c.get("sidechain"):
            skipped["sidechain"] += len(labels)
            continue
        path, start = transcripts.get(c.get("session") or ""), signals.parse_ts(c.get("ts") or "")
        if not path or start is None:
            skipped["no transcript"] += len(labels)
            continue
        nxt = None
        for name, grade in sorted(labels.items()):
            k = (signals.block_key(c), name)
            if k in conflicts or k in done:
                skipped["graded two ways" if k in conflicts else "duplicate"] += 1
                done.add(k)
                continue
            done.add(k)
            f = store.fact(name, c.get("commit") or "HEAD")
            if f is None:
                skipped["fact not in the store"] += 1
                continue
            if nxt is None:
                nxt = after_events(path, start)
            hook = c.get("event") or "prompt"
            request = clip(c.get("prompt") or "") if hook != "push" else "a tool call (" + clip(json.dumps(c.get("tool_input"), ensure_ascii=False), 300) + ")"
            rows.append({"session": c.get("session"), "fact": name, "grade": grade,
                         "case": {"block": c["id"], "fact": name, "hook": HOOKS.get(hook, hook), "line": name + ": " + f[0], "request": request, "next": nxt}})
    return rows, dict(skipped)


def judge(rows, tacit, model):
    """Verdicts for rows, in order, from `tacit delivery-judge`."""
    cmd = [tacit, "delivery-judge"] + (["--model", model] if model else [])
    out = []
    for i in range(0, len(rows), 12):
        batch = rows[i:i + 12]
        r = subprocess.run(cmd, input="".join(json.dumps(x["case"]) + "\n" for x in batch), capture_output=True, text=True, timeout=1800)
        if r.returncode != 0:
            sys.exit(f"delivery-judge failed at row {i}: {r.stderr.strip()[:500]}")
        got = [json.loads(line) for line in r.stdout.splitlines() if line.strip()]
        if len(got) != len(batch):
            sys.exit(f"delivery-judge answered {len(got)} of {len(batch)} at row {i}")
        out += got
        print(f"judged {len(out)}/{len(rows)}", file=sys.stderr)
    return out


def kappa(pairs):
    """Cohen's kappa of (judge, grade) booleans."""
    n = len(pairs)
    if n == 0:
        return None
    po = sum(j == g for j, g in pairs) / n
    pj, pg = sum(j for j, _ in pairs) / n, sum(g for _, g in pairs) / n
    pe = pj * pg + (1 - pj) * (1 - pg)
    return None if pe == 1 else (po - pe) / (1 - pe)


def measure(rows, verdicts, label, grade_true, n=2000, seed=7):
    by = collections.defaultdict(list)
    unsure = 0
    for r, v in zip(rows, verdicts):
        if label == "useful":
            if v["relevant"] == "no" or v["novel"] == "no":
                j = False
            elif v["relevant"] == "yes" and v["novel"] == "yes":
                j = True
            else:
                unsure += 1
                continue
        elif v[label] == "unsure":
            unsure += 1
            continue
        else:
            j = v[label] == "yes"
        by[r["session"]].append((j, r["grade"] in grade_true))
    pairs = [p for s in by.values() for p in s]
    k = kappa(pairs)
    rng, sess, ks = random.Random(seed), list(by), []
    for _ in range(n):
        sample = [p for s in (rng.choice(sess) for _ in sess) for p in by[s]]
        x = kappa(sample)
        if x is not None:
            ks.append(x)
    ks.sort()
    ci = [round(ks[int(0.025 * len(ks))], 3), round(ks[int(0.975 * len(ks)) - 1], 3)] if ks else None
    share_unsure = unsure / max(1, len(rows))
    enough = len(pairs) >= MIN_LINES and len(by) >= MIN_SESSIONS
    passes = enough and k is not None and ci is not None and k >= KAPPA and ci[0] >= KAPPA_LOW and share_unsure <= MAX_UNSURE
    agree = collections.Counter((j, g) for j, g in pairs)
    return {"lines": len(pairs), "sessions": len(by), "unsure": unsure, "unsure_share": round(share_unsure, 3),
            "kappa": None if k is None else round(k, 3), "ci": ci,
            "table": {"judge yes, grade yes": agree[(True, True)], "judge yes, grade no": agree[(True, False)],
                      "judge no, grade yes": agree[(False, True)], "judge no, grade no": agree[(False, False)]},
            "verdict": "passes" if passes else ("too few lines" if not enough else "fails")}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--cases", required=True)
    ap.add_argument("--store", required=True, help="a store clone, for the facts' descriptions at each case's commit")
    ap.add_argument("--grades", action="append", help="extra case-id/fact/grade TSV")
    ap.add_argument("--projects", default=os.path.expanduser("~/.claude/projects"), help="where the sessions' transcripts are")
    ap.add_argument("--tacit", default="tacit", help="the tacit binary whose delivery-judge runs")
    ap.add_argument("--model", default="", help="the judge's model (TACIT_DELIVERY_JUDGE_MODEL, else its default)")
    ap.add_argument("--verdicts", help="a verdicts file from an earlier run: measure it again, judge nothing")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    rows, skipped = rows_of(a)
    if a.verdicts:
        verdicts = [json.loads(line) for line in open(a.verdicts)]
    else:
        verdicts = judge(rows, a.tacit, a.model)
        with open(a.out + ".verdicts.jsonl", "w") as fh:
            fh.writelines(json.dumps(v) + "\n" for v in verdicts)
    if len(verdicts) != len(rows) or any(v.get("fact") != r["fact"] for r, v in zip(rows, verdicts)):
        sys.exit("the verdicts do not match the rows")
    base = collections.Counter(r["grade"] for r in rows)
    report = {"lines": len(rows), "sessions": len({r["session"] for r in rows}), "skipped": skipped, "grades": dict(base),
              "rule": {"min_lines": MIN_LINES, "min_sessions": MIN_SESSIONS, "kappa": KAPPA, "kappa_low": KAPPA_LOW, "max_unsure": MAX_UNSURE},
              "relevant": measure(rows, verdicts, "relevant", ("R", "W")),
              "useful": measure(rows, verdicts, "useful", ("R",)),
              "confused": dict(collections.Counter(v["confused"] for v in verdicts))}
    with open(a.out, "w") as fh:
        json.dump(report, fh, indent=1)
    for k in ("relevant", "useful"):
        m = report[k]
        print(f"{k}: kappa {m['kappa']} {m['ci']} over {m['lines']} lines in {m['sessions']} sessions, unsure {m['unsure_share']}: {m['verdict']}")


if __name__ == "__main__":
    main()
