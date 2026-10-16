# Local CPU scoring experiments

Research tools, not a change to Tacit's ranker or delivery policy. No private corpus,
labels, model weights, model implementation, virtual environment, or raw results
are included. `fixtures/smoke.jsonl` is synthetic. Quality labels are never loaded
by these scorers. A successful smoke proves execution and contracts, not utility.

## Workspaces and dependencies

Run from the repository root. Python 3.12+ on Linux or macOS is the intended target.
Every executable supports `--help` using only the standard library, before model
or optional dependency imports. Offline verification helpers need no dependencies.

`--workspace ROOT` or `TACIT_CPU_WORKSPACE` selects an **external cache root**.
The default is `~/.cache/tacit-cpu`. A root inside this repository is rejected.
The model paths are `ROOT/qwen/model-0.6B`, `ROOT/qwen/model-4B`,
`ROOT/laya/{base,typed}/model`, `ROOT/jina/model`, and
`ROOT/controls/{minilm,tiny}/model`. Manifests sit beside each model, except Laya's
`base-artifacts.json` and `typed-artifacts.json` at `ROOT/laya`.
Explicit `--input`, `--output`, `--report` and `--run` paths replace original
machine-local defaults. Keep outputs external too. `-` means stdin/stdout only
for Qwen and ONNX scoring; the other commands require file paths.

Use separate external environments for Torch and ONNX. Tested Linux versions:

- Torch: Python 3.12, torch 2.14.1+cpu, transformers 4.57.6, tokenizers 0.22.2,
  numpy 2.5.4, safetensors 0.8.0, einops 0.8.2.
- ONNX: Python 3.14, onnxruntime 1.31.0, tokenizers 0.23.3, numpy 2.5.4.

Install platform-appropriate CPU PyTorch wheels on Linux; macOS uses its native
wheel. A CUDA-capable PyTorch build is allowed: actual parameters, buffers and
inputs are kept on CPU. These scripts do not use MPS, CUDA, or model APIs.
Torch intra/inter-op and BLAS thread counts are one. ONNX permits only
`CPUExecutionProvider`, disables fallback and uses one intra/inter-op thread.
Process priority changes are best-effort; Linux `/proc` diagnostics are optional.
Peak-RSS units are normalized on macOS. Mac execution and ARM INT8 engine parity
have not been measured; use FP32 unless local benchmark validation passes.

```sh
export TACIT_CPU_WORKSPACE="$HOME/.cache/tacit-cpu"
mkdir -p "$TACIT_CPU_WORKSPACE/results"
python -m unittest discover -s eval/cpu -p test_cpu.py -v
python eval/cpu/qwen/score.py --help
```

## Artifact setup (explicit network access)

Download commands use only fixed public repositories and exact revisions in
`artifacts.py`. They validate the returned repository/revision and every selected
upstream LFS SHA256 or Git-blob SHA1, then record local SHA256 manifests.
Scoring is offline and verifies artifacts before loading. Existing model caches
can be linked into the workspace with their original manifests; do not mix
manifests or quantization validations from different model revisions/machines.
Downloaders do not copy scripts out of another workspace.

```sh
python eval/cpu/controls/download.py --model minilm
python eval/cpu/controls/download.py --model tiny
python eval/cpu/qwen/download.py --model 0.6B
# Optional larger checkpoint:
python eval/cpu/qwen/download.py --model 4B
python eval/cpu/laya/download.py --model base
python eval/cpu/laya/download.py --model typed
```

MiniLM uses the canonical `cross-encoder/ms-marco-MiniLM-L6-v2` repository at the
same revision as the original downloader's redirecting `ms-marco-MiniLM-L-6-v2`
request. TinyBERT keeps revision `81d1926f67cb8eee2c2be17ca9f793c7c3bd20cc`.
No model artifact is downloaded by `--help` or tests.

### External Laya source

No Laya source is vendored. Review and obtain the external checkout yourself:

```sh
git clone https://github.com/NandhaKishorM/laya.git "$TACIT_CPU_WORKSPACE/laya/package"
git -C "$TACIT_CPU_WORKSPACE/laya/package" checkout --detach 4ac226d5ee88ed6df8db288a05c766d7a34ee682
python eval/cpu/laya/setup_source.py --source "$TACIT_CPU_WORKSPACE/laya/package"
```

Install that checkout's dependencies into your external environment. The runner
imports the checkout directly and checks its exact commit and clean status
(including untracked files) before import. `--laya-source` overrides its location.
Set `PYTHONDONTWRITEBYTECODE=1` if the checkout does not ignore bytecode. Receipts
record actual source hashes instead of repeating machine-specific review claims.

### Jina: research/noncommercial only

The checkpoint and upstream `modeling.py` are CC-BY-NC-4.0. They are **not**
included in this repository; check license suitability before obtaining or using
them. `jina/download.py --model v3.5` explicitly downloads revision
`e8a93f33f0b22108f8c2364f8484ce3422552fbc` into the external workspace. The loader
requires SHA256 `ec6612461b4307eb3bab089e6916c00881b7e6b2e8edfbd56a9ace4560244837`
for the reviewed `modeling.py`, plus all artifact manifest hashes. Review remains
your responsibility. It imports only this hash-checked external module and supplies
a local tokenizer; it never uses `trust_remote_code=True`. Do not add this module,
weights, or downloaded cache contents to a public PR.

## Scoring and verification

Input JSONL: `{ "id": "row", "query": "task", "candidates": [{"id":"a","text":"memory"}] }`.
Output JSONL: same row/candidate order, `scores: [{"id":"a","score":0.5}]`, and
`elapsed_ms`. Scores have different meanings and must not share a calibrated
threshold without independent development data:

- Qwen: official softmax(no, yes) probability; default retrieval or utility prompt.
  FP32's two-row output head is algebraically equivalent to full-vocabulary logits.
- Laya: checkpoint Noul P(true), shipped temperature and official rounding retained.
- Jina: official listwise relevance score, dependent on the complete ordered pool.
- MiniLM/TinyBERT controls: raw cross-encoder logits, not probabilities. These alone
  retain longest-first pair truncation at 512 tokens. Other scorers reject truncation.

```sh
python eval/cpu/controls/score.py --model minilm --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/minilm.jsonl"
python eval/cpu/controls/run_checks.py --model minilm --output "$TACIT_CPU_WORKSPACE/results/control-checks" --benchmark
python eval/cpu/qwen/test_cli.py --model 0.6B --output "$TACIT_CPU_WORKSPACE/results/qwen-cli.json"
python eval/cpu/qwen/verify.py --model 0.6B --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/qwen-artifacts.json"
python eval/cpu/qwen/score.py --model 0.6B --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/qwen.jsonl" --instruction-mode utility
python eval/cpu/laya/score.py --model base --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/laya.jsonl" --warm-repeats 3
python eval/cpu/laya/verify.py --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/laya.jsonl" --timings "$TACIT_CPU_WORKSPACE/results/laya.jsonl.timings.jsonl" --report "$TACIT_CPU_WORKSPACE/results/laya-verification.json"
python eval/cpu/jina/score.py --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/jina" --order both
python eval/cpu/jina/verify.py --input eval/cpu/fixtures/smoke.jsonl --output "$TACIT_CPU_WORKSPACE/results/jina.natural.jsonl" --reversed "$TACIT_CPU_WORKSPACE/results/jina.reversed.jsonl" --measurements "$TACIT_CPU_WORKSPACE/results/jina.measurements.jsonl" --runtime "$TACIT_CPU_WORKSPACE/results/jina.runtime.jsonl" --report "$TACIT_CPU_WORKSPACE/results/jina-verification.json"
```

Jina exits 75 at its wall budget after durable payload writes. Repeat the same
command to resume; do not change source, dtype or model under the same output stem.
Its `elapsed_ms` is the median of three measured warm forward passes, **not** a
cache-hit time. Natural/reversed singleton pools can share one measurement. Optional
`--parity` checks the first pool in BF16 and FP32; no quantization claim is made.

Qwen `--benchmark --instruction-mode both --quant fp32` writes a run directory
with official-head parity, FP32 controls, three warm repeats, measurements, validation
and restored per-row scores. `--quant int8` validates max probability delta <=0.10,
same top1 and no inversion with FP32 gap >0.05 before emitting production scores.
Validation receipts are machine-local, not transferable attestations. Control
caches bind exact payloads and scorer provenance. INT8 admission binds model-manifest
and scorer hashes, dependency versions, CPU/platform and thread settings. A receipt
cannot authorize another instruction mode; validate each mode you intend to use.
Older unbound cache/validation files are rejected; regenerate them rather than copying receipts.
Cached official-head first-request timings retain their reused marker and are not
a new cold-start measurement. Input/output aliases (including hardlinks) are rejected.

Experimental
`try_perchannel.py` compares fresh FP32 against per-channel INT8 on the synthetic
control plus the first explicit input row; it does not authorize production INT8.
`prompt_ablation.py` preserves the three development prompts on explicit inputs
using external Laya. Neither helper should receive held-out labels.

## Source mapping and deliberate consolidation

All useful authored scripts from the requested source sets are represented:

| Original set | Packaged path | Changes/replacement |
| --- | --- | --- |
| Qwen `score.py` | `qwen/score.py` | Resident RPC, prompts, FP32/INT8, official-head parity, benchmark and dedup retained; lazy dependency imports and external models. |
| Qwen `download.py` | `qwen/download.py` + `artifacts.py` | Explicit model selection; pinned metadata and blob verification retained. |
| Qwen `verify.py` | `qwen/verify.py` | Offline artifact hashes and both-prompt token preflight; online blob verification moved to downloader. |
| Qwen `aggregate.py` | `qwen/aggregate.py` | Explicit input/run/output; inferred counts replace private corpus totals; complete/partial coverage and warm percentile/CPU-wall summaries retained. |
| Qwen `try_perchannel.py` | `qwen/try_perchannel.py` | Fresh FP32 controls replace fixed cached control paths; same limited per-channel probe. |
| Qwen `test_cli.py` | `qwen/test_cli.py` | Same-process repeated JSONL, exact ID/order, deterministic scores and overlength rejection. |
| Qwen `prompt_ablation.py` | `qwen/prompt_ablation.py` | All three prompts, no-truncation checks; explicit source/input/output. |
| Laya `score.py` | `laya/score.py` | Base/typed scoring, no-truncation preflight, warm repeats, CPU hooks, temperature and runtime metrics retained. Empty pools handled. |
| Laya `download.py` | `laya/download.py` + `artifacts.py` | Pinned base/typed artifacts in separate directories; hash receipts retained. |
| Laya `receipt.py` | `laya/receipt.py` + `setup_source.py` | Real current source/platform receipt replaces static findings; exact source commit checked. |
| Laya `verify.py` | `laya/verify.py` + `common.py` | General ID/order, probability, warm-repeat and artifact hash verification replaces hard-coded corpus counts; old private synthetic outcome summaries omitted. Use explicit synthetic outputs to inspect rankings. |
| Jina `score.py` | `jina/score.py` | Reviewed source hash, listwise/no-truncation guards, dtype parity, resumability, order arms and timings retained. New pinned external downloader added. |
| Jina `verify.py` | `jina/verify.py` + `common.py` | ID/order/finite checks, all/unique ranking changes, pair flips/ties/deltas, durable payload coverage, warm arm summaries and runtime events. Static prior-run claims and whole-workspace/private artifact dumps omitted. |
| MiniLM `score.py` | `controls/score.py` | Shared MiniLM/TinyBERT ONNX implementation, same logits, batching and CPU contract. |
| MiniLM `benchmark.py` | `controls/benchmark.py` | Three cold worker processes, warm repeats, batching parity, empty/long/malformed checks; portable thread/RSS diagnostics. |
| MiniLM `run_checks.py` | `controls/run_checks.py` | Tokenizer/vocabulary/truncation, workload coverage and receipts; optional benchmark. Workload is explicit; obsolete identical-source-copy assertions removed. |
| MiniLM `download.py`; tiny `download.py` | `controls/download.py` + `artifacts.py` | Both revisions preserved; upstream blob validation strengthened for tiny. No script copying or implicit private devcal job. |

The hosted Jev runners and offline label evaluator are described in
[README_jev.md](README_jev.md). Hosted runs require explicit egress authorization;
all private datasets and result files remain outside the repository.

## Verification and limits

Dependency-free tests exercise all 19 packaged command help paths with `python -S`,
workspace rejection, finite/ordered/duplicate ID contracts, tampered/path-escaping
artifacts, synthetic mocked download metadata/layouts, and reversed-order reporting.
Mock artifacts exist only in temporary unit tests; they are never scoring results.

Real cached Linux CPU smokes were run without downloads, APIs or private input:

- MiniLM: two synthetic rows / three candidate pairs; CPUExecutionProvider only;
  30,522 vocabulary IDs checked; 512-token pair truncation; three fresh benchmark
  workers, three warm repeats each; batch-1 vs batch-16 maximum score delta 0.
- Qwen 0.6B FP32 utility: two identical requests in one process, exact repeated
  scores and ID/order preservation; overlength rejected at 9,090 > 8,192 tokens.

Those results are execution checks, not benchmark claims for a user's machine.
No raw smoke outputs are checked in. Laya/Jina full model execution, downloads,
4B, INT8 and macOS were not rerun in this packaging task. No model source or
license assertion can substitute for the downstream user's own review.
