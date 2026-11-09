#!/usr/bin/env python3
"""Offline evaluation of a delivery policy on past moments (#129, stage 5).

Reads the private delivery log (<state>/delivery/<day>.jsonl): the prompt
and push moments, each with its candidates, their features and p, the
probability the logging policy had of delivering each (the hooks log it
before any draw), and the dreamer's judge: labels for a sample of the
delivered lines, each with the probability its line had of being judged,
and an entry per pass (when it ran, which judge, which drawn lines it lost
and why).

A policy gives each candidate of a moment the probability pi it would
deliver it. It may read the candidate's features and p; the logging policy
is pi = p. Per moment it estimates useful lines (the judge says relevant and
novel), noise lines (the judge says not relevant; unsure is neither) and
their net, useful - LAMBDA * noise, by:

  ips      sum over delivered, judged lines of pi / (p * p_judge) * reward
  snips    ips per unit of weight, times the policy's supported lines
  dr       doubly robust: pi * a reward model's prediction for every line,
           plus the ips-weighted residual where a label exists; the model
           is a stratified mean (by hook, provenance, share, matched terms,
           form) shrunk to the overall mean, cross-fitted by halves of the
           sessions and refit in every bootstrap sample
  dr_sup   dr over the supported lines only, the part ips and snips also
           estimate: a check of the model against them

support is the share of pi on lines the logging policy could deliver
(p > 0): ips and snips count nothing for the rest, dr only its model's
guess. Intervals are 95% bootstraps over sessions. The population is the
moments in the pools of the chosen judge's passes (each pass logs its pool's
window; a window is the judge's when its moments' labels all are, or with
none, when its pass asked the judge's prompt, no other model answered and
the model it asked is the judge's),
up to its last pass that left no drawn line waiting, less a pass's whole
window when it could not read a drawn line, and less the moments with no
transcript it could read when the pool was built; blocks that showed nothing
count. A drawn line whose calls kept failing stays a candidate every policy
sees but counts in no sum; the report gives their share. The
decision rule is eval/DELIVERY.md ("Scoring each delivery", step 6): no
offline estimate ships a change; one can only promote a candidate to a live
test. The log, labels and report stay private: write --out outside the
repository.

  python3 -I eval/delivery/offline.py --log ~/.local/state/tacit/delivery --out /abs/private/offline.json
"""
import argparse
import collections
import glob
import json
import os
import random
import sys
from datetime import datetime, timedelta

HOOKS = ("prompt", "push")  # the moments the judge reads
JUDGE_AGE = timedelta(minutes=30)  # delivery_judge.go: judgeAge
JUDGE_DAYS = timedelta(days=2)  # judgeDays
# The rule (eval/DELIVERY.md, step 6).
MIN_LINES, MIN_SESSIONS, MIN_SUPPORT = 300, 30, 0.8
LAMBDA = 1.0  # a noise line costs what a useful line gains
SHRINK = 5.0  # the reward model's prior weight on the overall mean
GATES = ("withheld", "seen", "held")  # lines no policy may deliver


def parse_ts(s):
    try:
        return datetime.fromisoformat(str(s).replace("Z", "+00:00"))
    except ValueError:
        return None


def read_log(path):
    """The log's entries, from a directory of <day>.jsonl files or a file."""
    files = sorted(glob.glob(os.path.join(path, "*.jsonl"))) if os.path.isdir(path) else [path]
    out = []
    for f in files:
        with open(f, encoding="utf-8", errors="replace") as fh:
            for line in fh:
                try:
                    e = json.loads(line)
                except ValueError:
                    continue
                if isinstance(e, dict) and e.get("id"):
                    out.append(e)
    return out


def moments_of(entries, judge=None):
    """The judged population (see the module's doc) and what was left out.
    judge is (model, prompt digest): only its labels count, and only the
    pools of its passes; with none given, the one judge the labels hold."""
    found, passes, judges = [], [], collections.Counter()
    when = {}
    for e in entries:
        if e.get("hook") == "judge" and e.get("judge"):
            j = e["judge"]
            judges[(j.get("model"), j.get("prompt"))] += 1
            found.append(j)
        elif e.get("hook") == "judge-pass" and e.get("judge_pass") and parse_ts(e.get("ts")):
            passes.append((parse_ts(e["ts"]), e["judge_pass"]))
        elif e.get("hook") in HOOKS and parse_ts(e.get("ts")):
            when[e["id"]] = parse_ts(e["ts"])
    if judge is None and len(judges) == 1:
        judge = next(iter(judges))
    labels = {(j.get("block"), j.get("fact")): j for j in found if judge is None or (j.get("model"), j.get("prompt")) == judge}
    passes.sort(key=lambda x: x[0])
    info = {"passes": len(passes), "labels": len(labels), "judges": {f"{m} {p}": n for (m, p), n in judges.items()}}
    # Each pass's pool window, from all passes: logged by the pass, else from
    # the passes' times.
    windows = []
    for i, (t, jp) in enumerate(passes):
        lo, hi = parse_ts(jp.get("from")), parse_ts(jp.get("to"))
        if lo is None or hi is None:
            lo, hi = t - JUDGE_DAYS, t - JUDGE_AGE
            if i > 0:
                lo = max(lo, passes[i - 1][0] - JUDGE_AGE)
        windows.append((lo, hi, jp))

    def window_of(t):
        return next((k for k, (lo, hi, _) in enumerate(windows) if t is not None and lo < t <= hi), None)

    # A window is the chosen judge's when the labels of its moments are all
    # the judge's; with none, when its pass asked with the judge's prompt, no
    # other model answered it, and the model it asked is the judge's (a pass
    # whose every call failed logs the model it asked and no answer). A pass
    # logs the model as asked and a label the one that answered, so an asked
    # name is the judge's when it is the judge's model, or when every answer
    # the log holds to that name came from the judge's model.
    answers = collections.defaultdict(set)
    for _, jp in passes:
        if jp.get("model"):
            answers[jp["model"]].update(jp.get("answered") or [])
    owners = collections.defaultdict(set)
    for j in found:
        k = window_of(when.get(j.get("block")))
        if k is not None:
            owners[k].add((j.get("model"), j.get("prompt")))

    def mine(k):
        jp = windows[k][2]
        if judge is None:
            return True
        if owners[k]:
            return owners[k] == {judge}
        asked = jp.get("model")
        if asked and asked != judge[0] and answers[asked] != {judge[0]}:
            return False
        return jp.get("prompt") in (None, "", judge[1]) and all(m == judge[0] for m in jp.get("answered") or [])

    keep = [k for k in range(len(windows)) if mine(k)]
    # Up to the judge's last pass that left nothing waiting: every line
    # drawn by then has its label or is lost.
    last = max((k for k in keep if not windows[k][2].get("waiting")), default=-1)
    keep = [k for k in keep if k <= last]
    # A drawn line can wait through several passes (the cap, a refusal), so
    # the pass that names it unread or failed is often not the pass whose
    # pool drew it: each name is put in the window its moment lies in. A
    # window holding a line the judge could not read is left out whole: what
    # failed (the transcript, the redactor) may have failed for the moments
    # it did not draw. A failed line is left out of the sums wherever it is.
    unread_blocks, failed, unread_in = set(), set(), set()
    for _, _, jp in windows:
        unread_blocks.update(jp.get("unread_blocks") or [])
        failed.update(jp.get("failed") or [])
        for name in jp.get("unread") or []:
            k = window_of(when.get(name.split("/", 1)[0]))
            if k is not None:
                unread_in.add(k)
    dropped = [k for k in keep if k in unread_in]
    keep = [k for k in keep if k not in dropped]
    if not keep:
        return [], info
    kept = set(keep)
    drawn = sum(int(windows[k][2].get("drawn") or 0) for k in keep)
    failed_kept = {n for n in failed if window_of(when.get(n.split("/", 1)[0])) in kept}
    out, left = [], collections.Counter()
    for e in entries:
        t = parse_ts(e.get("ts"))
        if e.get("hook") not in HOOKS or t is None:
            continue
        k = window_of(t)
        if k not in kept:
            left["window dropped (an unread drawn line)" if k in dropped else "outside the judge's pools"] += 1
            continue
        if not e.get("transcript_path") or (e.get("transcript") or 0) < 0 or e["id"] in unread_blocks:
            left["no transcript to read"] += 1
            continue
        cands = []
        for c in e.get("cands") or []:  # a block that showed nothing has none
            c = dict(c)
            c["label"] = labels.get((e["id"], c.get("name")))
            # A drawn line whose calls kept failing: it stays a candidate the
            # policy sees, and counts in no sum.
            c["excluded"] = e["id"] + "/" + str(c.get("name")) in failed
            left["failed lines"] += c["excluded"]
            cands.append(c)
        out.append({"id": e["id"], "session": e.get("session") or e["id"], "hook": e["hook"], "cands": cands})
    info.update({"judge": f"{judge[0]} {judge[1]}" if judge else None, "from": min(windows[k][0] for k in keep).isoformat(),
                 "to": max(windows[k][1] for k in keep).isoformat(), "left_out": dict(left),
                 # Failed lines are left out of the sums, but an undrawn line
                 # that would fail keeps its model guess in dr: the share says
                 # how much that can be.
                 "failed_share": round(len(failed_kept) / drawn, 4) if drawn else 0.0})
    return out, info


def rewards(label):
    """(useful, noise) of a judge label; unsure is neither."""
    if not label:
        return None
    return (1.0 if label.get("relevant") == "yes" and label.get("novel") == "yes" else 0.0,
            1.0 if label.get("relevant") == "no" else 0.0)


def weight(c):
    """1 / (p * p_judge) for a delivered, judged line; 0 otherwise."""
    lab = c.get("label")
    if not lab or not c.get("delivered"):
        return 0.0
    pd, pj = float(c.get("p") or 0), float(lab.get("p") or 0)
    return 1.0 / (pd * pj) if pd > 0 and pj > 0 else 0.0


def cell(m, c):
    """The reward model's stratum of a line."""
    share = float(c.get("share") or 0)
    return (m["hook"], c.get("prov") or "", min(int(share * 4), 3), (c.get("matched") or 0) >= 2, c.get("form") == "title")


def fit(moments, sessions):
    """A reward model from the labelled lines of sessions (a multiset: a
    session drawn twice counts twice): per stratum, the weighted mean
    shrunk to the overall weighted mean."""
    count = collections.Counter(sessions)
    tot = [0.0, 0.0, 0.0]
    cells = collections.defaultdict(lambda: [0.0, 0.0, 0.0])
    for m in moments:
        k = count.get(m["session"], 0)
        if not k:
            continue
        for c in m["cands"]:
            w, r = weight(c), rewards(c.get("label"))
            if w and r:
                for acc in (tot, cells[cell(m, c)]):
                    acc[0] += k * w
                    acc[1] += k * w * r[0]
                    acc[2] += k * w * r[1]
    gu, gn = (tot[1] / tot[0], tot[2] / tot[0]) if tot[0] else (0.0, 0.0)
    model = {k: ((v[1] + SHRINK * gu) / (v[0] + SHRINK), (v[2] + SHRINK * gn) / (v[0] + SHRINK)) for k, v in cells.items()}
    return lambda m, c: model.get(cell(m, c), (gu, gn))


def cross_fit(moments, sessions):
    """Each session's lines predicted by the model fit on the other half
    of the sessions (halves by the sessions' order)."""
    order = sorted(set(sessions))
    half = {s: i & 1 for i, s in enumerate(order)}
    models = [fit(moments, [s for s in sessions if half[s] != f]) for f in (0, 1)]
    return lambda m, c: models[half.get(m["session"], 0)](m, c)


SUMS = ("moments", "pi", "sup", "w", "ips_u", "ips_n", "dr_u", "dr_n", "drs_u", "drs_n", "labels")


def session_sums(moments, policy, model):
    out = collections.defaultdict(lambda: dict.fromkeys(SUMS, 0.0))
    for m in moments:
        s = out[m["session"]]
        s["moments"] += 1
        for c in m["cands"]:
            pi = policy(m, c)
            if pi <= 0 or c.get("excluded"):
                continue
            sup = float(c.get("p") or 0) > 0
            hu, hn = model(m, c)
            s["pi"] += pi
            s["dr_u"] += pi * hu
            s["dr_n"] += pi * hn
            if sup:
                s["sup"] += pi
                s["drs_u"] += pi * hu
                s["drs_n"] += pi * hn
            w, r = weight(c), rewards(c.get("label"))
            if w and r:
                s["labels"] += 1
                s["w"] += pi * w
                for k, x, h in (("u", r[0], hu), ("n", r[1], hn)):
                    s["ips_" + k] += pi * w * x
                    s["dr_" + k] += pi * w * (x - h)
                    s["drs_" + k] += pi * w * (x - h)
    return out


def values(sums):
    """Per-moment estimates from a list of session sums."""
    t = {k: sum(s[k] for s in sums) for k in SUMS}
    m = t["moments"] or 1
    v = {"lines": t["pi"] / m, "support": t["sup"] / t["pi"] if t["pi"] else 1.0}
    for k in ("u", "n"):
        v["ips_" + k] = t["ips_" + k] / m
        v["snips_" + k] = t["ips_" + k] / t["w"] * t["sup"] / m if t["w"] else 0.0
        v["dr_" + k] = t["dr_" + k] / m
        v["dr_sup_" + k] = t["drs_" + k] / m
    for est in ("ips", "snips", "dr", "dr_sup"):
        v[est + "_net"] = v[est + "_u"] - LAMBDA * v[est + "_n"]
    return v


def evaluate(moments, policies, base="logged", n=500, seed=7):
    """Each policy's estimates, and its difference from base, with 95%
    intervals by a bootstrap over sessions that refits the reward model in
    every sample."""
    sessions = sorted({m["session"] for m in moments})
    rng = random.Random(seed)

    def run(sample):
        model = cross_fit(moments, sample)
        out = {}
        for name, p in policies.items():
            sums = session_sums(moments, p, model)
            out[name] = values([sums[s] for s in sample])
        return out

    point = run(sessions)
    boots = [run([rng.choice(sessions) for _ in sessions]) for _ in range(n)]
    model = cross_fit(moments, sessions)
    report = {}
    for name, p in policies.items():
        ss = session_sums(moments, p, model)
        labels = (int(sum(x["labels"] for x in ss.values())), sum(1 for x in ss.values() if x["labels"]))
        keys = list(point[name])
        ci = {k: interval([b[name][k] for b in boots]) for k in keys}
        diff = {k: point[name][k] - point[base][k] for k in keys}
        dci = {k: interval([b[name][k] - b[base][k] for b in boots]) for k in keys}
        report[name] = {"point": rnd(point[name]), "ci": ci, "vs_" + base: rnd(diff), "vs_ci": dci,
                        "labels": labels[0], "label_sessions": labels[1],
                        "verdict": verdict(name, base, point[name], diff, dci, labels)}
    return report


def interval(xs):
    xs = sorted(xs)
    return [round(xs[int(0.025 * len(xs))], 4), round(xs[int(0.975 * len(xs)) - 1], 4)]


def rnd(d):
    return {k: round(v, 4) for k, v in d.items()}


def verdict(name, base, point, diff, dci, labels):
    """The rule: promote to a live test only with enough labels in enough
    sessions on the candidate's lines, enough support, ips and snips agreeing
    with the model on the supported lines (dr_sup's net difference within
    their intervals of it), and dr's net difference from base above zero."""
    if name == base:
        return "base"
    if labels[0] < MIN_LINES or labels[1] < MIN_SESSIONS:
        return "too few labels"
    if point["support"] < MIN_SUPPORT:
        return "too little support"
    for est in ("ips", "snips"):
        lo, hi = dci[est + "_net"]
        if not lo <= diff["dr_sup_net"] <= hi:
            return "flagged: " + est + " disagrees with the model"
    if dci["dr_net"][0] > 0:
        return "promote to a live test"
    return "no"


# Policies: a candidate's probability of being delivered, from its features
# and p.

def logged(m, c):
    return float(c.get("p") or 0)


def logged_where(keep):
    """The logging policy, narrowed: pi = p where keep holds."""
    return lambda m, c: float(c.get("p") or 0) if keep(c) else 0.0


def top_k(k):
    """The k best scored candidates that no gate held back."""
    def pi(m, c):
        live = sorted((x for x in m["cands"] if x.get("dropped") not in GATES), key=lambda x: (-float(x.get("score") or 0), str(x.get("name"))))
        return 1.0 if any(x is c for x in live[:k]) else 0.0
    return pi


POLICIES = {
    "logged": logged,
    "logged_share_0.5": logged_where(lambda c: float(c.get("share") or 0) >= 0.5),
    "logged_no_derived": logged_where(lambda c: c.get("prov") != "derived"),
    "top1": top_k(1),
    "top3": top_k(3),
}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", required=True, help="the delivery log directory (or one day's file)")
    ap.add_argument("--policy", action="append", help="policies to evaluate (default: all built in)")
    ap.add_argument("--judge", help="the judge whose labels count, as 'MODEL PROMPT-DIGEST' (the pair step 5's latest amendment names, whose relevant labels passed and useful labels did not: its useful and net values move nothing); required when the log holds more than one")
    ap.add_argument("--boot", type=int, default=500, help="bootstrap samples")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    entries = read_log(os.path.expanduser(a.log))
    judge = tuple(a.judge.split(" ", 1)) if a.judge else None
    moments, cover = moments_of(entries, judge)
    if judge is None and len(cover.get("judges", {})) > 1:
        sys.exit("the log holds labels of more than one judge: pass --judge 'MODEL PROMPT-DIGEST'")
    names = a.policy or list(POLICIES)
    policies = {"logged": logged, **{n: POLICIES[n] for n in names}}
    report = {"coverage": cover, "moments": len(moments), "sessions": len({m["session"] for m in moments}),
              "rule": {"min_lines": MIN_LINES, "min_sessions": MIN_SESSIONS, "min_support": MIN_SUPPORT, "lambda": LAMBDA},
              "policies": evaluate(moments, policies, n=a.boot) if moments else {}}
    # The report is private: this user's alone, whatever the umask.
    fd = os.open(a.out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w") as fh:
        json.dump(report, fh, indent=1)
    print(f"{len(moments)} moments, {report['sessions']} sessions, {cover.get('labels', 0)} labels")
    for n, r in report["policies"].items():
        print(f"{n}: dr net {r['point']['dr_net']} {r['ci']['dr_net']} support {r['point']['support']} labels {r['labels']}: {r['verdict']}")


if __name__ == "__main__":
    sys.exit(main())
