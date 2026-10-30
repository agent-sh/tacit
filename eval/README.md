# eval

How tacit was measured, and the scripts to measure it again on your own data. Results and their limits are in `VALUE.md` (value of delivery, curator, verifier, derived facts, results, staleness) and `BASELINE.md` (the grep baseline). The truth-set schema is `SCHEMA.md`.

[Methods](../docs/methods.md#how-to-evaluate-another-store) explains the evaluation sequence: coverage, offline retrieval, matched agent tasks, dreamer recall and held-out verifier agreement. The Python delivery and A/B block builders are offline experiments; use the shipped Go hooks when evaluating deployed hook behavior.

The author's data is not here. The truth set, the A/B tasks and raw results, the labelled pairs and the fact corpus come from one owner's private transcripts and store, so they stay private. Everything in `examples/` is synthetic: invented repos (`demo-api`, `web-app`), an invented host (`workstation`) and invented facts, made to match the schemas so the scripts run end to end.

Your own inputs (`truth.jsonl`, `ab/tasks.json`, `ab/results/`, a corpus manifest) are gitignored here.

## Scripts

| script | measures | model calls |
|---|---|---|
| `mine/digest.py` | Writes compact per-session digests from Claude Code and Codex transcripts (secrets redacted). Input for mining a truth set and for `dream_recall.py`. | none |
| `baseline/grep_baseline.py` | Grep over the memory files: is the stored fact in the top k? The baseline every ranker is measured against. Needs ripgrep. | none |
| `delivery/delivery.py` | Offline: would the SessionStart block for the situation's repo and host carry the fact? | none |
| `delivery/repo_first.py` | Offline replay of the shipped hooks (SessionStart, prompt, PreToolUse) for delivery settings on your cases: items or truth facts reached, characters and lines per case, the judge's noise share, tool-call lines. | the judge only, cached per case and line |
| `delivery/search_replay.py` | Offline replay of `tacit_search` through `tacit mcp`: each case's request as the query in its cwd, graded with repo_first.py's judge and cache: truth facts found, lines, noise share, characters. | the judge only, cached per case and line |
| `delivery/embed_rank.py` | Dense retrieval (EmbeddingGemma 2) against idf and RRF on the same check. Needs numpy and sentence-transformers. | none (local embedder) |
| `delivery/hindsight_rank.py` | Hindsight's recall against idf on the same check. Needs a running Hindsight API and `hindsight_client`. | none |
| `ab/run.py` | Live A/B: the same task with no block, a ranked block, or the fact guaranteed (oracle). Tool calls, detours, tokens, cost. | `claude -p` per cell |
| `ab/judge.py` | Grades decision-task answers against the ruling: yes, partial, no. | one cheap call per row |
| `dream/dream_recall.py` | Would the dreamer have written the facts nobody wrote, from the digest window around the detour? | two per item |
| `curate/split.py` | Rewrites bundled memory notes as one-fact entries into a scratch store. | one per batch |
| `curate/recheck.py` | Offline contradiction pass over a curated store, report only. | one per 25 pairs |
| `derived/backtest.py` | The thresholds behind derived hot spots and co-change, from plain git history. | none |
| `secrets/token_rules.py` | The pre-receive secret scan, run the hook's way, on random keys of each provider's shape (`sk-`, Slack, Tailscale) and on fact names with a segment that starts like one: the share it refuses, beside any `--compare` regex, for this hook or another (`--hook`). Needs GNU grep. | none |

## Inputs

Each script takes flags; the environment fills the defaults:

- `TACIT_TRUTH`: the truth set. Default `eval/truth.jsonl` if it exists, else `examples/truth.sample.jsonl`.
- `TACIT_STORE`: the store clone. Default `~/.local/share/tacit/store`.
- `TACIT_HOST`: the host whose `machines/<host>` scope is eligible. Default this machine's short hostname. `TACIT_IMPORT_HOST` names the host in `inbox/import@<host>/` when it differs.
- `TACIT_AB_TASKS`: A/B tasks. Default `eval/ab/tasks.json` if it exists, else `examples/tasks.sample.json`.
- `TACIT_PROJECTS`: where the checkouts live, for `backtest.py`. Default `~/projects`.
- `TACIT_MINE_OUT`, `TACIT_MINE_CLAUDE`, `TACIT_MINE_CODEX`: digest output (default `~/.cache/tacit-mine`) and the transcript roots (default `~/.claude/projects`, `~/.codex/sessions`).
- Models: `TACIT_AB_MODEL` (default `opus`), `TACIT_JUDGE_MODEL` (default `haiku`), `TACIT_DREAM_MODEL` (default `opus`), `TACIT_CURATE_MODEL` (default `opus`). The defaults are Claude Code CLI aliases; set full model ids if your provider needs them.

## Run the sample

No model, no network:

```
python3 -I eval/delivery/delivery.py --truth eval/examples/truth.sample.jsonl \
    --store eval/examples/store --host workstation
python3 -I eval/baseline/grep_baseline.py --truth eval/examples/truth.sample.jsonl \
    --home eval/examples/home
```

`--home` stands in for `~`, so the sample's `existing_memory` paths resolve to `examples/home/.claude/projects/...`.

## Run it on your own data

1. `python3 -I eval/mine/digest.py` writes digests of your sessions.
2. Read digests and write `eval/truth.jsonl` by the rules in `SCHEMA.md`: a situation where a stored fact would have cut a detour, the fact, and the memory file that already holds it (or null).
3. `grep_baseline.py` and `delivery.py` give the offline numbers. Pin the corpus with `grep_baseline.py --write-manifest` so a number can be reproduced later.
4. For the live A/B, write `eval/ab/tasks.json` in the shape of `examples/tasks.sample.json`, then `python3 eval/ab/run.py --reps 3 --arms bare,block10,oracle` and `python3 eval/ab/judge.py eval/ab/results/<stamp>.jsonl`. Each cell is a separate `claude -p` run; report medians of three.
