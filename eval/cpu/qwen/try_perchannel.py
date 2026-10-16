import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import argparse,json
from common import add_workspace,workspace,load_rows,nice,quant_engine,preflight_files

def main():
 p=argparse.ArgumentParser(description='Experimental per-channel INT8 parity against fresh FP32; no labels')
 add_workspace(p);p.add_argument('--model',choices=['0.6B','4B'],required=True);p.add_argument('--input',required=True);p.add_argument('--output',required=True);a=p.parse_args()
 r=workspace('qwen',a.workspace)
 preflight_files([a.input,r/('manifest-'+a.model+'.json'),r/('model-'+a.model)],[a.output])
 import score,torch
 score.ROOT=workspace('qwen',a.workspace);nice();s=score.Scorer(a.model)
 rows=[score.SMOKE]+load_rows(a.input)[:1]
 controls={(mode,i):s.score(row,mode)[0] for mode in ['default','utility'] for i,row in enumerate(rows)}
 quant_engine(torch)
 s.model.model=torch.ao.quantization.quantize_dynamic(s.model.model,{torch.nn.Linear:torch.ao.quantization.per_channel_dynamic_qconfig},dtype=torch.qint8,inplace=True)
 with open(a.output,'w') as f:
  for mode in ['default','utility']:
   for i,row in enumerate(rows):
    old=controls[mode,i];out,timing=s.score(row,mode)
    error=max((abs(x['score']-y['score']) for x,y in zip(old['scores'],out['scores'])),default=0)
    f.write(json.dumps({'mode':mode,'index':i,'quant':'dynamic-int8-per-channel-weight-per-tensor-activation','max_abs_diff':error,'fp32':old,'result':out,**timing})+'\n')
if __name__=='__main__':main()
