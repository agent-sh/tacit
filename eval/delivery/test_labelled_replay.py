#!/usr/bin/env python3
"""End-to-end check of deliveries.py and labelled_replay.py on a synthetic
store and transcript. Builds the client unless TACIT_BIN names one.

  python3 -I eval/delivery/test_labelled_replay.py
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))


def run(*args, cwd=None, env=None, input=None):
    return subprocess.run(args, cwd=cwd, env=env, input=input, capture_output=True, text=True, check=True).stdout


def write(path, text):
    with open(path, "w") as fh:
        fh.write(text)


def fact(root, scope, name, desc, typ="user_standard", commands=None):
    d = os.path.join(root, scope)
    os.makedirs(d, exist_ok=True)
    applies = ""
    if commands:
        applies = "applies_to:\n  repos:\n    - demo-api\n  commands:\n" + "".join(f"    - {c}\n" for c in commands)
    write(os.path.join(d, name + ".md"),
          f"---\nname: {name}\ndescription: \"{desc}\"\ntype: {typ}\nscope: {scope}\n{applies}source:\n  harness: curator\n  host: workstation\n  kind: observed\ncreated: 2026-10-01\n---\n\n{desc}.\n")


def load(path):
    with open(path) as fh:
        return [json.loads(l) for l in fh]


def commit(root, msg, when):
    env = dict(os.environ, GIT_AUTHOR_DATE=when, GIT_COMMITTER_DATE=when, GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid",
               GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid")
    run("git", "-C", root, "add", "-A", env=env)
    run("git", "-C", root, "commit", "-qm", msg, env=env)


class LabelledReplay(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Scratch on disk, not in a RAM-backed /tmp, unless TMPDIR says otherwise.
        base = os.environ.get("TMPDIR") or os.path.join(os.path.expanduser("~"), ".cache", "tacit-eval-tests")
        os.makedirs(base, exist_ok=True)
        cls.tmp = tempfile.mkdtemp(prefix="labelled-replay-", dir=base)
        cls.bin = os.environ.get("TACIT_BIN") or os.path.join(cls.tmp, "tacit")
        if not os.environ.get("TACIT_BIN"):
            run("go", "build", "-o", cls.bin, "./cmd/tacit", cwd=os.path.join(REPO, "client"))

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def hook(self, store, state, sub, payload):
        env = {k: v for k, v in os.environ.items() if not k.startswith("TACIT_")}
        env.update(TACIT_STORE=store, TACIT_INDEX=os.path.join(state, "index"), TACIT_STATE_DIR=state, TACIT_LOCAL=os.path.join(state, "local"),
                   HOME=os.path.join(self.tmp, "home"))
        cfg = os.path.join(self.tmp, "config.yaml")
        write(cfg, "# test\n")
        return run(self.bin, "--config", cfg, "hook", sub, env=env, input=json.dumps(payload))

    def test_replay_pins_the_commit_and_the_session(self):
        t = self.tmp
        store = os.path.join(t, "store")
        os.makedirs(store)
        run("git", "init", "-q", "-b", "main", store)
        fact(store, "projects/demo-api", "release-tags-are-signed", "Release tags of demo-api are signed with the release key")
        fact(store, "projects/demo-api", "deploy-runs-the-smoke-suite", "A deploy of demo-api runs the smoke suite first", typ="design_decision", commands=["make deploy"])
        for i in range(30):
            fact(store, "user", f"filler-{i}", f"Unrelated filler note number {i} about gardening and tea")
        commit(store, "first", "2026-10-20T10:00:00Z")
        repo = os.path.join(t, "home", "projects", "demo-api")
        os.makedirs(repo)
        run("git", "init", "-q", repo)
        prompt = "check how release tags of demo-api are signed before the next release"
        # The hook as it ran then: the prompt block and a push at the commit.
        state = os.path.join(t, "live")
        os.makedirs(os.path.join(state, "sessions"))
        block = self.hook(store, state, "prompt", {"session_id": "s1", "cwd": repo, "prompt": prompt})
        self.assertIn("release-tags-are-signed", block)
        push = self.hook(store, state, "pre-tool", {"session_id": "s1", "cwd": repo, "hook_event_name": "PreToolUse",
                                                    "tool_name": "Bash", "tool_input": {"command": "make deploy"}})
        push_ctx = json.loads(push)["hookSpecificOutput"]["additionalContext"]
        self.assertIn("deploy-runs-the-smoke-suite", push_ctx)
        # A later commit adds a fact the same prompt would get today.
        fact(store, "projects/demo-api", "release-tags-signed-since-v2", "Release tags of demo-api are signed since v2 with a hardware key")
        commit(store, "second", "2026-10-22T10:00:00Z")
        today = os.path.join(t, "today")
        os.makedirs(os.path.join(today, "sessions"))
        self.assertIn("release-tags-signed-since-v2", self.hook(store, today, "prompt", {"session_id": "s9", "cwd": repo, "prompt": prompt}),
                      "the later fact answers the prompt at HEAD, so pinning is what keeps it out")
        # The transcript of that session, with both blocks and a repeat of the prompt.
        orphan = "# tacit: facts matching this request\n- orphan-fact: agent note (2026-10-21): a block whose prompt record is gone\n"
        lines = [
            {"type": "attachment", "uuid": "a0", "parentUuid": "gone", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T08:59:00.000Z",
             "attachment": {"type": "hook_success", "hookEvent": "UserPromptSubmit", "content": orphan, "stdout": orphan}},
            {"type": "user", "uuid": "u1", "parentUuid": None, "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:00:00.000Z", "message": {"role": "user", "content": prompt}},
            {"type": "attachment", "uuid": "a1", "parentUuid": "u1", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:00:00.100Z",
             "attachment": {"type": "hook_success", "hookEvent": "UserPromptSubmit", "content": block, "stdout": block}},
            {"type": "assistant", "uuid": "m1", "parentUuid": "a1", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:00:05.000Z",
             "message": {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"command": "make deploy"}}]}},
            {"type": "attachment", "uuid": "a2", "parentUuid": "m1", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:00:05.100Z",
             "attachment": {"type": "hook_additional_context", "hookEvent": "PreToolUse", "toolUseID": "toolu_1", "content": [push_ctx]}},
            {"type": "user", "uuid": "u2", "parentUuid": "a2", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:05:00.000Z", "message": {"role": "user", "content": prompt}},
            {"type": "user", "uuid": "c1", "parentUuid": "u2", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:04:00.000Z", "isCompactSummary": True,
             "isVisibleInTranscriptOnly": True, "message": {"role": "user", "content": "This session is being continued from a previous conversation"}},
            {"type": "attachment", "uuid": "a3", "parentUuid": "u2", "sessionId": "s1", "cwd": repo, "timestamp": "2026-10-21T09:05:00.100Z",
             "attachment": {"type": "hook_success", "hookEvent": "UserPromptSubmit", "content": "", "stdout": ""}},
        ]
        tr = os.path.join(t, "home", ".claude", "projects", "-demo", "s1.jsonl")
        os.makedirs(os.path.dirname(tr))
        write(tr, "".join(json.dumps(l) + "\n" for l in lines))
        # Under the home directory, as the prompt hook reads a transcript only there.
        out = os.path.join(t, "home", "replay")
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "deliveries.py"), "--store", store, "--out", out, tr], check=True, capture_output=True)
        cases = load(os.path.join(out, "cases.jsonl"))
        self.assertEqual([c["event"] for c in cases], ["prompt", "pre-tool"])
        first = run("git", "-C", store, "rev-list", "--max-parents=0", "HEAD").strip()
        self.assertTrue(all(c["commit"] == first for c in cases), "a case reads the commit its hook read")
        self.assertIn("release-tags-are-signed", cases[1]["seen"])
        self.assertIn("orphan-fact", cases[0]["seen"], "a block that makes no case still joins the session's facts")
        # Grades for the lines printed then, and a probe for the prompt.
        lab = os.path.join(t, "labels.tsv")
        write(lab, "".join(f"{c['id']}\t{n}\tR\n" for c in cases for n in c["delivered"]))
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "deliveries.py"), "--store", store, "--out", out, "--labels", lab, "--silent", tr], check=True, capture_output=True)
        cases = load(os.path.join(out, "cases.jsonl"))
        self.assertEqual([(c["event"], c["silent"]) for c in cases], [("prompt", False), ("pre-tool", False), ("prompt", True)])
        self.assertTrue(all(c["labels"] for c in cases[:2]))
        clone = os.path.join(t, "clone")
        run("git", "clone", "-q", store, clone)
        replay = [sys.executable, "-I", os.path.join(HERE, "labelled_replay.py")]
        refused = subprocess.run(replay + ["run", "--store", clone, "--cases", os.path.join(out, "cases.jsonl"), "--out", os.path.join(t, "runs"), "--arm", "base=" + self.bin],
                                 capture_output=True, text=True)
        self.assertNotEqual(refused.returncode, 0, "a clone whose origin a sync could reach is refused")
        run("git", "-C", clone, "remote", "set-url", "origin", "/nonexistent/no-remote")
        runs = os.path.join(t, "runs")
        os.makedirs(os.path.join(t, "home"), exist_ok=True)
        env = dict(os.environ, HOME=os.path.join(t, "home"))
        subprocess.run([sys.executable, "-I", os.path.join(HERE, "labelled_replay.py"), "run", "--store", clone, "--cases", os.path.join(out, "cases.jsonl"),
                        "--out", runs, "--arm", "base=" + self.bin, "--workers", "1"], check=True, capture_output=True, env=env)
        res = {r["case"]: r for r in load(os.path.join(runs, "results.jsonl"))}
        self.assertEqual(res[cases[0]["id"]]["names"], cases[0]["delivered"], "the prompt block replays as printed")
        self.assertNotIn("release-tags-signed-since-v2", res[cases[0]["id"]]["names"], "a later commit's fact is not in the replay")
        self.assertEqual(res[cases[1]["id"]]["names"], cases[1]["delivered"], "the push replays as printed")
        self.assertEqual(res[cases[2]["id"]]["names"], [], "the repeated prompt's fact is in the session already")
        score = subprocess.run([sys.executable, "-I", os.path.join(HERE, "labelled_replay.py"), "score", "--cases", os.path.join(out, "cases.jsonl"),
                                "--out", runs], check=True, capture_output=True, text=True).stdout
        self.assertIn("reproduced=2/2", score)
        self.assertIn("useful_kept=2/2", score)
        cmp = subprocess.run(replay + ["compare", "--cases", os.path.join(out, "cases.jsonl"), "--out", runs, "--arms", "base,base"],
                             check=True, capture_output=True, text=True).stdout
        self.assertIn("3 cases, 0 changed, 0 left out", cmp)
        again = subprocess.run(replay + ["run", "--store", clone, "--cases", os.path.join(out, "cases.jsonl"), "--out", runs, "--arm", "base=" + shutil.which("true")],
                               capture_output=True, text=True, env=env)
        self.assertNotEqual(again.returncode, 0, "an arm name keeps its binary within one --out")
        # An arm that fails every call is reported, not taken for one that prints less.
        subprocess.run(replay + ["run", "--store", clone, "--cases", os.path.join(out, "cases.jsonl"), "--out", runs, "--arm", "broken=" + shutil.which("false"), "--workers", "1"],
                       check=True, capture_output=True, env=env)
        cmp = subprocess.run(replay + ["compare", "--cases", os.path.join(out, "cases.jsonl"), "--out", runs, "--arms", "base,broken"],
                             check=True, capture_output=True, text=True).stdout
        self.assertIn("(3 left out: an arm failed)", cmp)
        # A graded case the arm did not run is no useful line lost.
        extra = dict(cases[0], id="never-ran")
        with open(os.path.join(out, "cases.jsonl"), "a") as fh:
            fh.write(json.dumps(extra) + "\n")
        score = subprocess.run([sys.executable, "-I", os.path.join(HERE, "labelled_replay.py"), "score", "--cases", os.path.join(out, "cases.jsonl"),
                                "--out", runs], check=True, capture_output=True, text=True).stdout
        self.assertIn("useful_kept=2/2", score)


if __name__ == "__main__":
    unittest.main()
