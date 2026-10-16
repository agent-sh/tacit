# Evaluation writer safety audit

`common.preflight_files(inputs, outputs)` checks the **complete write set before
its first destructive open**, including append-only journals, generated sidecars,
partial files, and report files. It rejects resolved-path/symlink aliases and
existing hardlinks between any input/output or output/output pair. Input/input
aliases remain legal. Read-only model/source/store trees are checked by path and
by their existing files' identities; file contents are not read by the guard.
Only streaming scorers treat `-` as stdin/stdout.

## Writer inventory

| Entrypoint | Inputs and write set covered |
| --- | --- |
| `selection/harness.py retrieve` | Cases, store tree, baseline/exporter binaries; settings, manifest, pools, scorer input, request/response, index and isolated directories |
| `selection/harness.py fit/select` | Pools, labels/model, optional scores **and score manifest**; model/selection output |
| `selection/smoke.py` | All five generated synthetic inputs/model/selection outputs checked together before fixture creation |
| `controls/score.py` | Workload and model workspace; score output |
| `controls/benchmark.py` | Workload and model workspace; aggregate report (preflight before workers) |
| `controls/run_checks.py` | Workload and model workspace; scores, execution receipt, optional benchmark report |
| `jina/score.py` | Workload, manifest/model tree; active order outputs, resumable measurements and runtime journal |
| `laya/score.py` | Workload, source tree, artifact receipt/checkpoint; scores, timings, metrics |
| `laya/receipt.py` | Source tree and hashed scorer/downloader files; receipt |
| `jina/verify.py`, `laya/verify.py` → `common.verify_main` | Workload, scores, reversed scores, timings, measurements, runtime; report |
| `qwen/score.py` | Workload, model/manifest, quantization receipt when streaming; benchmark control cache, all score/unique/measurement/runtime/validation outputs, including workspace INT8 validation receipt |
| `qwen/aggregate.py` | Workload and run measurements/default/utility scores; report |
| `qwen/verify.py`, `qwen/try_perchannel.py` | Workload and model/manifest; report |
| `qwen/test_cli.py` | Model/manifest used by tokenizer and resident subprocess; report |
| `qwen/prompt_ablation.py` | Workload, Laya source/receipt/checkpoint; ablation output |
| `qwen/jev_devcal.py`, `qwen/jev_smoke.py` | Workload and optional key file; identity, request journal, all three score files **and partial files**, summary; resume files participate in output/output checks |
| All four `download.py` wrappers → `artifacts.py` | Entire metadata-selected artifact batch, each partial file, manifest, and Laya copied receipt checked together before any artifact download/write |

`laya/setup_source.py` and `selection/mutation_checks.py` do not write artifacts.
Test fixture writers use disposable directories and are not user-facing writers.

## Regression checks

```sh
python -m unittest discover -s eval/cpu -p 'test_*.py'
python -m unittest discover -s eval/selection -p 'test_*.py'
python eval/selection/mutation_checks.py
python eval/selection/smoke.py --audit-dir "$HOME/.cache/tacit-selection-smoke"
```

`test_aliases.py` uses only synthetic fixtures and the standard library. Its CLI
matrix checks explicit/implicit inputs against generated outputs under lexical,
symlink, and hardlink collisions, verifies **all preexisting file bytes remain
unchanged**, and rejects failures caused merely by missing ML dependencies.
Downloader tests mock HTTP metadata; they assert no artifact request occurs.
Positive empty-selection/verifier cases ensure normal separate files still work.

These guards prevent accidental clobbering of declared inputs and sibling
outputs. They do not provide a filesystem sandbox, atomic multi-file transactions,
or protection against another process swapping paths after preflight. Use an
exclusive output directory. Download metadata must be obtained before the full
artifact write set is known; tests never contact the network. Model execution,
real hosted requests, and production/private data are outside this test scope.
