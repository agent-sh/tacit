#!/usr/bin/env python3
"""Offline evaluation of a delivery policy on past moments (#129, stage 5).

Reads the private delivery log (<state>/delivery/<day>.jsonl): the prompt
and push moments, each with its candidates, their features and the
probability p the logging policy had of delivering each (logged by the
hooks), and the dreamer's judge entries: a label for a sample of delivered
lines, each with the probability p its line had of being judged (logged by
the judge's draw). A policy is a function from a moment's candidates to the
lines it would deliver. For each policy, and the difference from the logged
policy, it estimates two values per moment:

  useful   lines the judge would call relevant and novel
  noise    lines the judge would call not relevant

with three estimators, over the moments the judge's draws covered:

  ips      inverse propensity: a line counts when the logging policy
           delivered it and the judge labelled it, weighted by
           1 / (p_deliver * p_judge)
  snips    self-normalized ips (the weights normalized per policy)
  dr       doubly robust: a reward model's prediction for every line the
           policy picks, corrected by the ips residual where a label exists
           (the model is a logistic regression over the logged features,
           fit cross-fitted by session halves)

and the policy's support: the share of the lines it picks that the logging
policy could deliver (p_deliver > 0). Lines with no support count nothing in
ips and snips, and only the model's guess in dr. Intervals are 95%
bootstraps over sessions. The decision rule is eval/DELIVERY.md ("Scoring
each delivery", step 6): no offline estimate ships a change; one can only
promote a candidate policy to a live test. The log, labels and report stay
private: write --out outside the repository.

  python3 -I eval/delivery/offline.py --log ~/.local/state/tacit/delivery --out /abs/private/offline.json
"""
import argparse
import collections
import glob
import json
import math
import os
import random
import sys
from datetime import datetime, timedelta

HOOKS = ("prompt", "push")  # the moments the judge reads
JUDGE_AGE = timedelta(minutes=30)  # delivery_judge.go: judgeAge
JUDGE_DAYS = timedelta(days=2)  # judgeDays
PROVS = ("agent", "rule", "owner", "statement", "derived", "claim", "confirmed", "words")
# The rule (eval/DELIVERY.md, step 6).
MIN_LINES, MIN_SESSIONS, MIN_SUPPORT = 300, 30, 0.8


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
    """The judged population: prompt and push moments in the time the
    judge's passes covered (from 2 days before the first pass to 30 minutes
    before the last), with each candidate's label when the judge gave one.
    A line the judge drew but could not label is named in a pass's `lost`
    and counted."""
    labels, passes, lost = {}, [], set()
    judges = collections.Counter()
    for e in entries:
        if e.get("hook") == "judge" and e.get("judge"):
            j = e["judge"]
            judges[(j.get("model"), j.get("prompt"))] += 1
            if judge is None or (j.get("model"), j.get("prompt")) == judge:
                labels[(j.get("block"), j.get("fact"))] = j
        elif e.get("hook") == "judge-pass" and e.get("judge_pass"):
            passes.append(parse_ts(e.get("ts")))
            lost.update(e["judge_pass"].get("lost") or [])
    passes = [t for t in passes if t]
    if not passes:
        return [], {"passes": 0, "labels": len(labels), "lost": len(lost), "judges": {f"{m} {p}": n for (m, p), n in judges.items()}}
    lo, hi = min(passes) - JUDGE_DAYS, max(passes) - JUDGE_AGE
    out = []
    for e in entries:
        t = parse_ts(e.get("ts"))
        if e.get("hook") not in HOOKS or t is None or not (lo <= t <= hi) or not e.get("cands"):
            continue
        cands = []
        for c in e["cands"]:
            c = dict(c)
            c["label"] = labels.get((e["id"], c.get("name")))
            cands.append(c)
        out.append({"id": e["id"], "session": e.get("session") or e["id"], "hook": e["hook"], "cands": cands})
    return out, {"passes": len(passes), "labels": len(labels), "lost": len(lost), "from": lo.isoformat(), "to": hi.isoformat(),
                 "judges": {f"{m} {p}": n for (m, p), n in judges.items()}}


def rewards(label):
    """(useful, noise) of a judge label; unsure is neither."""
    if not label:
        return None
    return (1.0 if label.get("relevant") == "yes" and label.get("novel") == "yes" else 0.0,
            1.0 if label.get("relevant") == "no" else 0.0)


def features(m, c):
    rank = c.get("rank", -1)
    f = [1.0, float(c.get("score") or 0), float(c.get("share") or 0), min(max(rank, -1), 20) / 20.0,
         float(c.get("matched") or 0), float(c.get("tier") or 0), math.log1p(max(c.get("age_days", -1), 0)),
         1.0 if c.get("tied") else 0.0, float(c.get("echo") or 0), 1.0 if c.get("review") else 0.0,
         1.0 if c.get("form") == "title" else 0.0, 1.0 if m["hook"] == "push" else 0.0]
    f += [1.0 if c.get("prov") == p else 0.0 for p in PROVS]
    return f


class Logistic:
    """A weighted logistic regression by gradient descent (standard library
    only), with an L2 penalty."""

    def __init__(self, rows, ys, ws, epochs=300, lr=0.1, l2=1e-3):
        n = len(rows[0]) if rows else 0
        self.mu = [sum(r[i] for r in rows) / len(rows) for i in range(n)] if rows else []
        self.sd = [max(1e-9, math.sqrt(sum((r[i] - self.mu[i]) ** 2 for r in rows) / len(rows))) for i in range(n)] if rows else []
        self.w = [0.0] * n
        xs = [self.norm(r) for r in rows]
        tw = sum(ws) or 1.0
        for _ in range(epochs):
            g = [0.0] * n
            for x, y, w in zip(xs, ys, ws):
                d = (self.p(x) - y) * w / tw
                for i in range(n):
                    g[i] += d * x[i]
            for i in range(n):
                self.w[i] -= lr * (g[i] + l2 * self.w[i])

    def norm(self, r):
        return [1.0 if i == 0 else (r[i] - self.mu[i]) / self.sd[i] for i in range(len(r))]

    def p(self, x):
        z = sum(a * b for a, b in zip(self.w, x))
        return 1 / (1 + math.exp(-max(-30, min(30, z))))

    def predict(self, r):
        return self.p(self.norm(r)) if self.w else 0.0


def observed(c):
    """A candidate's label weight: delivered by the logging policy and
    judged; its propensity p_deliver * p_judge."""
    lab = c.get("label")
    if not lab or not c.get("delivered"):
        return 0.0
    pd, pj = float(c.get("p") or 0), float(lab.get("p") or 0)
    return pd * pj if pd > 0 and pj > 0 else 0.0


def fit_models(moments):
    """Reward models for useful and noise, cross-fitted: each session's
    lines are predicted by the model fit on the other half of the sessions."""
    sessions = sorted({m["session"] for m in moments})
    half = {s: i & 1 for i, s in enumerate(sessions)}
    models = {}
    for fold in (0, 1):
        rows, yu, yn, ws = [], [], [], []
        for m in moments:
            if half[m["session"]] == fold:
                continue
            for c in m["cands"]:
                prop = observed(c)
                r = rewards(c.get("label"))
                if prop and r:
                    rows.append(features(m, c))
                    yu.append(r[0])
                    yn.append(r[1])
                    ws.append(1.0 / prop)
        models[fold] = (Logistic(rows, yu, ws), Logistic(rows, yn, ws)) if rows else None
    return lambda m, c: (models[half[m["session"]]][0].predict(features(m, c)), models[half[m["session"]]][1].predict(features(m, c))) if models[half[m["session"]]] else (0.0, 0.0)


def per_session(moments, policy, model):
    """For each session: moments, and per estimator the sums of useful and
    noise over the lines the policy picks; support counts."""
    out = collections.defaultdict(lambda: collections.defaultdict(float))
    for m in moments:
        s = out[m["session"]]
        s["moments"] += 1
        pick = policy(m)
        for c in m["cands"]:
            if c.get("name") not in pick:
                continue
            s["lines"] += 1
            if float(c.get("p") or 0) > 0:
                s["supported"] += 1
            hat = model(m, c)
            s["dr_u"] += hat[0]
            s["dr_n"] += hat[1]
            prop = observed(c)
            r = rewards(c.get("label"))
            if prop and r:
                w = 1.0 / prop
                s["labels"] += 1
                s["ips_u"] += w * r[0]
                s["ips_n"] += w * r[1]
                s["w"] += w
                s["dr_u"] += w * (r[0] - hat[0])
                s["dr_n"] += w * (r[1] - hat[1])
    return out


def estimate(sess, keys):
    """Per-moment values from session sums."""
    m = sum(s["moments"] for s in sess) or 1
    lines = sum(s["lines"] for s in sess)
    w = sum(s["w"] for s in sess)
    v = {"ips_useful": sum(s["ips_u"] for s in sess) / m, "ips_noise": sum(s["ips_n"] for s in sess) / m,
         "dr_useful": sum(s["dr_u"] for s in sess) / m, "dr_noise": sum(s["dr_n"] for s in sess) / m,
         "lines": lines / m, "support": (sum(s["supported"] for s in sess) / lines) if lines else 1.0}
    # snips: the ips per-line mean, normalized by the weights, times lines per moment
    v["snips_useful"] = (sum(s["ips_u"] for s in sess) / w * lines / m) if w else 0.0
    v["snips_noise"] = (sum(s["ips_n"] for s in sess) / w * lines / m) if w else 0.0
    return {k: v[k] for k in keys}


KEYS = ("ips_useful", "ips_noise", "snips_useful", "snips_noise", "dr_useful", "dr_noise", "lines", "support")


def evaluate(moments, policies, base="logged", n=1000, seed=7):
    """Each policy's estimates and its difference from base, with 95%
    intervals by a bootstrap over sessions."""
    model = fit_models(moments)
    per = {name: per_session(moments, p, model) for name, p in policies.items()}
    sessions = sorted({m["session"] for m in moments})
    rng = random.Random(seed)
    boots = [[rng.choice(sessions) for _ in sessions] for _ in range(n)]
    report = {}
    for name in policies:
        point = estimate([per[name][s] for s in sessions], KEYS)
        diff = {k: point[k] - estimate([per[base][s] for s in sessions], KEYS)[k] for k in KEYS}
        ci, dci = {}, {}
        for k in KEYS:
            xs = sorted(estimate([per[name][s] for s in b], (k,))[k] for b in boots)
            ds = sorted(estimate([per[name][s] for s in b], (k,))[k] - estimate([per[base][s] for s in b], (k,))[k] for b in boots)
            ci[k] = [round(xs[int(0.025 * n)], 4), round(xs[int(0.975 * n) - 1], 4)]
            dci[k] = [round(ds[int(0.025 * n)], 4), round(ds[int(0.975 * n) - 1], 4)]
        labels = int(sum(per[name][s]["labels"] for s in sessions))
        report[name] = {"point": {k: round(v, 4) for k, v in point.items()}, "ci": ci,
                        "vs_" + base: {k: round(v, 4) for k, v in diff.items()}, "vs_ci": dci,
                        "labels": labels, "sessions": len(sessions), "verdict": verdict(name, base, point, dci, labels, len(sessions), ci)}
    return report


def verdict(name, base, point, dci, labels, sessions, ci=None):
    """The rule: promote to a live test only with enough labels and
    sessions, enough support, more useful lines (dr's interval above 0) and
    no more noise (dr's interval not above 0)."""
    if name == base:
        return "base"
    if labels < MIN_LINES or sessions < MIN_SESSIONS:
        return "too few labels"
    if point["support"] < MIN_SUPPORT:
        return "too little support"
    if ci:
        for est in ("ips", "snips"):
            for v in ("useful", "noise"):
                lo, hi = ci[est + "_" + v]
                if not lo <= point["dr_" + v] <= hi:
                    return "flagged: " + est + " disagrees with dr"
    if dci["dr_useful"][0] > 0 and dci["dr_noise"][1] <= 0:
        return "promote to a live test"
    return "no"


# Policies over a moment's logged candidates: the names they would deliver.

def logged(m):
    return {c["name"] for c in m["cands"] if c.get("delivered")}


def top_k(k):
    def pick(m):
        live = [c for c in m["cands"] if not c.get("dropped") or c.get("delivered")]
        return {c["name"] for c in sorted(live, key=lambda c: -float(c.get("score") or 0))[:k]}
    return pick


def logged_share_at_least(t):
    def pick(m):
        return {c["name"] for c in m["cands"] if c.get("delivered") and float(c.get("share") or 0) >= t}
    return pick


def logged_no_derived(m):
    """The logged lines less derived facts (a narrowing the log fully
    supports)."""
    return {c["name"] for c in m["cands"] if c.get("delivered") and c.get("prov") != "derived"}


POLICIES = {"logged": logged, "logged_share_0.5": logged_share_at_least(0.5), "logged_no_derived": logged_no_derived,
            "top1": top_k(1), "top3": top_k(3)}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", required=True, help="the delivery log directory (or one day's file)")
    ap.add_argument("--policy", action="append", help="policies to evaluate (default: all built in)")
    ap.add_argument("--judge", help="the judge whose labels count, as 'MODEL PROMPT-DIGEST' (the pair step 5 passed); required when the log holds more than one")
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
              "rule": {"min_lines": MIN_LINES, "min_sessions": MIN_SESSIONS, "min_support": MIN_SUPPORT},
              "policies": evaluate(moments, policies) if moments else {}}
    with open(a.out, "w") as fh:
        json.dump(report, fh, indent=1)
    print(f"{len(moments)} moments, {report['sessions']} sessions, {cover.get('labels', 0)} labels")
    for n, r in report["policies"].items():
        print(f"{n}: dr useful {r['point']['dr_useful']} noise {r['point']['dr_noise']} support {r['point']['support']} labels {r['labels']}: {r['verdict']}")


if __name__ == "__main__":
    sys.exit(main())
