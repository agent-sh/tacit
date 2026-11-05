#!/usr/bin/env python3
"""End-to-end check of signals.py on a synthetic store, case file and
transcript.

  python3 -I eval/delivery/test_signals.py
"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def line(**d):
    return json.dumps(d) + "\n"


def fact(root, scope, name, desc):
    os.makedirs(os.path.join(root, scope), exist_ok=True)
    with open(os.path.join(root, scope, name + ".md"), "w") as fh:
        fh.write(f"---\nname: {name}\ndescription: {desc}\ntype: design_decision\n---\nbody\n")


class Signals(unittest.TestCase):
    def test_signals_and_agreement(self):
        with tempfile.TemporaryDirectory() as d:
            store, projects = os.path.join(d, "store"), os.path.join(d, "projects", "-demo")
            os.makedirs(projects)
            fact(store, "projects/demo", "queue-retry-backoff", "Queue retries back off exponentially with jitter in the worker")
            fact(store, "projects/demo", "pool-lock-order", "Take the shard lock before the slot lock in the scheduler pool")
            fact(store, "projects/demo", "unrelated-note", "The office printer sits on the third floor near the kitchen")
            t0, t1, t2, t3 = "2026-10-01T10:00:00Z", "2026-10-01T10:05:00Z", "2026-10-01T10:10:00Z", "2026-10-01T10:20:00Z"
            with open(os.path.join(projects, "s1.jsonl"), "w") as fh:
                fh.write(line(type="user", timestamp="2026-10-01T09:59:00Z", userType="external", message={"role": "user", "content": "fix the retry loop"}))
                # The agent carries the backoff rule into a subagent's brief.
                fh.write(line(type="assistant", timestamp=t1, message={"role": "assistant", "content": [
                    {"type": "tool_use", "name": "Agent", "input": {"prompt": "Note: queue retries back off exponentially with jitter in the worker; keep that."}}]}))
                # A compaction keeps the lock order by name and drops the rest.
                fh.write(line(type="user", timestamp=t2, isCompactSummary=True, message={"role": "user", "content": "Summary: we read pool-lock-order and changed the loop."}))
                # After the drop, the agent opens the printer note again.
                fh.write(line(type="assistant", timestamp=t3, message={"role": "assistant", "content": [
                    {"type": "tool_use", "name": "mcp__tacit__tacit_fact", "input": {"name": "unrelated-note"}}]}))
            cases = os.path.join(d, "cases.jsonl")
            with open(cases, "w") as fh:
                fh.write(line(id="c1", session="s1", ts=t0, event="prompt",
                              labels={"queue-retry-backoff": "R", "pool-lock-order": "R", "unrelated-note": "N"}))
            out = os.path.join(d, "out.json")
            subprocess.run([sys.executable, "-I", os.path.join(HERE, "signals.py"), "--cases", cases, "--store", store,
                            "--projects", os.path.join(d, "projects"), "--out", out], check=True, capture_output=True, text=True)
            res = json.load(open(out))
            by = {r["fact"]: r for r in res["rows"]}
            self.assertTrue(by["queue-retry-backoff"]["carried_forward"])
            self.assertTrue(by["pool-lock-order"]["summary_kept"])
            self.assertFalse(by["pool-lock-order"]["summary_dropped"])
            self.assertTrue(by["unrelated-note"]["summary_dropped"])
            self.assertTrue(by["unrelated-note"]["opened"] and by["unrelated-note"]["needed_again"])
            self.assertFalse(by["unrelated-note"]["carried_forward"])
            rep = res["report"]["signals"]
            self.assertEqual(rep["carried_forward"]["fires"], 1)
            self.assertEqual(rep["summary_kept"]["R"], 1)
            self.assertEqual(oct(os.stat(out).st_mode & 0o777), "0o600")


if __name__ == "__main__":
    unittest.main()
