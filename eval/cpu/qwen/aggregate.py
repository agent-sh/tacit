import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import argparse,json,statistics
from common import load_rows,validate,percentile,preflight_files

def main():
 p=argparse.ArgumentParser(description='Aggregate Qwen benchmark events and validate complete output coverage')
 p.add_argument('--input',required=True);p.add_argument('--run',required=True);p.add_argument('--output',required=True);a=p.parse_args()
 base=Path(a.run)
 preflight_files([a.input]+[base/name for name in ('measurements.jsonl','default-scores.jsonl','utility-scores.jsonl')],[a.output])
 rows=load_rows(a.input);events=load_rows(base/'measurements.jsonl')
 result={'completed':any(x['event']=='complete' for x in events),'init':events[0],'modes':{}}
 for mode in ['default','utility']:
  mm=[x for x in events if x['event']=='warm-request' and x['mode']==mode];nonempty=[x for x in mm if x['lengths']];groups={}
  for x in mm:groups.setdefault(x['unique_index'],[]).append(x['elapsed_ms'])
  medians=[statistics.median(v) for v in groups.values() if len(v)==3]
  stats={'warm_samples':len(mm),'nonempty_samples':len(nonempty),'unique_requests_with_three_samples':len(medians),'median_of_three_request_p50_ms':percentile(medians,.5),'median_of_three_request_p95_ms':percentile(medians,.95),'nonempty_sample_p50_ms':percentile([x['elapsed_ms'] for x in nonempty],.5),'nonempty_sample_p95_ms':percentile([x['elapsed_ms'] for x in nonempty],.95)}
  wall=sum(x['elapsed_ms'] for x in nonempty)
  stats['cpu_wall_ratio']=sum(x['cpu_ms'] for x in nonempty)/wall if wall else None
  path=base/(mode+'-scores.jsonl')
  if path.exists():
   outputs=load_rows(path)
   expected=rows if result['completed'] else [r for r in rows if r['id'] in {o['id'] for o in outputs}]
   stats.update(validate(expected,outputs,True))
  result['modes'][mode]=stats
 Path(a.output).write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
if __name__=='__main__':main()
