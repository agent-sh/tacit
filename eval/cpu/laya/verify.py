import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts
if __name__ == '__main__':
 from common import verify_main
 verify_main(probability=True)
