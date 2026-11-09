#!/usr/bin/env python3
"""Checks of offline.py on a synthetic delivery log whose true values are
known: a logging policy that delivers some lines at random and logs its
probability before the draw, a judge that labels a sample at varying rates,
and policies whose expected useful and noise lines per moment follow from
the truth.

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

T_MOMENT, T_PASS = "2026-11-01T10:00:00Z", "2026-11-01T12:00:00Z"
DELIVER = (1.0, 1.0, 0.5, 0.0, 0.0)  # the logging policy's p for each rank


def world(seed=3, sessions=60, per=20):
    """Five candidates a moment; a line is useful with a probability that
    falls with its rank, noise with a share of the rest. The judge labels
    each delivered line at 0.2 or 0.6, drawn per line. Returns the log's
    entries and each line's expected (useful, noise)."""
    rng = random.Random(seed)
    entries, truth = [], {}
    for s in range(sessions):
        for i in range(per):
            bid = f"b{s}-{i}"
            cands = []
            for r in range(5):
                share = 1.0 - r * 0.2
                pu = 0.15 + 0.6 * share
                truth[(bid, f"f{r}")] = (pu, (1 - pu) * 0.6)
                useful = rng.random() < pu
                noise = (not useful) and rng.random() < 0.6
                p = DELIVER[r]
                delivered = rng.random() < p
                cands.append({"name": f"f{r}", "score": 10 - r, "share": share, "rank": r, "matched": 3 - min(r, 3), "tier": 0,
                              "prov": "agent", "age_days": 5, "echo": 0.0, "delivered": delivered, "p": p})
                pj = rng.choice((0.2, 0.6))
                if delivered and rng.random() < pj:
                    entries.append({"id": f"j{bid}f{r}", "ts": T_PASS, "hook": "judge",
                                    "judge": {"block": bid, "fact": f"f{r}", "relevant": "no" if noise else "yes",
                                              "novel": "yes" if useful else "no", "confused": "no", "p": pj, "model": "m", "prompt": "p"}})
            entries.append({"id": bid, "ts": T_MOMENT, "hook": "prompt", "session": f"s{s}", "cands": cands,
                            "transcript_path": "/t/" + bid, "transcript": 10})
    entries.append({"id": "pass1", "ts": T_PASS, "hook": "judge-pass", "judge_pass": {"pool": 1, "waiting": 0, "prompt": "p"}})
    return entries, truth


def true_value(entries, truth, policy, supported_only=False):
    moments = [e for e in entries if e.get("hook") == "prompt"]
    u = n = 0.0
    for e in moments:
        m = {"id": e["id"], "session": e["session"], "hook": "prompt", "cands": e["cands"]}
        for c in e["cands"]:
            pi = policy(m, c)
            if supported_only and not c["p"] > 0:
                continue
            pu, pn = truth[(e["id"], c["name"])]
            u += pi * pu
            n += pi * pn
    return u / len(moments), n / len(moments)


def covers(ci, x):
    return ci[0] <= x <= ci[1]


def near(ci, x):
    """x within the interval widened to about three standard errors: the
    tests check many intervals at once, and a 95% one misses 1 in 20."""
    mid, half = (ci[0] + ci[1]) / 2, (ci[1] - ci[0]) / 2
    return abs(x - mid) <= half * 1.6


class Estimates(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.entries, cls.truth = world(sessions=100)
        cls.moments, cls.info = off.moments_of(cls.entries)
        cls.pols = {"logged": off.logged, "share": off.logged_where(lambda c: c["share"] >= 0.7), "top2": off.top_k(2),
                    "top3": off.top_k(3), "top5": off.top_k(5)}
        cls.rep = off.evaluate(cls.moments, cls.pols, n=200)

    def test_supported_policies_find_the_truth(self):
        self.assertEqual(len(self.moments), 2000)
        # The logging policy (p 0.5 on the third line) and the policies over
        # supported lines: every estimator covers the truth.
        for name in ("logged", "share", "top2", "top3"):
            tu, tn = true_value(self.entries, self.truth, self.pols[name])
            r = self.rep[name]
            self.assertEqual(r["point"]["support"], 1.0, name)
            for est in ("ips", "snips", "dr"):
                self.assertTrue(near(r["ci"][est + "_u"], tu), f"{name} {est} useful {r['ci'][est + '_u']} misses {tu:.3f}")
                self.assertTrue(near(r["ci"][est + "_n"], tn), f"{name} {est} noise {r['ci'][est + '_n']} misses {tn:.3f}")

    def test_unsupported_lines(self):
        # top5's last two lines are never delivered: support 0.6. ips counts
        # nothing for them and falls short; snips and dr_sup are the value of
        # the supported lines; dr adds its model's guess for the rest, which
        # rests on no label (so the rule reads no verdict below 0.8).
        r = self.rep["top5"]
        self.assertAlmostEqual(r["point"]["support"], 0.6, places=6)
        tu, _ = true_value(self.entries, self.truth, self.pols["top5"])
        su, _ = true_value(self.entries, self.truth, self.pols["top5"], supported_only=True)
        self.assertLess(r["ci"]["ips_u"][1], tu)
        self.assertTrue(near(r["ci"]["snips_u"], su), (r["ci"]["snips_u"], su))
        self.assertTrue(near(r["ci"]["dr_sup_u"], su), (r["ci"]["dr_sup_u"], su))
        self.assertGreater(r["point"]["dr_u"], r["point"]["dr_sup_u"])
        self.assertEqual(r["verdict"], "too little support")

    def test_the_weights_matter(self):
        # Without the judge's p or the delivery p in the weights the
        # estimate misses the truth.
        tu, _ = true_value(self.entries, self.truth, self.pols["top3"])
        real = off.weight
        try:
            off.weight = lambda c: 1.0 if c.get("label") and c.get("delivered") else 0.0
            r = off.evaluate(self.moments, {"logged": off.logged, "top3": self.pols["top3"]}, n=50)["top3"]
        finally:
            off.weight = real
        self.assertFalse(covers(r["ci"]["ips_u"], tu))


class Population(unittest.TestCase):
    def test_window(self):
        entries, _ = world(sessions=2, per=2)
        moment = next(e for e in entries if e.get("hook") == "prompt")
        early = dict(moment, id="early", ts="2026-10-29T10:00:00Z")  # more than 2 days before the pass
        late = dict(moment, id="late", ts="2026-11-01T11:45:00Z")  # less than 30 minutes before it
        bare = dict(moment, id="bare", transcript_path="")
        unreadable = dict(moment, id="unreadable")
        empty = {k: v for k, v in moment.items() if k != "cands"}
        empty["id"] = "empty"  # a block that showed nothing
        after = dict(moment, id="after", ts="2026-11-02T10:00:00Z")  # in a pass that left lines waiting
        waiting = {"id": "pass2", "ts": "2026-11-02T12:00:00Z", "hook": "judge-pass", "judge_pass": {"pool": 1, "waiting": 3, "prompt": "p"}}
        entries = [e for e in entries if e.get("hook") != "judge-pass"]
        pass1 = {"id": "pass1", "ts": "2026-11-01T12:00:00Z", "hook": "judge-pass",
                 "judge_pass": {"pool": 1, "waiting": 0, "prompt": "p", "drawn": 10, "failed": [moment["id"] + "/f0"], "unread_blocks": ["unreadable"]}}
        moments, info = off.moments_of(entries + [early, late, bare, unreadable, empty, after, waiting, pass1])
        ids = {m["id"] for m in moments}
        self.assertFalse(ids & {"early", "late", "bare", "unreadable", "after"}, ids)
        self.assertIn("empty", ids)
        self.assertEqual(info["left_out"]["no transcript to read"], 2)
        self.assertEqual(info["failed_share"], 0.1)
        # The drawn line whose calls kept failing stays a candidate (a policy
        # ranks it), and counts in no sum.
        got = next(m for m in moments if m["id"] == moment["id"])
        f0 = next(c for c in got["cands"] if c["name"] == "f0")
        self.assertTrue(f0["excluded"])
        top1 = off.top_k(1)
        self.assertEqual([c["name"] for c in got["cands"] if top1(got, c)], ["f0"])
        sums = off.session_sums([got], top1, lambda m, c: (0.5, 0.5))
        self.assertEqual(sums[got["session"]]["pi"], 0.0)
        # A pass that could not read a drawn line's moment: its window goes.
        pass1["judge_pass"]["unread"] = [moment["id"] + "/f1"]
        moments, info = off.moments_of(entries + [pass1])
        self.assertEqual(moments, [])

    def test_a_line_that_waits(self):
        # Lines drawn in window 1 wait into pass 2, which names one unread
        # and one failed: window 1 is left out (unread), and the failed line
        # is attributed to its own window.
        entries, _ = world(sessions=2, per=2)
        entries = [e for e in entries if e.get("hook") != "judge-pass"]
        moments_ = [e for e in entries if e.get("hook") == "prompt"]
        a, b = moments_[0], moments_[1]
        b["ts"] = "2026-11-01T13:00:00Z"
        w1 = {"id": "w1", "ts": "2026-11-01T12:00:00Z", "hook": "judge-pass",
              "judge_pass": {"waiting": 2, "prompt": "p", "drawn": 4, "from": "2026-11-01T09:00:00Z", "to": "2026-11-01T11:30:00Z"}}
        w2 = {"id": "w2", "ts": "2026-11-01T14:00:00Z", "hook": "judge-pass",
              "judge_pass": {"waiting": 0, "prompt": "p", "drawn": 4, "from": "2026-11-01T11:30:00Z", "to": "2026-11-01T13:30:00Z",
                             "unread": [a["id"] + "/f0"], "failed": []}}
        got, _ = off.moments_of(entries + [w1, w2])
        ids = {m["id"] for m in got}
        self.assertNotIn(a["id"], ids)  # its window holds an unread line
        self.assertIn(b["id"], ids)
        # A failed line logged by a later pass whose own window is not kept
        # (its moment holds another model's label) is excluded in the window
        # its moment lies in, and counts in that window's failed share.
        w2["judge_pass"].update(unread=[], failed=[a["id"] + "/f1"])
        other = {"id": "jx", "ts": "2026-11-01T14:00:00Z", "hook": "judge",
                 "judge": {"block": b["id"], "fact": "f0", "relevant": "yes", "novel": "yes", "p": 0.5, "model": "m2", "prompt": "p"}}
        w3 = {"id": "w3", "ts": "2026-11-01T16:00:00Z", "hook": "judge-pass",
              "judge_pass": {"waiting": 0, "prompt": "p", "drawn": 2, "from": "2026-11-01T13:30:00Z", "to": "2026-11-01T15:30:00Z"}}
        got, info = off.moments_of(entries + [w1, w2, w3, other], ("m", "p"))
        ids = {x["id"] for x in got}
        self.assertNotIn(b["id"], ids)  # w2's window holds m2's label: not m's
        m = next(x for x in got if x["id"] == a["id"])
        self.assertTrue(next(c for c in m["cands"] if c["name"] == "f1")["excluded"])
        # Kept: w1 (4 drawn) and w3 (2 drawn); the one failed line lies in w1.
        self.assertEqual(info["failed_share"], round(1 / 6, 4))

    def test_another_judges_pool(self):
        # Passes of judges p, q, p: a moment in q's pool is not p's, though
        # p's next pass comes after it.
        entries, _ = world(sessions=1, per=1)
        entries = [e for e in entries if e.get("hook") != "judge-pass"]
        moment = next(e for e in entries if e.get("hook") == "prompt")
        mid = dict(moment, id="mid", ts="2026-11-01T13:00:00Z")
        p1 = {"id": "p1", "ts": "2026-11-01T12:00:00Z", "hook": "judge-pass", "judge_pass": {"waiting": 0, "prompt": "p"}}
        q = {"id": "q", "ts": "2026-11-01T14:00:00Z", "hook": "judge-pass", "judge_pass": {"waiting": 0, "prompt": "q"}}
        p2 = {"id": "p2", "ts": "2026-11-01T16:00:00Z", "hook": "judge-pass", "judge_pass": {"waiting": 0, "prompt": "p"}}
        moments, _ = off.moments_of(entries + [mid, p1, q, p2], ("m", "p"))
        self.assertEqual({m["id"] for m in moments}, {moment["id"]})
        # Logged windows win over the passes' times.
        q["judge_pass"].update({"from": "2026-11-01T11:30:00Z", "to": "2026-11-01T12:30:00Z"})
        p2["judge_pass"].update({"from": "2026-11-01T12:30:00Z", "to": "2026-11-01T15:30:00Z"})
        moments, _ = off.moments_of(entries + [mid, p1, q, p2], ("m", "p"))
        self.assertEqual({m["id"] for m in moments}, {moment["id"], "mid"})

    def test_another_model_under_one_prompt(self):
        # The model behind an alias changed under one prompt: a window whose
        # labels are another model's is not the chosen judge's, and one with
        # no labels is when no other model answered its pass.
        entries, _ = world(sessions=2, per=1)
        entries = [e for e in entries if e.get("hook") != "judge-pass"]
        for e in entries:
            if e.get("hook") == "judge":
                e["judge"]["model"] = "old"
        old = {"id": "w1", "ts": "2026-11-01T12:00:00Z", "hook": "judge-pass",
               "judge_pass": {"waiting": 0, "prompt": "p", "answered": ["old"], "from": "2026-11-01T09:00:00Z", "to": "2026-11-01T11:30:00Z"}}
        later = dict(next(e for e in entries if e.get("hook") == "prompt"), id="later", ts="2026-11-01T13:00:00Z")
        new = {"id": "w2", "ts": "2026-11-01T14:00:00Z", "hook": "judge-pass",
               "judge_pass": {"waiting": 0, "prompt": "p", "answered": ["new"], "from": "2026-11-01T11:30:00Z", "to": "2026-11-01T13:30:00Z"}}
        moments, _ = off.moments_of(entries + [old, later, new], ("new", "p"))
        self.assertEqual({m["id"] for m in moments}, {"later"})
        moments, _ = off.moments_of(entries + [old, later, new], ("old", "p"))
        self.assertNotIn("later", {m["id"] for m in moments})
        # With no --judge, the one judge the labels hold.
        moments, info = off.moments_of(entries + [old, later, new])
        self.assertEqual(info["judge"], "old p")
        self.assertNotIn("later", {m["id"] for m in moments})

    def test_a_silent_pass_of_another_judge(self):
        # A pass of another judge's model that no model answered (every call
        # failed) left its window without labels: the window is not the
        # chosen judge's, whose population would hold moments it never read.
        entries, _ = world(sessions=2, per=1)
        entries = [e for e in entries if e.get("hook") != "judge-pass"]
        for e in entries:
            if e.get("hook") == "judge":
                e["judge"]["model"] = "old"
        prompt = next(e for e in entries if e.get("hook") == "prompt")
        silent = dict(prompt, id="silent", ts="2026-11-01T13:00:00Z")
        quiet = {"id": "w1", "ts": "2026-11-01T14:00:00Z", "hook": "judge-pass",
                 "judge_pass": {"waiting": 0, "model": "old", "prompt": "p", "from": "2026-11-01T11:30:00Z", "to": "2026-11-01T13:30:00Z"}}
        later = dict(prompt, id="later", ts="2026-11-01T15:00:00Z")
        new = {"id": "w2", "ts": "2026-11-01T16:00:00Z", "hook": "judge-pass",
               "judge_pass": {"waiting": 0, "model": "new", "prompt": "p", "answered": ["new"], "from": "2026-11-01T13:30:00Z", "to": "2026-11-01T15:30:00Z"}}
        moments, _ = off.moments_of(entries + [silent, quiet, later, new], ("new", "p"))
        self.assertEqual({m["id"] for m in moments}, {"later"})
        moments, _ = off.moments_of(entries + [silent, quiet, later, new], ("old", "p"))
        self.assertIn("silent", {m["id"] for m in moments})
        self.assertNotIn("later", {m["id"] for m in moments})
        # The same when the other model left no label at all in the log.
        bare = [e for e in entries if e.get("hook") != "judge"]
        moments, _ = off.moments_of(bare + [silent, quiet, later, new], ("new", "p"))
        self.assertEqual({m["id"] for m in moments} & {"silent", "later"}, {"later"})
        # A pass logs the model as asked: a name the judge's model answered
        # to, and nothing else did, is the judge's.
        asked = {"id": "w3", "ts": "2026-11-01T18:00:00Z", "hook": "judge-pass",
                 "judge_pass": {"waiting": 0, "model": "alias", "prompt": "p", "answered": ["new"], "from": "2026-11-01T15:30:00Z", "to": "2026-11-01T17:30:00Z"}}
        mute = dict(prompt, id="mute", ts="2026-11-01T19:00:00Z")
        still = {"id": "w4", "ts": "2026-11-01T20:00:00Z", "hook": "judge-pass",
                 "judge_pass": {"waiting": 0, "model": "alias", "prompt": "p", "from": "2026-11-01T17:30:00Z", "to": "2026-11-01T19:30:00Z"}}
        moments, _ = off.moments_of(bare + [silent, quiet, later, new, asked, mute, still], ("new", "p"))
        self.assertEqual({m["id"] for m in moments} & {"silent", "later", "mute"}, {"later", "mute"})

    def test_cross_fitting(self):
        # Sessions of one half hold only useful labels, the other only
        # useless ones: each session's lines are predicted from the other half.
        moments = []
        for s in range(4):
            useful = s % 2 == 0
            cands = [{"name": "f0", "share": 1.0, "matched": 3, "prov": "agent", "delivered": True, "p": 1.0,
                      "label": {"relevant": "yes", "novel": "yes" if useful else "no", "p": 1.0}}]
            moments.append({"id": f"b{s}", "session": f"s{s}", "hook": "prompt", "cands": cands})
        model = off.cross_fit(moments, [m["session"] for m in moments])
        self.assertLess(model(moments[0], moments[0]["cands"][0])[0], 0.5)
        self.assertGreater(model(moments[1], moments[1]["cands"][0])[0], 0.5)


class Robust(unittest.TestCase):
    def test_a_wrong_model_is_corrected(self):
        # One stratum for every line: the model is the overall mean, wrong
        # for each rank. The model alone misses the truth; dr's residual
        # brings it back.
        entries, truth = world(sessions=100)
        moments, _ = off.moments_of(entries)
        pol = off.logged_where(lambda c: c["share"] >= 0.7)
        tu, _ = true_value(entries, truth, pol)
        real = off.cell
        try:
            off.cell = lambda m, c: ()
            r = off.evaluate(moments, {"logged": off.logged, "share": pol}, n=100)["share"]
            model = off.cross_fit(moments, [m["session"] for m in moments])
            only = sum(pol(m, c) * model(m, c)[0] for m in moments for c in m["cands"]) / len(moments)
        finally:
            off.cell = real
        self.assertGreater(abs(only - tu), 0.1)
        self.assertTrue(near(r["ci"]["dr_u"], tu), (r["ci"]["dr_u"], tu))

    def test_net(self):
        v = off.values([dict(dict.fromkeys(off.SUMS, 0.0), moments=2, pi=2, sup=2, w=2, ips_u=3, ips_n=1, dr_u=3, dr_n=1, drs_u=3, drs_n=1)])
        self.assertEqual(v["dr_net"], (3 - off.LAMBDA * 1) / 2)
        self.assertEqual(off.LAMBDA, 1.0)


class Refit(unittest.TestCase):
    def test_the_model_is_refit_in_every_sample(self):
        entries, _ = world(sessions=6, per=3)
        moments, _ = off.moments_of(entries)
        calls = []
        real = off.cross_fit
        try:
            off.cross_fit = lambda m, s: calls.append(len(s)) or real(m, s)
            off.evaluate(moments, {"logged": off.logged}, n=7)
        finally:
            off.cross_fit = real
        self.assertGreaterEqual(len(calls), 1 + 7)


class Rules(unittest.TestCase):
    def test_verdicts(self):
        point = {"support": 1.0}
        diff = {"dr_sup_net": 0.1}
        dci = {"dr_net": [0.05, 0.2], "ips_net": [0.0, 0.2], "snips_net": [0.0, 0.2]}
        self.assertEqual(off.verdict("c", "logged", point, diff, dci, (400, 40)), "promote to a live test")
        self.assertEqual(off.verdict("c", "logged", point, diff, dci, (400, 20)), "too few labels")
        self.assertEqual(off.verdict("c", "logged", point, diff, dci, (200, 40)), "too few labels")
        self.assertEqual(off.verdict("c", "logged", {"support": 0.7}, diff, dci, (400, 40)), "too little support")
        self.assertTrue(off.verdict("c", "logged", point, {"dr_sup_net": 0.5}, dci, (400, 40)).startswith("flagged"))
        self.assertEqual(off.verdict("c", "logged", point, diff, dict(dci, dr_net=[-0.01, 0.2]), (400, 40)), "no")
        self.assertEqual(off.verdict("logged", "logged", point, diff, dci, (0, 0)), "base")

    def test_label_sessions_are_the_candidates(self):
        entries, _ = world(sessions=8, per=3)
        moments, _ = off.moments_of(entries)
        only_s0 = lambda m, c: 1.0 if m["session"] == "s0" and c["name"] == "f0" else 0.0
        r = off.evaluate(moments, {"logged": off.logged, "s0": only_s0}, n=20)["s0"]
        self.assertEqual(r["label_sessions"], 1)

    def test_one_judge(self):
        entries, _ = world(sessions=4, per=3)
        first = next(e for e in entries if e.get("hook") == "judge")
        first["judge"]["model"] = "m2"
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "2026-11-01.jsonl"), "w") as fh:
                fh.writelines(json.dumps(e) + "\n" for e in entries)
            out = os.path.join(d, "o.json")
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out, "--boot", "20"], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("more than one judge", r.stderr)
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out, "--boot", "20", "--judge", "m p", "--policy", "top1"], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(out) as fh:
                rep = json.load(fh)
            self.assertEqual(os.stat(out).st_mode & 0o777, 0o600)
            n = sum(1 for e in entries if e.get("hook") == "judge")
            self.assertEqual(rep["coverage"]["labels"], n - 1)
            # The one pass's window holds another judge's label: it is not
            # m's, and nothing is read.
            self.assertEqual(rep["moments"], 0)
        # A log of one judge: the CLI reads it end to end.
        entries, _ = world(sessions=4, per=3)
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "2026-11-01.jsonl"), "w") as fh:
                fh.writelines(json.dumps(e) + "\n" for e in entries)
            out = os.path.join(d, "o.json")
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "offline.py"), "--log", d, "--out", out, "--boot", "20", "--policy", "top1"], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(out) as fh:
                rep = json.load(fh)
            self.assertEqual(sorted(rep["policies"]), ["logged", "top1"])
            self.assertGreater(rep["moments"], 0)


if __name__ == "__main__":
    unittest.main()
