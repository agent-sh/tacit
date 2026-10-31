# Phase 0 baseline (2026-10-07)

The truth set, the transcripts it was mined from and the fact corpus are the author's and are not published, and neither are the numbers measured on them. The results below are reproducible in method, not in data: mine your own transcripts with `eval/mine/digest.py`, write your truth set in the schema of `eval/SCHEMA.md`, and run the same script. `eval/examples/` has a synthetic set that runs end to end.

Truth set: situations mined from the top-level session digests of one owner's Claude Code and Codex history (subagent transcripts were not mined, `digest.py` can include them with `TACIT_MINE_SUBAGENTS=1`), schema in `eval/SCHEMA.md`. Every fact type is present; machine facts and user standards lead, and push situations outnumber pull ones. Review pass: `existing_memory` pointers that did not state the fact were cleared, partial dates were completed from transcript timestamps, and no `user_standard` quote lands on a harness-injected (`H:`) digest line.

Coverage: most situations have a fact already stored in some memory file; **a large minority were never written**, led by machine facts and past mistakes. The most repeated unrecorded detours were tool and shell pitfalls, each rediscovered in session after session.

Grep baseline (`eval/baseline/grep_baseline.py`): corpus = the fact files across all per-project auto-memory dirs plus the shared agent rules file and Codex's memory summary; index files, raw transcripts and mirrored copies excluded. The corpus was pinned in a manifest (path + sha256) written with `--write-manifest`; the manifest lists private file paths and is not published. For each situation with a stored fact, rank files by ripgrep term matches (pull: the query; push: paths, command, repo, intent) and check whether the stored fact's file is in the top k. Targets that live outside the corpus (a MEMORY.md index, rollout summaries) are skipped.

Ties are common and ripgrep's print order is not deterministic, so the script reports every hit rate as a range: target last in its tie group (pessimistic) to target first (optimistic).

Two weightings were compared at hit@1, 3, 5 and 10, and at hit@5 for pull and push apart: `idf` (a distinctive keyword) and `count` (naive grep). On the pessimistic bound idf is ahead at every k and for pull and push alike; on the optimistic bound naive counts are slightly ahead in every column but pull hit@5, where idf is, so idf's gain is in the worst case.

Reproduce on your own data: `python3 -I eval/baseline/grep_baseline.py --weight idf --truth eval/truth.jsonl --write-manifest manifest.json`, then the same with `--manifest manifest.json`. On the sample: `python3 -I eval/baseline/grep_baseline.py --truth eval/examples/truth.sample.jsonl --home eval/examples/home`.

Reading: with the fact already written, grep still misses it in the top 5 more often than not; push situations (no query, only paths and commands) are the weak half and their range is tight, so that is not a tie artifact. The two levers in order: write the missing facts (curator and inbox), then key facts to situations (`applies_to`) so push does not depend on text overlap. Any ranker proposed later is measured against idf's pessimistic bound.

Known limits: evidence paths pointed at the author's local digests; five miners labelled independently, so the same fact appears under several situations (intended: one fact, many triggers) and pull queries are miner-written, which favours vocabulary that matches the fact; the set comes from one owner's sessions and leans to ops and management work.
