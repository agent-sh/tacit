"""Standard-library helpers for opt-in local CPU experiments."""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path


def workspace(group, value=None):
    root = Path(value or os.environ.get('TACIT_CPU_WORKSPACE', '~/.cache/tacit-cpu')).expanduser().resolve()
    repo = Path(__file__).resolve().parents[2]
    if root == repo or repo in root.parents:
        raise ValueError('workspace must be outside the repository')
    return root / group


def add_workspace(parser):
    parser.add_argument('--workspace', help='external cache root (or TACIT_CPU_WORKSPACE; default ~/.cache/tacit-cpu)')


def preflight_files(inputs, outputs):
    """Check the complete write set before the first write (including appends).

    Inputs may be files or read-only trees (models, source, retrieval store).
    Repeated input aliases are legal; outputs must not alias any input or each
    other. Resolve missing paths too, so dangling symlinks are covered. This is
    an accidental-clobber guard, not protection against concurrent path swaps.
    Pass the paths actually opened (expand '~' at the caller if it does so).
    '-' is a literal filename here; only streaming callers may exclude it.
    """
    sources = [Path(p).resolve() for p in inputs if p is not None]
    destinations = [Path(p).resolve() for p in outputs if p is not None]
    trees = [p for p in sources if p.is_dir()]
    def tree_files(tree, protected=None):
        seen = set()
        def fail(error):
            raise error
        for directory, dirs, files in os.walk(tree, followlinks=True, onerror=fail):
            stat = Path(directory).stat()
            identity = (stat.st_dev, stat.st_ino)
            if identity in seen:
                dirs[:] = []
                continue
            seen.add(identity)
            if protected is not None:
                protected.add(Path(directory).resolve())
            yield from ((Path(directory) / name).resolve() for name in files)
    protected = set(trees)
    for tree in trees:
        sources.extend(tree_files(tree, protected))
    trees = protected
    sources.extend(trees)
    # A generated cache/index directory may already contain aliased files.
    for tree in list(destinations):
        if tree.is_dir():
            if any(source.is_relative_to(tree) for source in sources):
                raise ValueError('input and output must be different files: ' + str(tree))
            destinations.extend(tree_files(tree))
    def same(left, right):
        return left == right or (left.exists() and right.exists() and os.path.samefile(left, right))
    for i, dest in enumerate(destinations):
        if (any(dest.is_relative_to(tree) for tree in trees) or
                any(same(dest, source) for source in sources) or
                any(same(dest, other) for other in destinations[:i])):
            raise ValueError('input and output must be different files; outputs must also be distinct: ' + str(dest))


def distinct_files(source, *destinations):
    """Streaming scorer convenience wrapper; '-' denotes stdin/stdout only."""
    preflight_files([] if str(source) == '-' else [source],
                    [p for p in destinations if str(p) != '-'])


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(8 * 1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def nice():
    try:
        os.nice(max(0, 10 - os.getpriority(os.PRIO_PROCESS, 0)))
    except (AttributeError, OSError):
        pass


def priority():
    try:
        return os.getpriority(os.PRIO_PROCESS, 0)
    except (AttributeError, OSError):
        return None


def peak_rss_kib():
    import resource
    import sys
    value = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    return value / 1024 if sys.platform == 'darwin' else value


def proc_status():
    path = Path('/proc/self/status')
    return path.read_text() if path.exists() else None


def cpu_name():
    path = Path('/proc/cpuinfo')
    return next((s.strip() for s in path.read_text().splitlines() if s.startswith('model name')), None) if path.exists() else None


def quant_engine(torch):
    engines = torch.backends.quantized.supported_engines
    for engine in ('x86', 'fbgemm', 'qnnpack'):
        if engine in engines:
            torch.backends.quantized.engine = engine
            return engine
    raise RuntimeError('no supported CPU dynamic quantization backend')


def load_rows(path):
    return [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]


def validate(rows, outputs, probability=False):
    if len(rows) != len(outputs):
        raise ValueError('row count mismatch')
    if len({row['id'] for row in rows}) != len(rows):
        raise ValueError('duplicate row IDs')
    for row, out in zip(rows, outputs):
        expected = [c['id'] for c in row['candidates']]
        if row['id'] != out['id'] or expected != [s['id'] for s in out['scores']] or len(set(expected)) != len(expected):
            raise ValueError('ID/order/duplicate candidate mismatch')
        for score in out['scores']:
            v = score['score']
            if not math.isfinite(v) or (probability and not 0 <= v <= 1):
                raise ValueError('invalid score')
    return {'rows': len(rows), 'pairs': sum(len(row['candidates']) for row in rows)}


def percentile(values, p):
    if not values:
        return None
    values = sorted(values)
    index = (len(values) - 1) * p
    low = int(index)
    return values[low] + (values[min(low + 1, len(values) - 1)] - values[low]) * (index - low)


def verify_artifacts(root, manifest):
    records = manifest.get('files', manifest.get('sha256', {}))
    if isinstance(records, list):
        records = {r['name']: r for r in records}
    root = Path(root).resolve()
    if not records:
        raise ValueError('empty artifact manifest')
    for name, item in records.items():
        path = (root / name).resolve()
        if root not in path.parents:
            raise ValueError('artifact escapes model directory')
        expected = item if isinstance(item, str) else item['sha256']
        if digest(path) != expected:
            raise ValueError('artifact hash mismatch: ' + name)
    return len(records)


def verify_main(probability=False):
    p = argparse.ArgumentParser(description='Offline ID/order, finite-score, optional warm repetition verification')
    p.add_argument('--input', required=True)
    p.add_argument('--output', required=True, help='scorer output to verify')
    p.add_argument('--reversed', help='second arm, restored to original candidate order')
    p.add_argument('--timings', help='optional Laya timings JSONL')
    p.add_argument('--repeats', type=int, default=3)
    p.add_argument('--measurements', help='optional Jina durable unique-payload measurements JSONL')
    p.add_argument('--runtime', help='optional Jina runtime event JSONL')
    p.add_argument('--report', required=True)
    a = p.parse_args()
    preflight_files([a.input, a.output, a.reversed, a.timings, a.measurements, a.runtime], [a.report])
    rows, outputs = load_rows(a.input), load_rows(a.output)
    result = validate(rows, outputs, probability)
    result['output_sha256'] = digest(a.output)
    if a.timings:
        timings = load_rows(a.timings)
        for repeat in range(1, a.repeats + 1):
            if [x['id'] for x in timings if x['repeat'] == repeat] != [x['id'] for x in rows]:
                raise ValueError('warm repetition ID/order mismatch')
        if len(timings) != len(rows) * a.repeats:
            raise ValueError('warm timing count mismatch')
        result['warm_repeats'] = a.repeats
        result['max_repeat_score_delta'] = max((x['max_score_delta'] for x in timings), default=0)
    if a.reversed:
        reverse = load_rows(a.reversed)
        validate(rows, reverse, probability)
        deltas, flips, rankings, tops, pairs, ties = [], 0, 0, 0, 0, 0
        seen = set()
        unique_changes = 0
        for row, natural, rev in zip(rows, outputs, reverse):
            ns, rs = [[x['score'] for x in arm['scores']] for arm in (natural, rev)]
            deltas.extend(abs(x-y) for x, y in zip(ns, rs))
            norder = sorted(range(len(ns)), key=lambda i: -ns[i])
            rorder = sorted(range(len(rs)), key=lambda i: -rs[i])
            rankings += norder != rorder
            tops += bool(ns) and norder[0] != rorder[0]
            key = json.dumps([row['query'], [c['text'] for c in row['candidates']]])
            if key not in seen:
                unique_changes += norder != rorder
                seen.add(key)
            for i in range(len(ns)):
                for j in range(i):
                    pairs += 1
                    flips += (ns[i]-ns[j]) * (rs[i]-rs[j]) < 0
                    ties += ((ns[i] == ns[j]) != (rs[i] == rs[j]))
        result['sensitivity'] = {'top1_changes': tops, 'ranking_changes': rankings, 'unique_ranking_changes': unique_changes, 'pair_order_flips': flips, 'candidate_pairs': pairs, 'tie_status_changes': ties, 'max_abs_delta': max(deltas, default=0), 'mean_abs_delta': sum(deltas)/len(deltas) if deltas else 0}
    if a.measurements:
        measurements = load_rows(a.measurements)
        cache = {m['key']: m for m in measurements}
        if len(cache) != len(measurements):
            raise ValueError('duplicate durable measurement key')
        arms = {}
        orders = ['natural', 'reversed'] if a.reversed else ['natural']
        for order in orders:
            keys = set()
            for row in rows:
                cs = row['candidates'] if order == 'natural' else row['candidates'][::-1]
                key = hashlib.sha256(json.dumps([row['query'], [c['text'] for c in cs]], ensure_ascii=False).encode()).hexdigest()
                if key not in cache:
                    raise ValueError('missing durable payload: ' + key)
                keys.add(key)
            nonempty = [cache[k] for k in keys if cache[k]['candidates']]
            if any(len(m['warm_ms']) != a.repeats for m in nonempty):
                raise ValueError('unexpected warm repetition count')
            times = [t for m in nonempty for t in m['warm_ms']]
            arms[order] = {'unique_nonempty': len(nonempty), 'warm_calls': len(times), 'warm_p50_ms': percentile(times, .5), 'warm_p95_ms': percentile(times, .95), 'warm_sum_s': sum(times)/1000, 'first_plus_warm_sum_s': sum(m['first_ms']+sum(m['warm_ms']) for m in nonempty)/1000, 'max_tokens': max((m['tokens'] for m in nonempty), default=0)}
        result['arms'] = arms
        result['unique_payloads'] = len(cache)
        result['max_repeat_score_delta'] = max((m['repeat_max_abs_diff'] for m in measurements), default=0)
        result['durable_inference_sum_s'] = sum(m['first_ms']+sum(m['warm_ms']) for m in measurements)/1000
    if a.runtime:
        result['runtime_events'] = load_rows(a.runtime)
    Path(a.report).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
