# Offline task-context and per-fact selection

Experiment-only code. No production hooks, deployments or store mutations.
Python 3.9+ uses only the standard library. The Go transport compiles the existing command's
test package, so it calls the actual search engine and hook predicates rather than
reimplementing SQL. Do not install the resulting test binary as `tacit`.

## Frozen v1 contract

Freeze this implementation and its manifest hashes before examining held-out
outputs. Parent evaluation owns the sealed test labels and final report.

Input JSONL (or a JSON array):

```
{id, episode_id, split: dev|calibration|test, prompt, cwd,
 history: [{role, content, cwd?}]}
```

History must contain only turns preceding the current prompt. The schema cannot
verify chronology. Retrieval projects a whitelist of these fields and never
consumes expected/rejected/provenance/category labels. Only preceding user text
enters context, never assistant answers. Test retrieval is label-blind. Training
refuses any input file containing test rows. Entire episodes must stay in one split.

Context arms:

* `original`: exact original prompt.
* `unconditional`: append at most 1200 characters of the nearest preceding user
  turn. This is intentionally an unsafe-context ablation, not a deployment mode.
* `gated`: append the same span only for explicit grammatical anaphora
  (`it/that/this/those/these/them/same`), without syntactic identifiers, paths or
  backticks. A known history/current cwd mismatch vetoes context. Empty and
  tool-markup prompts never expand. No project/entity/action dictionaries.

All arms preserve original text and record source history index, role and offsets.
This is a deliberately small grammar gate, not a semantic topic-switch detector.
It misses implicit continuation and can misread anaphora in a new topic. The
1200-character cap is an execution bound, not a fitted optimum.

The exporter supplies original prompt as `Situation.Query` and appended context
as `Situation.Paths`. The search engine uses both for lexical/evidence retrieval,
but only Query can explicitly name additional repo scopes. History cannot grant
foreign repo eligibility. Existing relation routing from an unknown repo remains
possible, exactly as the engine allows. Known cwd and verified aliases still apply.

The default pool is the actual six curated candidates, separately one derived
candidate. Query's summary gate, lifecycle, scope, applicability, relationship
routing and pre-limit eligibility are unchanged. Curated `Printable` remains false
and derived true because that is what the current prompt path actually does.
Wider `--ceilings` are named candidate-recall experiments, not the baseline.
The exact default control is queried separately for every ceiling. Original-arm
control names must match the frozen binary's actual hook output or retrieval fails.
The hook allows three curated plus one derived, not three total.

Candidate IDs are fact paths, preserved into scoring and selection. Each candidate
has the engine Hit, Body and parsed `origin_ids`. Shared-origin connected components
form redundancy families; copied origins are not independent evidence. The helper
reads fact files, never invokes fact content or mutates the store. Raw artifacts
must be outside this repository and inside the audit root. Set `--audit-dir` on
any harness command, or `TACIT_SELECTION_AUDIT_DIR`; the default is
`~/.cache/tacit-utility-audit`. Paths are resolved through symlinks before checking.
Use a private directory: outputs can contain raw prompts and fact bodies. Never
commit them. These are offline evaluation options, not production client settings.

Retrieval isolates HOME, settings, keys, index and overlay. It removes inherited
Tacit settings and Typesafe credentials, explicitly disables hosted utility, and
uses the original local selector even if the operator enabled Jev globally. The
exporter calls `promptSelection` directly. It requires explicit absolute store,
index and overlay paths and does not load live configuration. Only `hook prompt`
is exercised, never the syncing `hook session-start` path.

Selectors on identical pools:

* Baseline floors: current 15 repo / 20 repo-less or supported relationship route,
  derived 12. Keep three curated before removing seen facts. No backfill.
* Learned: four standardized features: log1p(current score), IDF coverage,
  log1p(max discounted phrase lift), current-repo scope indicator. Optional fifth
  feature is a supplied semantic score. Fixed ridge least squares, lambda 1,
  unregularized intercept. Fit dev only. This predicts utility margins, not
  calibrated probabilities. Choose a threshold only on calibration explicit labels,
  maximizing candidate-level F1, ties favoring the higher threshold. This objective
  is not an established optimal delivery policy. Baseline thresholds remain a control.
  Rank curated separately, inject at most three and at most one derived; no backfill.

Only explicit expected/rejected facts train either stage. Unlisted facts are unknown,
not negative. For candidate-complete adjudication, enumerate negatives in rejected.
Unknown coverage is recorded. Positive and negative examples are required in both
training and calibration. The model is bound to context arm, ceiling and semantic
feature presence. Fit separate models for separate experiments. No test optimization.

The CLI uses independent requests with empty session_id, not longitudinal session
simulation. No-backfill functions and the Go helper accept seen sets, but the batch
runner does not carry delivery state across turns. Features do not bypass eligibility.

## Build and verify

From repository root:

```sh
export AUDIT="${TACIT_SELECTION_AUDIT_DIR:-$HOME/.cache/tacit-utility-audit}"
export TACIT_SELECTION_AUDIT_DIR="$AUDIT"
export TACIT_UTILITY_PROVIDER=disabled  # keep focused hook checks local too
mkdir -p "$AUDIT"
python3 -m unittest discover -s eval/selection -v
python3 eval/selection/mutation_checks.py
python3 eval/selection/smoke.py
```

From `client/`:

```sh
GOMAXPROCS=2 go test -p 2 ./cmd/tacit -run 'TestSelection|TestPrompt|TestHookRepoAliases' -count=1
GOMAXPROCS=2 go test -p 2 ./internal/search -count=1
GOMAXPROCS=2 go build -o "$AUDIT"/tacit-baseline ./cmd/tacit
GOMAXPROCS=2 go test -p 2 -c -o "$AUDIT"/selection-export ./cmd/tacit
```

The mutation check proves identifier, topic-switch, null and label-leakage tests
reject unrestricted whole-record expansion. The Go synthetic scope test rejects
using effective context as the repo-routing Query. Other fixtures exercise retired,
superseded, foreign-machine, inbox, derived isolation, origins and exact controls.
The local-control regression also enables the global hosted setting inside an
isolated, credential-free subtest and rejects any hosted audit-path entry.
Synthetic tests are contract evidence, not quality evidence.

## Retrieve and score

From repository root, retrieval against a clean, caller-supplied offline store:
`synthetic.jsonl` contains synthetic requests only, not a store. For a synthetic
run create an independent synthetic fixture store first; never point an example
at the live store. Record its HEAD rather than copying a private corpus identifier.

```sh
python3 eval/selection/harness.py retrieve \
  --cases eval/selection/synthetic.jsonl \
  --store "$AUDIT"/store \
  --store-head "$(git -C "$AUDIT/store" rev-parse HEAD)" \
  --baseline "$AUDIT"/tacit-baseline \
  --exporter "$AUDIT"/selection-export \
  --out "$AUDIT"/selection-synthetic \
  --ceilings 6 12 --repeats 3
```

For private offline evaluation supply label-free dev/calibration or sealed test
input and a fresh output directory. Do not publish or read private test labels. Retrieval
writes `manifest.json`, `pools.jsonl`, `scorer-input.jsonl`. The manifest records
binary/helper/script/cases hashes and store HEAD. Pools preserve raw frozen-binary
hook output and repeated baseline timings. Export calls have a five-second context
deadline, seven-second test deadline and eight-second subprocess timeout. Index
warmup is untimed and bounded at 60 seconds. Export timing includes process startup
and readback, one sample per pool; repeat full runs for performance comparisons.

External scorer JSONL contract:

```
input:  {id, query, candidates: [{id, text}]}
output: {id, scores: [{id, score}], elapsed_ms}
```

`text` is Name + Description + Body, joined by newlines. `query` is effective task
text. Scorer owns its CLI/model/cache. The harness makes no network or model call.
Every supplied row/candidate ID must match exactly; duplicates, missing IDs and
nonfinite scores fail closed. Scores are not assumed to be probabilities.

## Fit and select

Commands below use example private filenames the parent must supply:

```sh
python3 eval/selection/harness.py fit \
  --pools "$AUDIT"/devcal/pools.jsonl \
  --labels "$AUDIT"/devcal/labels.jsonl \
  --arm gated --ceiling 6 \
  --model "$AUDIT"/devcal/lexical-model.json

python3 eval/selection/harness.py select \
  --pools "$AUDIT"/sealed/pools.jsonl \
  --model "$AUDIT"/devcal/lexical-model.json \
  --out "$AUDIT"/sealed/lexical-selected.jsonl
```

For semantic-feature fitting/selection, add `--scores <corresponding-scores.jsonl>`
to both commands and use a separate model/output. Scorer files must cover the full
pools file, including arms/ceilings, before the model-specific slice is taken.
Omit `--model` from select to emit only unchanged floor selection on each pool.
Ranking margins and injection IDs are separate fields. Pool hashes and families
carry through for the parent's candidate-recall, redundancy and utility analysis.
No quality report or automatic relabelling is generated.

## Cross-file guards and scoring provenance

All input episodes, including episodes with no labelled candidates, are recorded
in fitted models. Test selection rejects any episode seen in development or
calibration, even when those pools were loaded from separate files.

With `--scores X`, fitting and selection require `X.manifest.json`. Its fields are
`identity`, `identity_sha256`, `scores_sha256`, `pools_sha256`, and
`input_records_sha256`. `object_sha` hashes canonical sorted-key JSON; the input
records are the exact scorer payload reconstructed from the pools. The identity
must record the scorer script hash, model/tokenizer manifest hash, dependency
lock hash, actual runtime versions and execution settings. Parent evaluation
creates this sidecar from actual executed artifacts, never model names alone.
Selection rejects a scorer identity different from the fitted model and records
model, score and pool hashes in each output. Keep the exact scorer-input file
hash in the sidecar as additional provenance.

Timing restriction: exporter timing includes a second curated query to verify
the control, process startup, file readback and origin parsing. Do not compare
it directly to baseline hook latency. Scorer time excludes initialization and
JSON I/O; selection is untimed. Report these as diagnostic components, not a
measured end-to-end deployment latency. A production latency claim needs an
instrumented equivalent request path.

## CPU-only summary comparison

Score the injected `Name + "\\n" + Description`, not Body, when judging passive
injection utility. Set sidecar `text_view` and identity `document_view` to
`summary`; the verifier reconstructs that exact payload. Legacy body-scoring
receipts remain explicitly `full` and cannot be substituted into a summary model.
Runtime provenance supports `CPUExecutionProvider` (NumPy, ONNX Runtime,
tokenizers) or `PyTorchCPU` (NumPy, PyTorch, transformers, tokenizers), with
positive integer execution limits. Model choice does not grant GPU permission.

Use `fit --selector score-only --scores ...` as the direct semantic-score control.
It does not fit feature coefficients: the only nonzero weight selects the semantic
score, and the threshold is chosen on calibration labels. The default `ridge`
selector remains a separate arm. This avoids mistaking a poor feature-fusion fit
for a failure of the underlying reranker or decision model.

When spawning a project virtualenv from an agent Python kernel, clear PYTHONPATH
and PYTHONHOME. Otherwise packages injected by the kernel can override that
virtualenv and corrupt runtime provenance or fail with a Python ABI mismatch.
Record versions from the actual scoring process, not the controlling kernel.
