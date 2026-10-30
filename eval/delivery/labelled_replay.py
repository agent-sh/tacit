#!/usr/bin/env python3
"""Replay graded hook deliveries through actual binaries, each at the store
commit it was delivered from, and score delivery rules on the grades.

A case is one hook call that really happened (a prompt or a tool call), with
the lines tacit printed then and a grade for each: R useful, W weak, N noise.
`run` puts a worktree of a store clone at each case's commit, builds that
commit's index once per index schema (cached), and runs every arm's binary on
the case's hook input with an isolated state directory that holds the facts
the session had already been given. `score` counts, per arm and label set,
the useful lines kept, the noise rate and the probes reached; `compare`
pairs two arms case by case. Lines an arm prints that no grade covers are
counted apart and listed by `todo` for grading, without the arm's name. No
model calls; nothing outside --out is written, and the clone is never pushed.

  labelled_replay.py run --store CLONE --cases cases.jsonl --out DIR \
      --arm base=/abs/tacit-main --arm cand=/abs/tacit-candidate[,TACIT_DELIVERY_X=off]
  labelled_replay.py score --cases cases.jsonl --out DIR [--grades g.tsv ...] [--exclude gone]
  labelled_replay.py compare --cases cases.jsonl --out DIR --arms base,cand [--grades g.tsv ...]
  labelled_replay.py todo --cases cases.jsonl --out DIR [--grades g.tsv ...]

Case file: JSON lines, one object per case (eval/delivery/LABELLED-REPLAY.md):
  id, set, event ("prompt" | "pre-tool"), ts, commit, cwd, session, prompt,
  transcript (a copy cut at the call, or ""), tool_name, tool_input (pre-tool),
  seen (fact names and "area:<folder>" the session had before the call),
  start_recorded (false: simulate the session's SessionStart block with the
  first arm, at the commit of the session's first case, and add its facts to
  seen), delivered (names printed then), labels {name: R|W|N}, silent (the
  hook printed nothing then), origin (optional: groups prompt cases),
  expected (probes only: the facts that would have answered it).
"""
import argparse
import collections
import concurrent.futures as cf
import hashlib
import json
import math
import os
import random
import re
import shutil
import subprocess
import sys
import threading
import time

NAME = re.compile(r"^- (?:RULE )?([a-z0-9][a-z0-9-]*)")
# Hook budgets (client/cmd/tacit): a call past them may have printed nothing
# because time ran out, not because the rule chose so.
BUDGET_MS = {"prompt": 2000, "pre-tool": 400}
# Files a hook writes in its state directory before it starts a background
# build or a dream: a replay must start neither.
STARTED = ("index-build.log", "dream-session.log", "dream-trigger.json")


def parse_block(text):
    """The fact lines of a hook block: [(name, rule, line)]."""
    out = []
    for line in text.splitlines():
        m = NAME.match(line)
        if m:
            out.append((m.group(1), line.startswith("- RULE "), line))
    return out


def load_cases(path):
    with open(path) as fh:
        return [json.loads(l) for l in fh if l.strip()]


def write_json(path, value):
    with open(path, "w") as fh:
        json.dump(value, fh)


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def parse_arm(spec):
    """NAME=/abs/binary[,KEY=VAL...]; a value may hold commas."""
    name, _, rest = spec.partition("=")
    parts = re.split(r",(?=[A-Za-z_][A-Za-z0-9_]*=)", rest)
    env = {}
    for p in parts[1:]:
        k, _, v = p.partition("=")
        env[k] = v
    if not name or not os.path.isabs(parts[0]):
        raise SystemExit(f"--arm {spec}: want NAME=/abs/binary[,KEY=VAL...]")
    return name, parts[0], env


class Runner:
    def __init__(self, a):
        self.a = a
        self.out = os.path.abspath(a.out)
        # Index files by schema and commit. A binary prunes every other
        # commit's file from the directory it builds in, so it never builds
        # here: each worker and arm has its own live directory, which holds
        # only the commit being replayed, hard-linked from this cache.
        self.cache = os.path.abspath(a.index_cache or os.path.join(self.out, "index"))
        os.makedirs(self.cache, exist_ok=True)
        self.cfg = os.path.join(self.out, "config.yaml")
        if not os.path.exists(self.cfg):
            with open(self.cfg, "w") as fh:
                fh.write("# labelled replay: defaults only\n")
        self.local = os.path.join(self.out, "empty-local")
        os.makedirs(self.local, exist_ok=True)
        self.arms = [parse_arm(s) for s in a.arm]
        self.check_arms()
        self.lock = threading.Lock()
        self.results_path = os.path.join(self.out, "results.jsonl")
        self.done = set()
        if os.path.exists(self.results_path):
            for r in load_cases(self.results_path):
                self.done.add((r["arm"], r["case"]))
        self.starts_path = os.path.join(self.out, "starts.json")
        self.starts = {}
        if os.path.exists(self.starts_path):
            with open(self.starts_path) as fh:
                self.starts = json.load(fh)

    def check_arms(self):
        """An arm name means one binary and one environment for the whole --out:
        resuming with another would mix their results under one name."""
        path = os.path.join(self.out, "arms.json")
        known = {}
        if os.path.exists(path):
            with open(path) as fh:
                known = json.load(fh)
        for name, binary, env in self.arms:
            want = {"binary_sha256": sha256(binary), "env": env}
            if name in known and known[name] != want:
                raise SystemExit(f"arm {name} ran with another binary or settings in {self.out}: use a new --out or a new arm name")
            known[name] = want
        first = known[self.arms[0][0]]
        if self.a.simulate_start and "_starts" in known and known["_starts"] != first:
            raise SystemExit(f"the simulated SessionStart blocks in {self.out} came from another first arm")
        known["_starts"] = first
        write_json(path, known)

    def env(self, tree, live, state, extra):
        env = {k: v for k, v in os.environ.items() if not k.startswith("TACIT_")}
        env.update(TACIT_STORE=tree, TACIT_INDEX=live, TACIT_STATE_DIR=state, TACIT_LOCAL=self.local)
        env.update(extra)
        return env

    def call(self, binary, args, payload, env, cwd, timeout=120):
        t = time.perf_counter()
        p = subprocess.run([binary, "--config", self.cfg, *args], input=payload, capture_output=True,
                           text=True, env=env, cwd=cwd if os.path.isdir(cwd) else self.out, timeout=timeout)
        return p, (time.perf_counter() - t) * 1000

    def warm(self, binary, tree, extra, live, sha):
        """Make live hold this commit's index for binary, and nothing else."""
        os.makedirs(live, exist_ok=True)
        for f in os.listdir(live):
            if not f.endswith(f"-{sha}.db"):
                os.remove(os.path.join(live, f))
        for f in os.listdir(self.cache):
            if f.endswith(f"-{sha}.db") and not os.path.exists(os.path.join(live, f)):
                os.link(os.path.join(self.cache, f), os.path.join(live, f))
        state = os.path.join(live + ".state")
        os.makedirs(state, exist_ok=True)
        # `hook index` builds with the hooks' own build budget (5 minutes), not
        # a search's 20 s, and exits 0 whatever happens: the file is the check.
        for _ in range(3):
            self.call(binary, ["hook", "index"], "{}", self.env(tree, live, state, extra), tree, timeout=360)
            if any(f.endswith(f"-{sha}.db") for f in os.listdir(live)):
                break
        files = sorted(os.listdir(live))
        if not any(f.endswith(f"-{sha}.db") for f in files) or any(not f.endswith(f"-{sha}.db") for f in files):
            raise RuntimeError(f"index for {sha} not alone in {live}: {files}")
        for f in files:
            if not os.path.exists(os.path.join(self.cache, f)):
                os.link(os.path.join(live, f), os.path.join(self.cache, f))
        return files

    def check_live(self, live, files):
        """No hook started a build: the live directory is as warm left it."""
        if sorted(os.listdir(live)) != files:
            raise RuntimeError(f"a hook changed {live}: {sorted(os.listdir(live))}, was {files}")

    def check_head(self, tree, sha):
        """Nothing moved the worktree off the commit (a hook's sync, say)."""
        head = subprocess.run(["git", "-C", tree, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        if head != sha:
            raise RuntimeError(f"{tree} moved from {sha} to {head}")

    def check_started(self, state):
        hit = [f for f in STARTED if os.path.exists(os.path.join(state, f))]
        if hit:
            raise RuntimeError(f"a hook started background work in {state}: {hit}")

    def state_for(self, arm, case, seen):
        state = os.path.join(self.out, "state", arm, case["id"])
        shutil.rmtree(state, ignore_errors=True)
        os.makedirs(os.path.join(state, "sessions"), exist_ok=True)
        if case.get("session") and seen:
            with open(os.path.join(state, "sessions", case["session"]), "w") as fh:
                fh.write("".join(n + "\n" for n in dict.fromkeys(seen)))
        for spec in self.a.state_file or []:
            name, _, src = spec.partition("=")
            shutil.copyfile(src, os.path.join(state, name))
        return state

    def start_names(self, case, tree, live):
        """The SessionStart facts of a session whose start block is not in the transcript."""
        key = case["session"]
        if key in self.starts:
            return self.starts[key]
        name, binary, extra = self.arms[0]
        state = os.path.join(self.out, "state", "start", key)
        shutil.rmtree(state, ignore_errors=True)
        os.makedirs(state, exist_ok=True)
        cwd = case.get("start_cwd") or case["cwd"]
        payload = json.dumps({"session_id": "", "cwd": cwd, "source": "startup", "hook_event_name": "SessionStart"})
        p, _ = self.call(binary, ["hook", "session-start", "--mcp"], payload, self.env(tree, live, state, extra), cwd)
        self.check_started(state)
        names = [n for n, _, _ in parse_block(p.stdout)]
        with self.lock:
            self.starts[key] = names
            write_json(self.starts_path, self.starts)
        return names

    def one(self, arm, binary, extra, case, tree, live, start):
        seen = list(case.get("seen") or []) + list(start or [])
        state = self.state_for(arm, case, seen)
        env = self.env(tree, live, state, extra)
        if case["event"] == "prompt":
            payload = {"session_id": case.get("session", ""), "cwd": case["cwd"], "prompt": case["prompt"],
                       "transcript_path": case.get("transcript", ""), "hook_event_name": "UserPromptSubmit"}
            p, ms = self.call(binary, ["hook", "prompt"], json.dumps(payload), env, case["cwd"])
            text = p.stdout
        else:
            payload = {"session_id": case.get("session", ""), "cwd": case["cwd"], "hook_event_name": "PreToolUse",
                       "tool_name": case.get("tool_name") or "", "tool_input": case.get("tool_input"),
                       "transcript_path": case.get("transcript", "")}
            p, ms = self.call(binary, ["hook", "pre-tool"], json.dumps(payload), env, case["cwd"])
            text = ""
            if p.stdout.strip():
                text = json.loads(p.stdout)["hookSpecificOutput"]["additionalContext"]
        self.check_started(state)
        lines = parse_block(text)
        shutil.rmtree(state, ignore_errors=True)
        return {"arm": arm, "case": case["id"], "names": [n for n, _, _ in lines], "rules": [n for n, r, _ in lines if r],
                "lines": [l for _, _, l in lines], "ms": round(ms, 1), "rc": p.returncode,
                "stderr": p.stderr.strip()[:200000 if "TACIT_TRACE" in extra else 300]}

    def worker(self, wid, commits, by_commit):
        tree = os.path.join(self.out, "trees", f"w{wid}")
        if not os.path.isdir(tree):
            subprocess.run(["git", "-C", self.a.store, "worktree", "add", "-q", "--detach", tree, commits[0]], check=True)
        for c in commits:
            todo = [(arm, b, e, case) for case in by_commit[c] for arm, b, e in self.arms if (arm, case["id"]) not in self.done]
            if not todo:
                continue
            subprocess.run(["git", "-C", tree, "checkout", "-q", "--detach", "-f", c], check=True)
            subprocess.run(["git", "-C", tree, "clean", "-qfdx"], check=True)
            live = {arm: os.path.join(self.out, "live", f"w{wid}", arm) for arm, _, _ in self.arms}
            files = {arm: self.warm(b, tree, e, live[arm], c) for arm, b, e in self.arms}
            rows = []
            with cf.ThreadPoolExecutor(self.a.jobs) as ex:
                futs = []
                for arm, b, e, case in todo:
                    start = self.starts.get(case["session"]) if self.a.simulate_start and not case.get("start_recorded") else None
                    futs.append(ex.submit(self.one, arm, b, e, case, tree, live[arm], start))
                for f in futs:
                    rows.append(f.result())
            for arm, _, _ in self.arms:
                self.check_live(live[arm], files[arm])
            self.check_head(tree, c)
            with self.lock:
                with open(self.results_path, "a") as fh:
                    for r in rows:
                        fh.write(json.dumps(r) + "\n")
                        self.done.add((r["arm"], r["case"]))
            print(f"w{wid} {c[:9]} {len(rows)} calls", flush=True)

    def run(self, cases):
        home = os.path.realpath(os.path.expanduser("~"))
        away = [c["id"] for c in cases if c.get("transcript") and not os.path.realpath(c["transcript"]).startswith(home + os.sep)]
        if away:
            print(f"warning: {len(away)} cases' transcript copies are outside the home directory; "
                  f"the prompt hook reads none of them (e.g. {away[0]})", file=sys.stderr)
        subprocess.run(["git", "-C", self.a.store, "worktree", "prune"], check=False)
        by_commit = collections.defaultdict(list)
        for c in cases:
            by_commit[c["commit"]].append(c)
        order = sorted(by_commit, key=lambda c: min(x["ts"] for x in by_commit[c]))
        n = max(1, min(self.a.workers, len(order)))
        chunks = [order[i * len(order) // n:(i + 1) * len(order) // n] for i in range(n)]
        # A session's start block is simulated at the commit of its first
        # case, before any case runs, so every chunk sees it.
        if self.a.simulate_start:
            self.prepare_starts(cases, by_commit, order)
        with cf.ThreadPoolExecutor(n) as ex:
            list(ex.map(lambda i: self.worker(i, chunks[i], by_commit), range(n)))
        for i in range(n):
            subprocess.run(["git", "-C", self.a.store, "worktree", "remove", "--force", os.path.join(self.out, "trees", f"w{i}")], check=False)

    def prepare_starts(self, cases, by_commit, order):
        need = {}
        for c in order:
            for x in by_commit[c]:
                if not x.get("start_recorded") and not x.get("sidechain") and x.get("session") and x["session"] not in self.starts:
                    need.setdefault(x["session"], (c, x))
        if not need:
            return
        tree = os.path.join(self.out, "trees", "start")
        if not os.path.isdir(tree):
            subprocess.run(["git", "-C", self.a.store, "worktree", "add", "-q", "--detach", tree, order[0]], check=True)
        byc = collections.defaultdict(list)
        for s, (c, x) in need.items():
            byc[c].append(x)
        live = os.path.join(self.out, "live", "start", self.arms[0][0])
        for c in sorted(byc, key=order.index):
            subprocess.run(["git", "-C", tree, "checkout", "-q", "--detach", "-f", c], check=True)
            files = self.warm(self.arms[0][1], tree, self.arms[0][2], live, c)
            for x in byc[c]:
                self.start_names(x, tree, live)
            self.check_live(live, files)
            self.check_head(tree, c)
        subprocess.run(["git", "-C", self.a.store, "worktree", "remove", "--force", tree], check=False)
        print(f"simulated {len(need)} session starts", flush=True)


def load_grades(paths):
    g = {}
    for p in paths or []:
        with open(p) as fh:
            for l in fh:
                f = l.rstrip("\n").split("\t")
                if len(f) >= 3 and f[2] in ("R", "W", "N"):
                    g[(f[0], f[1])] = f[2]
    return g


def load_results(out):
    res = collections.defaultdict(dict)
    for r in load_cases(os.path.join(out, "results.jsonl")):
        res[r["arm"]][r["case"]] = r
    return res


def wilson(k, n, z=1.96):
    if n == 0:
        return float("nan"), float("nan")
    p = k / n
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return (c - h) / d, (c + h) / d


def grade_of(case, name, grades):
    return case.get("labels", {}).get(name) or grades.get((case["id"], name))


def scored(c):
    """A case whose lines are scored: graded lines of the time, or a silent call."""
    return bool(c.get("labels")) or bool(c.get("silent"))


def failed(c, r):
    """A call that errored or ran past its hook's budget: printing nothing
    there may be the failure, not the rule."""
    return r["rc"] != 0 or bool(r.get("stderr")) or r["ms"] >= BUDGET_MS.get(c["event"], 2000)


def excluded(c, tags):
    return bool(set(tags or []) & set(c.get("cwd_note", "").split(":")))


def score(a):
    cases = [c for c in load_cases(a.cases) if not excluded(c, a.exclude)]
    grades = load_grades(a.grades)
    res = load_results(a.out)
    arms = a.arms.split(",") if a.arms else sorted(res)
    groups = collections.OrderedDict()
    for c in cases:
        groups.setdefault(c["set"], []).append(c)
        if c.get("silent"):
            groups.setdefault(f'{c["set"]}/silent', []).append(c)
        if c["event"] == "prompt" and c.get("origin") and scored(c):
            groups.setdefault(f'{c["set"]}/{c["origin"]}', []).append(c)
    report = {}
    for g, cs in groups.items():
        graded = [c for c in cs if scored(c)]
        probes = [c for c in cs if c.get("expected")]
        for arm in arms:
            # Over the cases this arm ran, so a partial run is no lost line.
            ref_r = sum(1 for c in graded if c["id"] in res[arm] for n in c["delivered"] if c["labels"].get(n) == "R")
            k = collections.Counter()
            for c in graded:
                r = res[arm].get(c["id"])
                if r is None:
                    k["missing"] += 1
                    continue
                k["failed"] += failed(c, r)
                gs = [grade_of(c, n, grades) for n in r["names"]]
                for x in gs:
                    k[x or "?"] += 1
                k["blocks"] += 1
                k["blocks_all_noise"] += bool(gs) and all(x == "N" for x in gs)
                k["blocks_with_R"] += "R" in gs
                k["kept_R"] += sum(1 for n in r["names"] if c["labels"].get(n) == "R" and n in c["delivered"])
                k["reproduced"] += sum(1 for n in r["names"] if n in c["delivered"])
                k["historical"] += len(c["delivered"])
            hit = sum(1 for c in probes if res[arm].get(c["id"]) and set(res[arm][c["id"]]["names"]) & set(c["expected"]))
            ran = sum(1 for c in cs if c["id"] in res[arm])
            probes_ran = [c for c in probes if c["id"] in res[arm]]
            lines = k["R"] + k["W"] + k["N"] + k["?"]
            gl = k["R"] + k["W"] + k["N"]
            row = dict(cases=len(cs), blocks=k["blocks"], missing=k["missing"], failed=k["failed"], lines=lines,
                       R=k["R"], W=k["W"], N=k["N"], ungraded=k["?"],
                       noise=round(k["N"] / gl, 3) if gl else None,
                       noise_ci=[round(x, 3) for x in wilson(k["N"], gl)] if gl else None,
                       useful=round(k["R"] / gl, 3) if gl else None,
                       useful_kept=f'{k["kept_R"]}/{ref_r}',
                       blocks_with_R=k["blocks_with_R"], blocks_all_noise=k["blocks_all_noise"],
                       reproduced=f'{k["reproduced"]}/{k["historical"]}',
                       probes=f"{hit}/{len(probes_ran)}" if probes_ran else "", ran=ran)
            report.setdefault(g, {})[arm] = row
    for g, rows in list(report.items()):
        rows = {arm: row for arm, row in rows.items() if row.pop("ran")}
        if not rows:
            del report[g]
            continue
        report[g] = rows
        print(f"== {g}")
        for arm, row in rows.items():
            print(f"  {arm:14} " + " ".join(f"{k}={v}" for k, v in row.items() if v not in ("", None)))
    if a.json:
        write_json(a.json, report)


def compare(a):
    """Two arms case by case: the change in useful and noise lines, with a
    bootstrap interval over cases (the lines of one block are not independent
    draws; cases are closer to it)."""
    cases = [c for c in load_cases(a.cases) if scored(c) and not excluded(c, a.exclude)]
    if a.sets:
        cases = [c for c in cases if c["set"] in a.sets.split(",")]
    grades = load_grades(a.grades)
    res = load_results(a.out)
    base, cand = a.arms.split(",")
    rows, broken = [], 0
    for c in cases:
        rb, rc = res[base].get(c["id"]), res[cand].get(c["id"])
        if rb is None or rc is None:
            continue
        if failed(c, rb) or failed(c, rc):
            broken += 1  # an error printing nothing is no line removed
            continue
        count = lambda r, g: sum(1 for n in r["names"] if grade_of(c, n, grades) == g)
        rows.append((count(rc, "R") - count(rb, "R"), count(rc, "N") - count(rb, "N"),
                     len(rc["names"]) - len(rb["names"]), sum(1 for n in rc["names"] if not grade_of(c, n, grades))))
    if not rows:
        print(f"no case both arms ran without a failure ({broken} left out: an arm failed)")
        return
    rnd = random.Random(7)

    def interval(i):
        sums = sorted(sum(rows[rnd.randrange(len(rows))][i] for _ in rows) for _ in range(2000))
        return sums[50], sums[1949]

    changed = sum(1 for r in rows if r[0] or r[1] or r[2])
    print(f"{cand} against {base}: {len(rows)} cases, {changed} changed, {broken} left out (a call of either arm failed)")
    for i, what in ((0, "useful lines"), (1, "noise lines"), (2, "lines")):
        lo, hi = interval(i)
        print(f"  {what:13} {sum(r[i] for r in rows):+d} (95% bootstrap over cases {lo:+d} to {hi:+d})")
    print(f"  lines of {cand} no grade covers: {sum(r[3] for r in rows)}")


def todo(a):
    cases = {c["id"]: c for c in load_cases(a.cases)}
    grades = load_grades(a.grades)
    res = load_results(a.out)
    want = {}
    for arm, rs in res.items():
        for cid, r in rs.items():
            c = cases.get(cid)
            if not c or not scored(c):
                continue
            for n, line in zip(r["names"], r["lines"]):
                if not grade_of(c, n, grades):
                    want[(cid, n)] = line
    keys = sorted(want)
    random.Random(7).shuffle(keys)
    for cid, n in keys:
        c = cases[cid]
        what = c["prompt"] if c["event"] == "prompt" else json.dumps(c.get("tool_input"))[:400]
        print("\t".join([cid, n, "", re.sub(r"\s+", " ", what)[:600], want[(cid, n)][:400]]))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--store", required=True, help="a clone of the store to check out commits in (never pushed)")
    r.add_argument("--cases", required=True)
    r.add_argument("--out", required=True)
    r.add_argument("--arm", action="append", required=True)
    r.add_argument("--index-cache", help="index files by schema and commit, shared across runs")
    r.add_argument("--workers", type=int, default=2, help="worktrees checked out in parallel")
    r.add_argument("--jobs", type=int, default=4, help="hook calls in parallel per worktree")
    r.add_argument("--simulate-start", action="store_true")
    r.add_argument("--sets", help="only the cases of these label sets (comma-separated)")
    r.add_argument("--state-file", action="append", help="NAME=PATH: a file every call's state directory holds")
    for name in ("score", "compare", "todo"):
        s = sub.add_parser(name)
        s.add_argument("--cases", required=True)
        s.add_argument("--out", required=True)
        s.add_argument("--grades", action="append")
        if name != "todo":
            s.add_argument("--arms", required=name == "compare")
            s.add_argument("--exclude", action="append", help="drop cases whose cwd_note has this tag")
        if name == "score":
            s.add_argument("--json")
        if name == "compare":
            s.add_argument("--sets")
    a = ap.parse_args()
    if a.cmd == "run":
        a.store = os.path.abspath(a.store)
        dirty = subprocess.run(["git", "-C", a.store, "status", "--porcelain"], capture_output=True, text=True).stdout
        if dirty.strip():
            sys.exit("the store clone must be clean")
        remote = subprocess.run(["git", "-C", a.store, "remote", "get-url", "origin"], capture_output=True, text=True).stdout.strip()
        if remote and (os.path.exists(remote) or "://" in remote or "@" in remote):
            sys.exit(f"the store clone's origin ({remote}) is reachable, so a hook's sync could move a worktree: "
                     "point it at nothing first (git remote set-url origin /nonexistent)")
        os.makedirs(a.out, exist_ok=True)
        cases = load_cases(a.cases)
        if a.sets:
            cases = [c for c in cases if c["set"] in a.sets.split(",")]
        Runner(a).run(cases)
    elif a.cmd == "score":
        score(a)
    elif a.cmd == "compare":
        compare(a)
    else:
        todo(a)


if __name__ == "__main__":
    main()
