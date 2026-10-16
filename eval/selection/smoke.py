#!/usr/bin/env python3
"""Synthetic end-to-end fit/select smoke test. Never a relevance measurement."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
from harness import private_output, family_ids

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--audit-dir', default=os.environ.get('TACIT_SELECTION_AUDIT_DIR') or
                    str(Path.home()/'.cache/tacit-utility-audit'))
args = parser.parse_args()
audit = Path(args.audit_dir).expanduser().resolve()
out = private_output(audit/'selection-fit-synthetic', audit)
pools, labels = [], []
for split in ('dev', 'calibration', 'test'):
    for i in range(4):
        case = split+str(i)
        candidates=[]
        for n, score in (('useful', 30+i), ('rejected', 2+i), ('unknown', 8+i)):
            candidates.append(dict(id='user/'+n+'.md', Name=n, Scope='user', Score=score,
                                   origin_ids=['synthetic-'+n], Description='Synthetic '+n, Body='Fixture only',
                                   Evidence=dict(Coverage=score/40)))
        pools.append(dict(id=case, case_id=case, episode_id=case, split=split, repo='',
                          ceiling=6, context=dict(arm='gated'), candidates=candidates, derived=[],
                          families=family_ids(candidates), pool_hash=case, frozen_baseline=''))
        if split!='test':
            labels.append(dict(id=case, episode_id=case, split=split, expected=['useful'], rejected=['rejected']))

def save(name, rows):
    p=out/name
    p.write_text(''.join(json.dumps(r)+'\n' for r in rows))
    return str(p)
train=save('train-pools.jsonl',[p for p in pools if p['split']!='test'])
test=save('test-input-pools.jsonl',[p for p in pools if p['split']=='test'])
labels_path=save('labels.jsonl',labels)
script=Path(__file__).with_name('harness.py')
subprocess.run([sys.executable,str(script),'fit','--audit-dir',str(audit),'--pools',train,'--labels',labels_path,'--model',str(out/'model.json')],check=True,timeout=10)
subprocess.run([sys.executable,str(script),'select','--audit-dir',str(audit),'--pools',test,'--model',str(out/'model.json'),'--out',str(out/'selected.jsonl')],check=True,timeout=10)
selected=[json.loads(l) for l in (out/'selected.jsonl').read_text().splitlines()]
assert len(selected)==4
assert all('user/useful.md' in r['injections']['learned'] for r in selected)
assert all('user/rejected.md' not in r['injections']['learned'] for r in selected)
model=json.loads((out/'model.json').read_text())
assert model['coverage']==dict(positive=8,negative=8,unknown=8)
# Test labels do not exist. The parent owns real held-out outcomes.
print('Synthetic fit/select passed; unknown=8 remained unlabelled. Not a quality result.')
