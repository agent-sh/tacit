# Phase 0 baseline (2026-10-07)

The truth set, the transcripts it was mined from and the fact corpus are the author's and are not published. The numbers below are reproducible in method, not in data: mine your own transcripts with `eval/mine/digest.py`, write your truth set in the schema of `eval/SCHEMA.md`, and run the same script. `eval/examples/` has a synthetic set that runs end to end.

Truth set: 202 situations mined from 1993 top-level session digests (Claude Code and Codex on the author's workstation; subagent transcripts were not mined, `digest.py` can include them with `TACIT_MINE_SUBAGENTS=1`), schema in `eval/SCHEMA.md`. Per type: machine_fact 63, user_standard 57, past_mistake 30, design_decision 27, history 10, structure 9, hot_spot 4, code_change 2. Per mode: push 141, pull 61. Review pass: three `existing_memory` pointers that did not state the fact were cleared, three partial dates were completed from transcript timestamps, and no `user_standard` quote lands on a harness-injected (`H:`) digest line.

Coverage: 122 of 202 situations (60%) have a fact already stored in some memory file; **80 (40%) were never written**, led by machine_fact 33 and past_mistake 18. The most repeated unrecorded detours were tool and shell pitfalls: a pre-commit hook that could not find `cargo` because its PATH lacked `~/.cargo/bin` (37 digests), a shell equals-expansion abort, `pgrep -f`/`pkill -f` matching the agent's own shell (62 exit-144 failures in 6 sessions), `timeout N ssh box 'job &'` exiting 124 (19 times), and a task runner's result file consumed after submit (15 worker sessions).

Grep baseline (`eval/baseline/grep_baseline.py`): corpus = the 460 fact files across all per-project auto-memory dirs plus the shared agent rules file and Codex's memory summary; index files, raw transcripts and mirrored copies excluded. The corpus was pinned in a manifest (path + sha256) written with `--write-manifest`; the manifest lists private file paths and is not published. For each situation with a stored fact, rank files by ripgrep term matches (pull: the query; push: paths, command, repo, intent) and check whether the stored fact's file is in the top k. 3 targets live outside the corpus and are skipped (a MEMORY.md index and two rollout summaries). n = 119.

Ties are common and ripgrep's print order is not deterministic, so every number is a range: target last in its tie group (pessimistic) to target first (optimistic).

| weight | hit@1 | hit@3 | hit@5 | hit@10 | pull hit@5 | push hit@5 |
|---|---|---|---|---|---|---|
| idf (distinctive keyword) | 0.18..0.31 | 0.30..0.43 | **0.36..0.49** | 0.45..0.56 | 15..29 /39 | 28..29 /80 |
| count (naive grep) | 0.13..0.32 | 0.22..0.45 | 0.25..0.50 | 0.39..0.61 | 12..28 /39 | 18..32 /80 |

Reproduce on your own data: `python3 -I eval/baseline/grep_baseline.py --weight idf --truth eval/truth.jsonl --write-manifest manifest.json`, then the same with `--manifest manifest.json`. On the sample: `python3 -I eval/baseline/grep_baseline.py --truth eval/examples/truth.sample.jsonl --home eval/examples/home`.

Reading: with the fact already written, grep puts it in the top 5 somewhere between a third and a half of the time; push situations (no query, only paths and commands) are the weak half and their range is tight (28..29), so that is not a tie artifact. The two levers in order: write the missing 40% (curator and inbox), then key facts to situations (`applies_to`) so push does not depend on text overlap. Any ranker proposed later is measured against the idf row's pessimistic bound.

Known limits: evidence paths pointed at the author's local digests; five miners labelled independently, so the same fact appears under several situations (intended: one fact, many triggers) and pull queries are miner-written, which favours vocabulary that matches the fact; the set comes from one owner's sessions and leans to ops and management work.
