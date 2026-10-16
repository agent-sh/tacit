# Jev evaluation scripts

These offline and explicit hosted tools complement the local CPU scorers. They do
not modify the installed Tacit binary, settings, store, or production selector.
No private inputs, labels, scores, requests, receipts, or credentials are included.

## Hosted inference is explicit

`jev_smoke.py` uses the checked-in synthetic smoke fixture. `jev_devcal.py` accepts
an explicit label-free input JSONL with `id`, `query`, and `candidates` containing
`id` and printed `text`. Obtain the data owner's permission before submitting
private input. `--allow-egress` is mandatory, including for resumed runs.

Set `TYPESAFE_API_KEY` securely in the process environment, or supply `--key-file`
with a private dotenv path. Do not put the key in command arguments or YAML.
The scripts read only that variable and never execute the dotenv file. Redirects
are refused; error response bodies and connection details are withheld.

```sh
export PRIVATE="$HOME/.cache/tacit-eval-private"
mkdir -p "$PRIVATE"
# Uses the environment credential; synthetic data only:
python3 eval/cpu/qwen/jev_smoke.py --allow-egress --output "$PRIVATE/jev-smoke"
# Explicitly authorized private inputs, never a file under the public repo:
python3 eval/cpu/qwen/jev_devcal.py \
  --input "$PRIVATE/input-summary.jsonl" --output "$PRIVATE/jev-devcal" \
  --key-file "$HOME/.config/typesafe/credentials.env" --allow-egress
```

The version is pinned to `jev-1.13.0` and checked in every response. Bare Noul,
explicit true/false Noul rubric, and useful/redundant/not-useful Choice questions
are evaluated together. Each unique ordered text payload gets three API calls by
default. Identical payloads reuse scores, with original row and candidate IDs
restored. HTTP round-trip timings include all three prompt variants and connection
overhead. They are not local warm-inference timings or per-arm latency.

The output directory must be outside this checkout. It contains a resume identity,
append-only request journal, three score files, and a summary. Resume validates the
exact input hash, complete request bodies (including prompts/rubrics), model and
repetition count. Use a new output directory if any changes. All-empty input works
without a credential or network call. Requests have a timeout and no automatic
retries; rerun an interrupted command with the same arguments to resume completed
requests. An incomplete final journal line requires inspection and removal before
resuming; the runner fails closed rather than silently discarding it.

## Label evaluation is offline

Use only scored development and calibration cases to choose prompts and thresholds.
The evaluator rejects test rows and cross-split episode overlap. Primary labels
use expected fact names; reviewed labels use candidate paths and explicit expected,
rejected and optional uncertain sets. Unknown/unlisted candidates are not negatives.
Pools are the selection harness's `pools.jsonl`. Each `--scores NAME=FILE` must cover
all pool rows and candidates exactly. `Name + Description` is recommended for
summary-utility evaluation; do not label a body answer as useful when the printed
summary contains no added information.

```sh
python3 eval/cpu/qwen/evaluate_jev.py \
  --pools "$PRIVATE/pools.jsonl" \
  --primary "$PRIVATE/primary-devcal.json" \
  --reviewed "$PRIVATE/reviewed-devcal.json" \
  --scores "jev-baseline=$PRIVATE/jev-devcal/baseline-scores.jsonl" \
  --scores "jev-rubric=$PRIVATE/jev-devcal/rubric-scores.jsonl" \
  --scores "jev-choice=$PRIVATE/jev-devcal/choice-scores.jsonl" \
  --output "$PRIVATE/quality" \
  --label-status "Describe label authorship, provenance and review here"
```

Other complete scorer outputs may be added with arbitrary unique names. The
baseline is the frozen pool's `control`. The evaluator reuses the checked-in
selection cap/no-backfill implementation, reports item and origin/equivalence-family
precision and recall, candidate ceiling, primary query hits, duplicates and unknowns.
It records fixed Jev threshold curves and separately maximizes family F0.5 over
calibration scores. Prompt choice uses development/original at fixed 0.50; ties
prefer baseline, rubric, then choice. Calibration-tuned quality is optimistic,
not held-out accuracy. Freeze the protocol before inspecting results, compare
baseline counts, and never tune against the sealed test set.

Outputs are `quality-evaluation.json`, `calibration-curves.json` and
`quality-errors.json`, with exact source/input hashes. Keep these private.

```sh
python3 -m unittest discover -s eval/cpu -p 'test_*.py' -v
```

Tests use generated synthetic labels and mocked transport only. They make no model
or API calls and do not read real credentials, private evaluation data or deployment
configuration. These checks establish contracts, not model quality.
