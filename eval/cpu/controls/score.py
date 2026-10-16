#!/usr/bin/env python3
"""Offline JSONL cross-encoder; raw logits (not calibrated probabilities).
One model initialization per process. One output per nonblank input line,
input order and candidate order preserved; no sorting or selection.
--batch-size is candidate pairs per ONNX call within a record (default 16).
Dynamic per-batch padding; longest-first pair truncation at 512 tokens.
elapsed_ms includes tokenization/inference, excludes initialization and JSON I/O.
Initialization diagnostics go to stderr. '-' means stdin/stdout. Local files
only, hash-checked against manifest.json; no remote code/model API calls.
"""
import os
for key in ('OMP_NUM_THREADS', 'MKL_NUM_THREADS', 'OPENBLAS_NUM_THREADS', 'NUMEXPR_NUM_THREADS'):
    os.environ[key] = '1'
os.environ['TOKENIZERS_PARALLELISM'] = 'false'
os.environ['CUDA_VISIBLE_DEVICES'] = ''
os.environ['HF_HUB_OFFLINE'] = '1'
import argparse
import hashlib
import json
import pathlib
import sys
import time

import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import workspace, add_workspace, nice, priority, proc_status, cpu_name, quant_engine, verify_artifacts, distinct_files
ROOT = workspace('controls') / 'minilm'

class Scorer:
    def __init__(self, batch_size=16):
        if batch_size < 1:
            raise ValueError('batch size must be positive')
        start = time.perf_counter()
        import numpy as np
        import onnxruntime as ort
        from tokenizers import Tokenizer
        self.np = np
        self.batch_size = batch_size
        manifest = json.loads((ROOT / 'manifest.json').read_text())
        from artifacts import PINS
        expected = PINS['controls'].get(ROOT.name)
        if expected is None or manifest['revision'] != expected[1]:
            raise ValueError('control artifact revision/model mismatch')
        for name, metadata in manifest['files'].items():
            path = ROOT / 'model' / name
            if hashlib.sha256(path.read_bytes()).hexdigest() != metadata['sha256']:
                raise ValueError(f'artifact hash mismatch: {name}')
        self.tokenizer = Tokenizer.from_file(str(ROOT / 'model/tokenizer.json'))
        self.tokenizer.enable_truncation(max_length=512, strategy='longest_first')
        self.tokenizer.enable_padding(pad_id=0, pad_token='[PAD]')
        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
        options.add_session_config_entry('session.intra_op.allow_spinning', '0')
        options.add_session_config_entry('session.inter_op.allow_spinning', '0')
        self.session = ort.InferenceSession(str(ROOT / 'model/onnx/model.onnx'), sess_options=options, providers=['CPUExecutionProvider'])
        self.session.disable_fallback()
        if self.session.get_providers() != ['CPUExecutionProvider']:
            raise RuntimeError('unexpected execution provider')
        self.names = [item.name for item in self.session.get_inputs()]
        self.init_ms = (time.perf_counter() - start) * 1000
        print(json.dumps({'init_ms': self.init_ms, 'providers': self.session.get_providers(), 'inputs': self.names, 'intra_threads': 1, 'inter_threads': 1, 'batch_size': batch_size, 'score_type': 'raw_logit'}), file=sys.stderr)

    def score(self, record):
        start = time.perf_counter()
        if 'id' not in record or not isinstance(record.get('query'), str) or not isinstance(record.get('candidates'), list):
            raise ValueError('expected id, string query, list candidates')
        candidates = record['candidates']
        for candidate in candidates:
            if not isinstance(candidate, dict) or 'id' not in candidate or not isinstance(candidate.get('text'), str):
                raise ValueError('candidate requires id and string text')
        result = []
        for offset in range(0, len(candidates), self.batch_size):
            batch = candidates[offset:offset + self.batch_size]
            encoded = self.tokenizer.encode_batch([(record['query'], c['text']) for c in batch])
            arrays = {'input_ids': [e.ids for e in encoded], 'attention_mask': [e.attention_mask for e in encoded], 'token_type_ids': [e.type_ids for e in encoded]}
            feeds = {name: self.np.asarray(arrays[name], dtype=self.np.int64) for name in self.names}
            logits = self.np.asarray(self.session.run(None, feeds)[0])
            if logits.shape != (len(batch), 1) or not self.np.isfinite(logits).all():
                raise ValueError(f'unexpected logits shape or nonfinite score: {logits.shape}')
            result.extend({'id': c['id'], 'score': float(value)} for c, value in zip(batch, logits[:, 0]))
        return {'id': record['id'], 'scores': result, 'elapsed_ms': (time.perf_counter() - start) * 1000}

def main():
    global ROOT
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--input', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--batch-size', type=int, default=16)
    add_workspace(parser)
    parser.add_argument('--model', choices=['minilm','tiny'], default='minilm')
    args = parser.parse_args()
    ROOT = workspace('controls',args.workspace) / args.model
    distinct_files(args.input,args.output)
    scorer = Scorer(args.batch_size)
    source = sys.stdin if args.input == '-' else open(args.input, encoding='utf-8')
    dest = sys.stdout if args.output == '-' else open(args.output, 'w', encoding='utf-8')
    try:
        for number, line in enumerate(source, 1):
            if not line.strip():
                continue
            try:
                output = scorer.score(json.loads(line))
                dest.write(json.dumps(output, allow_nan=False) + '\n')
                dest.flush()
            except Exception as exc:
                raise ValueError(f'input line {number}: {exc}') from exc
    finally:
        if source is not sys.stdin:
            source.close()
        if dest is not sys.stdout:
            dest.close()

if __name__ == '__main__':
    main()
