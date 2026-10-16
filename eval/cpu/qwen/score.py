#!/usr/bin/env python3
"""Pinned CPU-only Qwen3 scorer. --input - --output - is resident JSONL RPC.
Scores are official softmax(no,yes) probabilities (not calibrated usefulness).
"""
import os
for k in ['OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS','NUMEXPR_NUM_THREADS']: os.environ[k]='1'
os.environ['CUDA_VISIBLE_DEVICES']=''
os.environ['HF_HUB_OFFLINE']='1'
os.environ['TOKENIZERS_PARALLELISM']='false'
import sys,json,time,argparse,resource,hashlib,platform
from pathlib import Path
START=time.perf_counter()
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts, peak_rss_kib, distinct_files
ROOT=workspace('qwen')
PINS={'0.6B':'e61197ed45024b0ed8a2d74b80b4d909f1255473','4B':'22e683669bc0f0bd69640a1354a6d0aebcfeede5'}
DEFAULT='Given a web search query, retrieve relevant passages that answer the query'
UTILITY='Identify saved memories whose printed text adds concrete information or constraints useful for fulfilling the current request. Mere topic overlap or repeating the request is not sufficient.'
PREFIX='<|im_start|>system\nJudge whether the Document meets the requirements based on the Query and the Instruct provided. Note that the answer can only be "yes" or "no".<|im_end|>\n<|im_start|>user\n'
SUFFIX='<|im_end|>\n<|im_start|>assistant\n<think>\n\n</think>\n\n'
SMOKE={'id':'synthetic-action','query':'Before editing the Atlas repository, what instruction check must I perform?','candidates':[{'id':'relevant','text':'Atlas instructions\nRead all relevant AGENTS.md files before changing files in the Atlas repository.'},{'id':'wrong-action','text':'Atlas instructions\nAfter deploying Atlas, delete the AGENTS.md files without reading them.'},{'id':'topic-only','text':'Atlas repository\nThe Atlas repository contains code.'}]}
class Scorer:
 def __init__(self,size):
  global torch, AutoModelForCausalLM, AutoTokenizer
  import torch
  from transformers import AutoModelForCausalLM,AutoTokenizer
  torch.set_num_threads(1); torch.set_num_interop_threads(1)
  self.size=size; self.path=ROOT/('model-'+size)
  manifest=json.loads((ROOT/('manifest-'+size+'.json')).read_text()); assert manifest['revision']==PINS[size]
  verify_artifacts(self.path, manifest)
  t=time.perf_counter()
  self.tok=AutoTokenizer.from_pretrained(self.path,local_files_only=True,padding_side='left')
  self.model=AutoModelForCausalLM.from_pretrained(self.path,local_files_only=True,torch_dtype=torch.float32,attn_implementation='sdpa').cpu().eval()
  assert all(p.device.type=='cpu' for p in self.model.parameters())
  assert all(b.device.type=='cpu' for b in self.model.buffers())
  self.ids=[self.tok.convert_tokens_to_ids(x) for x in ['no','yes']]
  assert all(self.tok.encode(x,add_special_tokens=False)==[i] for x,i in zip(['no','yes'],self.ids))
  self.prefix=self.tok.encode(PREFIX,add_special_tokens=False); self.suffix=self.tok.encode(SUFFIX,add_special_tokens=False)
  self.load_ms=(time.perf_counter()-t)*1000; self.quant='fp32'; self.max_tokens=8192
  self.provenance={'revision':PINS[size], 'manifest_sha256':hashlib.sha256((ROOT/('manifest-'+size+'.json')).read_bytes()).hexdigest(), 'source_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(), 'torch':torch.__version__, 'transformers':__import__('transformers').__version__, 'platform':platform.platform(), 'cpu':cpu_name(), 'threads':torch.get_num_threads(), 'interop_threads':torch.get_num_interop_threads(), 'dtype':'fp32', 'attention':'sdpa', 'max_tokens':self.max_tokens, 'quant_algorithm':'dynamic-int8-linear-backbone-fp32-embedding-head'}
 def inputs(self,row,mode):
  instruction=DEFAULT if mode=='default' else UTILITY
  texts=[f'<Instruct>: {instruction}\n<Query>: {row["query"]}\n<Document>: {c["text"]}' for c in row['candidates']]
  ids=self.tok(texts,padding=False,truncation=False,return_attention_mask=False)['input_ids'] if texts else []
  ids=[self.prefix+x+self.suffix for x in ids]; lengths=list(map(len,ids))
  if lengths and max(lengths)>self.max_tokens: raise ValueError(f'No truncation allowed: {max(lengths)} > {self.max_tokens}')
  return self.tok.pad({'input_ids':ids},padding=True,return_tensors='pt') if ids else None,lengths
 def score(self,row,mode='default',official=False):
  start=time.perf_counter(); cpu=time.process_time(); inp,lengths=self.inputs(row,mode)
  if inp is None: scores=[]
  else:
   assert inp['input_ids'].device.type=='cpu'
   with torch.inference_mode():
    if official: logits=self.model(**inp,use_cache=False,logits_to_keep=1).logits[:,-1,self.ids]
    else:
     hidden=self.model.model(**inp,use_cache=False).last_hidden_state[:,-1,:]
     weight=self.model.lm_head.weight if self.model.lm_head.out_features==2 else self.model.lm_head.weight[self.ids]
     logits=torch.nn.functional.linear(hidden,weight)
    scores=torch.nn.functional.log_softmax(logits,dim=1)[:,1].exp().tolist()
  elapsed=(time.perf_counter()-start)*1000
  out={'id':row['id'],'scores':[{'id':c['id'],'score':s} for c,s in zip(row['candidates'],scores)],'elapsed_ms':elapsed}
  return out,{'elapsed_ms':elapsed,'cpu_ms':(time.process_time()-cpu)*1000,'lengths':lengths}
 def quantize(self):
  t=time.perf_counter()
  # Retain the exact two FP32 output rows; only backbone Linear weights INT8.
  small=torch.nn.Linear(self.model.config.hidden_size,2,bias=False)
  small.weight=torch.nn.Parameter(self.model.lm_head.weight[self.ids].detach().clone(),requires_grad=False)
  self.model.lm_head=small
  quant_engine(torch)
  self.model.model=torch.ao.quantization.quantize_dynamic(self.model.model,{torch.nn.Linear},dtype=torch.qint8,inplace=True)
  self.quant='pytorch-dynamic-int8-linear-backbone-fp32-embedding-head'
  return {'quantize_ms':(time.perf_counter()-t)*1000,'quantized_linear_count':sum(isinstance(m,torch.ao.nn.quantized.dynamic.Linear) for m in self.model.modules()),'engine':torch.backends.quantized.engine}
def key(row): return json.dumps([row['query'],[c['text'] for c in row['candidates']]],ensure_ascii=False)
def emit(f,x): f.write(json.dumps(x,ensure_ascii=False)+'\n'); f.flush()
def payload_hash(row,mode):
 return hashlib.sha256(json.dumps([mode,row],sort_keys=True,ensure_ascii=False).encode()).hexdigest()
def checked_control_cache(path,scorer,rows,modes):
 if not path: return []
 cached=[json.loads(x) for x in Path(path).read_text().splitlines() if x.strip()]
 expected={'provenance':scorer.provenance,'payloads':{mode:[payload_hash(row,mode) for row in rows] for mode in modes}}
 if not cached or cached[0].get('control_identity')!=expected:
  raise ValueError('control cache identity mismatch: exact payloads and scorer provenance required')
 for event in cached:
  if event['event']=='official-parity':
   if event.get('payload_sha256')!=payload_hash(SMOKE,'default') or not 0<=event.get('max_abs_diff',float('inf'))<1e-5:
    raise ValueError('control cache official parity mismatch')
  elif event['event']=='fp32-control':
   mode,index=event['mode'],event['index']
   if mode not in modes or not isinstance(index,int) or not 0<=index<len(rows) or event.get('payload_sha256')!=payload_hash(rows[index],mode):
    raise ValueError('control cache payload mismatch')
   result=event['result']; row=rows[index]
   if result['id']!=row['id'] or [c['id'] for c in result['scores']]!=[c['id'] for c in row['candidates']]:
    raise ValueError('control cache result IDs mismatch')
 return cached
def main():
 global ROOT, torch, AutoModelForCausalLM, AutoTokenizer
 p=argparse.ArgumentParser(); p.add_argument('--input',required=True); p.add_argument('--output',required=True); p.add_argument('--model',choices=PINS,default='0.6B'); p.add_argument('--instruction-mode',choices=['default','utility','both'],default='default'); p.add_argument('--quant',choices=['fp32','int8'],default='fp32'); p.add_argument('--benchmark',action='store_true'); p.add_argument('--control-cache'); add_workspace(p); a=p.parse_args(); ROOT=workspace('qwen',a.workspace)
 distinct_files(a.input,a.output)
 import torch
 from transformers import AutoModelForCausalLM,AutoTokenizer
 nice()
 s=Scorer(a.model)
 if a.benchmark: return benchmark(s,a)
 if a.instruction_mode=='both': raise ValueError('both is only for benchmark')
 if a.quant=='int8':
  validation=ROOT/(a.model+'-validation.json')
  receipt=json.loads(validation.read_text()) if validation.exists() else {}
  if not receipt.get('quant_pass') or receipt.get('provenance')!=s.provenance or receipt.get('engine')!=quant_engine(torch) or a.instruction_mode not in receipt.get('validated_modes',[]):
   raise ValueError('INT8 requires successful local benchmark validation for this exact scorer and engine')
  s.quantize()
 fin=sys.stdin if a.input=='-' else open(a.input); fout=sys.stdout if a.output=='-' else open(a.output,'w')
 try:
  for line in fin:
   if not line.strip(): continue
   row=json.loads(line); result,_=s.score(row,a.instruction_mode); emit(fout,result)
 finally:
  if fin is not sys.stdin: fin.close()
  if fout is not sys.stdout: fout.close()
def benchmark(s,a):
 rows=[json.loads(x) for x in Path(a.input).read_text().splitlines() if x.strip()]; unique={}
 for r in rows: unique.setdefault(key(r),r)
 modes=['default','utility'] if a.instruction_mode=='both' else [a.instruction_mode]
 validation_rows=[SMOKE]+list(unique.values())[:6]
 cached=checked_control_cache(a.control_cache,s,validation_rows,modes)
 destinations=[Path(a.output)/name for name in ['measurements.jsonl','validation.json','runtime.json']+[mode+suffix for mode in modes for suffix in ['-scores.jsonl','-unique.jsonl']]]
 if a.quant=='int8': destinations.append(ROOT/(s.size+'-validation.json'))
 distinct_files(a.input,*destinations)
 if a.control_cache: distinct_files(a.control_cache,*destinations)
 base=Path(a.output); base.mkdir(exist_ok=True,parents=True)
 evidence=open(base/'measurements.jsonl','w'); cpu0=time.process_time()
 meta={'model':s.size,'revision':PINS[s.size],'load_ms':s.load_ms,'startup_ms':(time.perf_counter()-START)*1000,'torch':torch.__version__,'transformers':__import__('transformers').__version__,'cuda':torch.version.cuda,'device':'cpu','threads':torch.get_num_threads(),'interop_threads':torch.get_num_interop_threads(),'env':{k:os.environ.get(k) for k in ['OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS','CUDA_VISIBLE_DEVICES']},'nice':priority(),'loadavg_start':os.getloadavg(),'platform':platform.platform(),'rows':len(rows),'candidate_pairs':sum(len(r['candidates']) for r in rows),'unique_requests':len(unique),'dedup':'identical query plus ordered printed candidate texts only; IDs restored','pid':os.getpid()}
 emit(evidence,{'event':'init',**meta,'control_identity':{'provenance':s.provenance,'payloads':{mode:[payload_hash(row,mode) for row in validation_rows] for mode in modes}}})
 controls={}; first=True
 # Full-head official vs mathematically identical two-row output head.
 cached_parity=next((x for x in cached if x['event']=='official-parity'),None)
 if cached_parity:
  emit(evidence,{**cached_parity,'reused_control_from':a.control_cache})
 else:
  full,ft=s.score(SMOKE,'default',official=True); reduced,rt=s.score(SMOKE,'default')
  diff=max(abs(x['score']-y['score']) for x,y in zip(full['scores'],reduced['scores']))
  assert diff<1e-5,diff
  emit(evidence,{'event':'official-parity','payload_sha256':payload_hash(SMOKE,'default'),'max_abs_diff':diff,'first_request':ft,'official':full,'reduced':reduced})
 for mode in modes:
  controls[mode]=[]
  for i,r in enumerate(validation_rows):
   cache=next((x for x in cached if x['event']=='fp32-control' and x['mode']==mode and x['index']==i),None)
   if cache:
    out=cache['result']; t={k:cache[k] for k in ['elapsed_ms','cpu_ms','lengths']}; t['reused_control_from']=a.control_cache
   else: out,t=s.score(r,mode)
   controls[mode].append(out)
   emit(evidence,{'event':'fp32-control','mode':mode,'index':i,'payload_sha256':payload_hash(r,mode),'result':out,**t})
 quant_meta=s.quantize() if a.quant=='int8' else {}
 passed=True; parity=[]
 for mode in modes:
  for i,r in enumerate(validation_rows):
   out,t=s.score(r,mode); old=controls[mode][i]
   v=[c['score'] for c in old['scores']]; w=[c['score'] for c in out['scores']]
   differences=[abs(x-y) for x,y in zip(v,w)]
   # Predeclared absolute-probability tolerance .10 plus same top1 and pair order
   # for pairs separated by >.05 in FP32; near ties are reported, not forced.
   inversions=sum((v[j]-v[k])*(w[j]-w[k])<0 and abs(v[j]-v[k])>.05 for j in range(len(v)) for k in range(j))
   ok=(not differences or max(differences)<=.10) and inversions==0 and (not v or v.index(max(v))==w.index(max(w)))
   passed=passed and ok
   item={'event':'quant-validation','mode':mode,'index':i,'max_abs_diff':max(differences,default=0),'material_inversions':inversions,'passed':ok,'result':out,**t}; parity.append(item); emit(evidence,item)
 validation={'quant_pass':passed,'quant':s.quant,'provenance':s.provenance,'validated_modes':modes,**quant_meta,'criteria':'max absolute probability difference <=0.10, identical top1, no pair inversion where FP32 gap >0.05','controls':controls,'comparisons':parity}
 (base/'validation.json').write_text(json.dumps(validation,indent=2))
 if a.quant=='int8': (ROOT/(s.size+'-validation.json')).write_text(json.dumps(validation,indent=2))
 if not passed:
  emit(evidence,{'event':'blocked','reason':'INT8 parity criteria failed; no production quant scores emitted'})
  # Reload FP32 in a fresh process, rather than holding both models.
  raise RuntimeError('Quant validation failed; rerun --quant fp32')
 all_results={}; stats={}
 for mode in modes:
  values={}; timings=[]; outpath=base/(mode+'-scores.jsonl'); uniqueout=open(base/(mode+'-unique.jsonl'),'w')
  for i,(k,r) in enumerate(unique.items()):
   repeats=[]
   for repeat in range(3):
    out,t=s.score(r,mode); repeats.append(t['elapsed_ms']); timings.append(t['elapsed_ms'])
    emit(evidence,{'event':'warm-request','mode':mode,'unique_index':i,'repeat':repeat,'id':r['id'],**t})
    if repeat==0: values[k]=out
   emit(uniqueout,{'payload_key':hashlib.sha256(k.encode()).hexdigest(),'result':values[k],'three_timings_ms':repeats})
   # Map every completed payload immediately, preserving all row/candidate IDs.
   with outpath.open('w') as f:
    for row in rows:
     if key(row) not in values: continue
     original=values[key(row)]
     emit(f,{'id':row['id'],'scores':[{'id':c['id'],'score':sc['score']} for c,sc in zip(row['candidates'],original['scores'])],'elapsed_ms':original['elapsed_ms']})
  import numpy as np
  stats[mode]={'warm_request_p50_ms':float(np.percentile(timings,50)),'warm_request_p95_ms':float(np.percentile(timings,95)),'measurements':len(timings),'latency_definition':'three actual uncached forward passes per unique full request; one batch containing all its candidates'}
  uniqueout.close()
 meta.update({'quant':s.quant,**quant_meta,'quant_pass':passed,'timings':stats,'process_wall_ms':(time.perf_counter()-START)*1000,'measured_cpu_seconds':time.process_time()-cpu0,'peak_rss_kib':peak_rss_kib(),'loadavg_end':os.getloadavg(),'proc_status':proc_status()})
 (base/'runtime.json').write_text(json.dumps(meta,indent=2)); emit(evidence,{'event':'complete',**meta}); evidence.close(); print(json.dumps(meta),flush=True)
if __name__=='__main__': main()
