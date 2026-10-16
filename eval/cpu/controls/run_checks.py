"""Local ONNX/tokenizer verification plus explicitly supplied workload receipts."""
import argparse
import json
from pathlib import Path
import subprocess
import sys
import time
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import add_workspace, digest, load_rows, validate, workspace, preflight_files


def main():
    p = argparse.ArgumentParser(description=__doc__)
    add_workspace(p)
    p.add_argument('--model', choices=['minilm', 'tiny'], default='minilm')
    p.add_argument('--input', default=str(Path(__file__).resolve().parents[1] / 'fixtures/smoke.jsonl'))
    p.add_argument('--output', required=True, help='external receipt directory')
    p.add_argument('--benchmark', action='store_true', help='also run three fresh worker processes')
    a = p.parse_args()
    preflight_files([a.input, workspace('controls', a.workspace)/a.model],
                    [Path(a.output)/name for name in ['scores.jsonl', 'execution.json'] +
                     (['benchmark.json'] if a.benchmark else [])])
    import score
    import numpy
    import onnxruntime
    import tokenizers
    score.ROOT = workspace('controls', a.workspace) / a.model
    scorer = score.Scorer()
    tokenizer = scorer.tokenizer
    vocab = (score.ROOT / 'model/vocab.txt').read_text().splitlines()
    assert all(tokenizer.token_to_id(token) == i for i, token in enumerate(vocab))
    left, right = tokenizer.encode_batch([('HELLO world', 'Goodbye'), ('hello', 'world')])
    assert left.tokens == ['[CLS]', 'hello', 'world', '[SEP]', 'goodbye', '[SEP]']
    assert left.type_ids == [0, 0, 0, 0, 1, 1]
    assert right.ids[-1] == 0 and right.attention_mask[-1] == 0
    long = tokenizer.encode('restart service ' * 1000, 'restart service ' * 2000)
    assert len(long.ids) == 512 and set(long.type_ids) == {0, 1}
    rows = load_rows(a.input)
    results = [scorer.score(row) for row in rows]
    coverage = validate(rows, results)
    out = Path(a.output)
    out.mkdir(parents=True, exist_ok=True)
    scores = out / 'scores.jsonl'
    scores.write_text(''.join(json.dumps(row) + '\n' for row in results))
    result = {'versions': {'python': sys.version, 'numpy': numpy.__version__, 'onnxruntime': onnxruntime.__version__, 'tokenizers': tokenizers.__version__}, 'providers': scorer.session.get_providers(), 'tokenization': {'vocab_ids_verified': len(vocab), 'pair_tokens': left.tokens, 'pair_type_ids': left.type_ids, 'long_pair_length': len(long.ids), 'strategy': 'longest_first'}, **coverage, 'input_sha256': digest(a.input), 'output_sha256': digest(scores), 'init_ms': scorer.init_ms, 'scoring_ms': sum(row['elapsed_ms'] for row in results)}
    if a.benchmark:
        start = time.perf_counter()
        run = subprocess.run([sys.executable, str(Path(__file__).with_name('benchmark.py')), '--workspace', str(score.ROOT.parents[1]), '--model', a.model, '--input', a.input, '--output', str(out / 'benchmark.json')], capture_output=True, text=True, timeout=600, check=True)
        result['benchmark'] = json.loads(run.stdout)
        result['benchmark_wall_ms'] = (time.perf_counter() - start) * 1000
    (out / 'execution.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))


if __name__ == '__main__':
    main()
