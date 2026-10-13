#!/usr/bin/env python3
"""Live A/B: does a SessionStart fact block change what the agent does?

Each task in tasks.json names a situation from the truth set, a prompt, a
working directory, the tools allowed, a success check on the final answer,
and detour markers (strings in tool results that show the known detour
happened). Every task runs in two arms with the same model and prompt:

  bare   no block
  block  a SessionStart hook prints the fact block for the task's repo/host

Metrics per run: tool calls, Bash calls, detour hits, success, input/output
tokens, cost, duration. Results go to eval/ab/results/<stamp>.jsonl (gitignored)
plus a summary table on stdout. Runs are independent `claude -p` processes.

  python3 eval/ab/run.py --reps 1              # one pass, both arms
  python3 eval/ab/run.py --tasks T1,T3 --reps 3
  python3 eval/ab/run.py --tasks-file eval/examples/tasks.sample.json \
      --truth eval/examples/truth.sample.jsonl  # the synthetic sample

Tasks: --tasks-file, else $TACIT_AB_TASKS, else eval/ab/tasks.json if present,
else eval/examples/tasks.sample.json. Truth (for the oracle arm): --truth, else
delivery.py's default. The block's store and host come from TACIT_STORE and
TACIT_HOST as in delivery.py; TACIT_AB_MODEL picks the working model (opus).
"""
import argparse
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
SCRATCH = Path(os.path.expanduser(os.environ.get("TACIT_AB_SCRATCH", str(Path.home() / ".cache/tacit-ab"))))
MODEL = os.environ.get("TACIT_AB_MODEL", "opus")
TRUTH = None  # set from --truth in main


def default_tasks():
    env = os.environ.get("TACIT_AB_TASKS")
    if env:
        return Path(os.path.expanduser(env))
    own = HERE / "tasks.json"
    return own if own.exists() else ROOT / "eval/examples/tasks.sample.json"


def arm_cap(arm, default=40):
    m = re.search(r"(\d+)c?$", arm)
    return int(m.group(1)) if m else default


def arm_store(arm):
    """Arms ending in `c` read the curated scratch store (eval/curate/split.py output)."""
    return os.environ.get("TACIT_CURATED", str(Path.home() / ".cache/tacit-curated")) if arm.endswith("c") else None


def build_block(task, oracle=False, cap=None, store=None):
    """Fact block for the task: eval/delivery ranking over the store for (repo, host).
    oracle=True forces the truth item's stored fact into the block (ceiling arm)."""
    sys.path.insert(0, str(ROOT / "eval/delivery"))
    import delivery  # noqa: E402

    facts = delivery.load_store(store)
    repo = task["repo"].lower()
    scopes = set(delivery.eligible_scopes(repo))
    eligible = [f for f in facts.values() if f["scope"] in scopes]
    from collections import Counter

    df = Counter()
    for f in facts.values():
        for t in set(delivery.terms(f["name"] + " " + f["description"] + " " + f["body"])):
            df[t] += 1
    sit = {"situation": {"paths": task.get("paths", []), "command": task.get("command", ""), "repo": repo, "intent": task["intent"]}, "query": None, "mode": "push"}
    cap = cap or task.get("cap", 40)
    ranked = delivery.rank(eligible, delivery.terms(delivery.situation_text(sit, False)), df, len(facts))[:cap]
    if oracle:
        truth = {r["id"]: r for r in delivery.load_truth(TRUTH)}
        em = truth[task["truth"]].get("existing_memory")
        ip = delivery.imported_path(em) if em else None
        # The ceiling: the fact as a curated one-fact entry would state it (the
        # truth-set wording). Imported memory files bundle many facts behind
        # one description line, so their description rarely carries it.
        ranked = [p for p in ranked if p != ip][: cap - 1]
        forced = f"- {task['truth']}: {' '.join(truth[task['truth']]['fact'].split())}"
    lines = ["# Facts for this machine and repo (tacit, read before acting)", ""]
    for p in ranked:
        f = facts[p]
        lines.append(f"- {f['name']}: {' '.join(f['description'].split())}")
    if oracle:
        # In the middle, so position alone does not carry it.
        lines.insert(2 + len(ranked) // 2, forced)
    return "\n".join(lines) + "\n"


def setup_scratch(task, tag=""):
    d = SCRATCH / (task["id"] + tag)
    if d.exists():
        shutil.rmtree(d)
    d.mkdir(parents=True)
    for rel, content in (task.get("files") or {}).items():
        p = d / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content)
        if content.startswith("#!"):
            p.chmod(0o755)
    for cmd in task.get("setup") or []:
        subprocess.run(cmd, shell=True, cwd=d, check=True, capture_output=True)
    return d


def run_once(task, arm, block_path, rep):
    cwd = setup_scratch(task, f"-{arm}-{rep}") if task.get("scratch") else Path(os.path.expanduser(task["cwd"]))
    import uuid
    sid = str(uuid.uuid4())
    # Keep eval sessions out of the dreamer (they are synthetic prompts).
    excl = Path.home() / ".local/state/tacit/dream-exclude"
    excl.parent.mkdir(parents=True, exist_ok=True)
    with open(excl, "a") as fh:
        fh.write(sid + "\n")
    cmd = [
        "claude", "-p", task["prompt"], "--session-id", sid, "--model", MODEL, "--output-format", "stream-json", "--verbose",
        "--max-turns", str(task.get("max_turns", 25)),
    ]
    if task["allowed_tools"]:
        cmd += ["--allowedTools", *task["allowed_tools"], "--disallowedTools", "Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch", "Agent"]
    else:
        cmd += ["--tools", ""]  # decision tasks: one turn, no tools
    # `live`: no injected block; whatever hooks are installed in the user's
    # settings run (the shipped tacit hooks after 2026-10-08).
    if arm not in ("bare", "live"):
        path = SCRATCH / f"{task['id']}-{arm}.md"
        settings = {"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": f"cat {path}"}]}]}}
        cmd += ["--settings", json.dumps(settings)]
    env = {**os.environ, "TACIT_AB": "1"}
    t0 = time.time()
    try:
        proc = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=task.get("timeout", 900), env=env)
    except subprocess.TimeoutExpired as e:
        # A hung run is a failed cell, not a failed experiment.
        class P:
            stdout = (e.stdout or b"").decode() if isinstance(e.stdout, bytes) else (e.stdout or "")
            stderr = "timeout"
            returncode = -1
        proc = P()
    dur = time.time() - t0
    tool_calls, bash_cmds, detours, result = 0, [], 0, {}
    markers = task.get("detour_markers", [])
    for line in proc.stdout.splitlines():
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        if ev.get("type") == "assistant":
            for b in ev.get("message", {}).get("content", []):
                if b.get("type") == "tool_use":
                    tool_calls += 1
                    if b.get("name") == "Bash":
                        bash_cmds.append(b.get("input", {}).get("command", ""))
        elif ev.get("type") == "user":
            for b in ev.get("message", {}).get("content", []) or []:
                if b.get("type") == "tool_result":
                    txt = b.get("content")
                    if isinstance(txt, list):
                        txt = " ".join(x.get("text", "") for x in txt if isinstance(x, dict))
                    txt = str(txt or "")
                    detours += sum(1 for m in markers if re.search(m, txt))
        elif ev.get("type") == "result":
            result = ev
    cmd_detours = sum(1 for c in bash_cmds for m in task.get("command_markers", []) if re.search(m, c))
    answer = result.get("result") or ""
    ok = bool(re.search(task["success"], answer, re.I | re.S)) if task.get("success") else None
    if task.get("post_check"):
        ok = ok and subprocess.run(task["post_check"], shell=True, cwd=cwd).returncode == 0
    if task.get("teardown"):
        subprocess.run(task["teardown"], shell=True, cwd=cwd, capture_output=True)
    usage = result.get("usage") or {}
    return {
        "task": task["id"], "arm": arm, "rep": rep, "ok": ok,
        "tool_calls": tool_calls, "bash_calls": len(bash_cmds), "detours": detours + cmd_detours,
        "turns": result.get("num_turns"), "subtype": result.get("subtype"), "cost": result.get("total_cost_usd"),
        "in_tokens": (usage.get("input_tokens") or 0) + (usage.get("cache_creation_input_tokens") or 0) + (usage.get("cache_read_input_tokens") or 0),
        "out_tokens": usage.get("output_tokens"), "seconds": round(dur, 1),
        "exit": proc.returncode, "answer": answer[:600], "bash": bash_cmds[:20], "stderr": proc.stderr[-400:],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", help="comma list of task ids")
    ap.add_argument("--reps", type=int, default=1)
    ap.add_argument("--arms", default="bare,block", help="bare, block[N], oracle[N]; N caps the block lines (default 40)")
    ap.add_argument("--workers", type=int, default=1)
    ap.add_argument("--resume", help="results file; cells already present there are skipped and merged into the summary")
    ap.add_argument("--tasks-file", help="tasks JSON (default: $TACIT_AB_TASKS, eval/ab/tasks.json, else the sample)")
    ap.add_argument("--truth", help="truth set for the oracle arm (default: delivery.py's)")
    ap.add_argument("--out-dir", default=str(HERE / "results"), help="where results go (default eval/ab/results)")
    args = ap.parse_args()
    global TRUTH
    TRUTH = args.truth
    tasks = json.load(open(args.tasks_file or default_tasks()))
    if args.tasks:
        want = set(args.tasks.split(","))
        tasks = [t for t in tasks if t["id"] in want]
    SCRATCH.mkdir(parents=True, exist_ok=True)
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    stamp = time.strftime("%Y%m%dT%H%M%S")
    out = open(out_dir / f"{stamp}.jsonl", "a")
    arms = args.arms.split(",")
    jobs = []
    for task in tasks:
        for arm in arms:
            if arm not in ("bare", "live"):
                (SCRATCH / f"{task['id']}-{arm}.md").write_text(build_block(task, oracle=arm.startswith("oracle"), cap=arm_cap(arm), store=arm_store(arm)))
        for rep in range(args.reps):
            for arm in arms:
                jobs.append((task, arm, rep))
    rows = []
    if args.resume:
        done_rows = [json.loads(l) for l in open(args.resume) if l.strip()]
        done = {(r["task"], r["arm"], r["rep"]) for r in done_rows}
        jobs = [j for j in jobs if (j[0]["id"], j[1], j[2]) not in done]
        rows += done_rows
        print(f"resume: {len(done)} cells done, {len(jobs)} to run")
    from concurrent.futures import ThreadPoolExecutor, as_completed

    with ThreadPoolExecutor(max_workers=args.workers) as ex:
        futs = [ex.submit(run_once, task, arm, None, rep) for task, arm, rep in jobs]
        for fut in as_completed(futs):
            r = fut.result()
            rows.append(r)
            out.write(json.dumps(r) + "\n")
            out.flush()
            print(f"{r['task']:4s} {r['arm']:8s} rep{r['rep']} ok={r['ok']} tools={r['tool_calls']:2d} detours={r['detours']} cost=${(r['cost'] or 0):.2f} in={r['in_tokens']} out={r['out_tokens']} {r['seconds']}s", flush=True)
    summarize(rows)


def summarize(rows):
    print("\nsummary (median over reps)")
    print(f"{'task':5s} {'arm':6s} {'ok':>4s} {'tools':>6s} {'detour':>6s} {'cost':>6s} {'in_tok':>8s} {'out_tok':>7s}")
    by = {}
    for r in rows:
        by.setdefault((r["task"], r["arm"]), []).append(r)
    for (t, a), rs in sorted(by.items()):
        med = lambda k: statistics.median([x[k] or 0 for x in rs])
        oks = sum(1 for x in rs if x["ok"])
        print(f"{t:5s} {a:6s} {oks:>2d}/{len(rs):<1d} {med('tool_calls'):6.0f} {med('detours'):6.0f} {med('cost'):6.2f} {med('in_tokens'):8.0f} {med('out_tokens'):7.0f}")
    for a in sorted({r["arm"] for r in rows}):
        rs = [r for r in rows if r["arm"] == a]
        if rs:
            print(f"{a}: tasks ok {sum(1 for r in rs if r['ok'])}/{len(rs)}, detours {sum(r['detours'] for r in rs)}, tools {sum(r['tool_calls'] for r in rs)}, cost ${sum(r['cost'] or 0 for r in rs):.2f}")


if __name__ == "__main__":
    sys.exit(main())
