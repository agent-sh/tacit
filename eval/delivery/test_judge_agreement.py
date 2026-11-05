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


# A fake delivery-judge: says relevant and novel for a line whose
# description says useful, and echoes the moment it got.
FAKE = """#!/usr/bin/env python3
import json, sys
for l in sys.stdin:
    m = json.loads(l)
    w = "yes" if "useful" in m["line"] else "no"
    print(json.dumps({"block": m["block"], "fact": m["fact"], "relevant": w, "confused": "no", "novel": w, "model": "m", "prompt": "p", "moment": m}))
"""


class Agreement(unittest.TestCase):
    def test_moments_built_and_measured(self):
        with tempfile.TemporaryDirectory() as d:
            store = os.path.join(d, "store")
            os.makedirs(os.path.join(store, "projects"))
            subprocess.run(["git", "init", "-q", store], check=True)
            for name, desc in (("alpha-fact", "a useful fact"), ("beta-fact", "an idle fact")):
                with open(os.path.join(store, "projects", name + ".md"), "w") as fh:
                    fh.write(f"---\nname: {name}\ndescription: {desc}\ntype: design_decision\nsource:\n  kind: observed\n---\nbody\n")
            proj = os.path.join(d, "projects", "p")
            os.makedirs(proj)
            lines = [
                line(type="user", userType="external", timestamp="2026-10-01T09:00:00Z", message={"content": "earlier"}),
                line(type="user", userType="external", timestamp="2026-10-01T10:00:05Z", message={"content": "do the thing"}),
                line(type="assistant", timestamp="2026-10-01T10:00:10Z", message={"content": [{"type": "tool_use", "name": "Bash", "input": {"command": "ls"}}]}),
                line(type="user", timestamp="2026-10-01T10:00:12Z", message={"content": [{"type": "tool_result", "content": "x"}]}),
            ]
            with open(os.path.join(proj, "s1.jsonl"), "w") as fh:
                fh.writelines(lines)
            prompt_off = len(lines[0].encode())
            push_off = len((lines[0] + lines[1] + lines[2]).encode())
            cases = os.path.join(d, "cases.jsonl")
            with open(cases, "w") as fh:
                fh.write(line(id="c1", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="do the thing",
                              delivered=["alpha-fact", "beta-fact"], labels={"alpha-fact": "R", "beta-fact": "N"}))
                # The same block again under another id: one row.
                fh.write(line(id="c2", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="do the thing",
                              delivered=["alpha-fact"], labels={"alpha-fact": "R"}))
                fh.write(line(id="c3", event="prompt", ts="2026-10-01T10:00:05Z", session="s1", prompt="x", sidechain=True,
                              delivered=["alpha-fact"], labels={"alpha-fact": "R"}))
                fh.write(line(id="c4", event="pre-tool", ts="2026-10-01T10:00:11Z", session="s1", prompt="", tool_name="Bash",
                              tool_input={"command": "ls"}, delivered=["beta-fact"], labels={"beta-fact": "W"}))
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
            self.assertEqual(rep["lines"], 3)
            self.assertEqual(rep["skipped"], {"duplicate": 1, "sidechain": 1})
            self.assertEqual(rep["relevant"]["verdict"], "too few lines")
            self.assertEqual(rep["judges"], {"m p": 3})
            with open(out + ".verdicts.jsonl") as fh:
                ms = {(v["block"], v["fact"]): v["moment"] for v in map(json.loads, fh)}
            self.assertEqual(ms[("c1", "alpha-fact")]["offset"], prompt_off)  # the prompt's own turn
            self.assertEqual(ms[("c1", "alpha-fact")]["hook"], "prompt")
            self.assertEqual(ms[("c1", "alpha-fact")]["line"], "alpha-fact: a useful fact")
            self.assertEqual(ms[("c4", "beta-fact")], {"block": "c4", "fact": "beta-fact", "hook": "push", "tool": "Bash",
                                                        "transcript": os.path.join(proj, "s1.jsonl"), "offset": push_off, "line": "beta-fact: an idle fact"})

    def rows(self, sessions=60):
        rows, verdicts = [], []
        for s in range(sessions):
            for i in range(6):
                g = "RWN"[i % 3]
                rows.append({"session": f"s{s}", "fact": f"f{i}", "grade": g})
                yes = "yes" if g in "RW" else "no"
                verdicts.append({"fact": f"f{i}", "relevant": yes if i != 5 or s % 4 else "unsure", "novel": "yes" if g == "R" else "no", "confused": "no"})
        return rows, verdicts

    def test_the_rule(self):
        rows, verdicts = self.rows()
        m = ja.measure(rows, verdicts, "relevant", ("R", "W"))
        self.assertEqual(m["verdict"], "passes")
        self.assertEqual(m["unsure"], 15)
        self.assertEqual(ja.measure(rows, verdicts, "useful", ("R",))["kappa"], 1.0)
        # Too many unsure answers fail it, whatever the agreement.
        for v in verdicts:
            if v["fact"] == "f0":
                v["relevant"] = "unsure"
        self.assertEqual(ja.measure(rows, verdicts, "relevant", ("R", "W"))["verdict"], "fails")

    def test_each_bar(self):
        # Too few sessions, with lines enough.
        rows, verdicts = self.rows(sessions=29)
        rows, verdicts = rows * 2, verdicts * 2
        self.assertEqual(ja.measure(rows, verdicts, "relevant", ("R", "W"))["verdict"], "too few lines")
        # Agreement below the bar: flip relevant on 1 in 3 sessions.
        rows, verdicts = self.rows()
        for k, v in enumerate(verdicts):
            if (k // 6) % 3 == 0 and v["relevant"] != "unsure":
                v["relevant"] = "no" if v["relevant"] == "yes" else "yes"
        m = ja.measure(rows, verdicts, "relevant", ("R", "W"))
        self.assertLess(m["kappa"], ja.KAPPA)
        self.assertEqual(m["verdict"], "fails")
        # The interval's low end below its bar fails it.
        old = ja.KAPPA_LOW
        try:
            ja.KAPPA_LOW = 1.01  # above any interval
            rows, verdicts = self.rows()
            self.assertEqual(ja.measure(rows, verdicts, "relevant", ("R", "W"))["verdict"], "fails")
        finally:
            ja.KAPPA_LOW = old


if __name__ == "__main__":
    unittest.main()
