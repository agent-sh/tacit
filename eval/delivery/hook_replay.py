#!/usr/bin/env python3
"""Replay labelled prompts through an actual binary, without live session writes.

Cases are private JSON: [{"split":"tune", "prompt":"...", "cwd":"...",
"expected":["relevant-fact-name"]}]. Expected lists must be labelled before
looking at the candidate's output. This measures delivery, not task outcomes.
Use a pinned store clone and a dedicated index/local-overlay directory. An
empty session ID disables delivered-state writes, making repeats independent.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import time


def run(binary, args, env, payload=None):
    p = subprocess.run([str(binary), *args], input=payload, text=True,
                       capture_output=True, env=env, timeout=30)
    if p.returncode or p.stderr.strip():
        raise RuntimeError(f"{args}: rc={p.returncode}: {p.stderr.strip()}")
    return p.stdout


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--binary', type=Path, required=True)
    ap.add_argument('--store', type=Path, required=True)
    ap.add_argument('--cases', type=Path, required=True)
    ap.add_argument('--out', type=Path, required=True)
    ap.add_argument('--repeats', type=int, default=3)
    a = ap.parse_args()
    if a.repeats < 1:
        ap.error('--repeats must be positive')
    a.out.mkdir(parents=True, exist_ok=True)
    cases = json.loads(a.cases.read_text())
    env = dict(os.environ, TACIT_STORE=str(a.store.resolve()),
               TACIT_INDEX=str(a.out.resolve()/'index'),
               TACIT_LOCAL=str(a.out.resolve()/'empty-overlay'))
    head = subprocess.check_output(['git', '-C', str(a.store), 'rev-parse', 'HEAD'], text=True).strip()
    # Prebuild the index outside the timed cells. Explicit search is not scored.
    run(a.binary, ['search', '--json', 'index-warmup'], env)
    cells = []
    for i, c in enumerate(cases):
        samples = []
        outputs = []
        for _ in range(a.repeats):
            start = time.perf_counter()
            text = run(a.binary, ['hook', 'prompt'], env,
                       json.dumps(dict(session_id='', cwd=c['cwd'], prompt=c['prompt'])))
            samples.append((time.perf_counter()-start)*1000)
            outputs.append(text)
        if len(set(outputs)) != 1:
            raise RuntimeError(f'non-deterministic output for case {i}')
        names = []
        for line in outputs[0].splitlines():
            if line.startswith('- '):
                name = line[2:].removeprefix('RULE ').split(':',1)[0].split(' (',1)[0]
                names.append(name)
        expected = set(c['expected'])
        cells.append(dict(case=i, **c, names=names, output=outputs[0],
                          useful=sorted(set(names)&expected),
                          unexpected=sorted(set(names)-expected),
                          latency_ms=samples, median_ms=statistics.median(samples)))
    summary = {}
    for split in sorted({c['split'] for c in cases}):
        group = [c for c in cells if c['split']==split]
        positives = [c for c in group if c['expected']]
        negatives = [c for c in group if not c['expected']]
        summary[split] = dict(cases=len(group), delivered=sum(len(c['names']) for c in group),
            useful=sum(len(c['useful']) for c in group),
            unexpected=sum(len(c['unexpected']) for c in group),
            positive_queries_hit=sum(bool(c['useful']) for c in positives),
            positive_queries=len(positives),
            negative_queries_injected=sum(bool(c['names']) for c in negatives),
            negative_queries=len(negatives), median_ms=statistics.median(c['median_ms'] for c in group))
    result = dict(store_head=head, binary_sha256=hashlib.sha256(a.binary.read_bytes()).hexdigest(),
                  cases_sha256=hashlib.sha256(a.cases.read_bytes()).hexdigest(),
                  repeats=a.repeats, summary=summary, cells=cells)
    (a.out/'result.json').write_text(json.dumps(result, indent=2)+'\n')
    print(json.dumps(summary, indent=2))


if __name__ == '__main__':
    main()
