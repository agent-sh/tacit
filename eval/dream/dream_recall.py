#!/usr/bin/env python3
"""Dreamer recall on the known gaps.

The truth situations with no stored fact anywhere each point at a digest
window where the detour happened
(`evidence.session`, a `path:lines` reference into the digests that
eval/mine/digest.py writes). The dreamer's job is to read such deltas
and write the fact nobody wrote. This measures whether it would have:

  for each gap item: digest window (evidence.session path:lines, +-WINDOW)
    -> model extracts facts in SPEC shape (JSON)
    -> judge: does any extracted fact state the truth fact? (yes/partial/no)

Reports recall, facts per window, tokens and cost per item. Model defaults
to opus (the dreamer's default); the judge to haiku.

  python3 eval/dream/dream_recall.py [--truth F] [--limit N] [--model opus] [--window 120]

Models: --model (default $TACIT_DREAM_MODEL, else the CLI alias opus) and
--judge-model (default $TACIT_JUDGE_MODEL, else haiku). Output goes to
$TACIT_DREAM_OUT, else ~/.cache/tacit-dream.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OUT = Path(os.path.expanduser(os.environ.get("TACIT_DREAM_OUT", str(Path.home() / ".cache/tacit-dream"))))
sys.path.insert(0, str(ROOT / "eval/delivery"))
import delivery  # noqa: E402

EXTRACT = """You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Extract the durable facts another agent would need next time: owner standards and rulings (from U: lines only), machine facts (paths, tools, hosts, what is dead), past mistakes with their fix, design decisions. Skip anything derivable from the code or from git log, and skip session narrative.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}}}]
If there is nothing durable, output [].

Repo: {repo}  Host: {host}
DIGEST:
{digest}
"""

EXTRACT_V2 = """You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, quoted or closely paraphrased, with the date if visible.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}}}]

Repo: {repo}  Host: {host}
DIGEST:
{digest}
"""

# v3 (2026-10-11): v2 plus the task-only skip rule (measured: it lost owner rules and fixes, so it did not ship).
EXTRACT_V3 = """You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, quoted or closely paraphrased, with the date if visible.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}}}]

Repo: {repo}  Host: {host}
DIGEST:
{digest}
"""

# v4 (2026-10-11): v3 that keeps failures and owner rules; the dreamer's prompt until v5.
EXTRACT_V4 = """You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, quoted or closely paraphrased, with the date if visible.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}}}]

Repo: {repo}  Host: {host}
DIGEST:
{digest}
"""

# v5 (2026-10-18): v4 plus the owner's exact words in "quote" for a ruling; the dreamer's prompt since then.
EXTRACT_V5 = """You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, with the date if visible. For each one, copy the owner's exact words from the U: line into "quote": verbatim, typos included, at least the words that state the rule. Without the owner's own words a ruling cannot be told from an agent's guess.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "quote": "<kind user only: the owner's exact words>", "applies_to": {{"paths": [], "repos": [], "hosts": [], "commands": []}}}}]

Repo: {repo}  Host: {host}
DIGEST:
{digest}
"""

JUDGE = """Does any of the CANDIDATE facts state the TARGET fact (same conclusion, same key path/command/host/number), such that an agent reading the candidate would avoid the detour the target describes?
Answer with exactly one word: yes, partial, or no.

TARGET: {target}

CANDIDATES:
{cands}
"""


BARE = False


def claude(prompt, model, tools=False):
    cmd = ["claude", "-p", prompt, "--model", model, "--output-format", "json", "--max-turns", "1"]
    if BARE:
        cmd += ["--bare", "--no-session-persistence"]
    if not tools:
        cmd += ["--tools", ""]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=600)
    try:
        res = json.loads(p.stdout)
    except json.JSONDecodeError:
        return "", 0.0, {}
    u = res.get("usage") or {}
    return res.get("result") or "", res.get("total_cost_usd") or 0.0, u


def parse_json_array(text):
    text = re.sub(r"^```(?:json)?\s*|\s*```$", "", text.strip())
    s, e = text.find("["), text.rfind("]")
    if s < 0 or e < 0:
        return []
    try:
        v = json.loads(text[s : e + 1])
        return [x for x in v if isinstance(x, dict) and x.get("description")]
    except json.JSONDecodeError:
        return []


def window(session_ref, width):
    path, _, lines = session_ref.partition(":")
    p = Path(os.path.expanduser(path))
    if not p.exists():
        return None
    text = p.read_text(errors="replace").split("\n")
    if lines:
        a = int(lines.split("-")[0])
        b = int(lines.split("-")[-1]) if "-" in lines else a
    else:
        a, b = 1, len(text)
    lo, hi = max(0, a - 1 - width), min(len(text), b + width)
    return "\n".join(text[lo:hi])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--truth", help="truth set (default as in delivery.py)")
    ap.add_argument("--model", default=os.environ.get("TACIT_DREAM_MODEL", "opus"))
    ap.add_argument("--window", type=int, default=120)
    ap.add_argument("--prompt", choices=["v1", "v2", "v3", "v4", "v5"], default="v1")
    ap.add_argument("--ids", help="file with truth ids, one per line, to restrict the run")
    ap.add_argument("--judge-model", default=os.environ.get("TACIT_JUDGE_MODEL", "haiku"))
    ap.add_argument("--bare", action="store_true", help="no CLAUDE.md, hooks or saved session (what the dreamer runs)")
    args = ap.parse_args()
    global BARE
    BARE = args.bare
    OUT.mkdir(parents=True, exist_ok=True)
    truth = delivery.load_truth(args.truth)
    items = [t for t in truth if not t.get("existing_memory")]
    if args.ids:
        want = {l.strip() for l in open(args.ids) if l.strip()}
        items = [t for t in items if t["id"] in want]
    if args.limit:
        items = items[: args.limit]
    tmpl = {"v1": EXTRACT, "v2": EXTRACT_V2, "v3": EXTRACT_V3, "v4": EXTRACT_V4, "v5": EXTRACT_V5}[args.prompt]
    tag = f"{args.prompt}-{args.model.split('.')[-1]}"
    out = open(OUT / f"recall-{tag}-{time.strftime('%Y%m%dT%H%M%S')}.jsonl", "w")
    tally = {"yes": 0, "partial": 0, "no": 0, "skipped": 0}
    cost = 0.0
    tok_in = 0
    for it in items:
        dig = window(it["evidence"]["session"], args.window)
        if not dig:
            tally["skipped"] += 1
            continue
        dig = dig[:60_000]
        text, c1, u1 = claude(tmpl.format(repo=it["situation"].get("repo"), host=it["situation"].get("host"), digest=dig), args.model)
        facts = parse_json_array(text)
        cands = "\n".join(f"- {f['description']}" for f in facts) or "(none)"
        verdict, c2, u2 = claude(JUDGE.format(target=it["fact"], cands=cands), args.judge_model)
        v = re.search(r"\b(yes|partial|no)\b", verdict.lower())
        v = v.group(1) if v else "no"
        tally[v] += 1
        cost += c1 + c2
        tok_in += (u1.get("input_tokens") or 0) + (u1.get("cache_creation_input_tokens") or 0) + (u1.get("cache_read_input_tokens") or 0)
        row = {"id": it["id"], "type": it["fact_type"], "mode": it["mode"], "verdict": v, "n_facts": len(facts), "digest_chars": len(dig), "cost": round(c1 + c2, 4), "facts": [f["description"] for f in facts][:12], "target": it["fact"]}
        out.write(json.dumps(row, ensure_ascii=False) + "\n")
        out.flush()
        print(f"{it['id']} {it['fact_type']:16s} {v:8s} facts={len(facts):2d} ${c1 + c2:.3f}", flush=True)
    n = sum(tally[k] for k in ("yes", "partial", "no"))
    print(f"\nrecall: yes {tally['yes']}/{n}, partial {tally['partial']}/{n}, no {tally['no']}/{n}, skipped {tally['skipped']}; cost ${cost:.2f}, avg in-tokens {tok_in // max(1, n)}")


if __name__ == "__main__":
    sys.exit(main())
