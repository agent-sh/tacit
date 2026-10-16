# Prompt-hook replay

`hook_replay.py` exercises the actual Go binary against a pinned store clone. It
is a delivery diagnostic, not a task-success benchmark or a dreamer evaluation.
Keep store contents, real prompts, labels and outputs private.

```sh
python3 eval/delivery/hook_replay.py \
  --binary /absolute/path/to/tacit \
  --store /absolute/path/to/pinned-store-clone \
  --cases /absolute/path/to/private-cases.json \
  --out /absolute/path/to/private-results
```

The case file is a JSON array:

```json
[
  {"split":"tune", "cwd":"/home/example/projects/demo-api",
   "prompt":"check the pinned go toolchain",
   "expected":["go-toolchain"]},
  {"split":"validation", "cwd":"/home/example",
   "prompt":"show the progress", "expected":[]}
]
```

- Pin one store commit for both arms. Verify both binaries can parse that store.
- Freeze contextual-utility labels before inspecting candidate results. Include
  valid positives, explicit no-memory requests, cross-project negatives, exact
  identifiers, short requests, paraphrases and non-English prompts where used.
- `expected` is the full set of acceptable relevant facts, not just one preferred
  slug. Audit unexpected facts before calling them irrelevant: a duplicate can
  state the same useful fact under another name. Report label revisions.
- Run the installed baseline and candidate into different output directories.
  Each uses an isolated index and overlay. An empty session ID prevents writes
  to real delivered-state files. The prompt hook does not sync the store.
- Each cell runs three times by default, after index warmup. The runner rejects
  non-deterministic outputs and reports median latency. It records the binary
  hash, case-file hash, store commit, every cell and its actual hook output.
- Keep tuning and validation results separate. Paraphrases of tuning questions
  check robustness but are not an independent sample of production traffic.
- Report useful and unexpected delivered lines, positive queries with a useful
  hit, and negative queries receiving anything. Do not trade away useful recall
  merely to make a context block smaller without stating the tradeoff.
- Floor sweeps must use the shipped tokenizer and final boosted query score,
  not raw SQL BM25 or an approximate Python tokenizer. Re-run the actual
  candidate after selecting a floor.

This runner deliberately leaves corpus creation, relevance labelling and floor
selection to the evaluator. It does not turn its own retrieved facts into gold
labels, send private material to a provider, or invoke an extraction model.
