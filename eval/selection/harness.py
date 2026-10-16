#!/usr/bin/env python3
"""Offline, label-blind retrieval and split-checked linear per-fact selection.

No network, provider, production hook changes or store writes. Stdlib only.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import statistics
import subprocess
from typing import Any
import time

VERSION = 'selection-v1'
ARMS = ('original', 'unconditional', 'gated')
# Grammatical anaphora, not project/entity/action dictionaries. This intentionally
# misses implicit continuations rather than treating all short prompts as vague.
ANAPHORA = re.compile(r'\b(?:it|that|this|those|these|them|same)\b', re.I)
IDENTIFIER = re.compile(r'\b\w*_\w+\b|\b(?=\w*[A-Za-z])(?=\w*\d)\w+\b|[/\\]|`')


def context_query(case, arm):
    if arm not in ARMS:
        raise ValueError('unknown context arm')
    original = case['prompt']
    result = dict(original=original, query=original, sources=[], arm=arm)
    if arm == 'original' or not original.strip() or original.lstrip().startswith('<'):
        return result
    if arm == 'gated' and (not ANAPHORA.search(original) or IDENTIFIER.search(original)):
        return result
    # Only the nearest prior user turn, never assistant answers, labels or metadata.
    for i in range(len(case.get('history', []))-1, -1, -1):
        h = case['history'][i]
        if h.get('role') != 'user':
            continue
        text = h.get('content', '')
        if not isinstance(text, str) or not text.strip() or text.lstrip().startswith('<'):
            break
        if arm == 'gated' and h.get('cwd', case['cwd']) != case['cwd']:
            break
        text = text[:1200]
        result['query'] = original + '\n' + text
        result['sources'] = [dict(history_index=i, role='user', start=0, end=len(text), text=text)]
        break
    return result


def baseline_select(candidates, derived, repo, seen=()):
    floor = 15 if repo else 20
    strong = [h for h in candidates if not h['Scope'].startswith('derived/') and
              (h['Score'] >= floor or (h.get('Evidence') or {}).get('Supported'))][:3]
    # Truncate before delivered filtering, never backfill.
    out = [h for h in strong if h['Name'] not in seen]
    if derived and derived[0]['Scope'].startswith('derived/') and derived[0]['Score'] >= 12 and derived[0]['Name'] not in seen:
        out.append(derived[0])
    return out


def frozen_names(text):
    return [line[2:].removeprefix('RULE ').split(':', 1)[0].split(' (', 1)[0]
            for line in text.splitlines() if line.startswith('- ')]


def learned_select(candidates, derived, values, threshold, seen=()):
    ranked = sorted(candidates, key=lambda h: (-values[h['id']], h['id']))
    strong = [h for h in ranked if values[h['id']] >= threshold][:3]
    # Same no-backfill convention as the hook, including derived isolation.
    strong += [h for h in derived[:1] if values[h['id']] >= threshold]
    return [h for h in strong if h['Name'] not in seen]


def feature_vector(hit, repo, semantic=None):
    e = hit.get('Evidence') or {}
    # Fixed, low capacity, corpus-derived features. No fact IDs/text/category.
    x = [math.log1p(max(0, hit['Score'])), float(e.get('Coverage', 0)),
         max([math.log1p(max(0, r['SupportDiscountedLift'])) for r in (e.get('Relations') or [])] or [0]),
         float(bool(repo) and hit['Scope'] in ('projects/'+repo, 'derived/'+repo))]
    if semantic is not None:
        if not math.isfinite(semantic):
            raise ValueError('nonfinite semantic score')
        x.append(semantic)
    return x


def family_ids(hits):
    """Connected components of shared origins, not a count of copied origins."""
    parent = {}
    def root(x):
        parent.setdefault(x, x)
        if parent[x] != x:
            parent[x] = root(parent[x])
        return parent[x]
    for h in hits:
        nodes = ['fact:'+h['id']] + ['origin:'+o for o in h.get('origin_ids', [])]
        for n in nodes[1:]:
            a, b = root(nodes[0]), root(n)
            parent[max(a, b)] = min(a, b)
    return {h['id']: root('fact:'+h['id']) for h in hits}


def fit(rows) -> dict[str, Any]:
    if not rows or any(r['split'] != 'dev' for r in rows):
        raise ValueError('fit accepts dev only')
    if any(r['y'] not in (0, 1) for r in rows):
        raise ValueError('unknown labels must not be fitted')
    d = len(rows[0]['x'])
    if any(len(r['x']) != d for r in rows):
        raise ValueError('feature mismatch')
    mean = [statistics.mean(r['x'][j] for r in rows) for j in range(d)]
    scale = [statistics.pstdev(r['x'][j] for r in rows) or 1 for j in range(d)]
    # Ridge least squares with fixed lambda=1, one unit of pseudo-observation
    # shrinkage per coefficient. Scores are utility margins, NOT probabilities.
    a = [[0.0]*(d+2) for _ in range(d+1)]
    for r in rows:
        x = [1]+[(r['x'][j]-mean[j])/scale[j] for j in range(d)]
        for i in range(d+1):
            for j in range(d+1):
                a[i][j] += x[i]*x[j]
            a[i][-1] += x[i]*r['y']
    for j in range(1, d+1):
        a[j][j] += 1
    for i in range(d+1):
        p = max(range(i, d+1), key=lambda k: abs(a[k][i]))
        a[i], a[p] = a[p], a[i]
        divisor = a[i][i]
        if abs(divisor) < 1e-12:
            raise ValueError('singular fit')
        a[i] = [v/divisor for v in a[i]]
        for k in range(d+1):
            if k != i:
                v = a[k][i]
                a[k] = [u-v*w for u, w in zip(a[k], a[i])]
    return dict(version=VERSION, mean=mean, scale=scale, weights=[r[-1] for r in a],
                dev_episodes=sorted({r['episode_id'] for r in rows}), features=d,
                objective='ridge utility margin; calibration explicit-label F1', ridge_lambda=1)


def predict(model, x):
    if len(x) != model['features']:
        raise ValueError('feature mismatch')
    return model['weights'][0]+sum(w*(v-m)/s for w, v, m, s in
           zip(model['weights'][1:], x, model['mean'], model['scale']))


def calibrate(model, rows):
    if not rows or any(r['split'] != 'calibration' for r in rows):
        raise ValueError('threshold accepts calibration only')
    if set(model['dev_episodes']) & {r['episode_id'] for r in rows}:
        raise ValueError('episode leakage across dev/calibration')
    scored = [(predict(model, r['x']), r['y']) for r in rows]
    thresholds = sorted({s for s, y in scored}) + [math.nextafter(max(s for s, y in scored), math.inf)]
    def objective(t):
        tp = sum(y == 1 and s >= t for s, y in scored)
        fp = sum(y == 0 and s >= t for s, y in scored)
        fn = sum(y == 1 and s < t for s, y in scored)
        return (2*tp/(2*tp+fp+fn) if 2*tp+fp+fn else 0, t)
    return dict(model, threshold=max(thresholds, key=objective),
                calibration_episodes=sorted({r['episode_id'] for r in rows}))


def run(cmd, env, *, payload=None, cwd=None, timeout=8):
    p = subprocess.run([str(v) for v in cmd], input=payload, text=True,
                       capture_output=True, env=env, cwd=cwd, timeout=timeout)
    if p.returncode or p.stderr.strip():
        raise RuntimeError(f'{cmd[0]} rc={p.returncode}: {p.stderr[:2000]} {p.stdout[:1000]}')
    return p.stdout


def load(path):
    text = Path(path).read_text()
    return json.loads(text) if text.lstrip().startswith('[') else [json.loads(l) for l in text.splitlines() if l.strip()]


def dump(path, value):
    Path(path).write_text(json.dumps(value, indent=2, allow_nan=False)+'\n')


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def private_output(path, audit_dir=None):
    """Resolve symlinks and keep raw artifacts inside an external audit root."""
    path = Path(path).expanduser().resolve()
    root = Path(audit_dir or os.environ.get('TACIT_SELECTION_AUDIT_DIR') or
                Path.home()/'.cache/tacit-utility-audit').expanduser().resolve()
    repo = Path(__file__).resolve().parents[2]
    if root.is_relative_to(repo) or path.is_relative_to(repo):
        raise ValueError('audit outputs must stay outside the repository')
    if not path.is_relative_to(root):
        raise ValueError('raw artifacts must stay under '+str(root))
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    return path


def retrieval_env(out, store):
    # Do not inherit operator settings, credentials, overlays or hosted utility.
    # A nonempty disabled provider also prevents YAML defaults from enabling Jev.
    env = {k: v for k, v in os.environ.items() if not k.startswith(('TACIT_', 'TYPESAFE_'))}
    home = out/'isolated-home'
    home.mkdir(exist_ok=True, mode=0o700)
    settings = out/'offline-settings.yaml'
    settings.write_text('{}\n')
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home/'config'),
               TACIT_SETTINGS=str(settings), TACIT_CONFIG=str(home/'keys'),
               TACIT_STORE=str(Path(store).expanduser().resolve()), TACIT_INDEX=str(out/'index'),
               TACIT_LOCAL=str(out/'empty-overlay'), TACIT_UTILITY_PROVIDER='disabled',
               GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
               GOMAXPROCS='2', OMP_NUM_THREADS='1', OPENBLAS_NUM_THREADS='1',
               MKL_NUM_THREADS='1', NUMEXPR_NUM_THREADS='1')
    return env


def retrieve(args):
    out = private_output(args.out, getattr(args, 'audit_dir', None))
    env = retrieval_env(out, args.store)
    overlay = out/'empty-overlay'
    if overlay.exists() and any(overlay.iterdir()):
        raise ValueError('overlay must be empty')
    dirty = run(['git', '-C', args.store, 'status', '--porcelain', '--untracked-files=all'], env)
    if dirty.strip():
        raise ValueError('store must be clean')
    head = run(['git', '-C', args.store, 'rev-parse', 'HEAD'], env).strip()
    if head != args.store_head:
        raise ValueError('store HEAD mismatch')
    # Only a whitelisted view survives input parsing. Labels never reach retrieval,
    # context building, feature extraction, model scorer or the result artifacts.
    cases = [{k: c[k] for k in ('id','episode_id','split','prompt','cwd','history') if k in c} for c in load(args.cases)]
    if len({c['id'] for c in cases}) != len(cases):
        raise ValueError('duplicate case id')
    episodes = {}
    for c in cases:
        if c['split'] not in ('dev','calibration','test'):
            raise ValueError('invalid split')
        if c['episode_id'] in episodes and episodes[c['episode_id']] != c['split']:
            raise ValueError('episode leakage')
        episodes[c['episode_id']] = c['split']
    # Prebuild frozen-binary index separately from measured requests. No hook sync.
    run([args.baseline, 'search', '--json', 'index-warmup'], env, timeout=60)
    manifest = dict(version=VERSION, store_head=head, baseline_sha256=sha(args.baseline),
                    exporter_sha256=sha(args.exporter), harness_sha256=sha(__file__),
                    cases_sha256=sha(args.cases), ceilings=args.ceilings, repeats=args.repeats)
    dump(out/'manifest.json', manifest)
    with (out/'pools.jsonl').open('w') as pools, (out/'scorer-input.jsonl').open('w') as scorer:
        for c in cases:
            baseline, timings = None, []
            for _ in range(args.repeats):
                start = time.perf_counter()
                text = run([args.baseline, 'hook', 'prompt'], env,
                           payload=json.dumps(dict(session_id='', cwd=c['cwd'], prompt=c['prompt'])))
                timings.append((time.perf_counter()-start)*1000)
                if baseline is not None and baseline != text:
                    raise ValueError('nondeterministic baseline')
                baseline = text
            for arm in ARMS:
                q = context_query(c, arm)
                for ceiling in args.ceilings:
                    # Structured ID cannot collide with dataset IDs or arm names.
                    key = json.dumps([c['id'], arm, ceiling], separators=(',', ':'))
                    req = dict(id=key, query=q['query'], original=q['original'], cwd=c['cwd'], ceiling=ceiling, seen=[])
                    dump(out/'request.json', req)
                    (out/'response.json').unlink(missing_ok=True)
                    callenv = dict(env, TACIT_SELECTION_REQUEST=str(out/'request.json'), TACIT_SELECTION_RESPONSE=str(out/'response.json'))
                    start = time.perf_counter()
                    run([args.exporter, '-test.run=^TestSelectionExport$', '-test.timeout=7s'], callenv)
                    result = json.loads((out/'response.json').read_text())
                    if arm == 'original' and [h['Name'] for h in (result['control'] or [])] != frozen_names(baseline):
                        raise ValueError('exporter control diverges from frozen binary for '+c['id'])
                    result.update(case_id=c['id'], episode_id=c['episode_id'], split=c['split'], context=q,
                                  retrieval_ms=(time.perf_counter()-start)*1000,
                                  frozen_baseline=baseline, baseline_ms=timings,
                                  baseline_median_ms=statistics.median(timings))
                    candidates = result['candidates']+result['derived']
                    if len({h['id'] for h in candidates}) != len(candidates):
                        raise ValueError('duplicate candidate identity')
                    result['pool_hash'] = hashlib.sha256(json.dumps([h['id'] for h in candidates]).encode()).hexdigest()
                    result['families'] = family_ids(candidates)
                    pools.write(json.dumps(result)+'\n'); pools.flush()
                    scorer.write(json.dumps(dict(id=key, query=q['query'], candidates=[dict(id=h['id'], text=h['Name']+'\n'+h['Description']+'\n'+h['Body']) for h in candidates]))+'\n'); scorer.flush()
    print(json.dumps(dict(cases=len(cases), pools=str(out/'pools.jsonl'), scorer_input=str(out/'scorer-input.jsonl'))))


def object_sha(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def validate_episode_splits(records):
    episodes = {}
    for r in records:
        if r['split'] not in ('dev', 'calibration', 'test'):
            raise ValueError('invalid split')
        if r['episode_id'] in episodes and episodes[r['episode_id']] != r['split']:
            raise ValueError('episode leakage across input records')
        episodes[r['episode_id']] = r['split']
    return episodes


def verify_score_manifest(scores_path, pools_path):
    manifest = json.loads(Path(str(scores_path)+'.manifest.json').read_text())
    identity = manifest.get('identity')
    hashes = ('scorer_sha256', 'model_manifest_sha256', 'lock_sha256')
    if not isinstance(identity, dict) or any(not isinstance(identity.get(k), str) or not re.fullmatch(r'[0-9a-f]{64}', identity[k]) for k in hashes):
        raise ValueError('missing scorer, model/tokenizer or dependency provenance')
    runtime = identity.get('runtime', {})
    execution = identity.get('execution', {})
    if not isinstance(execution, dict) or not all(k in execution for k in ('provider','intra_threads','inter_threads','batch_size','max_length','score_type')):
        raise ValueError('missing scorer execution settings')
    if execution['provider'] not in ('CPUExecutionProvider', 'PyTorchCPU') or any(type(execution[k]) is not int or execution[k] < 1 for k in ('intra_threads','inter_threads','batch_size','max_length')) or not isinstance(execution['score_type'], str) or not execution['score_type']:
        raise ValueError('invalid CPU scorer execution settings')
    required_runtime = ('numpy','tokenizers') + (('onnxruntime',) if execution['provider']=='CPUExecutionProvider' else ('torch','transformers'))
    if not isinstance(runtime, dict) or any(not isinstance(runtime.get(k), str) or not runtime[k] for k in required_runtime):
        raise ValueError('missing actual scorer runtime versions')
    text_view = manifest.get('text_view', 'full')
    if text_view not in ('full', 'summary') or identity.get('document_view', 'full') != text_view:
        raise ValueError('scorer document view mismatch')
    expected_input = [dict(id=p['id'], query=p['context']['query'], candidates=[
        dict(id=h['id'], text=h['Name']+'\n'+h['Description']+ ('\n'+h['Body'] if text_view=='full' else ''))
        for h in p['candidates']+p['derived']]) for p in load(pools_path)]
    if (manifest['scores_sha256'] != sha(scores_path) or
        manifest['pools_sha256'] != sha(pools_path) or
        manifest['input_records_sha256'] != object_sha(expected_input) or
        manifest['identity_sha256'] != object_sha(manifest['identity'])):
        raise ValueError('semantic artifact provenance mismatch')
    return manifest


def score_map(path, pools):
    if not path:
        return None
    data = load(path)
    for r in data:
        if len({h['id'] for h in r['scores']}) != len(r['scores']) or any(not math.isfinite(float(h['score'])) for h in r['scores']):
            raise ValueError('duplicate or nonfinite semantic score')
    scores = {r['id']: {h['id']: float(h['score']) for h in r['scores']} for r in data}
    if len(scores) != len(data) or set(scores) != {p['id'] for p in pools}:
        raise ValueError('semantic row identity mismatch')
    for p in pools:
        if set(scores[p['id']]) != {h['id'] for h in p['candidates']+p['derived']}:
            raise ValueError('semantic candidate identity mismatch')
    return scores


def labelled_rows(pools, labels, scores):
    rows, coverage = [], dict(positive=0, negative=0, unknown=0)
    for p in pools:
        label = labels[p['case_id']]
        if label['split'] != p['split'] or label['episode_id'] != p['episode_id']:
            raise ValueError('label identity/split mismatch')
        expected, rejected = set(label.get('expected', [])), set(label.get('rejected', []))
        if expected & rejected:
            raise ValueError('conflicting labels')
        for h in p['candidates']+p['derived']:
            # IDs are paths; allow legacy names but never infer negatives from absence.
            ids = {h['id'], h['Name']}
            if ids & expected and ids & rejected:
                raise ValueError('conflicting path/name labels')
            if ids & expected:
                y = 1; coverage['positive'] += 1
            elif ids & rejected:
                y = 0; coverage['negative'] += 1
            else:
                coverage['unknown'] += 1
                continue
            semantic = scores[p['id']][h['id']] if scores is not None else None
            rows.append(dict(split=p['split'], episode_id=p['episode_id'], x=feature_vector(h,p['repo'],semantic), y=y))
    return rows, coverage


def train(args):
    pools = load(args.pools)
    # Deliberately refuse mixed files containing test, not merely filter test out.
    if any(p['split'] not in ('dev','calibration') for p in pools):
        raise ValueError('training input contains test')
    labels_list = load(args.labels)
    if any(c['split'] not in ('dev','calibration') for c in labels_list):
        raise ValueError('label input contains test')
    # Validate every input episode, including pools with no labelled candidates.
    input_episodes = validate_episode_splits(pools + labels_list)
    score_manifest = verify_score_manifest(args.scores, args.pools) if args.scores else None
    scores = score_map(args.scores, pools)
    if len({c['id'] for c in labels_list}) != len(labels_list):
        raise ValueError('duplicate label case id')
    labels = {c['id']: c for c in labels_list}
    pools = [p for p in pools if p['context']['arm']==args.arm and p['ceiling']==args.ceiling]
    rows, coverage = labelled_rows(pools, labels, scores)
    dev = [r for r in rows if r['split']=='dev']
    cal = [r for r in rows if r['split']=='calibration']
    if {r['y'] for r in dev} != {0,1} or {r['y'] for r in cal} != {0,1}:
        raise ValueError('explicit positive and negative labels required on each split')
    if getattr(args, 'selector', 'ridge') == 'score-only':
        if scores is None:
            raise ValueError('score-only selector requires semantic scores')
        dimensions = len(dev[0]['x'])
        raw_model: dict[str, Any] = dict(version=VERSION, mean=[0.0]*dimensions, scale=[1.0]*dimensions,
                         weights=[0.0]*dimensions+[1.0], features=dimensions,
                         dev_episodes=sorted({r['episode_id'] for r in dev}),
                         objective='direct semantic score; calibration explicit-label F1')
    else:
        raw_model = fit(dev)
    model = calibrate(raw_model, cal)
    model['selector'] = getattr(args, 'selector', 'ridge')
    model.update(dev_episodes=sorted(e for e,s in input_episodes.items() if s=='dev'),
                 calibration_episodes=sorted(e for e,s in input_episodes.items() if s=='calibration'),
                 scores_sha256=sha(args.scores) if args.scores else None,
                 scorer_identity_sha256=score_manifest['identity_sha256'] if score_manifest else None)
    model.update(coverage=coverage, arm=args.arm, ceiling=args.ceiling,
                 semantic=bool(args.scores), pools_sha256=sha(args.pools), labels_sha256=sha(args.labels))
    target=Path(args.model).expanduser().resolve()
    out=private_output(target.parent, getattr(args, 'audit_dir', None))
    dump(out/target.name,model)
    print(json.dumps(dict(model=str(args.model), coverage=coverage)))


def select(args):
    pools=load(args.pools)
    validate_episode_splits(pools)
    score_manifest=verify_score_manifest(args.scores,args.pools) if args.scores else None
    scores=score_map(args.scores,pools)
    model=json.loads(Path(args.model).read_text()) if args.model else None
    if model and model['semantic'] != bool(args.scores):
        raise ValueError('model semantic feature mismatch')
    if model:
        training=set(model['dev_episodes']) | set(model['calibration_episodes'])
        if training & {p['episode_id'] for p in pools if p['split']=='test'}:
            raise ValueError('test episode overlaps training or calibration inputs')
        if score_manifest and model.get('scorer_identity_sha256') != score_manifest['identity_sha256']:
            raise ValueError('selection scorer differs from fitted scorer')
    provenance=dict(pools_sha256=sha(args.pools),model_sha256=sha(args.model) if args.model else None,
                    scores_sha256=sha(args.scores) if args.scores else None,
                    scorer_identity_sha256=score_manifest['identity_sha256'] if score_manifest else None)
    target=Path(args.out).expanduser().resolve()
    out=private_output(target.parent, getattr(args, 'audit_dir', None))
    with (out/target.name).open('w') as f:
        for p in pools:
            if model and (model['arm'] != p['context']['arm'] or model['ceiling'] != p['ceiling']):
                continue
            hs=p['candidates']+p['derived']
            base=baseline_select(p['candidates'],p['derived'],p['repo'])
            arms=dict(baseline_floors=[h['id'] for h in base])
            ranking=None
            if model:
                values={h['id']: predict(model,feature_vector(h,p['repo'],scores[p['id']][h['id']] if scores else None)) for h in hs}
                ranking=sorted(values,key=lambda k:(-values[k],k))
                chosen=learned_select(p['candidates'],p['derived'],values,model['threshold'])
                arms['learned']=[h['id'] for h in chosen]
                ranking=[dict(id=k,score=values[k]) for k in ranking]
            f.write(json.dumps(dict(id=p['id'],pool_hash=p['pool_hash'],ranking=ranking,injections=arms,
                                   families=p['families'],frozen_baseline=p['frozen_baseline'],provenance=provenance))+'\n')
    print(json.dumps(dict(output=str(args.out))))


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    sub=ap.add_subparsers(dest='command',required=True)
    p=sub.add_parser('retrieve')
    for flag in ('cases','store','store-head','baseline','exporter','out'):
        p.add_argument('--'+flag,required=True)
    p.add_argument('--ceilings',type=int,nargs='+',default=[6])
    p.add_argument('--repeats',type=int,default=3)
    p.set_defaults(func=retrieve)
    p=sub.add_parser('fit')
    for flag in ('pools','labels','model'):
        p.add_argument('--'+flag,required=True)
    p.add_argument('--scores')
    p.add_argument('--selector', choices=('ridge','score-only'), default='ridge')
    p.add_argument('--arm',choices=ARMS,default='gated')
    p.add_argument('--ceiling',type=int,default=6)
    p.set_defaults(func=train)
    p=sub.add_parser('select')
    p.add_argument('--pools',required=True); p.add_argument('--out',required=True)
    p.add_argument('--scores'); p.add_argument('--model'); p.set_defaults(func=select)
    for parser in sub.choices.values():
        parser.add_argument('--audit-dir', help='external raw-artifact root; defaults to '
                            '$TACIT_SELECTION_AUDIT_DIR or ~/.cache/tacit-utility-audit')
    args=ap.parse_args()
    if getattr(args,'repeats',1)<1 or any(n<1 or n>128 for n in getattr(args,'ceilings',[])):
        ap.error('positive repeats and ceilings 1..128 required')
    args.func(args)


if __name__=='__main__':
    main()
