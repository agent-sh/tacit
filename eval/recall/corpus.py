#!/usr/bin/env python3
"""Synthetic Claude Code session transcripts with known recovery targets.

Each transcript is one session of an invented project (/work/demo) with filler
turns (file reads, test and build logs, replies) and planted tasks. A task is
something an agent did earlier in the session and later needs back: the id a
deploy printed, the exact migration command it ran, the path of a config file
it wrote, the error a test run failed with. Every task has the words an agent
would search for, the value it must recover, and the regex of that value's
shape. Some tasks have an older decoy: the same words with an older value, so
only the newest is right. Each task's events carry a fake secret of a shape the
redactor knows (a GitHub token, an AWS key id, a password in a database URL, an
OpenAI project key), the markers counted in the context.

Everything is generated from a seed: the same seed and size give the same
bytes. Nothing here comes from a real session.

    python3 -I eval/recall/corpus.py --out DIR --mib 16 --seed 7
"""

import argparse
import json
import os
import random
import string
import sys

SERVICES = ["billing", "ledger", "search", "gateway", "notifier", "scheduler",
            "inventory", "accounts", "reports", "exporter", "pricing", "uploads"]
ENVS = ["staging", "canary", "sandbox", "preview", "perf", "eu-west"]
TOPICS = ["webhook", "quota", "ratelimit", "audit", "sharding", "backfill"]
WORDS = ("the a of to and in for with on from is was this that it as by at be "
         "check run build test load parse write read update value field request "
         "response handler worker queue table index cursor batch limit offset "
         "error retry schema client server cache "
         "token session route module package function return struct method").split()

HEX = "0123456789abcdef"
ALNUM = string.ascii_letters + string.digits
UPPER = string.ascii_uppercase + string.digits


def rs(r, n, alphabet=ALNUM):
    return "".join(r.choice(alphabet) for _ in range(n))


# Fake secrets, built from parts so no whole token sits in the source.
def secret(r, kind):
    if kind == "github":
        return "gh" + "p_" + rs(r, 36)
    if kind == "aws":
        return "AK" + "IA" + rs(r, 16, UPPER)
    if kind == "dburl":
        return rs(r, 20)
    return "sk-" + "proj-" + rs(r, 48)  # openai


SHAPES = {
    "id": r"rel-[0-9a-f]{10}",
    "command": r"make migrate STEP=\d+ SERVICE=[a-z-]+ TARGET=[a-z0-9-]+",
    "path": r"/work/demo/[a-z-]+/config/[a-z]+-[0-9a-f]{6}\.yaml",
    "error": r"lock timeout after \d+s \(code E\d{4}\)",
}


class Writer:
    """Writes Claude Code transcript lines for one session."""

    def __init__(self, f, session, r):
        self.f, self.session, self.r = f, session, r
        self.n, self.t, self.size = 0, 1_700_000_000, 0

    def _line(self, kind, message, **extra):
        self.n += 1
        self.t += self.r.randint(2, 40)
        rec = {"type": kind, "sessionId": self.session, "cwd": "/work/demo",
               "entrypoint": "cli", "uuid": "%s-%06d" % (kind[0], self.n),
               "timestamp": _iso(self.t), "message": message}
        rec.update(extra)
        line = json.dumps(rec, separators=(",", ":")) + "\n"
        self.f.write(line)
        self.size += len(line.encode())

    def prompt(self, text):
        self._line("user", {"role": "user", "content": text}, promptSource="typed")

    def say(self, text):
        self._line("assistant", {"role": "assistant", "content": [{"type": "text", "text": text}]})

    def call(self, name, tool_input, output):
        tid = "toolu_%06d" % (self.n + 1)
        self._line("assistant", {"role": "assistant", "content": [
            {"type": "tool_use", "id": tid, "name": name, "input": tool_input}]})
        self._line("user", {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": tid, "content": output}]})


def _iso(t):
    import time
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t))


def sentence(r, n):
    return " ".join(r.choice(WORDS) for _ in range(n))


def log_lines(r, n, s=None):
    """Build and test output about one service. An environment, or a word
    agents search with (deploy, migrate, release, config), shows up in a few
    lines only, so a search's words meet in some filler events, and more of
    them the longer the session. No line holds a value of a target's shape,
    but a planted event wraps such lines around its own value, so one task's
    words can match another task's event."""
    s = s or r.choice(SERVICES)
    out = []
    for _ in range(n):
        x = r.random()
        if x < 0.008:
            out.append("INFO %s: healthcheck on %s passed in %dms" % (s, r.choice(ENVS), r.randint(2, 900)))
        elif x < 0.014:
            out.append("INFO %s %s for %s" % (s, r.choice(["deployed preview", "migrate dry run", "release notes", "loaded config"]), r.choice(ENVS)))
        elif x < 0.5:
            out.append("ok   %s/internal/%s\t%.3fs" % (s, r.choice(WORDS), r.random()))
        elif x < 0.6:
            out.append("--- PASS: Test%s%s (%.2fs)" % (s.title(), r.choice(WORDS).title(), r.random()))
        elif x < 0.65:
            out.append("WARN %s: retry %d of 5 for %s" % (s, r.randint(1, 5), r.choice(WORDS)))
        else:
            out.append(sentence(r, r.randint(5, 14)))
    return "\n".join(out)


def source_file(r, n, s):
    lines = ["package %s" % s, ""]
    for _ in range(n):
        lines.append("\t%s := %s(%s, %d) // %s" % (
            r.choice(WORDS), r.choice(WORDS).title(), r.choice(WORDS), r.randint(0, 999), sentence(r, 5)))
    return "\n".join(lines)


def filler_turn(w, r):
    s = r.choice(SERVICES)
    w.prompt("%s %s in %s" % (r.choice(["look at", "fix", "check", "clean up", "speed up"]), sentence(r, 4), s))
    for _ in range(r.randint(1, 3)):
        if r.random() < 0.5:
            path = "/work/demo/%s/%s.go" % (s, r.choice(WORDS))
            w.call("Read", {"file_path": path}, source_file(r, r.randint(40, 160), s))
        else:
            cmd = r.choice(["go test ./%s/..." % s, "make build SERVICE=%s" % s, "grep -rn %s %s/" % (r.choice(WORDS), s)])
            w.call("Bash", {"command": cmd, "description": sentence(r, 4)}, log_lines(r, r.randint(30, 140), s))
    w.say(sentence(r, r.randint(8, 30)) + ".")


# A planted task: the events that hold its value, and what an agent would
# search for to get it back. A decoy is the same call planted earlier, with
# another value of the same shape.
def plant(w, r, kind, key, value_rng):
    s, e, topic = key
    if kind == "id":
        rel = "rel-" + rs(value_rng, 10, HEX)
        tok = secret(r, "github")
        w.prompt("deploy %s to %s" % (s, e))
        w.call("Bash", {"command": "./scripts/deploy.sh %s --env %s --token %s" % (s, e, tok), "description": "Deploy %s" % s},
               log_lines(r, r.randint(20, 250), s) + "\nauthenticated with %s\ndeployed %s to %s as release %s\n" % (tok, s, e, rel) + log_lines(r, 10, s))
        w.say("Deployed %s to %s." % (s, e))
        return rel, tok, ["deployed", s, e]
    if kind == "command":
        step = value_rng.randint(2, 99)
        pw = secret(r, "dburl")
        target = "%s-%s" % (e, rs(value_rng, 4, HEX))
        cmd = "make migrate STEP=%d SERVICE=%s TARGET=%s" % (step, s, target)
        w.prompt("run the %s migration on %s" % (s, e))
        w.call("Bash", {"command": "DATABASE_URL=" + "postgres" + "://app:" + pw + "@%s.internal:5432/%s %s" % (e, s, cmd), "description": "Migrate %s" % s},
               "applying migration step %d for %s\n" % (step, s) + log_lines(r, r.randint(10, 200), s))
        w.say("Ran the %s migration on %s." % (s, e))
        return cmd, pw, ["migrate", s, e]
    if kind == "path":
        path = "/work/demo/%s/config/%s-%s.yaml" % (s, topic, rs(value_rng, 6, HEX))
        key_ = secret(r, "openai")
        w.prompt("put the %s %s settings for %s in a new config file" % (s, topic, e))
        w.call("Write", {"file_path": path, "content": "%s:\n  env: %s\n  api_key: %s\n%s\n" % (topic, e, key_, source_file(r, 30, s))},
               "File created successfully at: %s" % path)
        w.say("Wrote the %s settings." % topic)
        return path, key_, [topic, s, "yaml"]
    # error
    sec = value_rng.randint(5, 90)
    err = "lock timeout after %ds (code E%04d)" % (sec, value_rng.randint(1000, 9999))
    aws = secret(r, "aws")
    w.prompt("run the %s %s tests against %s" % (s, topic, e))
    w.call("Bash", {"command": "go test ./%s/%s/..." % (s, topic), "description": "Test %s" % s},
           "env: AWS_ACCESS_KEY_ID=%s REGION=%s\n" % (aws, e) + log_lines(r, r.randint(20, 250), s)
           + "\n--- FAIL: Test%s%s\n    %s_test.go:%d: %s: %s\nFAIL\n" % (s.title(), topic.title(), topic, r.randint(10, 400), s, err))
    w.say("The %s %s tests fail." % (s, topic))
    return err, aws, ["FAIL", s, topic]


def generate(out, mib, seed, tasks_per_kind=4):
    r = random.Random("%d:%d" % (seed, mib))
    session = "5e55a0%02x-0000-4000-8000-%012x" % (mib % 256, seed)
    path = os.path.join(out, "home", ".claude", "projects", "-work-demo", session + ".jsonl")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    target_bytes = mib * 1024 * 1024
    kinds = ["id", "command", "path", "error"] * tasks_per_kind
    r.shuffle(kinds)
    keys = set()
    plan = []
    for k in kinds:
        while True:
            key = (r.choice(SERVICES), r.choice(ENVS), r.choice(TOPICS))
            # No two tasks of a kind share their search words.
            words = (k, key[0], key[1] if k in ("id", "command") else key[2])
            if words not in keys:
                keys.add(words)
                break
        # Where the task lands, as a share of the session; half have an older decoy.
        plan.append({"kind": k, "key": key, "at": r.uniform(0.05, 0.9), "decoy": r.random() < 0.5})
    events = []
    for i, p in enumerate(plan):
        if p["decoy"]:
            events.append((p["at"] * r.uniform(0.2, 0.9), i, True))
        events.append((p["at"], i, False))
    events.sort()
    tasks = [None] * len(plan)
    with open(path, "w") as f:
        w = Writer(f, session, r)
        w.prompt("we are working on the demo services under /work/demo today")
        for at, i, decoy in events:
            while w.size < at * target_bytes:
                filler_turn(w, r)
            p = plan[i]
            value, sec, words = plant(w, r, p["kind"], p["key"], random.Random(r.random()))
            if decoy:
                p.setdefault("decoy_secrets", []).append(sec)
                p["decoy_value"] = value
            else:
                tasks[i] = {"task": i, "kind": p["kind"], "words": words, "value": value,
                            "shape": SHAPES[p["kind"]], "secrets": [sec]}
        while w.size < target_bytes:
            filler_turn(w, r)
    for i, p in enumerate(plan):
        tasks[i]["secrets"] += p.get("decoy_secrets", [])
        tasks[i]["decoy"] = p.get("decoy_value")
    meta = {"session": session, "mib": mib, "seed": seed, "bytes": os.path.getsize(path),
            "events": w.n, "transcript": path, "home": os.path.join(out, "home"), "tasks": tasks}
    with open(os.path.join(out, "tasks.json"), "w") as f:
        json.dump(meta, f, indent=1)
    return meta


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, help="directory for home/ and tasks.json")
    ap.add_argument("--mib", type=int, default=16, help="transcript size in MiB")
    ap.add_argument("--seed", type=int, default=7)
    a = ap.parse_args()
    m = generate(a.out, a.mib, a.seed)
    print("%s: %d bytes, %d lines, %d tasks" % (m["transcript"], m["bytes"], m["events"], len(m["tasks"])))


if __name__ == "__main__":
    sys.exit(main())
