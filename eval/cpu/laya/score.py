#!/usr/bin/env python3
"""Pinned local CPU Laya scorer. Input: {id,query,candidates:[{id,text}]}.
Scores are checkpoint Noul P(true), NOT usefulness-calibrated probabilities.
"""
import os,time,sys,json,argparse,resource,hashlib,platform
from pathlib import Path
START=time.perf_counter()
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts, peak_rss_kib, preflight_files
ROOT=workspace('laya')
for k in ('OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS','NUMEXPR_NUM_THREADS'): os.environ[k]='1'
os.environ.update(CUDA_VISIBLE_DEVICES='',TOKENIZERS_PARALLELISM='false',USE_TF='0',HF_HUB_OFFLINE='1',TRANSFORMERS_OFFLINE='1',HF_HUB_DISABLE_TELEMETRY='1',LAYA_CPU_AMP='0',HF_HOME=str(ROOT/'hf'))


QUESTION='Does this memory add concrete information or a constraint useful for fulfilling the current request, beyond what the request already states? Mere topic overlap is not sufficient.'
QUESTIONS={'useful':{'type':'noul','instructions':QUESTION}}

def main():
 global ROOT
 ap=argparse.ArgumentParser(); ap.add_argument('--input',required=True); ap.add_argument('--output',required=True); ap.add_argument('--model',choices=['base','typed','convaiinnovations/laya','convaiinnovations/laya-typed-decisions'],required=True); ap.add_argument('--warm-repeats',type=int,default=0)
 add_workspace(ap); ap.add_argument('--laya-source', help='external git checkout at documented pinned commit'); a=ap.parse_args(); ROOT=workspace('laya',a.workspace)
 preflight_files([a.input],[a.output,a.output+'.timings.jsonl',a.output+'.metrics.json'])
 key={'convaiinnovations/laya':'base','convaiinnovations/laya-typed-decisions':'typed'}.get(a.model,a.model)
 preflight_files([ROOT/(key+'-artifacts.json'),ROOT/key,Path(a.laya_source).expanduser() if a.laya_source else ROOT/'package'],[a.output,a.output+'.timings.jsonl',a.output+'.metrics.json'])
 nice()
 os.environ['HF_HOME']=str(ROOT/'hf')
 from setup_source import check_source
 check_source(a.laya_source or ROOT/'package')
 key={'convaiinnovations/laya':'base','convaiinnovations/laya-typed-decisions':'typed'}.get(a.model,a.model)
 import torch,numpy as np,transformers,laya
 from laya.common import build_sequence,build_head
 torch.set_num_threads(1); torch.set_num_interop_threads(1)
 receipt=json.loads((ROOT/(key+'-artifacts.json')).read_text()); init=time.perf_counter()
 from artifacts import PINS
 if (receipt['repo'],receipt['revision'])!=PINS['laya'][key]: raise ValueError('Laya artifact revision mismatch')
 preflight_files([receipt['path']],[a.output,a.output+'.timings.jsonl',a.output+'.metrics.json'])
 agent=laya.load(receipt['path'],device='cpu',backend='eager',expected_sha256=receipt['sha256'])
 load_ms=(time.perf_counter()-init)*1000
 assert agent.device.type=='cpu'
 assert all(t.device.type=='cpu' for t in list(agent.model.parameters())+list(agent.model.buffers()))
 assert not agent.amp_enabled
 def check_inputs(module,args,kwargs):
  def walk(x):
   if isinstance(x,torch.Tensor): assert x.device.type=='cpu'
   elif isinstance(x,dict):
    for v in x.values(): walk(v)
   elif isinstance(x,(tuple,list)):
    for v in x: walk(v)
  walk(args); walk(kwargs)
 agent.model.register_forward_pre_hook(check_inputs,with_kwargs=True)
 rows=[json.loads(s) for s in Path(a.input).read_text().splitlines() if s.strip()]
 # Fail closed if any state, question, or option was truncated. Compare capped and uncapped heads.
 internal=agent._to_internal(QUESTIONS['useful']); headmax=agent.cfg['head_max_len']; maxlen=agent.cfg['max_len']
 assert build_head(agent.tok,internal,headmax)[0]==build_head(agent.tok,internal,100000)[0], 'question/options exceed head budget'
 lengths=[]; states_by_row=[]
 for row in rows:
  states=[{'task':row['query'],'memory':c['text']} for c in row['candidates']]; states_by_row.append(states)
  for s in states:
   ids,markers,stats,trunc=build_sequence(agent.tok,s,internal,max_len=maxlen,head_max_len=headmax,return_stats=True,return_truncation_stats=True)
   assert not trunc['truncated'] and trunc['state_tokens_dropped']==0, (row['id'],trunc)
   assert len(markers)==2 and stats['options_distinct']==2
   lengths.append(len(ids))
 cold_init_ms=(time.perf_counter()-START)*1000
 def score(row,states):
  t=time.perf_counter(); result=agent.predict_batch(states,QUESTIONS,batch_size=len(states)) if states else []
  scores=[{'id':c['id'],'score':float(r['answers']['useful']['noul'])} for c,r in zip(row['candidates'],result)]
  assert len(scores)==len(row['candidates']) and all(0<=s['score']<=1 for s in scores)
  return {'id':row['id'],'scores':scores,'elapsed_ms':(time.perf_counter()-t)*1000}
 outputs=[]; warm=[]
 with open(a.output,'w') as f:
  for row,states in zip(rows,states_by_row):
   out=score(row,states); outputs.append(out); f.write(json.dumps(out)+'\n'); f.flush()
 with open(a.output+'.timings.jsonl','w') as f:
  for rep in range(a.warm_repeats):
   for row,states,original in zip(rows,states_by_row,outputs):
    out=score(row,states); delta=max((abs(x['score']-y['score']) for x,y in zip(out['scores'],original['scores'])),default=0)
    rec={'repeat':rep+1,'id':row['id'],'elapsed_ms':out['elapsed_ms'],'max_score_delta':delta}; warm.append(rec); f.write(json.dumps(rec)+'\n'); f.flush()
 times=[r['elapsed_ms'] for r in warm] or [r['elapsed_ms'] for r in outputs]
 metrics={'model':receipt['repo'],'revision':receipt['revision'],'package_commit':'4ac226d5ee88ed6df8db288a05c766d7a34ee682','package_version':laya.__version__,'question':QUESTION,'rows':len(rows),'pairs':len(lengths),'input_sha256':hashlib.sha256(Path(a.input).read_bytes()).hexdigest(),'backend':agent.backend,'attention':agent.model.encoder.config._attn_implementation,'dtype':str(next(agent.model.parameters()).dtype),'torch':torch.__version__,'transformers':transformers.__version__,'python':platform.python_version(),'torch_intra':torch.get_num_threads(),'torch_inter':torch.get_num_interop_threads(),'nice':priority(),'cuda_build':torch.version.cuda,'device':str(agent.device),'environment':{k:os.environ[k] for k in ('OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS','CUDA_VISIBLE_DEVICES')},'max_len':maxlen,'head_max_len':headmax,'head_tokens':len(build_head(agent.tok,internal,headmax)[0]),'sequence_tokens_min':min(lengths,default=0),'sequence_tokens_max':max(lengths,default=0),'truncated':0,'cold_init_ms_including_import_hash_load_preflight':cold_init_ms,'load_ms_including_sha_verification':load_ms,'first_request_ms':outputs[0]['elapsed_ms'] if outputs else None,'warm_repeats':a.warm_repeats,'warm_request_count':len(warm),'request_p50_ms':float(np.percentile(times,50)) if times else None,'request_p95_ms':float(np.percentile(times,95)) if times else None,'initial_pass_ms':sum(x['elapsed_ms'] for x in outputs),'warm_pass_total_ms':[sum(x['elapsed_ms'] for x in warm if x['repeat']==i+1) for i in range(a.warm_repeats)],'warm_max_score_delta':max((x['max_score_delta'] for x in warm),default=0),'peak_rss_mib':peak_rss_kib()/1024,'cpu_user_s':resource.getrusage(resource.RUSAGE_SELF).ru_utime,'cpu_system_s':resource.getrusage(resource.RUSAGE_SELF).ru_stime,'process_wall_s':time.perf_counter()-START,'temperature':agent.temperature,'temperature_by_options':agent.temperature_by_options,'score_semantics':'Official checkpoint noul P(true), shipped temperature retained, official rounding to 4 decimals; no domain calibration or labels used'}
 Path(a.output+'.metrics.json').write_text(json.dumps(metrics,indent=2)); print(json.dumps(metrics),flush=True)
if __name__=='__main__': main()
