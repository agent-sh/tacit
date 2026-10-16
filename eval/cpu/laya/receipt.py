import sys
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
import argparse,json,platform,os,hashlib
from common import cpu_name,priority,digest
from setup_source import check_source,PIN

def main():
 p=argparse.ArgumentParser(description='Record current platform and validated external Laya source provenance')
 p.add_argument('--laya-source',required=True);p.add_argument('--output',required=True);a=p.parse_args();source=check_source(a.laya_source)
 result={'cpu':cpu_name(),'platform':platform.platform(),'loadavg':os.getloadavg(),'affinity':sorted(os.sched_getaffinity(0)) if hasattr(os,'sched_getaffinity') else None,'package_commit':PIN,'nice':priority(),'code_sha256':{n:digest(Path(__file__).with_name(n)) for n in ['score.py','download.py']},'source_sha256':{n:digest(source/n) for n in ['laya/__init__.py','laya/agent.py','laya/common.py','setup.py']}}
 Path(a.output).write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
if __name__=='__main__':main()
