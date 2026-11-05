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
| `delivery/deliveries.py` | Turns the tacit blocks Claude Code transcripts recorded into replay cases: the request or tool call, the cwd and store commit the hook read, the facts the session had. | none |
| `delivery/labelled_replay.py` | Replays graded deliveries through actual binaries at their store commits and scores rules on the grades: useful lines kept, noise share, probes reached. Method and fidelity: `delivery/LABELLED-REPLAY.md`. | none |
| `delivery/signals.py` | Cheap outcome signals of graded deliveries from the sessions' transcripts (a compaction summary keeping the line, the agent opening it, carrying it forward, needing it again after a summary dropped it; the owner restating it) and each signal's agreement with the grades. | none |
| `delivery/search_replay.py` | Offline replay of `tacit_search` through `tacit mcp`: each case's request as the query in its cwd, graded with repo_first.py's judge and cache: truth facts found, lines, noise share, characters. | the judge only, cached per case and line |
| `delivery/embed_rank.py` | Dense retrieval (EmbeddingGemma 2) against idf and RRF on the same check. Needs numpy and sentence-transformers. | none (local embedder) |
| `delivery/hindsight_rank.py` | Hindsight's recall against idf on the same check. Needs a running Hindsight API and `hindsight_client`. | none |
| `ab/run.py` | Live A/B: the same task with no block, a ranked block, or the fact guaranteed (oracle). Tool calls, detours, tokens, cost. | `claude -p` per cell |
| `ab/judge.py` | Grades decision-task answers against the ruling: yes, partial, no. | one cheap call per row |
| `dream/dream_recall.py` | Would the dreamer have written the facts nobody wrote, from the digest window around the detour? | two per item |
| `curate/split.py` | Rewrites bundled memory notes as one-fact entries into a scratch store. | one per batch |
| `curate/recheck.py` | Offline contradiction pass over a curated store, report only. | one per 25 pairs |
| `../client/tools/envelope` (Go) | The curator's rulings pass for single inbox notes through the judge route: how many answers are no JSON array (the curator refuses them whole), whether a sent-back answer comes back clean, and how many hold an entry; `-template` measures another prompt. | one per call, two with a retry |
| `curate/link_history.py` | Every link the curator's link step changed in a fact file, from git: whether the fact had replaced the target (provenance), another fact had, or no fact had the name. | none |
| `derived/backtest.py` | The thresholds behind derived hot spots and co-change, from plain git history. | none |
| `recall/bench.py` | Recall from a session's own transcript, by hand (grep, then the matching lines) and with `tacit episode`, on synthetic sessions from `recall/corpus.py` (1 to 64 MiB, each with 16 planted tasks: 4 ids, 4 commands, 4 paths, 4 errors, every value beside a fake secret): found and right, tool calls, characters brought into context, planted secrets that reached it, latency over 5 launches. Writes the CSVs and charts in `docs/images`. Needs grep, scrump and tacit. | none |
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

## Recall benchmark

```
python3 -I eval/recall/bench.py
```

It generates the sessions under `~/.cache/tacit-recall-bench` (about 130 MiB; `--work` moves them), runs both arms, and rewrites `docs/images/recall-tasks.csv` (one row per size, arm and task), `docs/images/recall-summary.csv` (medians with min and max over the tasks), `docs/images/recall-run.json` (tacit, scrump, grep and Python versions, seed, launches), the `recall-*.svg` charts and the table under them in README.md. `--charts-only` redraws the charts and the table from the summary. In the summary, a latency's min and max are over the tasks' medians; in the tasks file, over one task's launches. For a partial run (`--sizes 1`), pass `--out` elsewhere, or it overwrites the committed data. Each arm follows fixed rules, written at the top of `bench.py`, so no model is called. The latency is this machine's; the other columns are the same on any machine for the same seed and tool versions.

The by-hand arm counts everything grep printed: whole transcript lines, before any cap a harness puts on tool output, so its found-and-right count also assumes the whole output reaches the agent. The comparison is with reading the matching events: a grep narrowed to the value's shape (`grep -oE`) would print a few dozen characters and no secret, but the by-hand arm assumes the agent does not know the value's shape. Both arms are given the session: grep its transcript file, tacit its id. Every task's matching lines include the event that holds its fake secret, so grep's output carries one. In the committed run tacit is right in all 112 tasks. grep's one miss (64 MiB) is a newer deploy whose log holds the same three words. An index line's clip is centered on the smallest stretch of the event that holds every word. Before that, the clip sat on the first word's first mention, and four deploy tasks missed: in long deploy logs the clip landed on an earlier log line, and the rules went on to an older decoy or to no answer. One task (64 MiB) is right only on its tenth call, the cap: newer log events hold its three words, and the rules read three of them whole before they page back. A call's time is mostly scrump loading its rules; tacit starts scrump before it reads the transcript and checks the words in the same call as the first batch, so a small call runs one scrump, overlapped with the read.

## Run it on your own data

1. `python3 -I eval/mine/digest.py` writes digests of your sessions.
2. Read digests and write `eval/truth.jsonl` by the rules in `SCHEMA.md`: a situation where a stored fact would have cut a detour, the fact, and the memory file that already holds it (or null).
3. `grep_baseline.py` and `delivery.py` give the offline numbers. Pin the corpus with `grep_baseline.py --write-manifest` so a number can be reproduced later.
4. For the live A/B, write `eval/ab/tasks.json` in the shape of `examples/tasks.sample.json`, then `python3 eval/ab/run.py --reps 3 --arms bare,block10,oracle` and `python3 eval/ab/judge.py eval/ab/results/<stamp>.jsonl`. Each cell is a separate `claude -p` run; report medians of three.
