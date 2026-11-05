#!/usr/bin/env python3
"""Recall benchmark: getting a detail back from a session's own transcript,
by hand (grep) and with `tacit episode`.

For each transcript size, corpus.py writes one synthetic Claude Code session
with 16 planted tasks (ids, commands, paths, errors; see corpus.py). Each task
is recovered by two arms that follow fixed rules, so nothing depends on a model:

  by hand  `grep -n -i -F w1 FILE | grep -i -F w2 | grep -i -F w3`, given
           the transcript file: the matching lines in full. The answer is the first value
           of the target's shape in the newest line that holds one.
  tacit    `tacit episode find w1 w2 w3`, read newest first. A line whose
           clip holds a value of the target's shape gives the answer; a tool
           event whose clip holds none is read whole (`show`, at most 3 in a
           task) before an older line is taken. While no line of a page
           gives one, the next page (`--before`). At most 10 calls.

Per task and arm: found and right (the answer is the planted value, not the
older decoy), tool calls, characters brought into context (everything the
calls printed), planted secrets that reached the context, and latency. Every
call is timed over separate process launches (--reps, default 5); a task's
latency is the median of its repetitions' totals. The summary gives, per size
and arm, the median over tasks with the min and max.

Writes docs/images/recall-tasks.csv, docs/images/recall-summary.csv,
docs/images/recall-run.json (tool versions, seed, launches), the
charts (charts.py) and README.md's recall table. From the repository root:

    python3 -I eval/recall/bench.py

Needs grep, scrump (tacit redacts with it) and tacit on PATH, or --tacit
PATH; the committed run's tacit version is in recall-run.json. A tool that
fails, or a tacit answer cut by its time limit, stops the run.
"""

import argparse
import csv
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import corpus  # noqa: E402
import charts  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(HERE))
# - <ref> <date> <time> <who> <kind>[ <tool>][ error]: <clip>
LINE = re.compile(r"^[->] (\d+\.\d+) \S+ \S+ \S+ ([a-z_]+)(?: [^:]*)?: (.*)$")
MAX_CALLS = 10
MAX_SHOWS = 3


def timed(argv_list, env, ok_codes):
    """Runs a pipeline of argvs as separate processes; returns its output
    and the wall time from the first launch to the last exit. A process that
    exits with a code outside ok_codes stops the run."""
    t0 = time.perf_counter()
    procs = []
    prev = None
    for argv in argv_list:
        p = subprocess.Popen(argv, stdin=prev.stdout if prev else subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
        if prev:
            prev.stdout.close()
        procs.append(p)
        prev = p
    out = procs[-1].stdout.read()
    for p in procs:
        err = p.stderr.read()
        p.wait()
        if p.returncode not in ok_codes:
            raise SystemExit("%s exited %d: %s" % (" ".join(p.args), p.returncode, err.decode("utf-8", "replace").strip()))
    secs = time.perf_counter() - t0
    return out.decode("utf-8", "replace"), secs


class Call:
    def __init__(self, argv_list, env, reps, ok_codes):
        self.outs, self.secs = [], []
        for _ in range(reps):
            o, s = timed(argv_list, env, ok_codes)
            self.outs.append(o)
            self.secs.append(s)
        if len(set(self.outs)) != 1:
            raise SystemExit("a call printed different output across launches: %r" % (argv_list,))
        self.out = self.outs[0]
        if "time limit" in self.out:
            raise SystemExit("tacit hit a time limit, so the answer depends on this machine: %r" % (argv_list,))


def by_hand(task, path, env, reps):
    w = task["words"]
    argv = [["grep", "-n", "-i", "-F", "--", w[0], path]] + [["grep", "-i", "-F", "--", x] for x in w[1:]]
    c = Call(argv, env, reps, (0, 1))  # 1: no line matched
    shape = re.compile(task["shape"])
    answer = None
    for line in reversed(c.out.splitlines()):
        m = shape.search(line)
        if m:
            answer = m.group(0)
            break
    return answer, [c]


def with_tacit(task, tacit, session, env, reps):
    shape = re.compile(task["shape"])
    calls, shows = [], 0
    base = [tacit, "episode", "--harness", "claude", "--session", session]
    before = None
    while len(calls) < MAX_CALLS:
        argv = base + (["--before", before] if before else []) + ["find"] + task["words"]
        c = Call([argv], env, reps, (0,))
        calls.append(c)
        refs = []
        # Newest first: a clip that holds a value is the answer; a tool
        # event whose clip holds none is read whole before an older line.
        for line in c.out.splitlines():
            m = LINE.match(line)
            if not m:
                continue
            refs.append(m.group(1))
            v = shape.search(m.group(3))
            if v:
                return v.group(0), calls
            if m.group(2).startswith("tool_") and shows < MAX_SHOWS and len(calls) < MAX_CALLS:
                s = Call([base + ["show", m.group(1)]], env, reps, (0,))
                calls.append(s)
                shows += 1
                v = shape.search(s.out)
                if v:
                    return v.group(0), calls
        if not refs or "# older:" not in c.out:
            break
        before = refs[-1]
    return None, calls


def tacit_env(work, scrump_dir, home):
    env = {"HOME": home, "PATH": scrump_dir + os.pathsep + "/usr/bin:/bin",
           "TACIT_STORE": os.path.join(work, "store"), "TACIT_STATE_DIR": os.path.join(work, "state"),
           "TACIT_INDEX": os.path.join(work, "index"), "TACIT_LOCAL": os.path.join(work, "overlay"),
           "LANG": os.environ.get("LANG", "C.UTF-8"), "TZ": "UTC"}
    for k in ("TACIT_STORE", "TACIT_STATE_DIR"):
        os.makedirs(env[k], exist_ok=True)
    return env


def score(arm, meta, task, answer, calls, secrets):
    ctx = "".join(c.out for c in calls)
    per_rep = [sum(c.secs[i] for c in calls) for i in range(len(calls[0].secs))]
    return {"mib": meta["mib"], "bytes": meta["bytes"], "arm": arm, "task": task["task"], "kind": task["kind"],
            "found_right": int(answer == task["value"]),
            "answer": "right" if answer == task["value"] else "decoy" if answer and answer == task["decoy"] else "other" if answer else "none", "calls": len(calls), "chars": len(ctx),
            "markers": sum(1 for s in secrets if s in ctx),
            "latency_ms": round(1000 * statistics.median(per_rep), 1),
            "latency_ms_min": round(1000 * min(per_rep), 1), "latency_ms_max": round(1000 * max(per_rep), 1)}


def summarize(rows):
    out = []
    keys = sorted({(r["mib"], r["arm"]) for r in rows})
    for mib, arm in keys:
        rs = [r for r in rows if r["mib"] == mib and r["arm"] == arm]
        s = {"mib": mib, "bytes": rs[0]["bytes"], "arm": arm, "tasks": len(rs),
             "found_right": sum(r["found_right"] for r in rs)}
        for k in ("calls", "chars", "markers", "latency_ms"):
            v = [r[k] for r in rs]
            s[k + "_median"] = round(statistics.median(v), 2)
            s[k + "_min"], s[k + "_max"] = min(v), max(v)
        out.append(s)
    return out


def write_csv(path, rows):
    with open(path, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), lineterminator="\n")
        w.writeheader()
        w.writerows(rows)


def readme(out):
    """README.md when the output is the repository's own docs/images: its
    recall table is rewritten with the charts."""
    if os.path.abspath(out) == os.path.join(ROOT, "docs", "images"):
        return os.path.join(ROOT, "README.md")
    return None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--tacit", default=shutil.which("tacit") or "tacit", help="tacit binary (default: on PATH)")
    ap.add_argument("--sizes", default="1,2,4,8,16,32,64", help="transcript sizes in MiB")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--reps", type=int, default=5, help="process launches per call")
    ap.add_argument("--work", default=os.path.join(os.path.expanduser("~"), ".cache", "tacit-recall-bench"),
                    help="scratch directory for the transcripts")
    ap.add_argument("--out", default=os.path.join(ROOT, "docs", "images"), help="where the CSVs and charts go")
    ap.add_argument("--charts-only", action="store_true", help="redraw the charts from the summary CSV")
    a = ap.parse_args()
    if a.charts_only:
        charts.render(os.path.join(a.out, "recall-summary.csv"), a.out, readme(a.out))
        return 0
    scrump = shutil.which("scrump")
    if not scrump:
        raise SystemExit("scrump is not on PATH: tacit episode shows nothing without it")
    tacit = os.path.abspath(a.tacit) if os.sep in a.tacit else shutil.which(a.tacit)
    if not tacit or not os.access(tacit, os.X_OK):
        raise SystemExit("tacit not found: pass --tacit PATH")
    version = subprocess.run([tacit, "--version"], capture_output=True, text=True).stdout.strip()
    scrump_version = subprocess.run([scrump, "--version"], capture_output=True, text=True).stdout.strip()
    grep_version = subprocess.run(["grep", "--version"], capture_output=True, text=True).stdout.splitlines()[0]
    rows = []
    for mib in [int(x) for x in a.sizes.split(",")]:
        work = os.path.join(a.work, "%dmib" % mib)
        meta = corpus.generate(work, mib, a.seed)
        secrets = [s for t in meta["tasks"] for s in t["secrets"]]
        env = tacit_env(work, os.path.dirname(scrump), meta["home"])
        for task in meta["tasks"]:
            ans, calls = by_hand(task, meta["transcript"], os.environ.copy(), a.reps)
            rows.append(score("by hand", meta, task, ans, calls, secrets))
            ans, calls = with_tacit(task, tacit, meta["session"], env, a.reps)
            rows.append(score("tacit", meta, task, ans, calls, secrets))
        done = [r for r in rows if r["mib"] == mib]
        n = len(meta["tasks"])
        print("%3d MiB: by hand %d/%d right, tacit %d/%d right" % (
            mib, sum(r["found_right"] for r in done if r["arm"] == "by hand"), n,
            sum(r["found_right"] for r in done if r["arm"] == "tacit"), n), file=sys.stderr)
    os.makedirs(a.out, exist_ok=True)
    write_csv(os.path.join(a.out, "recall-tasks.csv"), rows)
    summary = summarize(rows)
    write_csv(os.path.join(a.out, "recall-summary.csv"), summary)
    with open(os.path.join(a.out, "recall-run.json"), "w") as f:
        json.dump({"tacit": version, "scrump": scrump_version, "grep": grep_version, "python": sys.version.split()[0],
                   "sizes": a.sizes, "seed": a.seed, "reps": a.reps}, f, indent=1)
        f.write("\n")
    print("%s, seed %d, %d launches per call" % (version, a.seed, a.reps), file=sys.stderr)
    for s in summary:
        print("%3d MiB %-7s right %2d/%d  calls %s  chars %s (%s..%s)  markers %s (%s..%s)  ms %s (%s..%s)" % (
            s["mib"], s["arm"], s["found_right"], s["tasks"], s["calls_median"], s["chars_median"], s["chars_min"],
            s["chars_max"], s["markers_median"], s["markers_min"], s["markers_max"], s["latency_ms_median"],
            s["latency_ms_min"], s["latency_ms_max"]))
    charts.render(os.path.join(a.out, "recall-summary.csv"), a.out, readme(a.out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
