"""Dependency-free synthetic contracts. Run: python -m unittest discover -s eval/cpu -p test_cpu.py."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from common import digest, percentile, validate, verify_artifacts, workspace
from artifacts import PINS, SOURCE_SHA256, patterns
ROOT = Path(__file__).resolve().parent


class CPUContracts(unittest.TestCase):
    def test_cli_help_without_site_packages(self):
        scripts = {
            'qwen': ['score', 'download', 'aggregate', 'verify', 'try_perchannel', 'prompt_ablation', 'test_cli'],
            'laya': ['score', 'download', 'verify', 'receipt', 'setup_source'],
            'jina': ['score', 'download', 'verify'],
            'controls': ['score', 'download', 'benchmark', 'run_checks'],
        }
        env = dict(os.environ)
        env.pop('TACIT_CPU_WORKSPACE', None)
        env['PYTHONDONTWRITEBYTECODE'] = '1'
        for group, names in scripts.items():
            for name in names:
                with self.subTest(group=group, script=name):
                    run = subprocess.run([sys.executable, '-S', str(ROOT / group / (name+'.py')), '--help'], capture_output=True, text=True, timeout=20, env=env)
                    self.assertEqual(run.returncode, 0, run.stderr)
                    self.assertIn('usage:', run.stdout)

    def test_workspace_outside_repository(self):
        with self.assertRaises(ValueError):
            workspace('qwen', ROOT.parents[1])
        with patch.dict(os.environ, {'TACIT_CPU_WORKSPACE': str(Path.home() / '.cache/synthetic-cpu-tests')}):
            self.assertEqual(workspace('qwen').name, 'qwen')

    def test_order_and_finite_validation(self):
        rows = [{'id':'row','candidates':[{'id':'a'},{'id':'b'}]}]
        out = [{'id':'row','scores':[{'id':'a','score':0.2},{'id':'b','score':0.8}]}]
        self.assertEqual(validate(rows, out, True), {'rows':1,'pairs':2})
        out[0]['scores'].reverse()
        with self.assertRaises(ValueError):validate(rows,out)
        out[0]['scores'].reverse()
        out[0]['scores'][0]['score'] = float('nan')
        with self.assertRaises(ValueError):validate(rows,out)
        out[0]['scores'][0]['score'] = -1
        with self.assertRaises(ValueError):validate(rows,out,True)
        self.assertEqual(validate([],[]), {'rows':0,'pairs':0})

    def test_hash_tamper_and_path_escape(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            path = root/'artifact'
            path.write_text('synthetic artifact')
            manifest = {'files':{'artifact':{'sha256':digest(path)}}}
            self.assertEqual(verify_artifacts(root,manifest),1)
            path.write_text('tampered artifact')
            with self.assertRaises(ValueError):verify_artifacts(root,manifest)
            with self.assertRaises(ValueError):verify_artifacts(root,{'files':{'../escape':{'sha256':'unused'}}})

    def test_percentile_and_pins(self):
        self.assertEqual(percentile([1,2,3],0.5),2)
        self.assertIsNone(percentile([],0.5))
        for models in PINS.values():
            for _, revision in models.values():self.assertRegex(revision,r'^[0-9a-f]{40}$')
        self.assertRegex(SOURCE_SHA256,r'^[0-9a-f]{64}$')
        self.assertIn('modeling.py',patterns('jina'))
        self.assertNotIn('modeling.py',patterns('qwen'))

    def test_synthetic_download_metadata_and_layout(self):
        import hashlib
        import io
        import artifacts
        data = b'synthetic public artifact'
        blob = hashlib.sha1(b'blob ' + str(len(data)).encode() + b'\0' + data).hexdigest()
        for group, key, names in [('controls','tiny',artifacts.CONTROL_FILES), ('qwen','0.6B',['config.json','model.safetensors']), ('laya','base',['rl_agent_config.json','model.safetensors'])]:
            repo, revision = artifacts.PINS[group][key]
            metadata = {'id':repo,'sha':revision,'siblings':[{'rfilename':name,'size':len(data),'blobId':blob} for name in names]}
            def opened(url, timeout):
                return io.BytesIO(json.dumps(metadata).encode() if '/api/models/' in url else data)
            with tempfile.TemporaryDirectory() as temp, patch('urllib.request.urlopen',side_effect=opened):
                out = Path(temp)
                if group == 'laya':
                    artifacts.download_laya(key,out)
                    receipt=json.loads((out/'base-artifacts.json').read_text())
                    self.assertEqual(verify_artifacts(receipt['path'],receipt),len(names))
                else:
                    artifacts.download(group,key,out)
                    base=out/key if group == 'controls' else out
                    manifest=json.loads((base/('manifest.json' if group == 'controls' else 'manifest-0.6B.json')).read_text())
                    model=base/('model' if group == 'controls' else 'model-0.6B')
                    self.assertEqual(verify_artifacts(model,manifest),len(names))

    def test_shared_verifier_and_order_sensitivity(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp)
            rows=[{'id':'r','query':'synthetic','candidates':[{'id':'a','text':'A'},{'id':'b','text':'B'}]}]
            natural=[{'id':'r','scores':[{'id':'a','score':0.8},{'id':'b','score':0.2}]}]
            reverse=[{'id':'r','scores':[{'id':'a','score':0.1},{'id':'b','score':0.9}]}]
            for name,data in [('input',rows),('natural',natural),('reverse',reverse)]:
                (root/name).write_text(''.join(json.dumps(x)+'\n' for x in data))
            run=subprocess.run([sys.executable,'-S',str(ROOT/'jina/verify.py'),'--input',str(root/'input'),'--output',str(root/'natural'),'--reversed',str(root/'reverse'),'--report',str(root/'report')],capture_output=True,text=True)
            self.assertEqual(run.returncode,0,run.stderr)
            report=json.loads((root/'report').read_text())
            self.assertEqual(report['sensitivity']['top1_changes'],1)
            self.assertEqual(report['sensitivity']['pair_order_flips'],1)


if __name__ == '__main__':
    unittest.main()
