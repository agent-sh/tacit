import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import argparse,json,subprocess,time,os
from common import workspace,add_workspace

def main():
 p=argparse.ArgumentParser(description='Resident Qwen JSONL RPC and overlength rejection smoke')
 add_workspace(p);p.add_argument('--model',choices=['0.6B','4B'],default='0.6B');p.add_argument('--output',required=True);a=p.parse_args()
 import score
 from transformers import AutoTokenizer
 r=workspace('qwen',a.workspace);s=score.Scorer.__new__(score.Scorer);s.tok=AutoTokenizer.from_pretrained(r/('model-'+a.model),local_files_only=True,padding_side='left');s.prefix=s.tok.encode(score.PREFIX,add_special_tokens=False);s.suffix=s.tok.encode(score.SUFFIX,add_special_tokens=False);s.max_tokens=8192
 try:s.inputs({'query':'test','candidates':[{'text':'token '*9000}]},'utility')
 except ValueError as e:rejection=str(e)
 else:raise AssertionError('overlength accepted')
 cmd=[sys.executable,str(Path(__file__).with_name('score.py')),'--workspace',str(r.parent),'--input','-','--output','-','--model',a.model,'--instruction-mode','utility']
 rows=[{**score.SMOKE,'id':'resident-'+str(i)} for i in range(2)];start=time.perf_counter()
 run=subprocess.run(cmd,input=''.join(json.dumps(row)+'\n' for row in rows),capture_output=True,text=True,timeout=240,check=True)
 results=[json.loads(x) for x in run.stdout.splitlines()]
 from common import validate
 validate(rows,results,True);assert results[0]['scores']==results[1]['scores']
 result={'passed':True,'results':results,'overlength_rejection':rejection,'wall_ms':(time.perf_counter()-start)*1000}
 Path(a.output).write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
if __name__=='__main__':main()
