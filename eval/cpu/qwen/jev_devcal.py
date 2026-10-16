"""Explicit opt-in hosted comparison on caller-supplied JSONL. No default private inputs.

All three prompt arms are scored together. Outputs and journal must live outside
this repository. Never submit private data without its owner's authorization.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import statistics
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import percentile, validate
from jev_smoke import MODEL, URL, payload, object_hash, get_key, request, validate_response


def external_output(path):
    path = Path(path).expanduser().resolve()
    repo = Path(__file__).resolve().parents[3]
    if path == repo or repo in path.parents:
        raise ValueError('Outputs must be outside the public repository')
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    return path


def load_input(path):
    rows = [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]
    if len({r['id'] for r in rows}) != len(rows):
        raise ValueError('Duplicate row IDs')
    for row in rows:
        if set(row) != {'id', 'query', 'candidates'} or not isinstance(row['query'], str):
            raise ValueError('Input must be label-free {id,query,candidates}')
        candidates = row['candidates']
        if len({c['id'] for c in candidates}) != len(candidates):
            raise ValueError('Duplicate candidate IDs')
        if any(set(c) != {'id', 'text'} or not isinstance(c['text'], str) for c in candidates):
            raise ValueError('Candidates require only id and printed text')
    return rows


def write_outputs(out, rows, cache, repeats):
    for arm in ('baseline', 'rubric', 'choice'):
        records = []
        for row in rows:
            key = object_hash(payload(row))
            if row['candidates'] and any((key, r) not in cache for r in range(repeats)):
                continue
            scores = []
            for i, candidate in enumerate(row['candidates']):
                answers = [cache[(key, r)]['response']['answers'][f'{arm}_{i}'] for r in range(repeats)]
                values = [a['noul'] if arm != 'choice' else a['probabilities']['useful'] for a in answers]
                scores.append({'id': candidate['id'], 'score': statistics.median(values)})
            records.append({'id': row['id'], 'scores': scores, 'elapsed_ms':
                statistics.median(cache[(key, r)]['elapsed_s']*1000 for r in range(repeats)) if scores else 0})
        pending = out / (arm + '-scores.jsonl.partial')
        pending.write_text(''.join(json.dumps(r) + '\n' for r in records))
        pending.replace(out / (arm + '-scores.jsonl'))


def run(args):
    if not args.allow_egress:
        raise ValueError('Hosted requests require --allow-egress; authorizes sending input text to TypeSafe')
    if args.repeats < 1 or not math.isfinite(args.timeout) or args.timeout <= 0:
        raise ValueError('Repeats and timeout must be positive')
    rows = load_input(args.input)
    out = external_output(args.output)
    unique = {object_hash(payload(r)): r for r in rows}
    identity = {'model': MODEL, 'endpoint': URL, 'repeats': args.repeats,
        'input_sha256': hashlib.sha256(Path(args.input).read_bytes()).hexdigest(),
        'requests_sha256': object_hash([payload(r) for r in rows]), 'schema': 1}
    manifest = out / 'identity.json'
    journal = out / 'requests.jsonl'
    if manifest.exists():
        if json.loads(manifest.read_text()) != identity:
            raise ValueError('Resume identity mismatch: input, prompt, model or repetition count changed')
    elif journal.exists() and journal.stat().st_size:
        raise ValueError('Refusing journal without an identity manifest')
    else:
        manifest.write_text(json.dumps(identity, indent=2) + '\n')
    cache = {}
    if journal.exists():
        for line in journal.read_text().splitlines():
            event = json.loads(line)
            key, repeat = event['payload_sha256'], event['repeat']
            if key not in unique or not 0 <= repeat < args.repeats or (key, repeat) in cache:
                raise ValueError('Unknown or duplicate journal record')
            validate_response(event['response'], payload(unique[key]))
            if not math.isfinite(event['elapsed_s']) or event['elapsed_s'] < 0:
                raise ValueError('Invalid journal timing')
            cache[(key, repeat)] = event
    key = None
    started = time.perf_counter()
    with journal.open('a') as log:
        for digest, row in unique.items():
            if not row['candidates']:
                continue
            for repeat in range(args.repeats):
                if (digest, repeat) in cache:
                    continue
                if key is None:
                    key = get_key(args.key_file)
                before = time.perf_counter()
                response = request(payload(row), key, args.timeout)
                event = {'payload_sha256': digest, 'repeat': repeat,
                    'elapsed_s': time.perf_counter()-before, 'response': response}
                log.write(json.dumps(event)+'\n'); log.flush()
                cache[(digest, repeat)] = event
            write_outputs(out, rows, cache, args.repeats)
    write_outputs(out, rows, cache, args.repeats)
    for arm in ('baseline', 'rubric', 'choice'):
        validate(rows, [json.loads(x) for x in (out/(arm+'-scores.jsonl')).read_text().splitlines()], probability=True)
    times = [e['elapsed_s'] for e in cache.values()]
    tokens = sum(e['response']['usage']['input_tokens'] for e in cache.values())
    report = {**identity, 'rows': len(rows), 'pairs': sum(len(r['candidates']) for r in rows),
        'unique_nonempty': sum(bool(r['candidates']) for r in unique.values()), 'requests': len(cache),
        'roundtrip_p50_ms': percentile(times, .5)*1000 if times else None,
        'roundtrip_p95_ms': percentile(times, .95)*1000 if times else None,
        'this_process_wall_s': time.perf_counter()-started, 'input_tokens': tokens,
        'latency_semantics': 'HTTP round trip, all three arms jointly; empty inputs excluded; includes connection overhead',
        'score_semantics': 'Median across repeats; Noul or Choice useful probability, not locally calibrated'}
    (out/'summary.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report))
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--key-file')
    parser.add_argument('--allow-egress', action='store_true')
    parser.add_argument('--repeats', type=int, default=3)
    parser.add_argument('--timeout', type=float, default=60)
    run(parser.parse_args())


if __name__ == '__main__':
    main()
