"""Offline dev/cal evaluation. No network, no default private inputs, no test tuning."""
import argparse
import hashlib
import importlib.util
import json
import math
from pathlib import Path

HARNESS = Path(__file__).resolve().parents[2] / 'selection' / 'harness.py'


def read_jsonl(path):
    return [json.loads(x) for x in Path(path).read_text().splitlines() if x.strip()]


def ratio(n, d):
    return n / d if d else None


def metrics(pools, labels, primary, selections):
    counts = {k: 0 for k in ['queries', 'selected_items', 'true_positive_items', 'false_positive_items',
        'unknown_selected_items', 'expected_items', 'candidate_positive_items', 'true_positive_families',
        'false_positive_families', 'expected_families', 'duplicate_selected_items', 'primary_positive_queries',
        'primary_query_hits', 'primary_candidate_hits', 'primary_null_queries', 'primary_null_injected']}
    errors = []
    for pool in pools:
        lab, src = labels[pool['case_id']], primary[pool['case_id']]
        expected, rejected = set(lab['expected']), set(lab['rejected'])
        uncertain = set(lab.get('uncertain', []))
        if expected & rejected or (expected | rejected) & uncertain:
            raise ValueError('Conflicting labels')
        hits = {h['Path']: h for h in (pool['candidates'] or []) + (pool['derived'] or [])}
        selected = selections[pool['id']]
        if not set(selected) <= set(hits) or len(set(selected)) != len(selected):
            raise ValueError('Unknown or duplicate selected IDs')
        family = dict(pool['families'])
        for group in lab.get('equivalence_groups', []):
            roots = {family.get(x, x) for x in group}
            if roots:
                representative = min(roots)
                family = {k: representative if v in roots else v for k, v in family.items()}
                for x in group:
                    family[x] = representative
        fam = lambda x: family.get(x, x)
        tp, fp = set(selected) & expected, set(selected) & rejected
        unknown = set(selected) - expected - rejected
        tf, ff = {fam(x) for x in tp}, {fam(x) for x in fp}
        counts['queries'] += 1
        counts['selected_items'] += len(selected)
        counts['true_positive_items'] += len(tp)
        counts['false_positive_items'] += len(fp)
        counts['unknown_selected_items'] += len(unknown)
        counts['expected_items'] += len(expected)
        counts['candidate_positive_items'] += len(expected & set(hits))
        counts['true_positive_families'] += len(tf)
        counts['false_positive_families'] += len(ff - tf)
        counts['expected_families'] += len({fam(x) for x in expected})
        counts['duplicate_selected_items'] += len(selected) - len({fam(x) for x in selected})
        primary_expected = set(src['expected'])
        counts['primary_positive_queries'] += bool(primary_expected)
        counts['primary_query_hits'] += bool({hits[x]['Name'] for x in selected} & primary_expected)
        counts['primary_candidate_hits'] += bool({h['Name'] for h in hits.values()} & primary_expected)
        counts['primary_null_queries'] += not primary_expected
        counts['primary_null_injected'] += not primary_expected and bool(selected)
        if fp or expected - set(selected) or unknown:
            errors.append({'id': pool['id'], 'split': pool['split'], 'selected': selected,
                'false_positives': sorted(fp), 'unknown_selected': sorted(unknown),
                'missed_reviewed_positives': sorted(expected - set(selected)),
                'missing_from_candidate_pool': sorted(expected - set(hits))})
    counts['item_precision'] = ratio(counts['true_positive_items'], counts['true_positive_items'] + counts['false_positive_items'])
    counts['item_recall'] = ratio(counts['true_positive_items'], counts['expected_items'])
    tp, fp, total = counts['true_positive_families'], counts['false_positive_families'], counts['expected_families']
    counts['family_precision'] = ratio(tp, tp + fp)
    counts['family_recall'] = ratio(tp, total)
    counts['family_f0_5'] = ratio(1.25 * tp, 1.25 * tp + .25 * (total - tp) + fp) or 0
    counts['candidate_item_recall'] = ratio(counts['candidate_positive_items'], counts['expected_items'])
    return counts, errors


def load_labels(path):
    rows = json.loads(Path(path).read_text())
    result = {r['id']: r for r in rows}
    if not result or len(result) != len(rows):
        raise ValueError('Empty or duplicate labels')
    return result


def evaluate(args):
    from jev_devcal import external_output
    out = external_output(args.output)
    pools = read_jsonl(args.pools)
    labels, primary = load_labels(args.reviewed), load_labels(args.primary)
    if set(labels) != set(primary):
        raise ValueError('Primary/reviewed case IDs differ')
    if any(r['split'] not in ('dev', 'calibration') for r in pools + list(labels.values()) + list(primary.values())):
        raise ValueError('Dev/cal fitting refuses test rows')
    if len({p['id'] for p in pools}) != len(pools):
        raise ValueError('Duplicate pool IDs')
    episodes = {s: {p['episode_id'] for p in pools if p['split'] == s} for s in ('dev', 'calibration')}
    if episodes['dev'] & episodes['calibration']:
        raise ValueError('Episode leakage')
    for pool in pools:
        if pool['split'] != labels[pool['case_id']]['split'] or pool['split'] != primary[pool['case_id']]['split']:
            raise ValueError('Split mismatch')
        for group in ('candidates', 'derived'):
            pool[group] = [dict(h, id=h['Path']) for h in pool[group] or []]
    paths, scores = {}, {}
    for value in args.scores:
        name, sep, path = value.partition('=')
        if not sep or not name or name in paths:
            raise ValueError('Scores require unique NAME=FILE arguments')
        paths[name] = Path(path)
        rows = read_jsonl(path)
        byid = {r['id']: r for r in rows}
        if len(rows) != len(byid) or set(byid) != {p['id'] for p in pools}:
            raise ValueError('Score/pool rows mismatch')
        scores[name] = {}
        for pool in pools:
            row = byid[pool['id']]
            values = {h['id']: h['score'] for h in row['scores']}
            if len(values) != len(row['scores']) or set(values) != {h['Path'] for h in pool['candidates'] + pool['derived']}:
                raise ValueError('Score candidate mismatch')
            if not all(math.isfinite(v) for v in values.values()):
                raise ValueError('Nonfinite score')
            scores[name][pool['id']] = values
    spec = importlib.util.spec_from_file_location('selector_harness', HARNESS)
    harness = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(harness)

    def measure(name, threshold, subset):
        if name == 'frozen-baseline':
            selections = {p['id']: [h['Path'] for h in p['control'] or []] for p in subset}
        else:
            selections = {p['id']: [h['id'] for h in harness.learned_select(p['candidates'], p['derived'], scores[name][p['id']], threshold)] for p in subset}
        return metrics(subset, labels, primary, selections)

    report = {'label_status': args.label_status, 'test_accessed': False, 'deployment_changed': False,
        'context_results': {}, 'fixed_jev_thresholds': {},
        'limitations': ['Calibration-tuned metrics are optimistic, not held-out accuracy.',
            'Context arms repeat cases, not independent observations.', 'Unknowns excluded, never treated as negatives.',
            'No downstream-task-success claim. Label provenance is supplied by the caller.']}
    curves, errors = {}, {}
    prompts = [n for n in ('jev-baseline', 'jev-rubric', 'jev-choice') if n in scores]
    for context in sorted({p['context']['arm'] for p in pools}):
        subset = [p for p in pools if p['context']['arm'] == context]
        splits = {s: [p for p in subset if p['split'] == s] for s in ('dev', 'calibration')}
        if not all(splits.values()):
            raise ValueError('Every context requires dev and calibration cases')
        result = {'frozen-baseline': {s: measure('frozen-baseline', 0, ps)[0] for s, ps in splits.items()}}
        curves[context] = {}
        for name in scores:
            thresholds = sorted({v for p in splits['calibration'] for v in scores[name][p['id']].values()})
            thresholds.append(math.nextafter(max(thresholds, default=1), math.inf))
            curve = [{'threshold': t, **measure(name, t, splits['calibration'])[0]} for t in thresholds]
            best = max(curve, key=lambda x: (x['family_f0_5'], x['family_recall'] or 0, x['threshold']))
            t = best['threshold']
            curves[context][name] = curve
            result[name] = {'threshold_calibrated': t, **{s: measure(name, t, ps)[0] for s, ps in splits.items()}}
            errors[context + '/' + name] = measure(name, t, subset)[1]
        report['context_results'][context] = result
        report['fixed_jev_thresholds'][context] = {name: {str(t): {s: measure(name, t, ps)[0] for s, ps in splits.items()} for t in (.3, .5, .7, .9, .95)} for name in prompts}
    if prompts and 'original' in report['context_results']:
        dev = [p for p in pools if p['context']['arm'] == 'original' and p['split'] == 'dev']
        report['selected_prompt_from_dev_at_0_5'] = max(prompts, key=lambda n: (measure(n, .5, dev)[0]['family_f0_5'], -prompts.index(n)))
    report['pins'] = {str(p): hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in [args.primary, args.reviewed, args.pools, HARNESS, *paths.values()]}
    for name, data in [('quality-evaluation.json', report), ('calibration-curves.json', curves), ('quality-errors.json', errors)]:
        (out / name).write_text(json.dumps(data, indent=2) + '\n')
    print(json.dumps({'output': str(out), 'selected_prompt': report.get('selected_prompt_from_dev_at_0_5')}))
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ('pools', 'primary', 'reviewed', 'output'):
        parser.add_argument('--' + flag, required=True)
    parser.add_argument('--scores', action='append', required=True, metavar='NAME=FILE')
    parser.add_argument('--label-status', default='Caller-supplied labels; provenance not independently established')
    evaluate(parser.parse_args())


if __name__ == '__main__':
    main()
