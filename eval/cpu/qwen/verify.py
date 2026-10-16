import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import argparse,json
from common import workspace,add_workspace,verify_artifacts,load_rows,preflight_files

def main():
 p=argparse.ArgumentParser(description='Offline full artifact hashes and no-truncation token preflight, both prompts')
 add_workspace(p);p.add_argument('--model',choices=['0.6B','4B'],required=True);p.add_argument('--input',required=True);p.add_argument('--output',required=True);a=p.parse_args()
 r=workspace('qwen',a.workspace)
 preflight_files([a.input,r/('manifest-'+a.model+'.json'),r/('model-'+a.model)],[a.output])
 import score
 from transformers import AutoTokenizer
 r=workspace('qwen',a.workspace);manifest=json.loads((r/('manifest-'+a.model+'.json')).read_text())
 if manifest['revision']!=score.PINS[a.model]:raise ValueError('revision mismatch')
 count=verify_artifacts(r/('model-'+a.model),manifest)
 s=score.Scorer.__new__(score.Scorer);s.tok=AutoTokenizer.from_pretrained(r/('model-'+a.model),local_files_only=True,padding_side='left');s.prefix=s.tok.encode(score.PREFIX,add_special_tokens=False);s.suffix=s.tok.encode(score.SUFFIX,add_special_tokens=False);s.max_tokens=8192
 lengths={}
 for mode in ['default','utility']:
  values=[n for row in load_rows(a.input) for n in s.inputs(row,mode)[1]]
  lengths[mode]={'min':min(values,default=0),'max':max(values,default=0),'pairs':len(values),'truncated':0}
 result={'revision':manifest['revision'],'files_verified':count,'token_lengths':lengths}
 Path(a.output).write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
if __name__=='__main__':main()
