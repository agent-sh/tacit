"""Synthetic, dependency-free hosted transport and offline evaluation contracts."""
import argparse
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / 'qwen'))
import jev_smoke as jev
import jev_devcal as runner
import evaluate_jev as evaluation


def response(body):
    answers = {}
    for name, q in body['questions'].items():
        answers[name] = {'type': 'noul', 'noul': .8} if q['type'] == 'noul' else {
            'type': 'choice', 'choice': 'useful', 'probabilities': {'useful': .8, 'redundant': .1, 'not_useful': .1}}
    return {'model': jev.MODEL, 'answers': answers, 'usage': {'input_tokens': 100}}


class JevTests(unittest.TestCase):
    def test_help_stdlib_only(self):
        for name in ('jev_smoke', 'jev_devcal', 'evaluate_jev'):
            run = subprocess.run([sys.executable, '-S', str(ROOT/'qwen'/(name+'.py')), '--help'], capture_output=True, text=True, timeout=20)
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertIn('usage:', run.stdout)

    def test_keys_payload_validation_and_no_redirect(self):
        with patch.dict(os.environ, {'TYPESAFE_API_KEY': 'dummy-test-only'}):
            self.assertEqual(jev.get_key(), 'dummy-test-only')
        with patch.dict(os.environ, {'TYPESAFE_API_KEY': ''}):
            with self.assertRaises(ValueError):
                jev.get_key()
        body = jev.payload({'query': 'Task', 'candidates': [{'id': 'hidden-label', 'text': 'Synthetic'}]})
        self.assertNotIn('hidden-label', json.dumps(body))
        self.assertIsNone(jev.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://example.test'))
        data = response(body)
        jev.validate_response(data, body)
        data['model'] = 'wrong'
        with self.assertRaises(ValueError):
            jev.validate_response(data, body)

    def test_opt_in_resume_identity_and_empty_input(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root/'input.jsonl'
            rows = [{'id': 'a', 'query': 'Synthetic', 'candidates': [{'id': 'c', 'text': 'Memory'}]},
                    {'id': 'b', 'query': 'Synthetic', 'candidates': [{'id': 'd', 'text': 'Memory'}]},
                    {'id': 'empty', 'query': 'Empty', 'candidates': []}]
            source.write_text(''.join(json.dumps(r)+'\n' for r in rows))
            args = argparse.Namespace(input=str(source), output=str(root/'out'), key_file=None, allow_egress=False, repeats=3, timeout=2)
            with self.assertRaises(ValueError):
                runner.run(args)
            args.allow_egress = True
            with patch.object(runner, 'get_key', return_value='dummy-only'), patch.object(runner, 'request', side_effect=lambda body, *a: response(body)) as network, redirect_stdout(io.StringIO()):
                report = runner.run(args)
                self.assertEqual(network.call_count, 3)
                self.assertEqual(report['pairs'], 2)
                runner.run(args)
                self.assertEqual(network.call_count, 3)
                with patch.object(jev, 'BASELINE', 'Changed instructions'):
                    with self.assertRaises(ValueError):
                        runner.run(args)
            source.write_text(json.dumps(rows[2])+'\n')
            args.output = str(root/'empty-out')
            with patch.object(runner, 'get_key', side_effect=AssertionError('empty input needs no credential')), redirect_stdout(io.StringIO()):
                self.assertEqual(runner.run(args)['requests'], 0)
            self.assertEqual(json.loads((root/'empty-out/rubric-scores.jsonl').read_text())['scores'], [])

    def test_metrics_unknown_and_family_deduplication(self):
        pool = {'id': 'p', 'case_id': 'c', 'split': 'dev', 'candidates': [{'Path': x, 'Name': x} for x in ('a', 'b', 'bad', 'unknown')], 'derived': [], 'families': {}}
        labels = {'c': {'expected': ['a', 'b', 'missing'], 'rejected': ['bad'], 'uncertain': ['unknown'], 'equivalence_groups': [['a', 'b']]}}
        m, errors = evaluation.metrics([pool], labels, {'c': {'expected': ['a']}}, {'p': ['a', 'b', 'bad', 'unknown']})
        self.assertEqual(m['item_precision'], 2/3)
        self.assertEqual(m['family_recall'], .5)
        self.assertEqual(m['unknown_selected_items'], 1)
        self.assertEqual(m['duplicate_selected_items'], 1)
        self.assertEqual(errors[0]['missing_from_candidate_pool'], ['missing'])

    def test_evaluator_synthetic_and_refuses_test(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pools, primary, reviewed, scores = [], [], [], []
            for split in ('dev', 'calibration'):
                candidates = [{'Path': x, 'Name': x, 'Scope': 'system'} for x in ('good', 'bad')]
                pools.append({'id': split, 'case_id': split, 'episode_id': split, 'split': split,
                    'context': {'arm': 'original'}, 'candidates': candidates, 'derived': [], 'control': candidates, 'families': {}})
                primary.append({'id': split, 'split': split, 'expected': ['good']})
                reviewed.append({'id': split, 'split': split, 'expected': ['good'], 'rejected': ['bad']})
                scores.append({'id': split, 'scores': [{'id': 'good', 'score': .9}, {'id': 'bad', 'score': .1}]})
            for name, records in [('pools', pools), ('scores', scores)]:
                (root/name).write_text(''.join(json.dumps(x)+'\n' for x in records))
            for name, records in [('primary', primary), ('reviewed', reviewed)]:
                (root/name).write_text(json.dumps(records))
            args = argparse.Namespace(pools=str(root/'pools'), primary=str(root/'primary'), reviewed=str(root/'reviewed'),
                scores=['jev-rubric='+str(root/'scores')], output=str(root/'out'), label_status='synthetic')
            with redirect_stdout(io.StringIO()):
                report = evaluation.evaluate(args)
            metric = report['context_results']['original']['jev-rubric']['calibration']
            self.assertEqual(metric['true_positive_items'], 1)
            self.assertEqual(metric['false_positive_items'], 0)
            pools[0]['split'] = 'test'
            (root/'pools').write_text(''.join(json.dumps(x)+'\n' for x in pools))
            with self.assertRaises(ValueError):
                evaluation.evaluate(args)


if __name__ == '__main__':
    unittest.main()
