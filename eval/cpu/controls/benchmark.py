"""Synthetic CPU contract, repeated warm timing and batch equivalence checks."""
import argparse
import json
import os
from pathlib import Path
import resource
import statistics
import subprocess
import sys
import time
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import add_workspace, load_rows, workspace


def main():
    p = argparse.ArgumentParser(description=__doc__)
    add_workspace(p)
    p.add_argument('--model', choices=['minilm', 'tiny'], default='minilm')
    p.add_argument('--input', default=str(Path(__file__).resolve().parents[1] / 'fixtures/smoke.jsonl'))
    p.add_argument('--output', required=True)
    p.add_argument('--runs', type=int, default=3)
    p.add_argument('--worker', action='store_true', help=argparse.SUPPRESS)
    a = p.parse_args()
    if a.runs < 1:
        p.error('--runs must be positive')
    if a.worker:
        import score
        score.ROOT = workspace('controls', a.workspace) / a.model
        rows = load_rows(a.input)
        scorer = score.Scorer()
        cold = [scorer.score(row) for row in rows]
        warm = [[scorer.score(row) for row in rows] for _ in range(3)]
        assert scorer.score({'id': 'empty', 'query': '', 'candidates': []})['scores'] == []
        assert len(scorer.score({'id': 'long', 'query': 'restart service ' * 1000, 'candidates': [{'id': 'long', 'text': 'restart service ' * 2000}]})['scores']) == 1
        scorer.batch_size = 1
        single = [scorer.score(row) for row in rows]
        differences = [abs(x['score'] - y['score']) for left, right in zip(cold, single) for x, y in zip(left['scores'], right['scores'])]
        assert max(differences, default=0) < 1e-4
        try:
            scorer.score({'id': 'bad', 'query': 'x', 'candidates': [{'id': 'bad', 'text': 123}]})
        except ValueError:
            pass
        else:
            raise AssertionError('malformed candidate accepted')
        result = {'init_ms': scorer.init_ms, 'cold': cold, 'warm': warm, 'batch_equivalence_max_abs': max(differences, default=0), 'max_rss_native': resource.getrusage(resource.RUSAGE_SELF).ru_maxrss, 'rss_unit': 'bytes' if sys.platform == 'darwin' else 'KiB', 'threads_after_scoring': len(list(Path('/proc/self/task').iterdir())) if Path('/proc/self/task').exists() else None, 'loadavg': os.getloadavg(), 'tests': ['empty_candidates', 'long_pair_truncation', 'batch_1_vs_16', 'malformed_candidate']}
    else:
        results = []
        for _ in range(a.runs):
            start = time.perf_counter()
            command = [sys.executable, __file__, '--worker', '--workspace', str(workspace('controls', a.workspace).parent), '--model', a.model, '--input', a.input, '--output', a.output]
            run = subprocess.run(command, capture_output=True, text=True, timeout=120, check=True)
            result = json.loads(run.stdout)
            result['subprocess_wall_ms'] = (time.perf_counter() - start) * 1000
            results.append(result)
        result = {'runs': results, 'median_init_ms': statistics.median(r['init_ms'] for r in results), 'median_warm_request_pool_ms': statistics.median(sum(x['elapsed_ms'] for x in warm) for r in results for warm in r['warm']), 'median_subprocess_wall_ms': statistics.median(r['subprocess_wall_ms'] for r in results)}
        Path(a.output).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))


if __name__ == '__main__':
    main()
