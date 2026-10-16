#!/usr/bin/env python3
"""Pinned reviewed Jina listwise CPU scorer; no labels, network, or truncation.
Writes quality outputs plus resumable unique-payload measurements. elapsed_ms is
measured median warm inference for the identical payload, NOT cache-hit latency.
"""
import os
for k in ('OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS'): os.environ[k]='1'
os.environ['CUDA_VISIBLE_DEVICES']=''
os.environ['HF_HUB_OFFLINE']='1'
os.environ['TRANSFORMERS_OFFLINE']='1'
import argparse,json,time,hashlib,resource,platform,sys
from pathlib import Path
START=time.perf_counter()
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts, peak_rss_kib, preflight_files
p=argparse.ArgumentParser(description=__doc__);add_workspace(p);p.add_argument('--input',required=True);p.add_argument('--output',required=True);p.add_argument('--order',choices=['natural','reversed','both'],default='natural');p.add_argument('--dtype',choices=['auto','float32'],default='auto');p.add_argument('--budget-seconds',type=float,default=280);p.add_argument('--parity',action='store_true');a=p.parse_args()
outputs = [a.output+'.'+order+'.jsonl' for order in ('natural','reversed')] if a.order=='both' else [a.output]
outputs += [a.output+'.measurements.jsonl',a.output+'.runtime.jsonl']
preflight_files([a.input], outputs)
ROOT=workspace('jina',a.workspace); ROOT.mkdir(parents=True,exist_ok=True); MODEL=ROOT/'model'; REV='e8a93f33f0b22108f8c2364f8484ce3422552fbc'
preflight_files([ROOT/'manifest.json',MODEL], outputs)
import torch,numpy as np,transformers
from transformers import AutoTokenizer
manifest=json.loads((ROOT/'manifest.json').read_text())
if manifest['revision']!=REV: raise ValueError('Jina artifact revision mismatch')
verify_artifacts(MODEL,manifest)
assert hashlib.sha256((MODEL/'modeling.py').read_bytes()).hexdigest()=='ec6612461b4307eb3bab089e6916c00881b7e6b2e8edfbd56a9ace4560244837', 'Reviewed source changed'
sys.path.insert(0,str(MODEL))
from modeling import JinaForRanking,format_docs_prompts_func
torch.set_num_threads(1);torch.set_num_interop_threads(1)
def digest(path): return hashlib.sha256(Path(path).read_bytes()).hexdigest()
def append(path,obj):
    with open(path,'a') as f: f.write(json.dumps(obj)+'\n');f.flush();os.fsync(f.fileno())
rows=[json.loads(x) for x in open(a.input)]; assert all(set(r)=={'id','query','candidates'} for r in rows)
orders=['natural','reversed'] if a.order=='both' else [a.order]
out=Path(a.output); cachepath=Path(str(out)+'.measurements.jsonl');receiptpath=Path(str(out)+'.runtime.jsonl')
cache={}
if cachepath.exists():
    for line in open(cachepath):
        r=json.loads(line);cache[r['key']]=r
load_start=time.perf_counter()
model=JinaForRanking.from_pretrained(str(MODEL),dtype='auto',local_files_only=True,attn_implementation='sdpa').cpu().eval()
model._tokenizer=AutoTokenizer.from_pretrained(str(MODEL),local_files_only=True,trust_remote_code=False);model._tokenizer.padding_side='left'
assert all(p.device.type=='cpu' for p in model.parameters())
assert all(b.device.type=='cpu' for b in model.buffers())
load_ms=(time.perf_counter()-load_start)*1000
tok=model._tokenizer
calls=[]
orig=model._compute_single_batch
def counted(query,docs,instruction=None):
    calls.append(len(docs)); return orig(query,docs,instruction)
model._compute_single_batch=counted
def run(query,docs):
    qlen=len(tok(query)['input_ids']); dlens=[len(tok(d)['input_ids']) for d in docs]
    assert qlen<1024 and max(dlens)<8192 and len(docs)<125
    assert tok.model_max_length-2*qlen-sum(dlens)>8192
    prompt=format_docs_prompts_func(query,docs,special_tokens=model.special_tokens)
    plen=len(tok(prompt)['input_ids']); assert plen<=model.config.max_position_embeddings
    calls.clear();t=time.perf_counter()
    with torch.inference_mode(): result=model.rerank(query,docs)
    ms=(time.perf_counter()-t)*1000
    assert calls==[len(docs)],calls
    assert sorted(int(r['index']) for r in result)==list(range(len(docs)))
    scores=[None]*len(docs)
    for r in result: scores[int(r['index'])]=float(r['relevance_score'])
    assert all(np.isfinite(scores))
    return scores,ms,plen
receipt={'revision':REV,'license':'CC-BY-NC-4.0; research only','source':'https://huggingface.co/jinaai/jina-reranker-v3.5/tree/'+REV,'python':sys.version,'torch':torch.__version__,'transformers':transformers.__version__,'numpy':np.__version__,'platform':platform.platform(),'cpu':cpu_name(),'provider':'PyTorch CPU SDPA','threads':torch.get_num_threads(),'interop_threads':torch.get_num_interop_threads(),'env_threads':{k:os.environ[k] for k in ('OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS')},'load_ms':load_ms,'input_sha256':digest(a.input),'source_sha256':digest(MODEL/'modeling.py'),'dtype_requested':a.dtype,'pid':os.getpid(),'nice':priority(),'cold_definition':'fresh process/model load; OS filesystem cache not flushed'}
if a.parity:
    q=rows[0]['query'];ds=[c['text'] for c in rows[0]['candidates']]
    bs,bms,_=run(q,ds);receipt['auto_bfloat16_first_ms']=bms
    t=time.perf_counter();model.float();receipt['float_conversion_ms']=(time.perf_counter()-t)*1000
    fs,fms,_=run(q,ds)
    receipt['float32_first_ms']=fms;receipt['parity']={'bf16':bs,'fp32':fs,'max_abs_diff':float(np.max(np.abs(np.array(bs)-fs))),'same_ranking':bool(np.array_equal(np.argsort(bs),np.argsort(fs))),'scope':'first natural payload only; precision change not quantization'}
    if a.dtype=='auto':model.bfloat16()
elif a.dtype=='float32':model.float()
receipt['dtype_actual']=str(next(model.parameters()).dtype); receipt['first_request_ms']=None
append(receiptpath,{'event':'startup',**receipt}); print(json.dumps({'event':'startup',**receipt}),flush=True)
for order in orders:
    for row in rows:
        cs=row['candidates'] if order=='natural' else row['candidates'][::-1]
        docs=[c['text'] for c in cs];q=row['query'];key=hashlib.sha256(json.dumps([q,docs],ensure_ascii=False).encode()).hexdigest()
        if key in cache:
            assert cache[key]['dtype']==receipt['dtype_actual'], 'Do not mix dtype arms in one cache'
            continue
        if not docs:
            rec={'key':key,'order_first_seen':order,'scores':[],'first_ms':0.0,'warm_ms':[],'warm_p50_ms':0.0,'warm_p95_ms':0.0,'repeat_max_abs_diff':0.0,'tokens':0,'candidates':0,'dtype':receipt['dtype_actual'],'peak_rss_kib':peak_rss_kib(),'empty_pool':True}
            append(cachepath,rec);cache[key]=rec;continue
        if time.perf_counter()-START>a.budget_seconds:
            append(receiptpath,{'event':'budget_stop','wall_s':time.perf_counter()-START,'peak_rss_kib':peak_rss_kib()}); print('Budget stop: rerun same command to resume',flush=True);sys.exit(75)
        # Untimed-for-warm first request per payload, followed by three real repetitions.
        scores,first,plen=run(q,docs)
        if receipt['first_request_ms'] is None:receipt['first_request_ms']=first
        times=[];diffs=[]
        for _ in range(3):
            ss,ms,_=run(q,docs);times.append(ms);diffs.append(float(np.max(np.abs(np.array(scores)-ss))))
        rec={'key':key,'order_first_seen':order,'scores':scores,'first_ms':first,'warm_ms':times,'warm_p50_ms':float(np.percentile(times,50)),'warm_p95_ms':float(np.percentile(times,95)),'repeat_max_abs_diff':max(diffs),'tokens':plen,'candidates':len(cs),'dtype':receipt['dtype_actual'],'peak_rss_kib':peak_rss_kib()}
        append(cachepath,rec);cache[key]=rec;print(json.dumps({'done_unique':len(cache),'order':order,'tokens':plen,'warm_ms':times}),flush=True)
for order in orders:
    target=out if len(orders)==1 else Path(str(out)+'.'+order+'.jsonl')
    with open(target,'w') as f:
        for row in rows:
            cs=row['candidates'] if order=='natural' else row['candidates'][::-1]
            key=hashlib.sha256(json.dumps([row['query'],[c['text'] for c in cs]],ensure_ascii=False).encode()).hexdigest();rec=cache[key]
            mapped={c['id']:score for c,score in zip(cs,rec['scores'])};assert len(mapped)==len(cs)
            f.write(json.dumps({'id':row['id'],'scores':[{'id':c['id'],'score':mapped[c['id']]} for c in row['candidates']],'elapsed_ms':rec['warm_p50_ms']})+'\n')
        f.flush();os.fsync(f.fileno())
append(receiptpath,{'event':'complete','wall_s':time.perf_counter()-START,'peak_rss_kib':peak_rss_kib(),'first_request_ms':receipt['first_request_ms'],'unique_payloads':len(cache)})
print('complete',flush=True)
