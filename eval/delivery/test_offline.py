#!/usr/bin/env python3
"""Checks of offline.py on a synthetic delivery log whose true values are
known: logging that delivers some lines at random, a judge that labels a
sample, and policies whose expected useful and noise lines per moment are
computed from the truth.

  python3 -I eval/delivery/test_offline.py
"""
import importlib.util
import json
import os
import random
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("offline", os.path.join(HERE, "offline.py"))
off = importlib.util.module_from_spec(spec)
spec.loader.exec_module(off)


def world(seed=3, sessions=60, per=20, judge=0.4):
    """Synthetic moments: five candidates each, a line useful with
    probability rising in its share; the logging policy delivers the best
    two always and the third with p 0.5; the judge labels each delivered
    line with p `judge`. Returns the log's entries and the truth."""
    rng = random.Random(seed)
    entries, truth = [], {}
    t0 = "2026-11-01T10:00:00Z"
    for s in range(sessions):
        for i in range(per):
            bid = f"b{s}-{i}"
            cands = []
            for r in range(5):
                share = 1.0 - r * 0.2
                pu = 0.15 + 0.6 * share
                useful = rng.random() < pu
                noise = (not useful) and rng.random() < 0.6
                truth[(bid, f"f{r}")] = (pu, (1 - pu) * 0.6)
                p = 1.0 if r < 2 else (0.5 if r == 2 else 0.0)
                delivered = rng.random() < p
                # p is the line's probability of being delivered, as the
                # hooks log it.
                c = {"name": f"f{r}", "score": 10 - r, "share": share, "rank": r, "matched": 3 - min(r, 3), "tier": 0,
                     "prov": "agent", "age_days": 5, "echo": 0.0, "delivered": delivered, "p": p}
                cands.append(c)
                if delivered and rng.random() < judge:
                    entries.append({"id": "j" + bid + c["name"], "ts": t0, "hook": "judge",
                                    "judge": {"block": bid, "fact": c["name"], "relevant": "yes" if (useful or not noise) else "no",
                                              "novel": "yes" if useful else "no", "confused": "no", "p": judge}})
            entries.append({"id": bid, "ts": t0, "hook": "prompt", "session": f"s{s}", "cands": cands})
    entries.append({"id": "pass1", "ts": "2026-11-01T12:00:00Z", "hook": "judge-pass", "judge_pass": {"pool": 1}})
    return entries, truth


def true_value(entries, truth, policy):
    moments = [e for e in entries if e.get("hook") == "prompt"]
    u = n = 0.0
    for e in moments:
        m = {"id": e["id"], "session": e["session"], "hook": "prompt", "cands": e["cands"]}
        for name in policy(m):
            pu, pn = truth[(e["id"], name)]
            u += pu
            n += pn
    return u / len(moments), n / len(moments)


class Offline(unittest.TestCase):
    def test_estimates_find_the_truth(self):
        entries, truth = world()
        moments, cover = off.moments_of(entries)
        self.assertEqual(len(moments), 1200)
        pols = {"logged": off.logged, "top2": off.top_k(2), "top3": off.top_k(3)}
        rep = off.evaluate(moments, pols, n=200)
        for name in ("top2", "top3"):
            tu, tn = true_value(entries, truth, pols[name])
            r = rep[name]
            # top2's lines are always delivered: full support, ips is unbiased.
            # top3's third line has p 0.5: still supported, weighted by 2.
            self.assertEqual(r["point"]["support"], 1.0)
            for est in ("ips", "snips", "dr"):
                lo, hi = r["ci"][est + "_useful"]
                self.assertTrue(lo <= tu <= hi, f"{name} {est} useful {r['ci'][est + '_useful']} misses {tu:.3f}")
                lo, hi = r["ci"][est + "_noise"]
                self.assertTrue(lo <= tn <= hi, f"{name} {est} noise {r['ci'][est + '_noise']} misses {tn:.3f}")
        # Without the weights the third line would count at half its value:
        # the unweighted mean of the labels is off the truth.
        tu, _ = true_value(entries, truth, pols["top3"])
        naive = 0.0
        for e in entries:
            if e.get("hook") == "prompt":
                for c in e["cands"]:
                    if c["name"] in ("f0", "f1", "f2"):
                        lab = next((x["judge"] for x in entries if x.get("hook") == "judge" and x["judge"]["block"] == e["id"] and x["judge"]["fact"] == c["name"]), None)
                        if lab:
                            naive += (1.0 if lab["novel"] == "yes" else 0.0) / 0.4
        naive /= 1200
        self.assertGreater(abs(naive - tu), 0.05)

    def test_support_and_the_model(self):
        entries, truth = world()
        moments, _ = off.moments_of(entries)
        pols = {"logged": off.logged, "top5": off.top_k(5)}
        r = off.evaluate(moments, pols, n=100)["top5"]
        # Two of five lines are never delivered: support 0.6, and ips
        # counts nothing for them, so it falls short of the truth; dr's model
        # fills them in.
        self.assertAlmostEqual(r["point"]["support"], 0.6, delta=0.02)
        tu, _ = true_value(entries, truth, pols["top5"])
        self.assertLess(r["ci"]["ips_useful"][1], tu)
        self.assertLess(abs(r["point"]["dr_useful"] - tu), abs(r["point"]["ips_useful"] - tu))
        self.assertEqual(r["verdict"], "too little support")

    def test_the_rule(self):
        entries, _ = world(sessions=10, per=5)
        moments, _ = off.moments_of(entries)
        r = off.evaluate(moments, {"logged": off.logged, "top3": off.top_k(3)}, n=50)
        self.assertEqual(r["top3"]["verdict"], "too few labels")
        self.assertEqual(r["logged"]["verdict"], "base")

    def test_cli(self):
        entries, _ = world(sessions=5, per=4)
        with tempfile.TemporaryDirectory() as d:
            log = os.path.join(d, "2026-11-01.jsonl")
            with open(log, "w") as fh:
                fh.writelines(json.dumps(e) + "\n" for e in entries)
            out = os.path.join(d, "o.json")
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out, "--policy", "top1"], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(out) as fh:
                rep = json.load(fh)
            self.assertEqual(sorted(rep["policies"]), ["logged", "top1"])
            self.assertEqual(rep["moments"], 20)


class Rules(unittest.TestCase):
    def test_one_judge(self):
        entries, _ = world(sessions=4, per=3)
        for e in entries:
            if e.get("hook") == "judge":
                e["judge"].update(model="m1", prompt="p1")
        first = next(e for e in entries if e.get("hook") == "judge")
        first["judge"]["model"] = "m2"
        with tempfile.TemporaryDirectory() as d:
            log = os.path.join(d, "2026-11-01.jsonl")
            with open(log, "w") as fh:
                fh.writelines(json.dumps(e) + "\n" for e in entries)
            out = os.path.join(d, "o.json")
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("more than one judge", r.stderr)
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out, "--judge", "m1 p1"], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(out) as fh:
                rep = json.load(fh)
            n = sum(1 for e in entries if e.get("hook") == "judge")
            self.assertEqual(rep["coverage"]["labels"], n - 1)

    def test_disagreement_flags(self):
        point = {"support": 1.0, "dr_useful": 0.9, "dr_noise": 0.1}
        dci = {"dr_useful": [0.1, 0.2], "dr_noise": [-0.2, -0.1]}
        ok = {"ips_useful": [0.8, 1.0], "snips_useful": [0.8, 1.0], "ips_noise": [0.0, 0.2], "snips_noise": [0.0, 0.2]}
        self.assertEqual(off.verdict("c", "logged", point, dci, 400, 40, ok), "promote to a live test")
        bad = dict(ok, ips_useful=[0.2, 0.4])
        self.assertTrue(off.verdict("c", "logged", point, dci, 400, 40, bad).startswith("flagged"))
        self.assertEqual(off.verdict("c", "logged", dict(point, support=0.7), dci, 400, 40, ok), "too little support")
        self.assertEqual(off.verdict("c", "logged", point, dict(dci, dr_noise=[-0.1, 0.05]), 400, 40, ok), "no")


if __name__ == "__main__":
    unittest.main()
