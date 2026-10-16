"""Dependency-free regressions for the resident CLI and control-cache identity."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT))
spec = importlib.util.spec_from_file_location('qwen_score', ROOT/'qwen/score.py')
qwen = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qwen)


class FakeScorer:
    provenance = {'synthetic': True}
    size = '0.6B'
    load_ms = 0
    quant = 'fp32'
    def score(self, row, mode='default', official=False):
        value = .9 if self.quant == 'fp32' else .2
        return ({'id': row['id'], 'scores': [{'id': c['id'], 'score': value} for c in row['candidates']], 'elapsed_ms': 1},
                {'elapsed_ms': 1, 'cpu_ms': 1, 'lengths': [100]*len(row['candidates'])})
    def quantize(self):
        self.quant = 'int8'
        return {}


def fake_modules():
    torch = types.ModuleType('torch')
    torch.__version__ = 'synthetic'
    torch.version = types.SimpleNamespace(cuda=None)
    torch.get_num_threads = torch.get_num_interop_threads = lambda: 1
    transformers = types.ModuleType('transformers')
    transformers.__version__ = 'synthetic'
    transformers.AutoModelForCausalLM = transformers.AutoTokenizer = None
    numpy = types.ModuleType('numpy')
    numpy.percentile = lambda values, p: values[0]
    return {'torch': torch, 'transformers': transformers, 'numpy': numpy}


class QwenContracts(unittest.TestCase):
    def test_int8_admission_requires_requested_instruction_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'qwen').mkdir()
            source=root/'input.jsonl';source.write_text(json.dumps(qwen.SMOKE)+'\n')
            receipt={'quant_pass':True,'provenance':FakeScorer.provenance,'engine':'synthetic','validated_modes':['utility']}
            (root/'qwen/0.6B-validation.json').write_text(json.dumps(receipt))
            argv=['score.py','--workspace',str(root),'--input',str(source),'--output',str(root/'output.jsonl'),'--quant','int8','--instruction-mode','default']
            with patch.dict(sys.modules,fake_modules()),patch.object(sys,'argv',argv),patch.object(qwen,'Scorer',return_value=FakeScorer()),patch.object(qwen,'quant_engine',return_value='synthetic'):
                with self.assertRaisesRegex(ValueError,'validation'):
                    qwen.main()
            self.assertFalse((root/'output.jsonl').exists())

    def test_workspace_validation_receipt_cannot_overwrite_input(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            source=root/'0.6B-validation.json'
            original=json.dumps(qwen.SMOKE)+'\n';source.write_text(original)
            args=argparse.Namespace(input=str(source),output=str(root/'out'),instruction_mode='utility',control_cache=None,quant='int8')
            with patch.dict(sys.modules,fake_modules()),patch.object(qwen,'torch',fake_modules()['torch'],create=True),patch.object(qwen,'ROOT',root):
                with self.assertRaisesRegex(ValueError,'input.*output'):
                    qwen.benchmark(FakeScorer(),args)
            self.assertEqual(source.read_text(),original)

    def test_all_cpu_scorers_reject_file_aliases_without_dependencies(self):
        import subprocess
        from common import distinct_files
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)/'input.jsonl'
            original = json.dumps(qwen.SMOKE)+'\n'
            source.write_text(original)
            alias = Path(directory)/'alias.jsonl'
            os.link(source,alias)
            with self.assertRaises(ValueError):
                distinct_files(source,alias)
            scripts = [('qwen/score.py',[]),('laya/score.py',['--model','base']),('jina/score.py',[]),('controls/score.py',[])]
            for script, extra in scripts:
                result = subprocess.run([sys.executable,'-S',str(ROOT/script),'--input',str(source),'--output',str(alias),*extra],capture_output=True,text=True,timeout=10)
                self.assertNotEqual(result.returncode,0)
                self.assertIn('input and output must be different files',result.stderr)
                self.assertEqual(source.read_text(),original)

    def test_bound_control_cache_accepts_exact_identity_only(self):
        with tempfile.TemporaryDirectory() as directory:
            cache = Path(directory)/'cache.jsonl'
            row = qwen.SMOKE
            identity = {'provenance':FakeScorer.provenance,'payloads':{'utility':[qwen.payload_hash(row,'utility')]}}
            result,timing = FakeScorer().score(row)
            event = {'event':'fp32-control','mode':'utility','index':0,'payload_sha256':qwen.payload_hash(row,'utility'),'result':result,**timing}
            cache.write_text(json.dumps({'event':'init','control_identity':identity})+'\n'+json.dumps(event)+'\n')
            self.assertEqual(len(qwen.checked_control_cache(cache,FakeScorer(),[row],['utility'])),2)
            changed = dict(row,query='Different synthetic request')
            with self.assertRaises(ValueError):
                qwen.checked_control_cache(cache,FakeScorer(),[changed],['utility'])
            scorer = FakeScorer();scorer.provenance={'synthetic':False}
            with self.assertRaises(ValueError):
                qwen.checked_control_cache(cache,scorer,[row],['utility'])
            event['payload_sha256']='wrong'
            cache.write_text(json.dumps({'event':'init','control_identity':identity})+'\n'+json.dumps(event)+'\n')
            with self.assertRaises(ValueError):
                qwen.checked_control_cache(cache,FakeScorer(),[row],['utility'])

    def test_same_file_rejected_before_loading_or_erasing(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory)/'input.jsonl'
            original = json.dumps(qwen.SMOKE)+'\n'
            source.write_text(original)
            argv = ['score.py', '--input', str(source), '--output', str(source)]
            with patch.dict(sys.modules, fake_modules()), patch.object(sys, 'argv', argv), patch.object(qwen, 'Scorer', return_value=FakeScorer()):
                with self.assertRaisesRegex(ValueError, 'input.*output'):
                    qwen.main()
            self.assertEqual(source.read_text(), original)

    def test_unbound_cached_controls_cannot_authorize_quantization(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, cache = root/'input.jsonl', root/'cache.jsonl'
            source.write_text(json.dumps(qwen.SMOKE)+'\n')
            events = [{'event': 'init', 'revision': qwen.PINS['0.6B']},
                      {'event': 'official-parity', 'max_abs_diff': 0}]
            for i in range(2):
                result, timing = FakeScorer().score(qwen.SMOKE)
                for item in result['scores']:
                    item['score'] = .2  # Unrelated cached values conceal the actual .70 quantization error.
                events.append({'event': 'fp32-control', 'mode': 'utility', 'index': i, 'result': result, **timing})
            cache.write_text(''.join(json.dumps(e)+'\n' for e in events))
            args = argparse.Namespace(input=str(source), output=str(root/'out'), instruction_mode='utility', control_cache=str(cache), quant='int8')
            with patch.dict(sys.modules, fake_modules()), patch.object(qwen, 'torch', fake_modules()['torch'], create=True), patch.object(qwen, 'ROOT', root):
                with self.assertRaisesRegex(ValueError, 'cache'):
                    qwen.benchmark(FakeScorer(), args)
            validation = root/'out/validation.json'
            self.assertFalse(validation.exists() and json.loads(validation.read_text())['quant_pass'])


if __name__ == '__main__':
    unittest.main()
