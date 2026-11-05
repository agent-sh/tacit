#!/usr/bin/env python3
"""Checks of signals.py on a synthetic store, case file and transcript, and
of its decision rule on synthetic rows.

  python3 -I eval/delivery/test_signals.py
"""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("signals", os.path.join(HERE, "signals.py"))
signals = importlib.util.module_from_spec(spec)
spec.loader.exec_module(signals)


def line(**d):
    return json.dumps(d) + "\n"


def fact(root, scope, name, desc, ruling=False):
    os.makedirs(os.path.join(root, scope), exist_ok=True)
    src = "source:\n  kind: user\n" if ruling else "source:\n  kind: observed\n"
    with open(os.path.join(root, scope, name + ".md"), "w") as fh:
        fh.write(f"---\nname: {name}\ndescription: {desc}\ntype: design_decision\n{src}---\nbody\n")


def assistant(ts, *blocks):
    return line(type="assistant", timestamp=ts, message={"role": "assistant", "content": list(blocks)})


def tool(tool_name, **inp):
    return {"type": "tool_use", "name": tool_name, "input": inp}


class Signals(unittest.TestCase):
    def run_signals(self, d, cases):
        store = os.path.join(d, "store")
        subprocess.run(["git", "init", "-q", store], check=True)
        out = os.path.join(d, "out.json")
        path = os.path.join(d, "cases.jsonl")
        with open(path, "w") as fh:
            for c in cases:
                fh.write(line(**c))
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "signals.py"), "--cases", path, "--store", store,
                        "--projects", os.path.join(d, "projects"), "--out", out], check=True, capture_output=True, text=True)
        self.assertEqual(oct(os.stat(out).st_mode & 0o777), "0o600")
        with open(out) as fh:
            return json.load(fh)

    def test_signals_in_their_window(self):
        with tempfile.TemporaryDirectory() as d:
            store, projects = os.path.join(d, "store"), os.path.join(d, "projects", "-demo")
            os.makedirs(projects)
            fact(store, "projects/demo", "queue-retry-backoff", "Queue retries back off exponentially with jitter in the worker")
            fact(store, "projects/demo", "pool-lock-order", "Take the shard lock before the slot lock in the scheduler pool")
            fact(store, "projects/demo", "printer-floor", "The office printer sits on the third floor near the kitchen")
            fact(store, "projects/demo", "signoff-rule", "Every commit carries a signed-off-by trailer line", ruling=True)
            t = lambda m: f"2026-10-01T10:{m:02d}:00Z"
            with open(os.path.join(projects, "s1.jsonl"), "w") as fh:
                # At the moment itself: no signal (the window starts after it).
                fh.write(assistant(t(0), {"type": "text", "text": "Take the shard lock before the slot lock in the scheduler pool"}))
                fh.write(assistant(t(5), tool("Agent", prompt="Note: queue retries back off exponentially with jitter in the worker.")))
                # The rule's words without its name: a ruling is held by its name only.
                fh.write(assistant(t(6), {"type": "text", "text": "Every commit carries a signed-off-by trailer line."}))
                fh.write(line(type="user", timestamp=t(10), isCompactSummary=True, message={"role": "user", "content": "Summary: we read pool-lock-order and changed the loop."}))
                fh.write(assistant(t(12), tool("mcp__tacit__tacit_search", query="office printer floor")))
                fh.write(line(type="attachment", timestamp=t(13), attachment={"type": "queued_command", "commandMode": "task-notification",
                              "prompt": "<task-notification> the office printer sits on the third floor near the kitchen </task-notification>"}))
                fh.write(line(type="user", timestamp=t(14), userType="external", message={"role": "user", "content": "the office printer sits on the third floor near the kitchen, remember"}))
                # The backoff note, dropped by the summary, opened again.
                fh.write(assistant(t(16), tool("mcp__tacit__tacit_fact", name="queue-retry-backoff")))
                # The start after a later compaction delivers the printer note
                # again: its window ends there.
                fh.write(line(type="attachment", timestamp=t(20), attachment={"type": "hook_success", "hookEvent": "SessionStart",
                              "content": "# tacit: facts for demo, again after the compaction\n- printer-floor: agent note (2026-10-01): The office printer\n"}))
                fh.write(assistant(t(30), tool("mcp__tacit__tacit_fact", name="printer-floor")))
            with open(os.path.join(projects, "s2.jsonl"), "w") as fh:
                fh.write(line(type="user", timestamp=t(14), userType="external", message={"role": "user", "content": "the office printer sits on the third floor near the kitchen"}))
            res = self.run_signals(d, [
                {"id": "c1", "session": "s1", "ts": t(0), "event": "prompt", "origin": "typed", "commit": "HEAD",
                 "delivered": ["queue-retry-backoff", "pool-lock-order", "printer-floor", "signoff-rule"],
                 "labels": {"queue-retry-backoff": "R", "pool-lock-order": "R", "printer-floor": "N", "signoff-rule": "R"}},
                {"id": "c2", "session": "s2", "ts": t(0), "event": "prompt", "origin": "headless", "commit": "HEAD", "delivered": ["printer-floor"], "labels": {"printer-floor": "N"}},
                {"id": "c4", "session": "s1", "ts": t(14), "event": "prompt", "origin": "typed", "commit": "HEAD", "delivered": []},
                # The same block in a second label set: one row; a fact
                # graded two ways there is left out.
                {"id": "c5", "session": "s1", "ts": t(0), "event": "prompt", "origin": "typed", "commit": "HEAD",
                 "delivered": ["queue-retry-backoff", "pool-lock-order"], "labels": {"queue-retry-backoff": "R", "pool-lock-order": "N"}},
                {"id": "c3", "session": "s1", "ts": t(1), "event": "prompt", "sidechain": True, "commit": "HEAD", "labels": {"pool-lock-order": "W"}},
            ])
            by = {r["fact"]: r for r in res["rows"] if r["session"] == "s1"}
            other = [r for r in res["rows"] if r["session"] == "s2"][0]
            self.assertIsNone(other["owner_restated"], "a session of no owner case is not the owner's")
            self.assertTrue(by["queue-retry-backoff"]["carried_forward"])
            self.assertTrue(by["queue-retry-backoff"]["needed_again"], "opened after the drop")

            self.assertFalse(by["signoff-rule"]["carried_forward"], "a ruling is held by its name only")
            p = by["printer-floor"]
            self.assertTrue(p["needed_again"], "the search after the drop")
            self.assertFalse(p["opened"], "the open after its next delivery is outside the window")
            self.assertTrue(p["window_ended_early"])
            self.assertTrue(p["owner_restated"])
            self.assertEqual(res["report"]["skipped"], {"sidechain": 1, "duplicate": 1, "graded two ways": 2})
            self.assertNotIn("pool-lock-order", by, "graded two ways")

    def test_the_decision_rule(self):
        base = {"R": 0.2, "W": 0.4, "N": 0.4}
        mk = lambda s, f, g: {"session": s, "fact": f, "grade": g, "sig": True}
        strong = [mk(f"s{i}", f"f{i}", "R" if i % 10 < 8 else "N") for i in range(30)]
        weak = [mk(f"s{i}", f"f{i}", "R" if i % 10 < 3 else "N") for i in range(30)]
        few = [mk("s1", f"f{i}", "R") for i in range(5)]
        one_session = [mk("s1", f"f{i}", "R") for i in range(30)]
        self.assertEqual(signals.verdict(strong, "sig", "R", base)["verdict"], "passes")
        self.assertEqual(signals.verdict(weak, "sig", "R", base)["verdict"], "fails")
        self.assertEqual(signals.verdict(few, "sig", "R", base)["verdict"], "unmeasured")
        self.assertEqual(signals.verdict(one_session, "sig", "R", base)["verdict"], "unmeasured", "units from too few sessions")



class Rulings(unittest.TestCase):
    def test_rulings_and_descriptions_at_the_case_commit(self):
        """In a store with the rulings audit a kind-user fact is a ruling only
        with its evidence; a description is read as it was at the case's
        commit."""
        with tempfile.TemporaryDirectory() as d:
            store = os.path.join(d, "store")
            os.makedirs(os.path.join(store, "review"))
            subprocess.run(["git", "init", "-q", store], check=True)
            env = dict(os.environ, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@t", GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@t", GIT_CONFIG_GLOBAL="/dev/null")
            def write(name, desc, extra):
                os.makedirs(os.path.join(store, "user"), exist_ok=True)
                with open(os.path.join(store, "user", name + ".md"), "w") as fh:
                    fh.write(f"---\nname: {name}\ndescription: {desc}\ntype: user_standard\nsource:\n  kind: user\n{extra}---\nbody\n")
            with open(os.path.join(store, "review", "rulings-audit.json"), "w") as fh:
                fh.write("{}")
            write("quoted-rule", "Every release gets a signed tag from the owner", "  owner_quote: sign the tags\n  owner_quote_from: claude:x\n")
            write("bare-claim", "Every release gets a signed tag from the owner", "")
            write("moved-desc", "The cache lives under the old state folder", "")
            subprocess.run(["git", "-C", store, "add", "-A"], check=True, env=env)
            subprocess.run(["git", "-C", store, "commit", "-qm", "one"], check=True, env=env)
            first = subprocess.run(["git", "-C", store, "rev-parse", "HEAD"], capture_output=True, text=True, check=True).stdout.strip()
            write("moved-desc", "The cache moved to the new data volume", "")
            subprocess.run(["git", "-C", store, "commit", "-qam", "two"], check=True, env=env)
            st = signals.Store(store)
            self.assertTrue(st.fact("quoted-rule", first)[1])
            self.assertFalse(st.fact("bare-claim", first)[1], "no evidence: no ruling in an audited store")
            self.assertEqual(st.fact("moved-desc", first)[0], "The cache lives under the old state folder")
            self.assertEqual(st.fact("moved-desc", "HEAD")[0], "The cache moved to the new data volume")



class Transcripts(unittest.TestCase):
    def test_a_push_block_ends_a_window_and_counts(self):
        """A PreToolUse block comes as stdout JSON: its re-delivery ends the
        window. A compaction with no start block after it is counted. A
        queued prompt the owner typed is their turn; an older file's "owner"
        origin makes the session theirs."""
        with tempfile.TemporaryDirectory() as d:
            store, projects = os.path.join(d, "store"), os.path.join(d, "projects", "-demo")
            os.makedirs(projects)
            fact(store, "projects/demo", "lint-strict", "Run the linter with the strict profile before every push")
            subprocess.run(["git", "init", "-q", store], check=True)
            t = lambda m: f"2026-10-01T10:{m:02d}:00Z"
            ctx = json.dumps({"hookSpecificOutput": {"hookEventName": "PreToolUse", "additionalContext": "# tacit: before this step\n- lint-strict: agent note (2026-10-01): Run the linter\n"}})
            with open(os.path.join(projects, "s1.jsonl"), "w") as fh:
                fh.write(line(type="user", timestamp=t(2), isCompactSummary=True, message={"role": "user", "content": "Summary: other work."}))
                fh.write(line(type="attachment", timestamp=t(3), attachment={"type": "queued_command", "commandMode": "prompt",
                              "prompt": "run the linter with the strict profile before every push, as I said"}))
                fh.write(line(type="attachment", timestamp=t(10), attachment={"type": "hook_success", "hookEvent": "PreToolUse", "content": "", "stdout": ctx}))
                fh.write(assistant(t(12), tool("mcp__tacit__tacit_fact", name="lint-strict")))
            cases = os.path.join(d, "cases.jsonl")
            with open(cases, "w") as fh:
                fh.write(line(id="c1", session="s1", ts=t(0), event="prompt", origin="owner", commit="HEAD", delivered=["lint-strict"], labels={"lint-strict": "R"}))
                fh.write(line(id="c2", session="s1", ts=t(3), event="prompt", origin="queued", commit="HEAD", delivered=[]))
            out = os.path.join(d, "out.json")
            subprocess.run([sys.executable, "-I", os.path.join(HERE, "signals.py"), "--cases", cases, "--store", store,
                            "--projects", os.path.join(d, "projects"), "--out", out], check=True, capture_output=True, text=True)
            with open(out) as fh:
                res = json.load(fh)
            r = res["rows"][0]
            self.assertTrue(r["window_ended_early"], "the push's re-delivery ends the window")
            self.assertFalse(r["opened"], "the open after the re-delivery is the push's")
            self.assertTrue(r["crossed_unrecorded_compaction"])
            self.assertEqual(res["report"]["windows_through_unrecorded_compaction"], 1)
            self.assertTrue(r["owner_restated"], "a queued prompt at an owner case's time")

    def test_the_owner_s_own_writing_is_a_ruling(self):
        with tempfile.TemporaryDirectory() as d:
            store = os.path.join(d, "store")
            os.makedirs(os.path.join(store, "review"))
            with open(os.path.join(store, "review", "rulings-audit.json"), "w") as fh:
                fh.write("{}")
            for rel, harness in (("user/owner-wrote.md", "human"), ("local/claude@h/local-human.md", "human"), ("user/agent-wrote.md", "claude")):
                os.makedirs(os.path.dirname(os.path.join(store, rel)), exist_ok=True)
                name = os.path.basename(rel)[:-3]
                with open(os.path.join(store, rel), "w") as fh:
                    fh.write(f"---\nname: {name}\ndescription: a rule of some kind for the tests here\ntype: user_standard\nsource:\n  kind: user\n  harness: {harness}\n---\nbody\n")
            subprocess.run(["git", "init", "-q", store], check=True)
            st = signals.Store(store)
            self.assertTrue(st.fact("owner-wrote", "HEAD")[1])
            self.assertFalse(st.fact("local-human", "HEAD")[1], "local/ is not the owner's writing")
            self.assertFalse(st.fact("agent-wrote", "HEAD")[1])


if __name__ == "__main__":
    unittest.main()
