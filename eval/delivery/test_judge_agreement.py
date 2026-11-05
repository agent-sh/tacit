#!/usr/bin/env python3
"""Checks of judge_agreement.py on a synthetic store, case file and
transcript, with a fake `tacit delivery-judge`, and of its rule on synthetic
verdicts.

  python3 -I eval/delivery/test_judge_agreement.py
"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("judge_agreement", os.path.join(HERE, "judge_agreement.py"))
ja = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ja)


def line(**d):
    return json.dumps(d) + "\n"


FAKE = """#!/usr/bin/env python3
import json, sys
for l in sys.stdin:
    c = json.loads(l)
    good = "useful" in c["line"]
    w = "yes" if good else "no"
    print(json.dumps({"block": c["block"], "fact": c["fact"], "relevant": w, "confused": "no", "novel": w, "next": len(c["next"]), "req": c["request"]}))
"""


class Agreement(unittest.TestCase):
    def test_cases_built_and_measured(self):
        with tempfile.TemporaryDirectory() as d:
            store = os.path.join(d, "store")
            os.makedirs(os.path.join(store, "projects"))
            subprocess.run(["git", "init", "-q", store], check=True)
            for name, desc in (("alpha-fact", "a useful fact"), ("beta-fact", "an idle fact")):
                with open(os.path.join(store, "projects", name + ".md"), "w") as fh:
                    fh.write(f"---\nname: {name}\ndescription: {desc}\ntype: design_decision\nsource:\n  kind: observed\n---\nbody\n")
            proj = os.path.join(d, "projects", "p")
            os.makedirs(proj)
            with open(os.path.join(proj, "s1.jsonl"), "w") as fh:
                fh.write(line(type="user", userType="external", timestamp="2026-10-01T10:00:00Z", message={"content": "before the moment"}))
                fh.write(line(type="assistant", timestamp="2026-10-01T10:00:10Z", message={"content": [
                    {"type": "text", "text": "working"}, {"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}]}))
                fh.write(line(type="user", timestamp="2026-10-01T10:00:11Z", message={"content": [{"type": "tool_result", "content": "x" * 2000}]}))
                fh.write(line(type="assistant", isSidechain=True, timestamp="2026-10-01T10:00:12Z", message={"content": [{"type": "text", "text": "side"}]}))
            cases = os.path.join(d, "cases.jsonl")
            with open(cases, "w") as fh:
                fh.write(line(id="c1", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="do the thing",
                              delivered=["alpha-fact", "beta-fact"], labels={"alpha-fact": "R", "beta-fact": "N"}))
                # The same block again under another id: one row each.
                fh.write(line(id="c2", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="do the thing",
                              delivered=["alpha-fact"], labels={"alpha-fact": "R"}))
                fh.write(line(id="c3", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="x", sidechain=True,
                              delivered=["alpha-fact"], labels={"alpha-fact": "R"}))
            fake = os.path.join(d, "tacit")
            with open(fake, "w") as fh:
                fh.write(FAKE)
            os.chmod(fake, 0o755)
            out = os.path.join(d, "judge.json")
            r = subprocess.run([sys.executable, "-I", os.path.join(HERE, "judge_agreement.py"), "--cases", cases, "--store", store,
                                "--projects", os.path.join(d, "projects"), "--tacit", fake, "--out", out], capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            with open(out) as fh:
                rep = json.load(fh)
            self.assertEqual(rep["lines"], 2)
            self.assertEqual(rep["skipped"], {"duplicate": 1, "sidechain": 1})
            self.assertEqual(rep["relevant"]["kappa"], 1.0)
            self.assertEqual(rep["relevant"]["verdict"], "too few lines")
            with open(out + ".verdicts.jsonl") as fh:
                v = [json.loads(x) for x in fh]
            # The moment's next events: reply, call and clipped result, no sidechain.
            self.assertEqual(v[0]["next"], 3)
            self.assertEqual(v[0]["req"], "do the thing")

    def test_the_rule(self):
        rows, verdicts = [], []
        for s in range(60):
            for i in range(6):
                g = "RWN"[i % 3]
                rows.append({"session": f"s{s}", "fact": f"f{i}", "grade": g})
                yes = "yes" if g in "RW" else "no"
                verdicts.append({"fact": f"f{i}", "relevant": yes if i != 5 or s % 4 else "unsure", "novel": "yes" if g == "R" else "no", "confused": "no"})
        m = ja.measure(rows, verdicts, "relevant", ("R", "W"))
        self.assertEqual(m["verdict"], "passes")
        self.assertEqual(m["unsure"], 15)
        u = ja.measure(rows, verdicts, "useful", ("R",))
        self.assertEqual(u["kappa"], 1.0)
        # Too many unsure answers fail it, whatever the agreement.
        for v in verdicts:
            if v["fact"] == "f0":
                v["relevant"] = "unsure"
        self.assertEqual(ja.measure(rows, verdicts, "relevant", ("R", "W"))["verdict"], "fails")
        # A judge that answers at random agrees by chance only.
        flip = [{**v, "relevant": "yes" if (k * 7) % 3 else "no"} for k, v in enumerate(verdicts)]
        self.assertEqual(ja.measure(rows, flip, "relevant", ("R", "W"))["verdict"], "fails")


if __name__ == "__main__":
    unittest.main()
