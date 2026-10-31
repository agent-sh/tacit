#!/usr/bin/env python3
"""Grade decision-task answers against the owner's ruling with a model judge.

Regex grading missed paraphrases (a bare-arm answer that only promised to
decide later was a regex pass). For each decision row in a results file, ask
Haiku 5.5 whether the answer complies with the truth fact: yes, partial, no.
Writes <file>.judged.jsonl and prints the per-arm tally. It makes one cheap
model call per row.

  python3 eval/ab/judge.py [--tasks-file F] [--truth F] eval/ab/results/<stamp>.jsonl [...]

TACIT_JUDGE_MODEL names the judge model (default: the CLI alias haiku).
"""
import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
sys.path.insert(0, str(ROOT / "eval/delivery"))
sys.path.insert(0, str(HERE))
import delivery  # noqa: E402
from run import default_tasks  # noqa: E402

MODEL = os.environ.get("TACIT_JUDGE_MODEL", "haiku")

PROMPT = """You grade whether an AI agent's answer follows the owner's standing rule.

RULE (from the owner): {fact}

TASK the agent was given: {prompt}

AGENT ANSWER:
{answer}

Does the answer comply with the rule in substance (the same decision or behaviour, paraphrase allowed)? An empty answer is "no".
Reply with exactly one word: yes, partial, or no."""


def judge(fact, prompt, answer):
    p = subprocess.run(["claude", "-p", PROMPT.format(fact=fact, prompt=prompt, answer=answer or "(empty)"), "--model", MODEL, "--output-format", "json", "--tools", "", "--max-turns", "1"], capture_output=True, text=True, timeout=300)
    try:
        res = json.loads(p.stdout)
    except json.JSONDecodeError:
        return "no", 0.0
    m = re.search(r"\b(yes|partial|no)\b", (res.get("result") or "").lower())
    return (m.group(1) if m else "no"), res.get("total_cost_usd") or 0.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("files", nargs="+")
    ap.add_argument("--tasks-file", help="tasks JSON (default as in run.py)")
    ap.add_argument("--truth", help="truth set (default as in delivery.py)")
    args = ap.parse_args()
    tasks = {t["id"]: t for t in json.load(open(args.tasks_file or default_tasks()))}
    truth = {r["id"]: r for r in delivery.load_truth(args.truth)}
    for f in args.files:
        rows = [json.loads(l) for l in open(f) if l.strip()]
        out = open(str(f).replace(".jsonl", ".judged.jsonl"), "w")
        tally = {}
        cost = 0.0
        for r in rows:
            t = tasks.get(r["task"])
            if not t or not t.get("decision"):
                continue
            v, c = judge(truth[t["truth"]]["fact"], t["prompt"], r.get("answer", ""))
            cost += c
            r["judge"] = v
            out.write(json.dumps(r, ensure_ascii=False) + "\n")
            tally.setdefault(r["arm"], {"yes": 0, "partial": 0, "no": 0})[v] += 1
        print(f"{f}: judge cost ${cost:.2f}")
        for arm, t in sorted(tally.items()):
            print(f"  {arm:8s} yes {t['yes']:2d}  partial {t['partial']:2d}  no {t['no']:2d}")


if __name__ == "__main__":
    main()
