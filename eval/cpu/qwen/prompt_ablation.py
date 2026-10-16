"""Development-only prompt ablation; no held-out cases or labels loaded."""
import os,sys,json,time,hashlib,argparse
from pathlib import Path
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts
ROOT=workspace('qwen'); LAYA=workspace('laya')
for k in ['OMP_NUM_THREADS','MKL_NUM_THREADS','OPENBLAS_NUM_THREADS','NUMEXPR_NUM_THREADS']: os.environ[k]='1'
os.environ.update(CUDA_VISIBLE_DEVICES='',USE_TF='0',HF_HUB_OFFLINE='1',TRANSFORMERS_OFFLINE='1',HF_HUB_DISABLE_TELEMETRY='1',TOKENIZERS_PARALLELISM='false',LAYA_CPU_AMP='0')
PROMPTS={
'baseline':'Does this memory add concrete information or a constraint useful for fulfilling the current request, beyond what the request already states? Mere topic overlap is not sufficient.',
'new_constraint':'The task is already known. Does the memory supply an additional requirement, restriction, preference, or fact that should change how the task is carried out? Answer true only for additional actionable information. A paraphrase of the task or background on its topic is false.',
'action_difference':'Compare carrying out the task with and without this memory. Would the memory change a concrete choice, step, or content of the answer because it supplies information missing from the task? Return false for a restatement of the task, topic overlap, or unrelated background.'}
p=argparse.ArgumentParser(description=__doc__);add_workspace(p);p.add_argument('--input',required=True);p.add_argument('--output',required=True);p.add_argument('--laya-source');p.add_argument('--model',choices=['base','typed'],required=True);a=p.parse_args(); LAYA=workspace('laya',a.workspace)
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'laya'))
from setup_source import check_source
check_source(a.laya_source or LAYA/'package')
import torch,laya
from laya.common import build_head,build_sequence
torch.set_num_threads(1);torch.set_num_interop_threads(1)
nice()
receipt=json.loads((LAYA/(a.model+'-artifacts.json')).read_text());agent=laya.load(receipt['path'],device='cpu',backend='eager',expected_sha256=receipt['sha256'])
rows=[json.loads(x) for x in Path(a.input).read_text().splitlines()]
out=Path(a.output)
with out.open('w') as f:
 for name,instruction in PROMPTS.items():
  question={'useful':{'type':'noul','instructions':instruction}}; internal=agent._to_internal(question['useful'])
  assert build_head(agent.tok,internal,agent.cfg['head_max_len'])[0]==build_head(agent.tok,internal,100000)[0]
  for row in rows:
   states=[{'task':row['query'],'memory':c['text']} for c in row['candidates']];lengths=[]
   for state in states:
    ids,markers,stats,trunc=build_sequence(agent.tok,state,internal,max_len=agent.cfg['max_len'],head_max_len=agent.cfg['head_max_len'],return_stats=True,return_truncation_stats=True)
    assert not trunc['truncated'] and trunc['state_tokens_dropped']==0;lengths.append(len(ids))
   t=time.perf_counter();results=agent.predict_batch(states,question,batch_size=len(states)) if states else [];elapsed=time.perf_counter()-t
   scores={c['id']:float(v['answers']['useful']['noul']) for c,v in zip(row['candidates'],results)}
   record={'model':receipt['repo'],'revision':receipt['revision'],'prompt_name':name,'instruction':instruction,'id':row['id'],'scores':scores,'elapsed_s':elapsed,'tokens':lengths,'truncated':False,'threads':torch.get_num_threads(),'device':'cpu','evaluation':'previously inspected synthetic development examples only'}
   f.write(json.dumps(record)+'\n');f.flush();print(json.dumps(record),flush=True)
