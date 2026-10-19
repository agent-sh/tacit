# Does delivering facts have value? (2026-10-08)

Every measurement in this file ran on one owner's private store, transcripts and repositories, on the author's workstation. That data is not published: the truth set, the A/B tasks and their raw results, the labelled pairs and the store stay private, because they are the owner's own sessions and rulings. The numbers are reproducible in method, not in data. The scripts are in this directory, `eval/README.md` says how to run each one on your own store, and `eval/examples/` has a synthetic sample that runs end to end. Fact names, quotes, pair ids and private repository names are left out; where a private repo matters to a number it is called "repo D" (the largest repo in the store) or "a private repo".

The question before more infrastructure: when a fact from the store reaches the agent at session start, does the session go better? Three measurements, all on the author's workstation, all with Opus 5.5 as the working model.

## 1. Offline delivery (`eval/delivery/delivery.py`)

For the 114 truth situations whose fact exists in the imported store, build the 40-line SessionStart block for the situation's repo and host and check whether the needed fact is in it.

| selection | hit | pull | push |
|---|---|---|---|
| eligible by scope (uncapped, median 327 facts) | 0.93 | 35/38 | 71/76 |
| top 40 by idf term overlap | 0.75 | 31/38 | 54/76 |
| top 40 with the pull query added | 0.79 | 32/38 | 58/76 |

Over all 202 detours this is 42 percent delivered today; the 80 never-written facts are the other half of the loss.

Finding: imported memory files bundle many facts behind one description line. For 9 of the 11 A/B tasks the stored note contains the fact in its body but its description does not state it, so a description-line block cannot carry it. One fact per entry is a precondition for delivery, not a style rule. The curator split (`eval/curate/split.py`) rewrites the 475 inbox notes as one-fact entries; measured below once complete.

## 2. Live A/B, read-mostly tasks (`eval/ab/run.py`, tasks T1-T13)

11 tasks reproducing truth-set situations whose facts are still true on the workstation (several candidates were dropped because their fact had gone stale on the machine). Arms: `bare`, `block` (ranked 40-line block), `oracle` (block with the truth wording guaranteed inside). One rep per cell so far.

| arm | tasks ok | tool calls | input tokens | cost | wall time |
|---|---|---|---|---|---|
| bare | 11/11 | 40 | 2114k | $3.66 | 561 s |
| block | 11/11 | 36 | 2108k | $3.99 | 475 s |
| oracle | 11/11 | 33 | 2034k | $3.81 | 455 s |

No task failed and no known detour recurred in any arm: Opus 5.5 solves these read tasks by reading. The gain is efficiency, not correctness: the oracle saves 18 percent of tool calls and 19 percent of wall time; the ranked block saves 10 percent of tool calls at 9 percent more cost (the block is paid on every turn). n=1, so per-task deltas are noise except two tasks (one went from 2 to 1 tools and 116 s to 19 s; the other from 7 to 2 tools under oracle).

Three reps (sum of per-task medians, 11 tasks):

| arm | tasks ok | tool calls | cost |
|---|---|---|---|
| bare | 32/33 | 125 | $6.82 |
| block10 (raw store) | 32/33 | 130 | $7.99 |
| oracle | 33/33 | 103 | $8.33 |

The oracle's 18 percent tool-call saving repeats at n=3. The 10-line block from the raw store saves nothing here because it rarely contains the fact. The same block from the curated store (`block10c`, 3 reps): 33/33 ok, 117 tool calls, between bare and oracle. Per-cell cost is not a usable metric at this scale: identical token counts cost $0.05 or $0.37 depending on whether the ~100k-token system prompt was a prompt-cache hit (one task, bare against oracle), which swamps the block's own cost; tokens and tool calls are the stable measures.

## 3. Live A/B, decision tasks (D1-D14)

14 one-answer tasks built from owner rulings in the truth set, each a decision where the ruling was the agent's only source: approvals, merge and release order, secret handling, measurement settings and the like. Graded by a regex on the ruling and by a Haiku 5.5 judge (`eval/ab/judge.py`, yes/partial/no on substance). First run was invalid (no tools, one turn: the model's first move was a tool call and most answers were empty). Second run (read-only tools, six turns, n=1): regex ok bare 9, block 11, oracle 12 of 14; judged violations bare 4, block 3, oracle 1.

Three reps, four arms, 14 tasks (168 cells; one cell timed out and counts as a failure):

| arm | regex ok | tool calls | cost |
|---|---|---|---|
| bare | 36/42 | 134 | $8.49 |
| block (40 lines) | 34/42 | 130 | $10.04 |
| block10 (10 lines) | 37/42 | 108 | $8.56 |
| oracle (40 lines, fact guaranteed) | 37/42 | 132 | $8.58 |

The 40-line block costs 18 percent more and helps nothing. The 10-line block is the only arm that is better on every axis: most passes, 19 percent fewer tool calls, flat cost.

Judged (Haiku 5.5, substance), 168 cells, score = yes + 0.5 partial:

| arm | yes | partial | no | score |
|---|---|---|---|---|
| bare | 18 | 18 | 6 | 0.64 |
| block (40) | 19 | 17 | 6 | 0.65 |
| block10 | 15 | 22 | 5 | 0.62 |
| oracle | 24 | 13 | 5 | 0.73 |

A 10-line block from the **curated** store (`block10c`, 3 reps): regex ok 35/42, tool calls 120, judged yes 17 / partial 18 / no 7, score 0.62. No compliance gain over bare. Inspection of the secret-in-a-transcript task's block: the curated store held facts about the transcript scrubber and its excluded paths, and even a note that contradicted the ruling, but the ruling itself was not produced by the split. Two lessons: the curator can drop the owner's ruling while keeping its surroundings, and nothing yet detects a fact that contradicts another.

The oracle gain (+0.09, 14 percent relative) sits where the bare agent had no source for the rule at all: the secret-in-a-transcript task went no/no/no to yes/yes/yes, and two more (a merged fix is done only when released; an idle rented machine is shut down) moved from partial to yes. Where the rule already reaches the agent through the shared rules file or the repo's auto-memory (four tasks), every arm passes. The ranked blocks add nothing on compliance because the ranking rarely carried the needed fact (the same failure the offline check shows); the 10-line block still saves tool calls because it distracts less than the 40-line one.

Finding from the invalid run that stands on its own: on the task that asks the agent to print a new token's value, the bare arm refused on the shared-rules-file rule; both fact arms offered to print it "if you confirm", because the delivered fact quoted a dated ruling next to the rule that the current request wins. A fact that restates a rule as a dated quote can weaken the rule. Failure mode for the hardening review.

## 4. Dreamer recall on the never-written facts (`eval/dream/dream_recall.py`)

For each of the 80 gaps, give the model the digest window around the detour (median 34k chars, about 55k tokens with the system prompt) and ask for facts; a judge call checks whether the truth fact is among them.

| extractor | yes | partial | no | cost |
|---|---|---|---|---|
| Haiku 5.5, prompt v1 (generic "durable facts") | 27 | 17 | 36 | $0.78 for 80 |
| Haiku 5.5, prompt v2 on the 36 v1 misses (every failed command and its fix is a fact) | 11 | 5 | 20 | $0.49 for 36 |
| Opus 5.5, prompt v2 on the same 36 | 14 | 1 | 21 | $13.03 for 36 |

Combined Haiku v1 then v2: 38 yes, 22 partial, 20 no of 80 (75 percent at least partial) for about $1.30. Opus recovers 3 more full hits at 26 times the cost; Haiku 5.5 with prompt v2 is the dreamer. Machine facts were the weak class under v1 (19 of 33 missed) and are what v2 recovered.

## 5. Dense retrieval (`eval/delivery/embed_rank.py`)

EmbeddingGemma 2 (text backbone, 256 d, task prefixes, GPU) vs idf vs RRF fusion (k=60) on the same eligible sets, hit@40:

| store | idf | dense | rrf |
|---|---|---|---|
| raw (n=114) | 0.75 | 0.71 | 0.80 |
| curated (n=108) | 0.81 | 0.73 | 0.81 |

Dense is worse than lexical on both stores; fusion adds 5 points on the raw store (where descriptions are weak and bodies carry the facts) and nothing on the curated store. With one-fact entries whose descriptions state the fact, a lexical ranker over descriptions is the right tool and an embedding sidecar is not worth its cost here. Negative result; the eval script stays as the record, the client has no dense path.

Query form matters more than expected: for the 20 A/B tasks whose fact survived the split, using the task's actual prompt as the query (what a UserPromptSubmit hook has) instead of the one-line intent (what SessionStart has) lifts curated hit@10 from 10/20 to 12/20 and hit@40 from 15/20 to 18/20.

## 6. Curator split (`eval/curate/split.py`)

Opus 5.5 rewrites each inbox note as one-fact entries (one note gives 5 to 11 facts). Half-way through the store (1154 entries covering 56 of the 114 stored situations) the offline top-40 delivery is 0.86 (push 34/37 = 0.92) against 0.75 (push 0.71) for the raw notes, with the eligible set grown from 327 to 841. Full store: 463 notes became 3356 one-fact entries (2 batches failed), $36.69 of Opus 5.5. Offline delivery against the raw store, same ranker (idf over the situation's paths, command, repo, intent):

| cap | raw store | curated store |
|---|---|---|
| 10 lines | 0.54 | 0.61 |
| 20 lines | 0.62 | 0.69 |
| 40 lines | 0.75 | 0.81 |

Curated wins by 6 to 7 points at every cap, but the eligible set grew from 327 to 2151 facts for a situation in repo D (930 repo, 562 user, 474 machine, 185 system), and the lexical ranker is now the limit: scope weights, dropping the user scope, and an applies_to boost (3194 of 3356 entries carry applies_to lists, too generic to discriminate) change nothing; ranking on descriptions only gives +3 points (0.64 at 10, 0.73 at 20). Dense retrieval was the remaining ranker lever; measured in section 5 and rejected.

## Verdict (2026-10-08)

Decision rule set in advance: build only if (a) the oracle shows a repeatable gain on decision tasks and (b) curation plus ranking makes that gain reachable without an oracle.

(a) holds. With the right fact in the block, compliance on owner-ruling decisions goes 0.64 to 0.73 (3 reps, 168 judged cells), tool calls on read tasks drop 18 percent at n=3, every task passes, and the gain sits exactly where the agent has no other source for the rule. A delivered fact is worth having.

(b) holds for the store, not yet for the ranker. The curator split lifts offline delivery by 6 to 7 points at every block size and the curated 10-line block already recovers a third of the oracle's tool-call saving (117 vs 125 bare vs 103 oracle). But a 10-line block carries the needed fact about 60 percent of the time, lexical ranking has saturated, and when the block misses, its other lines cost more than they save (the raw 40-line block raised tool calls). The decision-task compliance of the curated block equals bare.

Total spend for the evidence: about $150 of Opus 5.5 ($37 of it the one-time split), $3 of Haiku 5.5.

**Build, with the priorities the data sets:**

1. Precision over recall in what reaches the agent. SessionStart carries at most 10 facts; UserPromptSubmit retrieves with the real prompt as the query; nothing else is pushed. A fact that is not clearly relevant is a cost.
2. The store before the ranker. One fact per entry (the split), owner rulings kept verbatim as `kind: user` facts and never paraphrased away (the split lost one ruling while keeping its surroundings), and contradiction detection at curation time (the same store held a ruling next to a note that contradicted it).
3. The dreamer closes the 40 percent gap: Haiku 5.5 with the failed-command prompt recovers 75 percent at least partially for about two cents a session window.
4. No embedding sidecar: dense retrieval measured below lexical on both stores (section 5). The ranker levers left are query form (the real prompt at UserPromptSubmit) and description quality from the curator. applies_to as emitted by the model is too generic to rank on and should be filled by the hook from what actually happened (paths touched, commands run), not guessed.
5. Measure in tokens and tool calls, never in per-cell dollars.

What the numbers do not show: value on long sessions (every task here was one prompt) and value for Codex, pi and Hermes (all runs were Claude Code with Opus 5.5).

## Review after the build (2026-10-08)

A clean-context reviewer found seven live defects (repo detection returned "projects" for every subdirectory and worktree; the 8 s sync budget did not bound git over HTTP; a refused push froze the clone; two hooks could corrupt one index build; the dreamer lost deltas past its call cap; the curator spent up to three model calls per fact and orphaned output on a deadline; model calls loaded CLAUDE.md and fired the hooks from inside the dreamer). All fixed with tests where the behaviour can be reproduced locally.

### Token efficiency

What reaches the model now, measured on the live store:

| event | when | size | per session |
|---|---|---|---|
| SessionStart | startup only (not resume, clear, compact) | 10 lines, 1.8 KB, about 450 tokens | once |
| UserPromptSubmit | prompts with 2+ searchable terms, not harness-injected turns | 0 to 3 lines, 0 to 0.6 KB; 0 bytes for "hello there friend", "what is the weather" and any repeat | each fact at most once |

Before the review the prompt hook injected five lines on every turn including notifications (about 26k tokens over a hundred prompts, none of it measured as useful). The floor of 15 comes from the score distribution: junk prompts peak at 13.6, real requests score 18 to 21. Bodies are never injected; `tacit fact <name>` or the MCP tool fetches them on demand.

### Failure modes and what catches them

- Store missing or network down: hooks print nothing and exit 0; sync is fetch plus ff-merge or rebase, bounded by process-group kill and http timeouts.
- Refused push (scope, secret, key): commit undone, files set aside as `.rejected`, clone keeps syncing. Still open: surfacing `.rejected` files to the owner.
- Stale or corrupt index: keyed by HEAD and schema version, rebuilt on first use, per-pid temp file, concurrent builders lose the race gracefully.
- Dreamer: cursor per transcript file advances only after its windows succeeded; 4 MB per file per run; shrunk files restart; unparsable model output yields no facts and the delta is retried next run; cost capped by `--max-calls`; secrets redacted by pattern before the digest leaves the machine (a secret the patterns miss would reach the model; the server's pre-receive scan is the second net before it reaches the store).
- Curator: per-batch commit and push, so a crash or deadline loses at most one batch; notes that produced nothing are remembered, not re-sent daily; the split can still drop a fact silently (measured once: one ruling), which only a second pass over the same note would catch.
- Owner rulings: never retired on a judge's verdict; a supersede against `kind: user` is a review item.
- A fact that quotes a ruling beside "the current request wins" weakened the rule (the print-a-token task): rulings now render as `RULE name: <imperative>` with the dated preamble stripped. Whether that closes it is not yet re-measured.

### Contradictions

`conflicts_with` is frontmatter checked by lint (L13) and indexed; retrieval never delivers both sides of a known pair (the higher-ranked side stays and is marked "contradicted by X, unresolved"); the curator judges each new fact against its closest existing fact and against same-scope new facts of the same run in one batched call, writes pairs to `review/<date>.md`, and the owner resolves by superseding one side. Not covered: contradictions between two facts that were both in the store before the check existed (the 3356 curated entries), which needs one offline pass; and contradictions phrased in different vocabularies that bm25 does not pair.

### First live curator run (2026-10-08)

22 pending inbox notes, one batch of 15: 93 one-fact entries (10 owner rulings), 93 conflict pairs in 4 judge calls, 3 contradictions, 5 supersessions, $0.55, pushed as journal seq 13 with `review/2026-10-08.md`. The contradictions are real and useful: an early-July note that recommended one research direction against a late-July ruling that rejected it; a target of how many agents fit on one GPU against a measured fact that two of the models do not fit together. Two of the five supersessions were wrong: a fact listing several mirror-only repos was retired by narrower facts about one repo each. Fixed the same hour: automatic supersession now needs the same origin note or at least 0.6 term overlap; everything else is a review item for the owner. The wrongly retired fact was restored.

### Re-measure with the shipped hooks (2026-10-08)

Decision battery, 3 reps, judged, `live` arm = the installed hooks against the live store (curated split, RULE rendering, floor 15, top 3, once per session):

| arm | score | violations | tool calls |
|---|---|---|---|
| bare (no facts) | 0.64 | 6 | 134 |
| live (shipped hooks) | **0.69** | **4** | 123 |
| oracle (fact guaranteed) | 0.73 | 5 | 132 |

The shipped system recovers a bit over half of the oracle's compliance gain and the whole tool-call saving. The idle-machine task moved from partial to yes under the RULE rendering, the print-a-token task improved from all-partial; the secret-in-a-transcript task stays low because its ruling is still absent from the store (the split dropped it).

### Offline contradiction pass (2026-10-08)

3356 curated facts, 1175 same-family nearest-neighbour pairs with 3+ shared terms, batched Haiku judge, $0.10: 67 "contradicts", 27 "supersedes". A sample of eight "contradicts" held two or three real ones (a rule against one git command next to a recipe that uses it; two different orders for one migration); the rest were refinements, exceptions or steps of one procedure. At about 30 percent precision a verdict must not hide a fact at delivery. Changed the same hour: the curator writes contradictions to `review/` only, the judge definition excludes refinements and exceptions and answers "no" when unsure, the automatic `conflicts_with` from the first live run was removed from the store, and the 94 candidates went to a recheck review file for the owner. `conflicts_with` stays as the mechanism, set on confirmation.

Second live run with the ruling-recovery pass and the gated supersession: 7 notes, 155 facts, 29 rulings of which 7 were recovered by the second pass (the split alone had folded them away), 1 contradiction and 2 supersessions to review, $1.02. The inbox is fully curated; the daily timer takes over.

### What to iterate next, in order

1. `tacit resolve <a> <b>`: the owner confirms a review item and the curator sets `superseded_by` or `conflicts_with`; until then the review files are the queue.
2. Judge precision on contradictions: label the 67 candidates once, measure the stricter prompt against them, and only then consider any automatic effect on delivery.
3. The dropped-ruling failure: a second curator pass that lists every owner sentence in a note and checks each has a `kind: user` entry.
4. A second machine (a laptop or a GPU box) to prove cross-machine delivery and the dreamer there.
5. Hook end-to-end test with a bare remote; `.rejected` surfacing to the owner.

## Verifier against the owner's labels (2026-10-09)

30 pairs the owner would know (their own rulings, repos and projects), picked from the store and labelled by them in `tacit review --label` (keep A, keep B, both stand; skip was offered and not used). Labels live in `~/.local/state/tacit/labels/`, outside every root the verifier can read. The verifier (Sonnet 5.5, `--dry-run --ids`) answered 27; three were already settled in the replay store. Owner: 13 "both stand", 14 one side retired. Three runs per prompt, medians, range in brackets. "Retired a kept fact" counts a wrong-side verdict too.

Raw model marks:

| prompt | agrees | missed retirement | retired a kept fact | question or query |
|---|---|---|---|---|
| before | 16 (15-17) | 9 | 1 | 1 |
| v1: "the store holds what is true now" (shipped) | 20 (18-20) | 5 | 2 | 0 |
| v2: v1 plus "a pause never retires a standing rule", "lessons do not age", "never retire a standard, ask" | 13 (13-14) | 11 | 1 | 2 |
| v3: v1 plus "a pause retires a plan, not a standing rule", "lessons do not age" | 18 (17-18) | 7 | 1 | 0 |

Before v1 the verifier read an outdated ruling or a paused plan as "a history note on a different subject" and kept both. v1 halves the misses. Its one new error is a single pair: it retired a standing owner standard because a later fact says the related work is dormant (3 of 3 runs). v2 and v3, written to fix that in the prompt, gave back most of the gain and stay unshipped.

That error is closed in code instead. Many owner standards sit in the store as `type: user_standard` with `source.kind: observed`, because the curator read them in a transcript, and the ruling guard only looked at `kind: user`. The guard now covers `type: user_standard` too, so an agent's retirement of a standard becomes a question for the owner (test `TestAgentMayNotRetireAnObservedStandard`). Scored with the guard applied, these are the 14 retirements the owner made and how each one reached them:

| prompt | applied | asked, same direction | missed | wrong side applied | wrong side asked |
|---|---|---|---|---|---|
| before | 2 | 1 | 9 | 1 | 0 |
| v1 + guard (shipped) | 2 | 5 | 5 | 1 | 1 |
| v3 + guard | 2 | 3 | 7 | 1 | 0 |

All 13 "both stand" pairs stayed "both" in the median run of every prompt; one v1 run returned "query" on one of them. The remaining wrong-side retirement (one project pair, all prompts, all runs) is a state the owner knows and the sources contradict: the repo shows active commits after the stop. This is a development-set result, since the rules were written after reading the misses on these 27 pairs. The next labelled sample is the held-out check, and it should include a standing rule against a later pause.

Label file format (`~/.local/state/tacit/labels/<sample>.jsonl`): one JSON record per mark, `{id, action, a, b, at, by: "owner-label"}`, where `action` is `left`, `right`, `both`, `query`, `skip` or `reopen` and the last record per `id` stands. From sample3 on, a record can carry `owner_only: true`: the owner says no source settles the pair and only the owner knows the answer. Scoring rule for those pairs: the verifier is right only when its verdict reaches the owner, "owner" (an ask) or "query" (a flagged conflict); a keep or both there counts as a guess, even when it matches. Other pairs score as above. Also from sample3 on, `action` can be `wrong`: neither fact is true. The verifier has no verdict for that today, so on such a pair it is right only when its verdict reaches the owner ("owner" or "query"); any keep or both counts as wrong.

## Hindsight's recall on the same delivery check (2026-10-09)

Part of deciding whether an existing open-source memory system should replace tacit. Hindsight 0.10.3 (vectorize-io, MIT) ran locally with its embedded Postgres, its default local embedder (BAAI/bge-small-en-v1.5) and reranker (ms-marco-MiniLM-L-6-v2), CPU only. The 3356 curated facts went in verbatim (`retain_extraction_mode: chunks`, no LLM), one document per fact, the fact's scope as its tag. Each of the 108 situations was asked with the same text tacit's ranker gets, restricted by tag to the same eligible scopes (`any_strict`, every returned document checked against them), `budget: high` (`eval/delivery/hindsight_rank.py`, per-situation positions in `HS_OUT`).

| ranker | hit@10 | hit@40 |
|---|---|---|
| tacit idf (`delivery.rank`, the prototype the shipped bm25 ranker replaced; section 6 has it a few points lower than shipped) | 66/108 = 0.61 | 87/108 = 0.81 |
| Hindsight recall (semantic + BM25 + rerank) | 57/108 = 0.53 | 73/108 = 0.68 |

Two runs: the first with Hindsight's default BM25 cap (the 16 most selective query terms), the validation run with the cap off. Both gave identical numbers. In the top 10, 52 situations are hits for both, 14 only for idf, 5 only for Hindsight. A recall took 30.7 s median, 56.5 s max, at `budget: high` on the loaded workstation CPU; a lower budget or a GPU would be faster, and neither was measured.

What this does and does not show: on tacit's one-fact entries, a strong off-the-shelf hybrid retriever ranks below tacit's lexical ranker, the same direction as the EmbeddingGemma result in section 5. Every asymmetry runs against Hindsight: no LLM extraction (its default mode rewrites each fact and builds entity links at one model call per fact), a query form already tuned for idf on these same situations, and no tuning on the Hindsight side. So the gap is not a best-case Hindsight number, but nothing here argues for swapping tacit's retrieval for it.

## Derived facts: thresholds and delivery floor (2026-10-10)

`tacit derive` builds two facts per repo from plain git (SPEC.md "Derived facts"). Every number in `client/internal/derive/analyze.go` comes from `eval/derived/backtest.py` (git history of memra, repo D, eigen and sglang; output of the committed script, run once on the loaded workstation).

Window. Rank files by touch count over the K code-touching, non-merge commits before a split point; score = share of the top 10 touched again in the next 100 commits. 17 split points per repo.

| repo | K=50 | K=100 | K=200 | K=400 |
|---|---|---|---|---|
| memra | 0.66 | 0.67 | 0.67 | 0.64 |
| repo D | 0.46 | 0.45 | 0.48 | 0.65 |
| eigen | 0.76 | 0.79 | 0.74 | 0.69 |
| sglang | 0.88 (4.6 files) | 0.81 | 0.83 | 0.84 |
| mean | 0.69 | 0.68 | 0.68 | 0.70 |

The window barely matters; 400 has the best mean and the only full ten-file lists (sglang at K=50 ranks 4.6 files, so its 0.88 is over fewer). Counting commits instead of days keeps it a function of the commit: 400 code commits are about a week of memra (55 a day), the whole history of a small repo.

Mass-edit limit. Code files per code-touching commit: memra p50 2, p99 30; eigen p99 27; repo D p99 74; sglang p99 65. The limit of 30 drops 0.6 to 3.2 percent of commits. At 60, memra's co-change rule applications went from 654 to 5190 and precision from 0.46 to 0.83: a handful of bulk commits dominate.

Co-change rule f -> g (learn on the window, test whether g is in f's commit over the next 150), changeset limit 30:

| rule | memra | repo D | eigen | sglang |
|---|---|---|---|---|
| 3 changes, 0.3 of f's commits | 0.35 | 0.41 | 0.31 | 0.30 |
| 3 changes, 0.5 (shipped) | 0.46 | 0.52 | 0.42 | 0.35 |
| 5 changes, 0.5 | 0.52 | 0.52 | 0.46 | 0.40 |

Rules fired for 3 and 0.5: 654, 1035, 2330, 1471; for 5 and 0.5: 309, 383, 1481, 254. The stated "half of f's commits" roughly holds out of sample, and 5 changes would buy 3.5 points for 36 to 83 percent fewer rules.

Prompt floor. Derived facts are ranked alone in their own scope and added after the curated block, so their floor is separate. 31 prompts for memra and repo D; scores are the derived fact's bm25 as the hook computes it, on the live store plus the derived facts of the checked-out repos:

| prompt kind | derived score |
|---|---|
| names no file or module (18 work and junk prompts) | none to 7.9 |
| about a module without naming a file ("deploy the X worker") | 9.7 |
| asks about the layout without naming a directory ("which dir has the api router") | 8.6 |
| names one hot file by a short name or part of it (6 prompts) | 9.0, 9.1, 12.3, 12.6, 15.4, 16.8 |
| names two files or a full path, or asks for hot spots or the layout (5 prompts) | 15.9, 21.2, 21.6, 27.0, 29.4 |

There is no clean gap below 12.3: the prompts that name one file by a short name run from 9.0 to 16.8. The floor is 12, which admits four of those six, all five of the stronger prompts and none of the 18 that name no file. It is a judgment inside the 9 to 12.3 band, not a measured optimum, and it cannot be tuned without an outcome metric. Being wrong is cheap either way: a false positive is one line of about 60 tokens (name, "computed from git", description), a miss leaves the agent where it is today, and `tacit search` and the MCP tool find the facts by name. At 9 it would admit all six and the module deploy prompt (9.7), but not the layout question (8.6).

Not measured: whether the facts reduce tool calls or change any decision. The A/B harness (`eval/ab`) has no task that needs a layout or a hot file.

Size over the 95 project scopes: 56 have no checkout; the rest got 73 facts (35 hot-spot, 38 module-map; 4 repos have only a map, 1 only hot spots), 148 KB in total, largest file 4.0 KB. The whole pass took 19 to 44 s with the workstation at load 40.

Fetch before the read (2026-10-10). One branch, `git fetch --no-tags origin +refs/heads/<b>:refs/remotes/origin/<b>`, timed in scratch clones that share objects with the checkouts, over ssh and https at load 40: valkey-glide main 1.1 s, an sglang fork's dev branch 2.0 s, memra main 2.6 s, sglang main 9.9 s (upstream's busiest branch, a few days behind). `git ls-remote` alone costs 0.5 s over https and 1.9 s over ssh, so most of a small fetch is the handshake. The timeout is 30 s, three times the worst of those; after three failures in a row the rest of the run does not fetch. A real run against the sglang fork with its `dev` branch configured took 8 s end to end, left HEAD, local `dev` and the working tree as they were, and read `origin/dev`.

## Git timeline as verifier input (2026-10-10)

The owner finds labelling too technical: the owner often does not remember, and a decision has to be checked against repo history. So each finding gets a git timeline built without a model (`client/internal/gitev`, `tacit timeline`): the facts' own dates, the commits in the repos the facts are about that touched what they name (with author, committer and co-author), and what HEAD shows now. The review page shows it under each proof, in normal and label mode. The question here is whether the verifier is better when it receives the same timeline.

Same 30 sample2 pairs, same replay store (the store at the commit that created the sample, so the same 27 findings are open and 3 are already settled), same scoring as above (owner labels read from outside every verifier root, medians, range in brackets, "retired a kept fact" counts a wrong-side verdict too). Sonnet 5.5, `tacit verify --dry-run --ids`, each run inside `systemd-run --user --wait --pipe` with the model provider's environment file. Arm `main`: the shipped prompt and findings. Arm `timeline`: the same plus a `Timeline` field in each finding's JSON and one paragraph saying git is the primary truth for what happened and when, the state at HEAD wins, a fact older than a commit that changed its subject is suspect, fact dates are compared with commit dates, and rulings or preferences with no git trace stay "owner". Timelines were built as of 2026-10-09 02:20, when the owner finished labelling (`--as-of`), so the repos are as the owner saw them; the verifier's own git probes see the repos today in both arms. Rounds were paired, one run of each arm side by side, so both saw the same machine load.

Raw model marks, 10 paired runs per arm:

| arm | agrees | missed retirement | retired a kept fact | question or query | both-stand pairs kept (of 13) |
|---|---|---|---|---|---|
| main | 19 (17-20) | 5 (4-7) | 2 (2-3) | 1 (0-2) | 12 (12-13) |
| timeline | 19 (19-21) | 4 (3-6) | 2 (1-2) | 1.5 (0-2) | 12 (12-13) |

The 14 retirements the owner made, with the ruling guard applied (10 runs):

| arm | applied | asked, same direction | missed | wrong side applied | wrong side asked |
|---|---|---|---|---|---|
| main | 2 | 4.5 | 5 | 1 | 1 |
| timeline | 2 | 5 | 4 | 1 | 1 |

A first batch of 3 paired runs, with a timeline build that differed only in listing a path from one repo as "not present" in another, gave the same medians: agrees 19 (19-20) in both arms, missed 5 (5-6) against 5 (4-5), retired a kept fact 2 (1-2) against 2 (2-2). The 10-run batch above was run after that tweak, and a first look at its 5 runs had the timeline arm one pair ahead (20 against 19); five more runs closed it, which is the reason for ten.

**Verdict: not shipped.** The medians of agreement are equal (19 and 19), so the arm is not better on the median, and `tacit verify` stays as it was. Why the change is so small:

- Five of the 27 findings change mark between runs inside each arm, and seven differ between arms. The arm difference is the size of the run-to-run noise.
- The model almost never uses it: 3 of 270 evidence strings in the timeline arm name the timeline.
- Git has little to say about this sample. 12 of the 30 findings have a commit on what the facts name, 21 have a repo, 9 name no repo at all (owner rules for agents, preferences). The verifier already had `git-log`, `git-show` and `git-blame` through the probe, so where a commit matters it was already looking.
- What git cannot show is the owner's intent, and the committer is usually an agent. The wrong-side retirements stay at one applied and one asked in both arms; the project pair described above is the kind of case: the repo shows commits after a stop the owner knows about.

Cost per run was $0.12 to $0.22 in both arms. Building the 30 timelines takes about one second each on the loaded workstation, cold. The timeline stays on the review page, where the owner decides, and in `gitev`, which the curator can use for staleness checks and for git-sourced facts (bench results, where newer commits supersede an old number). The measured verifier wiring was kept on a tag in the development history and is not in this tree.

## Verifier against sample3 (2026-10-10)

The held-out check the 2026-10-09 section asked for. The shipped prompt (v1) and the ruling guard have not changed since that result, so no rule here was written after reading these misses.

Sample: 30 pairs drawn 2026-10-10 (15 contradictions, 15 supersede proposals) and labelled by the owner in label mode. The owner skipped 17 because the owner could not know without git. The owner answered 13: 10 retire one side (9 keep B, 1 keep A), 1 both stand, 2 neither fact is true, both flagged owner-only. Only the 13 answered pairs are scored; the verifier saw only those ids. Scoring follows the label rules above: on an owner-only or "neither true" pair the verifier is right only when its verdict reaches the owner ("owner" or "query"), and a retirement of a ruling becomes a question (`isRuling`: `source.kind: user` or `type: user_standard`).

Replay store: a clone of the live store, never the live one. No single store commit has all 30 pairs open, because the pairs were flagged between 2026-10-08 and 2026-10-10 and agent verdicts on them landed in six review commits. The replay uses the parent of the last of those commits (the last curator run): every pair exists there and 11 are open. For the 19 pairs that already carried an agent verdict, those agent records were removed from `review/decisions.jsonl` and the relations they changed were put back from each record's `before` snapshot (what reopen does), `MEMORY.md` was regenerated, and the git history was flattened to one commit so `git-log` cannot show the earlier verdicts. Nothing else changed. All 30 pairs were then open. Labels and the pairs file stayed outside every root the verifier can read and were opened only by the scoring script. A check on the replay: its median mark equals the agent mark the sample file records for the pair (the live verifier's, or both stand where none was recorded) on 11 of 13 pairs.

Runs: Sonnet 5.5, `tacit verify --dry-run --ids` on the 13 answered ids, 3 runs, each inside `systemd-run --user --wait --pipe` with the model provider's environment file and the replay store as `TACIT_STORE`. $0.09 to $0.16 a run. Medians, range in brackets.

| owner's mark | pairs | right | missed retirement | retired a kept fact | asked | guessed |
|---|---|---|---|---|---|---|
| retire one side | 10 | 2 (2-2) | 8 (8-8) | 0 | 0 | |
| both stand | 1 | 1 (1-1) | | 0 | 0 | |
| neither true, owner-only | 2 | 0 (0-0) | | | | 2 (2-2) |
| all | 13 | 3 (3-3) | 8 | 0 | 0 | 2 |

"Guessed" is a keep or a "both" on an owner-only pair: here one "both" and one retirement, the same in all three runs. With the ruling guard applied, the 10 retirements read: applied 2, asked in the same direction 0, missed 8, wrong side 0. The guard changed none of the 39 verdicts: 5 of the 26 facts are rulings, and none was retired or had its relations changed.

Against the 2026-10-09 result (shipped prompt plus guard, tuning set):

| | 2026-10-09, 27 pairs, tuning | 2026-10-10, 13 pairs, held out |
|---|---|---|
| both-stand pairs kept | 13 of 13 | 1 of 1 |
| retirements in the owner's direction (applied or asked) | 7 of 14 | 2 of 10 |
| retirements on the wrong side | 2 of 14 | 0 of 10 |
| owner-only or neither-true pairs that reached the owner | not scored | 0 of 2 |
| pairs whose mark changed between runs | 5 of 27 | 0 of 13 |

What it says. The wrong-side errors did not return and the verifier is stable, but it catches few of the retirements the owner makes: 2 of 10, against 7 of 14 on the set the prompt was tuned on. The 95% intervals overlap (0.06 to 0.51 against 0.27 to 0.73) because n is small, so the gap is not proven; the direction is not good news either. The eight misses read alike in the evidence: the two facts cover different aspects, complement each other or do not conflict, so both stay. The v1 rule "the store holds what is true now" fixed the pairs it was written from and did not move this pattern. The verifier also has no way to say "neither is true" and does not ask: on both owner-only pairs it kept both or retired one side. Net: the verifier stays a filter for clear cases and the owner's queue still holds the retirements, and the next prompt change needs a third labelled sample, since this one is spent as held-out once its misses are read. This sample covers only pairs the owner could judge without git; the 17 the owner skipped are not measured.

## Verifier prompt v2: same subject, later state; ask when nothing settles it (2026-10-10)

The owner approved two prompt changes after the sample3 misses. Nothing else in the prompt changed. Inserted verbatim:

1. After "Two facts on the same decision are not "different subjects" because one is older.": `Same subject, later state: when two facts are about the same subject and the newer one states a later state of it (moved, replaced, destroyed, renamed, finished, reversed, a newer number or role), the default is "keep" with the newer and the older retired, unless the older carries something still true and useful that the newer lacks; then mark "both" and say what in "evidence". "They cover different aspects" is not enough when both describe the state of the same thing at different times.`
2. After "Use it sparingly; not having looked is not owner.": `The exception: when you did look (files, git, GitHub) and nothing settles the pair, and it turns on the owner's intent, preference or knowledge, answer "owner" with a short question in "question" instead of guessing "both" or "keep".`

Setup, same scoring as the two sections above (owner labels read only by the scoring script, which lives outside every verifier root; no label sits in a replay store or in this repo). Sonnet 5.5, `tacit verify --dry-run --ids`, each run inside `systemd-run --user --wait --pipe` with the model provider's environment file and the replay store as `TACIT_STORE`. Arm `baseline`: the shipped binary (main's prompt). Arm `new`: a binary built from main plus the two changes. Three paired rounds per sample, a baseline run and a new run side by side (two at once, `Nice=19`), 12 runs in all, $0.04 to $0.25 a run. Replay stores are clones, never the live store, with `origin` pointing at a local bare copy of the same state, so the verifier's own sync cannot move them:

- sample2: pinned at the store commit that created it (history after it removed from the clone). 27 findings open, 3 already settled and skipped, as before. Scored on the 27.
- sample3: rebuilt as described above (19 agent records on the sample removed, 13 fact files put back from the `before` snapshots, `MEMORY.md` regenerated, history flattened to one commit). All 30 findings open, 13 answered ones sent. Scored on the 13.

Raw model marks, medians with range in brackets, "agrees" counts reaching the owner on an owner-only pair:

| sample | arm | agrees | missed retirement | retired a kept fact | question or query | guessed (owner-only) |
|---|---|---|---|---|---|---|
| sample2 (27 pairs) | baseline | 20 (19-20) | 5 (4-6) | 2 (1-2) | 1 (0-1) | 0 |
| sample2 (27 pairs) | new | 20 (18-20) | 6 (5-6) | 1 (1-2) | 1 (0-1) | 0 |
| sample3 (13 pairs) | baseline | 3 (3-4) | 8 (7-8) | 0 | 0 | 2 (2-2) |
| sample3 (13 pairs) | new | 3 (3-3) | 8 (8-8) | 0 | 0 | 2 (2-2) |
| both (40 pairs, per round) | baseline | 23 (22-24) | 13 (11-14) | 2 (1-2) | 1 (0-1) | 2 (2-2) |
| both (40 pairs, per round) | new | 23 (21-23) | 14 (13-14) | 1 (1-2) | 1 (0-1) | 2 (2-2) |

Per run (agrees, missed, retired a kept fact): sample2 baseline (19, 6, 2), (20, 5, 1), (20, 4, 2); sample2 new (20, 5, 1), (18, 6, 2), (20, 6, 1); sample3 baseline (3, 8, 0), (3, 8, 0), (4, 7, 0); sample3 new (3, 8, 0), (3, 8, 0), (3, 8, 0). "Retired a kept fact" counts a wrong-side verdict too, so on sample2 it is all wrong-side verdicts: the 13 both-stand pairs stayed "both" in the median run of both arms (13, range 12-13, and no retirement of one in any run).

With the ruling guard applied, the owner's retirements:

| sample | arm | applied | asked, same direction | missed | wrong side applied | wrong side asked |
|---|---|---|---|---|---|---|
| sample2 (14) | baseline | 2 (2-3) | 5 (4-5) | 5 (4-6) | 1 (1-1) | 1 (0-1) |
| sample2 (14) | new | 2 (2-2) | 5 (4-5) | 6 (5-6) | 1 (1-1) | 0 (0-1) |
| sample3 (10) | baseline | 2 (2-3) | 0 | 8 (7-8) | 0 | 0 |
| sample3 (10) | new | 2 (2-2) | 0 | 8 (8-8) | 0 | 0 |

The guard changed 5 to 7 of 27 verdicts per run on sample2 and none of 13 on sample3, in both arms. Pairs whose mark changes between runs: sample2 4 of 27 in each arm; sample3 1 of 13 baseline, 0 of 13 new. Across the three runs the verifier raised 2 "query" marks (baseline) and 2 "owner" asks (new) on sample2 and none in either arm on sample3.

**Verdict: not shipped.** The ship rule needed a higher median agreement on both samples and no more kept facts retired. Agreement is equal on sample2 (20 and 20) and on sample3 (3 and 3), and the new arm's combined median is not higher either (23 and 23). The prompt stays as on main and there is no prompt test. Neither change helped on its own terms, so the "test the one that helps alone" step did not apply. Only two pairs changed their majority mark between the arms, both on sample2, and neither became an agreement: a wrong-side retirement became "both", and a baseline split (both, query, a retirement) became "both" with one ask.

Why nothing moved:

- Same subject, later state. The new arm's evidence on the sample3 misses still reads "consistent", "complementary", "narrower", "neither contradicts the other". On one of them the model applies the rule's own exception (the older fact "still carries" a detail the newer lacks) and marks "both". The owner retired a side on all of these pairs, so for them "the older still carries something" does not keep it; the verifier does not know that, and the prompt text does not teach it.
- Ask when nothing settles it. On the two owner-only pairs the new arm guessed in all three runs, one "both" and one retirement. It believes the sources settle them, so the loosened line never applies.

Limits. Both samples were used to design this: sample2 is the set v1 was written from, and the sample3 misses are what these two changes answer. Neither is held out, and a third labelled sample is the real test of any future wording. Since the result here is flat on both, a third sample would not rescue this wording. Three paired runs with medians and no five-run rerun: on sample3 the new arm's three runs gave identical counts, 3 agreements, equal to the baseline median. The untested idea left is a structured answer that makes the verifier state, per pair, which fact is newer and what the older carries that the newer lacks, before it marks.

## Merge and rescope verdicts (2026-10-10)

Two owner decisions after the prompt v2 result, written down before the runs.

A. Merge. In sample3 the owner retired a side in 10 pairs and the verifier kept both in 8, calling the facts "complementary". The owner retires the older side even when it still carries a detail. New verdict "merge": the verifier names the fact that stays (`keep`) and the still-true detail of the other (`fold`, exact short text); the other fact is retired as superseded by the kept one and the detail is appended to the kept fact as a marked paragraph (`Folded in from [[other]] (date): ...`, redacted, fail closed). The decision keeps both files' exact bytes, so reopen restores them. An owner ruling (`isRuling`) is never edited or retired by an agent: a merge touching one becomes a question that carries the proposed merge, and the review page offers it as a mark (key m).

B. Rescope. The owner's diagnosis of the pairs only the owner can answer: most of them are an agent's rule that is wrong in scope, because an agent read a remark the owner made about one repo as a rule for every repo. New verdict "rescope": the verifier names the fact, the narrower scope `projects/<repo>`, optionally a one-line description that names the repo, and what the pair is after (`both`, or `query`). The fact's file moves to the scope's directory, the pair is marked as the verdict says, lint stays clean, reopen moves it back. A ruling is never moved by an agent (it becomes a question with the proposed scope), and neither is a fact into a scope the store has no directory for yet. The prompt gets a paragraph telling the verifier to check scope first (the fact's `origin` inbox note, its source, the repo) when a rule seems to conflict with another repo's fact.

Scoring, fixed before the runs (the earlier sections' rules still apply):

- A merge that keeps side X counts as the owner's "keep X" retirement; a merge that keeps the other side is a wrong-side verdict.
- A rescope followed by both standing counts as the pair's "both": agreement only where the owner marked both stand, a missed retirement where the owner retired a side. A rescope followed by a query counts as a flagged conflict.
- On an owner-only pair or a pair where the owner marked neither fact true (the 2 `wrong` pairs of sample3, both owner-only), the verifier is right only when its verdict reaches the owner: an ask ("owner"), a query, or a rescope that names a repo. A keep, a merge or a plain "both" there is a guess.
- The ruling guard is applied as before, to merges and rescopes too: a merge touching a ruling, a rescope of a ruling, and a rescope into a scope with no directory in the store are questions, not writes. They count as "asked", in the merge's direction.
- Ship rule: the new arm ships only if its median agreement is higher on both samples and it retires no more facts the owner kept (sample2's 13 both-stand pairs). If only A or only B passes, it is tested alone and that one ships.
- Arms: baseline (main's prompt and code) against new (A and B), 3 paired runs per sample, Sonnet 5.5, `tacit verify --dry-run --ids` inside `systemd-run --user --wait --pipe` with the model provider's environment file, at most two at once, on the same replay stores as the v2 section (both clones, origin a local bare copy). Labels are read only by a scoring script outside every verifier root.
- Both samples were seen while this was designed: sample2 is the set v1 was written from and the sample3 misses are what merge answers. Neither is held out.

### Runs

2026-10-10, about 21 minutes. Three binaries: `base` (main), `new` (this branch, both changes) and, because `new` failed the ship rule, two single-change builds made from the same source by cutting the other change's prompt text and schema fields: `A` (merge only) and `B` (rescope and the scope-first paragraph only). `base` and `new` ran as paired rounds. `A` and `B` ran paired with each other and are compared with the same three baseline runs. 24 runs in all (3 per sample per arm), $0.03 to $0.30 each, $3.00 together. One unscored smoke run of `new` on sample3 came first and gave the same counts as the scored runs. The raw dry-run outputs, the replay stores and the scoring script were kept in scratch and deleted after the numbers below were taken. The measured wiring (prompt, schema, ops, page marks, tests, the scope pre-check) was kept on a tag in the development history and is not in this tree.

Raw model marks, medians with the range in brackets (27 scored pairs in sample2, 13 in sample3; "retired a kept fact" counts a wrong-side verdict too):

| sample | arm | agrees | missed retirement | retired a kept fact | question or query | guessed (owner-only) |
|---|---|---|---|---|---|---|
| sample2 | base | 19 (18-21) | 5 (4-6) | 2 (2-2) | 0 (0-2) | 0 |
| sample2 | new (A+B) | 19 (19-20) | 5 (4-6) | 3 (2-3) | 0 (0-0) | 0 |
| sample2 | A, merge | 19 (19-19) | 5 (3-5) | 3 (2-4) | 1 (0-1) | 0 |
| sample2 | B, rescope | 18 (17-18) | 6 (6-7) | 3 (2-3) | 0 (0-1) | 0 |
| sample3 | base | 3 (3-3) | 8 (7-8) | 0 (0-0) | 0 (0-1) | 2 (2-2) |
| sample3 | new (A+B) | 4 (3-4) | 6 (6-6) | 1 (1-2) | 0 (0-0) | 2 (2-2) |
| sample3 | A, merge | 4 (3-4) | 6 (6-6) | 1 (1-2) | 0 (0-0) | 2 (2-2) |
| sample3 | B, rescope | 3 (3-3) | 8 (8-8) | 0 (0-0) | 0 (0-0) | 2 (2-2) |

Per run (agrees, missed, retired a kept fact): sample2 base (19, 6, 2), (18, 5, 2), (21, 4, 2); new (19, 6, 2), (20, 4, 3), (19, 5, 3); A (19, 5, 3), (19, 5, 2), (19, 3, 4); B (18, 6, 3), (17, 7, 2), (18, 6, 3). sample3 base (3, 8, 0), (3, 7, 0), (3, 8, 0); new (4, 6, 1), (3, 6, 2), (4, 6, 1); A the same as new; B (3, 8, 0) three times.

The 13 both-stand pairs of sample2, retired by a verdict (any side): base 0 (0-0), new 0 (0-1), A 1 (1-2), B 1 (0-1). Every one in `new` and `A` was a merge on a pair of two rulings (one pair in three of three runs of A, another in one run), which the guard turns into a question, so nothing would have been written. In `B` it was a plain "keep" on a pair the baseline left standing (two runs of three).

The owner's retirements with the ruling guard applied (14 in sample2, 10 in sample3):

| sample | arm | applied | asked, same direction | missed | wrong side applied | wrong side asked |
|---|---|---|---|---|---|---|
| sample2 | base | 2 (2-3) | 4 (4-5) | 6 (4-6) | 1 (1-1) | 1 (1-1) |
| sample2 | new | 2 (2-2) | 5 (4-5) | 5 (4-6) | 1 (1-1) | 1 (1-2) |
| sample2 | A | 2 (2-2) | 5 (5-6) | 5 (4-6) | 1 (1-1) | 1 (0-1) |
| sample2 | B | 2 (2-2) | 4 (3-4) | 6 (6-7) | 1 (1-1) | 1 (1-1) |
| sample3 | base | 2 (2-2) | 0 (0-0) | 8 (8-8) | 0 (0-0) | 0 (0-0) |
| sample3 | new | 3 (2-3) | 0 (0-0) | 6 (6-6) | 1 (1-2) | 0 (0-0) |
| sample3 | A | 3 (2-3) | 0 (0-0) | 6 (6-6) | 1 (1-2) | 0 (0-0) |
| sample3 | B | 2 (2-2) | 0 (0-0) | 8 (8-8) | 0 (0-0) | 0 (0-0) |

### Per-pair flips

Counted without pair ids or fact names: an id next to an outcome would hand a verifier that can read this file the owner's label.

Against the baseline, pairs whose majority outcome over the three runs changed. The baseline's own three runs disagree on 3 pairs of sample2, so a one-run difference is noise.

- sample2, new: 1 majority flip, a miss that became an agreement (base missed, missed, agreed; new missed, agreed, agreed). Three more pairs differ in single runs: one where the baseline once asked and `new` once merged two rulings, one wrong-side merge of two rulings that became a question, and the rescope pair.
- sample2, A: 2 majority flips. A miss became an agreement in all three runs, and a pair the baseline left standing was merged in all three (two rulings, so three questions and no write). One more pair differed in one run.
- sample2, B: 3 majority flips, one of them an agreement turned into a miss in all three runs ("both" where the baseline kept), one an agreement turned into a retirement of a pair the owner kept (2 of 3), and one miss turned into an agreement (2 of 3).
- sample3, new and A: 2 majority flips. One miss became an agreement (the merge took the owner's side in two runs and the other in one). One miss became a retirement of the wrong fact in all three runs: a merge that keeps the wrong fact and is applied, since neither is a ruling.
- sample3, B: no flip.

On the two owner-only "neither is true" pairs of sample3 nothing reached the owner in any arm: a "both", a keep or a merge, as before. The rescope verdict was never used there.

### Verdict: not shipped

The ship rule needed a higher median agreement on both samples and no more kept facts retired. `new` ties sample2 (19 and 19) and wins sample3 by one (4 and 3), and its retired-a-kept-fact column is higher on both samples (3 against 2, 1 against 0). `A` has the same sample3 gain and the same tie on sample2. `B` is lower on sample2 (18) and equal on sample3. Neither change passes alone, so neither is tested further and neither ships. The prompt, the schema and the code stay as on main. The page marks, the ops and their tests exist in the tag only.

What the runs show:

- Merge turns 2 of the 8 missed retirements of sample3 into verdicts. It takes the owner's side on one in two of three runs and the wrong side on the other in three of three. The direction is the weak part: for two complementary facts the model does not know which one the owner counts as the current one. A wrong-side merge costs more than a miss: with no ruling involved it is applied, writes a fold into the wrong fact and retires the right one.
- On sample2 the merge verdict landed mostly on pairs of rulings, where the guard asks: 5 pairs, of which one asks in the owner's direction, two ask the wrong way (one of them the wrong side the baseline already asked) and two ask about facts the owner keeps. The only sample2 merge that would have been applied is on the owner's side, in one run each of `new` and `A`.
- Rescope was used on one pair in two of three `new` runs and three of three `B` runs: a rule that one repo's policy had left filed under user (its origin note named the repository's policy). That is the owner's described mechanism, read correctly from the origin note. By the scoring rule it is a miss, because the owner did not mark that pair both stand. Rescope was never used on the two owner-only pairs. The scope-first paragraph also made the model say "both" where the baseline kept, on one pair in all three runs, and "keep" on a both-stand pair twice, so on these samples it costs more than it returns.
- The effect sizes are inside the noise. The baseline's three runs differ by up to 3 agreements on sample2 (18, 19, 21) and the arm medians differ from it by 0 to 1.

Limits. Both samples were seen while this was designed: sample2 because v1 was written from it, sample3 because its misses are what merge answers. Neither is held out. Three runs per arm and medians, no five-run rerun, because the ship rule failed on a tie and on a decrease that more runs would not turn into a gain on both samples. `A` and `B` were compared with the baseline runs of the first round, not with fresh paired baselines. The wrong-side merges say what a next step needs: the model must be told, or must find, which of two complementary facts is the current one before it folds. A structured answer that states per pair which fact is newer and what the older carries, the idea left at the end of the v2 section, is still untested.

### Scope check without a model

The cheap deterministic pre-check the owner asked for: a live curated rule fact (type `user_standard`, `design_decision` or `past_mistake`) filed under `user` or `system` whose origin inbox note (the one its `origin` names) was filed by its writer under one `projects/<repo>`, and whose `applies_to.repos` names no other repo. It reads files only and calls no model (`client/internal/scopecheck` and the verb `tacit scopecheck`, in the tag and not in this tree).

On a read-only clone of the live store (2026-10-10) it finds 106 candidates among the 823 live facts filed under user or system (56 under user, 50 under system; 54 `user_standard`, 52 `past_mistake`, no `design_decision`). By repo: repo D 54, memra 23, and 29 spread over 11 other repos (at most 10 in one). In 17 the fact's own `applies_to.repos` names exactly that repo.

Hand check, by the agent and not by the owner: candidates drawn with a fixed seed, each fact read against its origin note. "Over-wide" means the rule is only true or only meaningful in that repo and is filed as if it held everywhere.

| | over-wide | partly (a general rule with a repo-only half, or repo vocabulary) | general rule, correctly wide |
|---|---|---|---|
| first 15 | 2 | 3 | 10 |
| second 15 (one fact repeats the first) | 0 | 1 | 14 |
| 29 distinct | 2 | 4 | 23 |

Precision is 13% on the first 15 (33% counting "partly") and 7% on the 29 (21% counting "partly"). The reason is how the store is filled: the curator promotes a general law the owner wrote while working in one repo (about tone, gaps, time zones, experiment design), and the origin note records only which repo's memory directory it sat in. Three cheap refinements did not separate the groups: the curator's own `applies_to.repos` naming the repo (17 candidates; 4 of the 29 checked, one over-wide), the fact naming its repo in its text (14 candidates; neither over-wide fact does), and the fact's rare words appearing in no other repo's notes (54 candidates; 9 of the first 15, one over-wide). So the pre-check is not shipped: it is a recall list of about one real case in ten. The owner's own reading of a few of these may differ from the agent's.

## Fewer pairs for the owner: two levers (2026-10-10)

The owner approved cutting how many pairs reach them, since the owner cannot answer most of them from memory. Two deterministic levers, no new model calls: (a) raise the judge's bar, (b) a git gate. This section holds the rules. They were written after the label counts, the text features and a partial git read (the first 18 of the 78 labelled pairs) had been looked at, and before the other 60 pairs' git columns existed; the results follow.

Ground truth: the owner's labels in label mode (`sample`, `sample2`, `sample3`; last record per id stands, a reopen clears it, the later record stands where one pair is in two samples). 78 distinct pairs, all still findings in the live store: 43 real problems (`left`, `right`, `wrong` and the one `query`), 18 `both` (false alarms), 17 `skip` (the owner could not tell; every skip is in sample3). The two older samples were hand-picked as "pairs the owner would know", so their false-alarm and real rates do not describe the live flow; sample3 is a draw across the findings of the time, 15 of each kind, settled by the verifier or not. No label, id or fact name appears in this file next to an outcome.

Ship rule, fixed before the final tables:

- Recall of real problems: lose at most 1 of the 43 (42 kept, 97.7%). A real problem that a gate holds back is a contradiction or a stale fact nobody looks at again; a false alarm costs the owner a few seconds. One pair is 2.3 points, the finest step this sample resolves, and 42 of 43 has a 95% Wilson lower bound of 0.88, so a stricter level is not measurable and a looser one (2 lost) cannot be told from losing five.
- A lever is a rule with a stated reason. A threshold fitted to the 78 pairs is not a reason, so fitted splits are reported as ceilings (the most any single split could hold on this data) and are never shipped.
- Ship the lever or combination that holds back the most pairs while meeting the recall level; if none meets it, ship nothing.
- Held pairs are not deleted: they stay in the review files and would show under a filter on the page.

What each lever is:

- (a) The judge's bar. The judge answers one word per pair and the bm25 hit score that decides which pairs it sees is not logged, so no raw judge score exists. Recomputed on a read-only clone of the live store: the overlap of the two descriptions' search terms (the curator's own `overlap`), the overlap of description and body terms, the number of shared terms found in at most 3% of curated facts, and the bm25 score of the other fact when one fact's description is the query (current index, an approximation of the raise-time score). The one natural bar is 0.5 description overlap, which the curator already requires for pairs it forms between new facts; the rest are curves.
- (b) The git gate. A pair is raised when git shows that what one fact names changed after the other fact's date, or when the two facts name the same path or identifier and it is present at HEAD. "Changed after" is read per fact with the existing `gitev` subject (applies_to, backticked and bare paths and identifiers, the repos it names), a commit on a later day than the other fact's date (its newest text date, else its source note, else its filing day) that touches a named path, adds or removes a named identifier, or names it in the message. Variant b+ also raises every pair that holds an owner ruling (`source.kind: user` or `type: user_standard`), since the owner can answer those from memory and the trust rule sends ruling conflicts to them. Otherwise the pair is held. A git read that fails or runs out of time never holds a pair.
- (c) Owner-facing after the verifier: the findings the page lists under "Needs you" today (no mark, or an agent flag or question), counted on the clone.

What is counted: for each lever on the 78 labelled pairs, pairs still raised, real problems kept of 43, false alarms (`both`) held of 18, skips held of 17; on all findings of the live clone, pairs still raised of the total and still open of the pairs open now.

### Where the queue stands today (lever c)

The live clone (read-only, 2026-10-10) holds 154 findings: 92 from the 2026-10-08 offline pass, 30 hand-picked for the first labelling sample, 32 from the daily curator runs of 10-08 to 10-10. The verifier has settled 145 of them (110 "both stand", 22 and 13 retirements) and flagged one as a conflict; 9 are open under "Needs you": 7 questions the verifier left, that 1 conflict, and 1 finding the verifier never reached. All 9 are in the labelled set: 6 real problems, 3 skips, no false alarm. There is no stability threshold in the code: one verifier run's "both" closes a finding, so (c) is already in force and leaves 9.

The daily inflow is small. The judge saw 558 pairs on 10-09 and 569 on 10-10 and raised 11 each time (2%); of the 32 findings the three daily files hold, 3 are still open. Whatever a lever does to the queue is a fraction of about one pair a day.

What the verifier did with the 78 labelled pairs, which sets the comparison for any gate placed before it:

| owner's label | pairs | reached the owner | retirement applied by the verifier | closed "both stand" by the verifier |
|---|---|---|---|---|
| real problem | 43 | 6 | 12 | 25 |
| false alarm | 18 | 0 | 0 | 18 |
| skip | 17 | 3 | 7 | 7 |

### Results on the 78 labelled pairs

Raised means the pair stays an owner-facing finding. Git columns were read at the time of this run, with a read that failed or ran out of time counted as "changed" (27 findings, 6 of them labelled, all in the two largest repos, because the workstation's load average was above 100; counting those as "nothing changed" only lowers the git rows). One deterministic pass; no model is involved, so there is no run-to-run spread. A second pass read every repo as of the day after the review file that raised each pair (so no commit made after the pair existed is counted): b1 raised 19 instead of 18 (one more skip kept) with the same 13 real problems kept, every other row identical.

| lever | pairs still raised (of 78) | real problems kept (of 43) | false alarms held (of 18) | skips held (of 17) | skips kept |
|---|---|---|---|---|---|
| none (today) | 78 | 43 | 0 | 0 | 17 |
| a: description overlap >= 0.5 | 2 | 1 | 18 | 16 | 1 |
| b1: what one fact names changed after the other's day | 18 | 13 | 16 | 14 | 3 |
| b2: both name the same path or identifier, present at HEAD | 5 | 3 | 17 | 16 | 1 |
| b: b1 or b2 | 20 | 14 | 16 | 13 | 4 |
| b+: b, or an owner ruling is involved | 53 | 28 | 1 | 9 | 8 |

The ship rule needs 42 of 43 real problems kept. The best row keeps 28. Nothing passes.

Lever (a), the curves behind the one natural bar. A higher bar on any text overlap removes real problems about as fast as it removes false alarms and skips, because the three groups overlap on every measure: the median description overlap is 0.14 for real problems, 0.11 for false alarms and 0.19 for skips.

| bar | pairs still raised | real kept (of 43) | false alarms held (of 18) | skips held (of 17) |
|---|---|---|---|---|
| description overlap >= 0.10 | 63 | 36 | 8 | 0 |
| description overlap >= 0.15 | 37 | 19 | 11 | 6 |
| description overlap >= 0.20 | 20 | 11 | 15 | 11 |
| description overlap >= 0.30 | 11 | 7 | 18 | 13 |
| >= 3 shared terms found in at most 3% of facts | 63 | 34 | 5 | 1 |
| >= 5 shared terms found in at most 3% of facts | 36 | 23 | 15 | 7 |
| bm25 score >= 8, the bar on the existing-fact hit today | 62 | 35 | 2 | 6 |
| bm25 score >= 32 | 42 | 24 | 9 | 8 |

No row keeps 42. The bm25 rows are a recomputation on the current index (16 of the 78 pairs have no score because the other fact is no longer a top hit, for example after a retirement), not the score at raise time, which was never stored.

The ceiling. Fitting one split on one feature to all 78 pairs (every overlap and score above, the facts' types, kinds, scopes, ages, lengths, the verifier's mark, the git signals), the most pairs any split could hold while losing one real problem is 9 of 78 (12%), and the best of them is a set of fact types picked by looking at the labels. A lever with a reason holds fewer.

Why git cannot gate these pairs. Git can be asked about at least one fact for 23 of the 43 real problems (53%), 6 of the 18 false alarms and 11 of the 17 skips; the rest name no repo with a checkout, or a repo and nothing git can search for (an owner rule about how agents work, a machine fact, a plan). Where it can be asked, a later commit on a named subject exists for 11 real problems, 2 false alarms and 2 skips. 15 of the 43 real problems have neither an owner ruling nor any git signal. On sample3, the draw across all findings, b keeps 4 of 12 real problems and holds 13 of 17 skips: the signal "no git evidence" is not far from independent of "the owner could not tell".

### Volume on the live clone

| lever | findings still raised (of 154) | still open now (of 9) |
|---|---|---|
| none (today) | 154 | 9 |
| a: description overlap >= 0.5 | 3 | 0 |
| b1 | 60 | 3 |
| b2 | 18 | 1 |
| b | 64 | 3 |
| b+ | 106 | 7 |

Held by b+ among the 9 open: 2, both skips. Held by b: 6, three real problems and three skips. These cuts come with the recall losses above; only b+ keeps every real problem among the 9, and it loses 15 of the 43 elsewhere.

Post hoc, not part of the rules above: the same levers applied only to the pairs that would still reach the owner after the verifier. That placement leaves the 25 + 12 real problems the verifier already closed or retired untouched, so b+ keeps all 6 real problems among the 9 open and holds 2 skips. Two pairs is a cut of 22% of the queue; picking two of the nine at random lands on two skips with probability 3 in 36 (8%), so this is not evidence of a signal, and 22% of an inflow of about one pair a day is about one pair every five days. Not shipped, for the cost of a curator step, a git read per open pair and a new filter on the page against that.

### Verdict: not shipped

Neither lever meets the recall level, so nothing ships: the curator, the review page and the verifier are as on main, and there is no held-pairs filter to add because no pair is held. The mechanism of the failure is that real problems, false alarms and skips come out of the same judge and look alike on every deterministic measure the store has: a bar that holds most of the false alarms (0.20 description overlap holds 15 of 18) also holds three quarters of the real problems (32 of 43), and git has nothing to ask about for nearly half of the real ones (20 of 43).

What the numbers say about where the owner's time goes, as observations and not tested levers: the daily findings are the ones the owner cannot answer. In sample3, 9 of 11 pairs from the newest curator run and 5 of 11 from the one before were skips, against 3 of 8 from the 2026-10-08 offline pass. The sample3 section records that the owner skipped the 17 because the owner could not know without git, and the timeline under each proof on the page already gives them the means to check. And the verifier, not an upstream gate, decides which real problems reach them: it closes 25 of the 43 as "both stand" and retires 12, the open verifier work of the sections above.

Limits. 78 pairs, of which 48 come from two hand-picked samples whose real and false-alarm rates do not describe the live flow; skips exist only in sample3. One id is in two samples (the later label stands). Both git signals depend on what `gitev` can extract from a fact's text, which is what the page's timeline uses too; a better extractor would raise b1 and b2 but 15 of the 43 real problems have nothing for git to find. The measured wiring (`gitev.MovedAfter` with tests, the harness that writes one record per finding, a pair scoring script) was kept on a tag in the development history and is not in this tree. Labels, ids and fact names are not in this file.

## Results from git (2026-10-10)

Step 4 of "Git as a source" (docs/PLAN.md): measured results committed to repos become facts that carry their commit, a newer result replaces the older, and a saved fact that quotes the older number is flagged. Built as `client/internal/results`, `tacit results`, and a pass in `tacit curate`; the contract is SPEC.md "Results from git". No model, no network. Round 2 (2026-10-10, after a clean-context review) changed what is read, what a hook may print and what is flagged; the numbers below are from the final build, and where round 1 differs it says so.

### Where results live (survey of every repo with a store scope and a checkout)

Read-only over the projects directory, the refs each checkout already had (the repo's remote default branch, `dev` for the sglang fork).

| repo | curated facts | commits | files with a metric column | structured files touched twice | results-named docs (touched twice) | files read | series | recurring | saved facts quoting | result facts | commit messages with a number |
|---|---:|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|
| memra | 364 | 3736 | 20874 | 409 | 350 (42) | csv 2, json 4, jsonl 83, md 57 | 4480 | 34 | 2 | 6 | 497 |
| repo D | 997 | 1550 | 6300 | 86 | 161 (31) | csv 2, json 27, jsonl 1, md 63 | 2290 | 23 | 5 | 24 | 401 |
| sglang | 40 | 19393 | 22 | 22 | 4 (2) | json 3, md 6 | 86 | 4 | 0 | 4 | 85 |
| private repo B | 318 | 226 | 66 | 19 | 1 (1) | json 5, md 7 | 228 | 3 | 0 | 1 | 56 |
| hqmtp | 35 | 102 | 13 | 0 | 1 (1) | md 2 | 55 | 0 | 2 | 1 | 37 |
| recipe-lab | 22 | 110 | 5 | 80 | 3 (2) | json 80, md 1 | 4837 | 0 | 0 | 0 | 25 |
| eigen | 59 | 808 | 1 | 5 | 1 (1) | md 2 | 5 | 0 | 0 | 0 | 81 |
| ocaml-valkey | 4 | 177 | 0 | 0 | 3 (3) | md 3 | 60 | 0 | 0 | 0 | 39 |
| valkey-glide | 16 | 4072 | 0 | 23 | 0 (0) | - | 0 | 0 | 0 | 0 | 33 |
| tacit | 42 | 32 | 17 | 1 | 2 (1) | jsonl 1, md 1 | 10 | 0 | 0 | 0 | 7 |
| private repo C | 13 | 19 | 0 | 0 | 0 (0) | md 1 | 12 | 0 | 0 | 0 | 3 |
| the other 28 repos | | | | | | | 196 | 0 | 0 | 0 | 120 together |
| total | | 34177 | 27482 | 771 | | | 12259 | 64 | 9 | 36 | 1384 |

Round 1 read 14,696 series, 419 recurring, 14 saved facts quoting and 43 result facts from the same repos. The 2,437 series and 355 recurring rows that left are mostly JSON statistics, per-item records and baseline or parity files (see "Fact quality"). recipe-lab's 80 JSON receipts are the largest source of series with no recurring row, and none becomes a fact.

"Files with a metric column" is jsonl, csv, tsv and json files at the default branch whose first row or top level has a column named like a measured quantity. "Files read" is the files the step reads: touched by at least two of the newest 6000 commits, with a measured number in their newest version. Pull request bodies (`gh`, merged): memra 270 of 589 carry a measured number with a unit, repo D 276 of 993, tacit 3 of 19; for squash-merged repos most of that text is also the commit message.

What the numbers say about where results live:

- **Per-experiment receipts, written once.** The 27,482 files are mostly folders like `research/<lane>-<date>/raw/` committed in one commit: per-request timings, telemetry, run dumps. Of memra's 68,640 jsonl, csv, tsv and json files 409 are touched twice (repo D 86 of 12,984). A file written once has no older value; it is not read. Rows of one setup with no timestamp are samples of a run, not results measured again, and are dropped.
- **Row logs that append a row per run with its own timestamp**: memra `research/tune-data/perf-ci.jsonl` (1154 rows over 29 commits, 19 cells by profile, each row a single run with the code sha it measured and a `window_clean` flag), the bench boards beside it. This is the one place a number is measured again and again.
- **Boards rewritten as results change**: repo D's board of published numbers (93 commits) and a site data file; sglang's CI baselines (`perf_baselines/*.json`, no longer read); a private repo's plan files.
- **Tables in lane documents**: memra `research/*/RESULTS.md` and `PROFILE-12.md`, `docs/PERFORMANCE.md`, `docs/SPEECH.md`; repo D's lane documents; a private repo's server docs. A table is edited while its lane runs, so a cell does change.
- **Free text**: commit messages and PR bodies. 1384 of 34,177 commits carry a measured number.

### Deterministic, and why no model

Row logs, boards and tables have a key (a setup, a metric, a unit) and are read with no model. Free text was measured, since the brief allows a model there only if nothing deterministic covers it. Haiku 5.5 (the dreamer's `claude -p` path, no tools, one turn, 8 messages per call) over the 1089 commit messages of seven repos (memra, hqmtp, eigen, recipe-lab, ocaml-valkey and two private repos) that carry a measured number: 3901 results for $0.65. A deterministic check drops what the message does not contain (the quote must be in the message and the value in the quote): 16 dropped. 2293 results have one number, a unit, a named metric and a setup. Hand-checked sample of 30 of those: 29 right on value, unit, metric and setup (96.7 percent); the one wrong took the low end of a range ("3.7x to 23x").

Extraction was not the problem, keying was. Grouping those 2293 results by repo, unit and setup words (Jaccard 0.7) gives 1556 groups; 20 of them (1.3 percent) hold two values from two commits, and those are sibling arms and before/after pairs from one lane written within hours, not a number that a later commit replaced. Commit messages record the arms of an experiment; the living number is in the file the lane edits. The stored facts that contain one of their values are coincidences of the number, or a lane's own inbox note quoting its own arms. So the model path is not in the step: it would buy nothing for "newer replaces older" and cost a call per new commit message.

### What was built

Details in SPEC.md. Per repo, the newest 6000 non-merge commits are listed once with their files; every row log, board or table touched at least twice is read in its newest version, kept if it holds a measured number, then read through up to 40 older versions. A series is one quantity for one setup in one file; each point carries the commit that wrote it. Log rows that share a full timestamp and a code sha are one run and become one point, their median, labelled with the row count; per-item rows never make a median; a table or board number is "run count not stated", and "single run" appears only where the file says it. A setup the newest version no longer holds (a renamed column) is dropped, so its last value is never shown as current. Groups become `derived/<repo>/<repo>-results-<file>-<group>-<hash>.md`, at most 20 rows a fact and 24 facts a repo, kept if a row recurs or a curated fact quotes it. A fact is rewritten by a newer commit and removed when its series leaves, unless a saved fact points at it. A git read that fails or is cut retires nothing (tested with a git wrapper that fails on demand). `derived_from` is the commit that wrote the newest row, and a repo's read is cached by its HEAD, so the daily run of an unchanged repo writes nothing.

A saved fact is flagged when it states a number with at least three significant digits that equals a value the row held at the precision it was written, with that row's unit after it, with the words that tell the row from its group's other rows (two at least), when the value was replaced by more than 1 percent (and outside the noise band for a log), the fact is not older than the value by more than a week, and the sentence carries no date on or before the replacement. Round 2 added: no flag when the fact also states the current value (in any word order, `->` included), a number inside a bound ("must stay above", "target", "floor", "budget", "at least") is not a claim, `history`, `past_mistake` and `code_change` facts are records and never flagged, and the noise band needs a sample: under three points 5 percent stands in (perf-ci's laptop-GPU cells move 2 to 5 percent between runs of the same code), three or four points use half the observed range, five or more use three times the median absolute deviation. A flag is a line in `review/<date>-results-<repo>.md`, read by the review page as a `result-superseded` pair whose generated side is never patched; an owner ruling becomes a `RULING CHANGE?` line. Nothing is retired.

### Fact quality (round 2)

The review found that the facts were more precise than useful: 26 of the 43 result facts and 112 of the 165 rows came from JSON, and a sample of them were confidence bounds, a per-item score or a CI gate. Three filters, in the order the review gave them:

1. **Not a result of a run**: JSON statistics (`ci*`, `lower`, `upper`, `one_sided_*`, `delta*`, `stderr`, `std`, `p_value`), anything under per-item or per-slice records and `stages_ms`, files named `*baseline*` or `*parity*`.
2. **Round**: a number with seven or more decimals shows four significant digits and keeps the raw text in brackets (`61.54% (raw 61.53846153846154)`).
3. **Gate what a hook may print**: a result fact is type `result` (printable) only for a row log, a group a curated fact quotes, or a group a saved fact was retired in favour of (accepting a flag retires the saved fact; the replacement stays printable); every other group is type `result_ref`, found by `tacit search` and the MCP tool and never printed by the hook. The description is `<repo> <file>: <metric> for N setups, newest <date>`, plus one row when its label is a cell id (a `key=value` pair, a slug with a digit, a table row `slug / column`; anything else stays in the body). The row is not matched to the prompt: it is the fact's first row, a row a curated fact quotes if there is one and otherwise the one that changed most recently, and the hook prints the stored line. So `how fast is e4b spec short` can print the 26b plain short row. No other free text of the repo reaches a hook line.

Result: 36 facts and 72 rows (round 1: 43 and 165); JSON is 22 of the 36 facts and 25 of the 72 rows (before: 26 of 43 and 112 of 165). The "14 of 30" in the round-1 text understated the JSON share; it counted the sample, not the output.

### Precision of extraction

Thirty rows drawn at random (seed 3) from the 72 rows of the 36 result facts of the final run, each checked against git: 19 table and log rows hold the value at the cited line at the commit the body names (the file's newest commit), the 11 JSON rows hold it in that blob, and the commit named as the writer holds it too; 30 of 30. Row label and unit read right against the cited line on all 30. All 25 JSON rows of the output were then followed by key path (array elements by their name field, table entries by their key) to the same value: 25 of 25. An earlier draw of this check found a real bug: a table that renamed a column (`lookahead`, 480 ms, became `encoder chunk`, 560 ms) kept the old column's last value as current. A series the newest version no longer holds is now dropped, with a test.

The round-1 text said line numbers were "as the file is at that commit". They are the file's numbering at its newest commit (the body says which); the writing commit of a row may have numbered the line differently, and the extraction check reads the line at the newest commit.

### What the live store gives

Checked against all 4688 curated facts (and, in a separate pass, the 2895 inbox notes):

- **Named, not stale: 9 curated facts quote 11 rows at their current value** (round 1: 14 facts, 19 rows). Examples: a speech-latency fact quotes two numbers from memra's `docs/SPEECH.md`, a hardware fact quotes a memory bandwidth, and a recipe fact quotes a tok/s figure from hqmtp's `results/engine-e2e-v1.md`. The 8 that left, by hand: two are record types (a `code_change` and a `past_mistake`), two state a bound (one inside a latency bar, one a requirement), one is a JSON statistic (`delta_points`), one is a measurement misread as a bound (a "times off its floor" figure; the skip is on the safe side and costs a match, not a flag), and two were not traced.
- **Flags: 0.** No curated fact quotes a value that a later commit replaced. A loose search (any unit, two digits, no setup check) over curated and inbox facts finds 20,594 candidates, all coincidences of small numbers. A search of git history for any number a curated fact quotes being replaced in place (139 decimal quantities, seven repos, `git log -G`) finds no replacement. The store's facts are filed after a lane's last number and the lanes append, so the case the owner described (git is more correct, and newer commits replaced the old results) does not occur on this corpus today.
- **A near miss that the dated-record rule exists for.** One fact says a headline number "was published at" a value on a given day; the board that holds that headline moved it four times in eleven days. The fact dates its number, so it is a record that stays true, and two digits are below the rule. Not flagged.
- **Reproduction on real history** (one fact constructed, because none exists): a scratch store, a copy of the live one plus `memra-26b-spec-balanced-speed` ("memra Gemma 26B spec decode at d1736 on the balanced profile ran 295.3 tok/s on the 5090 laptop", created 2026-09-03). 295.3 is a real perf-ci value (row of 2026-07-15, code `c4556e2`); the cell measured 240.75 on 2026-08-11 (code `1bd749d3`). `tacit results --out` writes the review file:

  `- result-superseded? [[memra-results-perf-ci-toks-bdb1fa]] may replace [[memra-26b-spec-balanced-speed]] - quotes 295.3 tok/s for cell=26b-spec-d1736 profile=balanced, which git recorded in commit 49d1d6f6594a on 2026-09-01, row time 2026-07-15T05:56:31Z, code c4556e2 and replaced in commit 49d1d6f6594a on 2026-09-01, row time 2026-08-11T06:49:03Z, code 8aa05f7b; git now holds 240.75 tok/s in commit 49d1d6f6594a on 2026-09-01, row time 2026-08-11T15:03:04Z, code 1bd749d3 (run count not stated, one row). Source: research/tune-data/perf-ci.jsonl.`

  (The commit is the one that bulk-imported the log into memra's tree; the rows' own times and code shas are what date the measurements.)

### Delivery, measured twice

The prompt hook's second query ranks `derived/<repo>` for one line. Two prompt sets, each run through `tacit hook prompt` with the cwd of the repo on a scratch copy of the live store (with no result facts, with round 1's, with the final build's), every line judged by hand against the prompt: helpful (the line is about what the prompt asks and carries or points at the number), weak (right topic, no number or a neighbouring row), wrong (a different quantity, setup or model than the prompt asks).

| set | prompts | round 1 result lines | round 1 helpful / weak / wrong | final result lines | final helpful / weak / wrong |
|---|---:|---:|---|---:|---|
| A: 31 prompts of the derived-facts trial (memra and repo D; 18 name no file, 2 a module, 6 one file, 5 several files or the layout) | 31 | 1 | 0 / 1 / 0 | 0 | 0 / 0 / 0 |
| B: the reviewer's 21 prompts (perf-flavoured and plain, five repos) | 21 | 17 | 8 / 5 / 4 | 8 | 4 / 4 / 0 |

On set A the hot-spot and module-map lines are identical to the run with no result facts. On set B the four helpful lines are the row logs (perf-ci toks twice, perf-ci accept) and the hqmtp board a saved fact quotes; the four weak lines are table groups a saved fact quotes whose line gives the file, a unit and a count and not the number. Nine of the 17 lines no longer print (counted from the totals: 4 helpful, 1 weak and all 4 wrong are among them); the numbers they carried are still found by `tacit search` and the MCP tool. Wrong is 0 against 4 helpful, so the hook line stays, restricted to type `result`. The floor stays 12: on set A the highest score of a result fact on a prompt that asks for no number is 9.7, on set B the eight printed lines score 15.3 to 35.3, and helpful and weak lines overlap in that range, so no floor separates them (the gating does the work). A result line never enters SessionStart (tested). Set B is the sample the reviewer drew; both sets are small and were used to design the gate, so a third set is the real test.

### Cost

Every checked-out repo, `nice -n 19`, at a load of 60 to 100 on the workstation: 34 to 46 s cold and 11 to 12 s with the per-repo cache, with byte-identical output; one repo 2.4 to 2.8 s with the cache (the three largest), against 34.5 s for `tacit derive --dry-run --out` over all repos. The cache is 6.5 MB, one entry per repo and HEAD. No network, no model. The store lock is held only for the write, as in `tacit derive`. Round 1 spawned the redactor once per fact (34 s warm against 40 s cold); redaction is now one call per repo.

### Limits

- Recall is bounded by the structure the extractors see. A number in prose, or in a table whose rows have one distinguishing word, is not matched; free text is out for the reason above.
- Flag precision is measured on fixtures and by the 11 current-value matches, not on real stale quotes, because the store holds none. The first live run is the test.
- A curator fact's date is its filing day, so a flag never says a fact was wrong when saved, only that a newer commit holds another number.
- The bound filter errs toward silence: a "times off its floor" figure is skipped as a bound.
- A log whose rows each have a time of their own is read as a series even if a column is unique to each row. A per-request log with a request id and a timestamp per row would make a series (an earlier guard dropped any setup whose rows mostly differ, and with it perf logs that carry a uuid or note column; it fired on nothing in the the checked-out repos, which read identically with and without it, 12,259 series). Items that share a run's timestamp still make no series.
- The hook trial is two small sets judged by one reader; the reviewer's 21 prompts were also used to tune the gate.

## Staleness check (2026-10-10)

PLAN step 3. `tacit stale` (also the last step of `tacit curate`) flags curated facts that git has overtaken and sends them to the verifier or the owner with their timeline. No model, no write to a fact. This section is the rule it shipped with, the rule it replaced and why, and what it costs.

Setup. Two copies of the live store, taken about two hours apart on 2026-10-10 (4688 curated facts both times), read against the real checkouts in the projects directory, workstation load 80 to 110, everything at nice 19, 4 to 6 facts at a time. Dry runs with `--max 0`; the live store, the live review pages and the server were not touched, and the copies and caches were deleted afterwards. The three-rule comparison and the hand check of the changed-line rule come from the first copy and the first version of the code; the shipped rule's day-one numbers come from the second copy and the final code (after the clean-context review: repos read at their default branch, committer dates, path presence across the repos a fact names, reads that did not finish flag nothing). Every hand label below is the agent's, not the owner's, and was written before the verifier's answers were read.

Funnel, whole store, final code: 4688 curated facts; 1643 name no repo with a checkout; 1107 name one but nothing git can be asked about (no path, no specific identifier); 1938 have a git trace; **21 flagged**, 1916 quiet, 1 incomplete (its read did not finish, so it is retried later).

Three rules on the same facts:

| rule | what flags a fact | flagged | by hand |
|---|---|---|---|
| naive | any commit on a path, an identifier or a message the fact names, after its date (gitev's full read) | 39 of 100 facts with a git trace (random 100) | not checked |
| changed line | a commit after the fact's date removed or rewrote a line holding a specific identifier it names, or a named file is gone | 26 of 496 (every 4th fact with a git trace, 5.2 percent); on the same 100 as above: 8 | 20 of the 26 (seeded random): 4 stale, 1 unclear, 15 still true |
| deletion (shipped) | a named file is gone from every repo read, or a named identifier was removed after the date and no repo read holds it any more; repos read at their default branch | 21 of 4688 on the final code (the first version: 17; 4 of the 496; on the same 100: 1) | all 21: 18 have something stale, 3 still true |

The hot-file case is the naive rule: a fact about a file that commits keep editing is flagged on its first commit and every later one. The unit tests `TestHotFileTouchedByUnrelatedCommitsIsNotFlagged` and `TestAChangedValueWithItsNameStandingIsNotFlagged` fail when a file touch or a rewritten line is allowed to trigger (checked by letting it, then reverting).

Why the changed-line rule went. All 4 of its flags that came from a deleted file were stale, and none of its 16 flags that came from a changed line was (15 still true, 1 unclear). The 16: five facts flagged on flags that other tools share (`--workspace`, `--state`, `--base`, `--filter`, `--locked`, `--days`, found in another tool's docs), six on a reworded or refactored line around a name HEAD still holds (a doc paragraph, a rules line, a table row), three on a name removed from an old script or comment but still used elsewhere, one on field names that another repo's docs share, one lesson whose code was deleted and whose advice stands. The deletion rule keeps only what that split left: something the fact names was deleted. A changed value with its name standing is no longer flagged; there is no measurement of a value-aware rule, and step 4 reads committed results directly.

The 21 on day one, by hand. Eleven are outdated and the fact should retire: two plans whose scripts were deleted, two facts about code that was removed, a plan item already done, notes on lines of a file that no longer exists, a lesson about a site that was folded into another, and a scheduled job removed on 2026-10-07 (4 facts). Seven are true in substance with a detail to fix: guidance files consolidated into one (3 facts), a directory folded into another (3), a kernel table naming a deleted kernel. Three are still true: a fact that records the deletion itself, a GitHub behaviour lesson whose repo no longer names the token, and a CLI lesson whose setup text quoted a flag of a removed script. 18 of 21 is 86 percent (95 percent interval 65 to 95); 11 of 21 should retire (52 percent, 29 to 68). Four of the 21 are owner rulings (`kind: user` or `user_standard`); they go to the owner as questions and are never sent to a model (three of them are a reference to a moved file under a rule that stands, one is a read-first instruction that names a deleted file). n is 21 on one store, so this is a development-set number.

What the review changed in the count: the first version read each checkout's HEAD, and one checkout was behind `origin/main`, so a job removal (5 flags, 4 of them stale) was invisible; reading the default branch found it. Judging a path as gone only if no repo the fact names holds it dropped one flag, a file that moved from one repo to another (it had been a stale detail).

After day one. The 21 come from 10 commits dated 2026-06-14 to 2026-10-07. Five consolidation or retirement commits (a site folded into another, the guidance files consolidated, a recovery chain removed, a feature retired, a job removed) flag 2 to 5 facts each. The newest is from 2026-10-07, three days before the run, so on this store the steady state is about one flag a week in bursts of a few, far under the cap of 60 a run. A flag is written once per newest triggering commit, an unanswered finding is never flagged again, and a deletion after a "still true" mark is a new event (unit tests `TestSettledFindingsDoNotMuteALaterDeletion`, `TestStillTrueIsSpentUntilANewCommitLands`).

Cost. The whole store, final code, cold cache, took 3079 s and 11970 git commands (6 facts at a time, load 80 to 110; the first version, with part of the evidence cached, took 1849 s and 15010); a warm repeat over 496 facts took 70 s. About 6 git commands per fact with a trace; the time goes to `git grep` of each identifier at HEAD (4 to 16 s each on the workstation). A command that times out leaves its fact incomplete: nothing is flagged or recorded as checked, and the fact is retried the next day, then every 2nd, 4th and 7th day. `curate` gives the step 45 minutes at nice 19 and idle I/O priority, runs it last (after its own state is saved), skips it in `--dry-run`, and keeps a per-machine cursor (`~/.local/state/tacit/stale.json`), so a run that runs out of time continues where it stopped.

Verifier on 13 non-ruling flags of the first version (12 of them are still flagged by the final code). Sonnet 5.5 (`tacit verify --dry-run --ids`, replay copy, each run in `systemd-run --user --wait --pipe` with the model provider's environment file), 3 runs, medians. The stale prompt carries the fact, the commits and the one-fact timeline. Against the agent's hand labels (7 retire, 4 fix, 2 keep): 12 of 13 match exactly (11, 12, 12), 13 of 13 on stale against still true (12, 13, 13); it never retired a fact the labels kept (0 in 3 runs) and retired 6 of the 7 (the seventh, a plan item that is half done, it marked fix in all 3 runs); one run kept a fact labelled fix (a historical record that cites a file that moved). $0.13 to $0.18 a run of 13. Not re-run on the 5 flags the review added.

Not measured. Recall: how many stale facts the rule misses (a value that changed under a surviving name, a plan overtaken without deleting anything). Anything on a second store. The owner's own labels. The step did not run in the live `curate` timer yet.

## Verifier model: Opus 5.5 against Sonnet 5.5 (2026-10-10)

The evaluation setup on 2026-10-10 used Opus 5.5 first, with Sonnet 5.5 after a capacity failure. Each verdict recorded the model that produced it. These are historical evaluation settings, rather than current runtime defaults. The runs below test whether a stronger model moves the ceiling the earlier sections hit (Sonnet: sample2 about 19 to 20 of 27, sample3 3 of 13, with 8 of the 10 retirements missed as "complementary").

How a capacity failure surfaces. `claude -p --output-format json` exits 1 and still prints its result object: `is_error: true`, the HTTP status in `api_error_status` and the provider's text in `result` (for example `API Error: Repeated 529 Overloaded errors`, or a cloud provider's `ThrottlingException`, `ServiceUnavailableException` or `ModelNotReadyException`). An unknown model id is a 400 (`The provided model identifier is invalid`, plus `[claude-code:unrecognized_model]` on stderr), checked live. The fallback takes 429, 503 and 529, and the overloaded, throttling, unavailable and high-load texts on a failure with no status or a 5xx status, read from `result` and stderr together (Claude Code's own "Repeated 529 Overloaded errors" can come with no status). A 403 or 404 (Opus access lost, the model not offered) is not capacity: it fails the run, non-zero, rather than running on Sonnet unseen. A 400, a budget stop, a schema failure, a plain 500 or a crash fails the batch as before and is not retried on Sonnet. Same-model retries for a hiccup before any work (30 s, then 60 s) come first; the fallback is for capacity that does not come back. The Opus call's cost counts toward the run even when it failed after some turns. `--model` names one model and turns the fallback off. The verdict's model goes into the decision record (`model` in `review/decisions.jsonl`), the dry-run output, and the run line (`by model: ...; N batch(es) fell back to ...`). The CLI's own `--fallback-model` is not used: it switches models inside one session, so one batch's verdicts could come from both and could not be attributed.

Tests (`client/cmd/tacit/verify_fallback_test.go`) put a fake `claude` on PATH that logs the model it was asked for and answers per model: Opus answering is one Opus call and every decision records Opus; Opus failing with 503 before any work, with 529 after some paid turns, or with "Repeated 529 Overloaded errors" in an error result with no status, ends in exactly one Sonnet call, every decision records Sonnet, the run counts one fallback and the failed Opus call's cost is kept; a 400 invalid model id, a 403 lost access or a budget stop fails the batch with no Sonnet call and no verdict, and verdicts that come back with an error are discarded; a stale finding falls back the same way and records its model; with a named model a 503 fails the batch with no Sonnet call; a table checks the classification.

### Runs

Same setup as the earlier verifier sections, rebuilt from scratch for this run: replay stores are clones of the live store (never the live one) with `origin` a local bare copy. sample2 pinned at the commit that created it, history after it removed; 27 findings open, 3 settled and skipped. sample3 rebuilt as before: the 19 agent records on the sample removed (one of them an ask with no before snapshot), the relations they changed put back from their `before` snapshots as reopen does (13 fact files), `MEMORY.md` regenerated, history flattened to one commit; the 13 answered ids sent. Labels were read only by the scoring script under `~/.cache`, outside every verifier root. One binary (this branch), `tacit verify --dry-run --ids ... --model M` with the model named, so no run fell back. Each run inside `systemd-run --user --wait --pipe` with the model provider's environment file, `Nice=19`, its own clone of the replay store. Three paired rounds per sample, Opus and Sonnet side by side, two at once, 12 runs, 2026-10-10, 34 minutes. A first round was cut off by a reboot; it was discarded and everything was rebuilt and rerun. Scoring as before: on an owner-only or "neither true" pair only a verdict that reaches the owner ("owner" or "query") is right, and the ruling guard is applied to every verdict as `applyOps` would. "Retired a kept fact" counts a wrong-side verdict too.

Raw model marks, medians with the range in brackets:

| sample | model | agrees | missed retirement | retired a kept fact | question or query | guessed (owner-only) | both-stand pairs kept |
|---|---|---|---|---|---|---|---|
| sample2 (27) | Opus 5.5 | 20 (19-20) | 6 (5-6) | 1 (1-1) | 0 (0-2) | 0 | 13 (12-13) of 13 |
| sample2 (27) | Sonnet 5.5 | 18 (17-20) | 6 (4-7) | 2 (2-2) | 1 (0-1) | 0 | 12 (12-13) of 13 |
| sample3 (13) | Opus 5.5 | 3 (2-4) | 8 (6-8) | 0 (0-1) | 0 (0-1) | 2 (2-2) | 1 of 1 |
| sample3 (13) | Sonnet 5.5 | 3 (3-3) | 8 (8-8) | 0 (0-0) | 0 (0-0) | 2 (2-2) | 1 of 1 |

Per run (agrees, missed, retired a kept fact): sample2 Opus (20, 6, 1), (20, 6, 1), (19, 5, 1); Sonnet (18, 6, 2), (20, 4, 2), (17, 7, 2). sample3 Opus (2, 8, 1), (3, 8, 0), (4, 6, 0); Sonnet (3, 8, 0) three times. One Sonnet run on sample2 returned 26 verdicts for 27 findings; the missing one counts as not agreeing.

The owner's retirements with the ruling guard applied (14 in sample2, 10 in sample3). "Missed" includes a query or question that names no side:

| sample | model | applied | asked, same direction | missed | wrong side applied | wrong side asked |
|---|---|---|---|---|---|---|
| sample2 | Opus 5.5 | 2 (2-2) | 5 (5-5) | 6 (6-6) | 1 (1-1) | 0 (0-0) |
| sample2 | Sonnet 5.5 | 2 (2-2) | 4 (3-5) | 6 (5-7) | 1 (1-1) | 1 (1-1) |
| sample3 | Opus 5.5 | 2 (1-3) | 0 | 8 (7-8) | 0 (0-1) | 0 |
| sample3 | Sonnet 5.5 | 2 (2-2) | 0 | 8 (8-8) | 0 | 0 |

Cost and time per run: sample2 Opus $1.50, $1.19, $1.11 (382 to 497 s), Sonnet $0.19, $0.18, $0.28 (75 to 128 s); sample3 Opus $1.00, $0.86, $0.98 (239 to 307 s), Sonnet $0.13, $0.07, $0.10 (39 to 71 s). Opus costs 6 to 10 times as much, about $0.04 to $0.08 a finding against $0.01, and takes 3.0 to 7.9 times as long (range against range). All 12 runs: $7.59 ($6.64 Opus, $0.95 Sonnet).

### Per-pair flips

Counted without pair ids or fact names, as before. Majority outcome over the three runs of each model:

- sample2: 3 of 27 pairs differ. Two go Opus's way: a miss Sonnet made became an agreement, and a pair where Sonnet asked or flagged became an agreement. One goes the other way in kind: a wrong-side retirement Sonnet made became a miss ("both") under Opus. That is the 2 against 1 in the retired-a-kept-fact column; the remaining wrong side is the project pair the earlier sections describe, wrong in every run of both models. Pairs whose outcome changes between runs: Opus 2 of 27, Sonnet 4.
- sample3: 1 of 13 differs: a retirement Sonnet applied in the owner's direction in all three runs is split under Opus (one agreement, one query that names no side, one wrong-side retirement that would be applied, since neither fact is a ruling). Two other retirements that both models miss in their median were caught by Opus in one run each, both in its third run (4 agreements, 6 misses). Pairs whose outcome changes between runs: Opus 3 of 13, Sonnet 0.
- Replay check on sample3: the majority mark equals the agent mark the sample file records on 11 of 13 pairs for Sonnet (as in the earlier section) and 10 of 13 for Opus.

### What it says

Opus does not move the ceiling. On sample2, the set the prompt was written from, Opus agrees on 20 against Sonnet's 18 in the median run, keeps every both-stand pair in the median run and has one wrong-side verdict where Sonnet has two; Sonnet's own three runs span 17 to 20, so the gap is inside Sonnet's run-to-run range. On sample3, the held-out sample for model choice (no model was chosen on it before), both models agree on 3 of 13 in the median and miss 8 of the 10 retirements. Opus's evidence on those misses reads like Sonnet's: the facts "agree", there is "no conflict", B "is consistent with" A, or they are "scoped cases of one rule". Neither model reaches the owner on the two "neither is true" pairs in any run. Opus is not worse at keeping facts the owner kept: it retires fewer of them on sample2 and the one wrong-side retirement it made on sample3 came in one run of three. It is less stable on sample3 (3 pairs change between runs against none).

So the missed retirements are not a model-strength limit that Opus lifts; they follow from what the verifier is asked and shown (the earlier sections: the owner retires the older side of two complementary facts, and nothing in the sources tells the model which one the owner counts as current). The default is Opus per the owner's call, at 6 to 10 times the cost per finding, which at the daily inflow of about one open finding a day is cents.

Limits. Three paired runs per model per sample and medians, no five-run rerun: the sample3 medians are equal and the sample2 gap is inside Sonnet's own range, so more runs would not turn either into a clear difference in the direction that matters (sample3). sample3 was seen during the failed prompt attempts but never for model choice. Workstation load during the runs was low (after a reboot), and the two models ran side by side, so load affects both.

## Rulings are the owner's own words (2026-10-11)

The owner answered every Needs-you item and found that many "your rule" facts were agents' statements. On the live store (2026-10-11) 802 live curated facts were treated as rulings: 596 `source.kind: observed` with `type: user_standard` (the type half added 2026-10-09, see above), 138 kind user with `user_standard`, about 70 other kind user. A ruling is now `source.kind: user` with evidence that the words are theirs (SPEC.md, Rulings), and `tacit rulings audit` decides every old ruling with no model.

Dry run on a copy of the live store (never the live one), after the second review: their words come from 17,227 of their turns in 717 sessions (transcripts 2,945, Claude prompt history 12,634, Codex prompt history 1,051, a remote-session tool's task store 597); 5,583 agent sessions and 297 of unknown origin skipped. The first version read 17,583 turns: the difference is agents' text it took for theirs (peer and task-notification turns, remote-tool frames, pastes, probe and runner sessions).

| Outcome | All (808) | Live (802) |
|---|---|---|
| Kept: a statement of theirs, six words or most of the description | 77 | 76 |
| Kept: the owner kept it in a review decision | 1 | 1 |
| Kept: the owner wrote it in tacit app | 0 | 0 |
| Put to them: their words, too few to prove it (Needs you) | 27 | 27 |
| Not a ruling: no words of theirs found | 702 | 698 |
| Not a ruling: their note says it is not theirs | 1 | 0 |

Kind user set to observed: 199, of which 192 stay protected until the owner answers (no words of theirs found; their words may be in a transcript that is gone). Observed standards set to kind user because a statement of theirs was found: 68. Files changed: 277 (only the `source` block), plus the quote ledger and the audit marker. With `--strict` (no corrected typo) 62 quotes keep a fact: 15 of the 77 rest on a typo the agent fixed.

The two facts the review named: one is not a ruling (its first quote is a question the owner asked, and its other quote is in no turn on the machine), and the other is put to them (its only words of theirs are a session's name).

Hand check, one random sample (seed fixed), each fact read beside their turn, asking "is this fact their ruling":

- 20 kept: 18 are their rulings. 2 hold their words but state an agent's rule drawn from a complaint of theirs.
- 30 not rulings: none holds a run of five or more of their words (one holds a model id the owner typed in a config). 7 of them were kind user and stay protected until the owner answers.
- The 18 short-quote facts of the first pass, all read: about 15 are rulings of theirs (short imperatives of three to five words), 2 are not (a session's name, an encouragement); the owner confirms each once. 9 more (a run of six of their words in a reworded quote) join them.

Limits. Recall is bounded by what survives on the machine: Claude Code deletes transcripts after a few weeks (the prompt histories go back to 2026-04-27; desktop sessions write none). One sample, one hand check, no second rater. The proof test reads words and context, not meaning: a long quote of a complaint can still carry an agent's rule.

## Curator backlog and the lost conflict checks (2026-10-11)

### The 509 index errors

The 05:30 run logged 509 `curate: index query: unable to open database file (14)` and made 96 conflict checks for 605 facts. 509 is exactly the facts of batches 2 to 6 (99 + 118 + 79 + 110 + 103); batch 1 (96 facts) queried at 05:35, before the follower restarted at 05:36:58 on a new build (index schema v3). The curator was the 05:30 build (schema v2). `search.Open` opened the database lazily: no file was opened until the first query, which came after the five to seven minute split call. In that time the follower synced, built `index-v3-<head>.db` and pruned every other `index-*` file, the curator's included; the follower did the same after each of the curator's pushes. The follower's own `disk I/O error (5898)` at 06:07:51 is the reverse: the curator's prune deleted the journal of a build in progress. The same race exists within one build: any push (the dreamer's, a hook's) moves HEAD, and the next index build prunes the file a long-running holder has not opened yet. Not the cause: open files, paths under a worktree or scratch HOME (both processes used `~/.local/state/tacit/index`).

Fix: `search.Open` opens and reads the file before it returns and keeps one connection (an open file survives its unlink); a file gone between build and open, or one that fails to read, is built again. `prune` keeps newer schema versions and the files of a build whose process is alive. The curator opens the index after the split, opens it again once when a query fails, and otherwise writes nothing for the batch and stops non-zero. `tacit curate --recheck-conflicts --since D [--until D]` runs the check alone. Dry run on a copy of the store for 2026-10-11: 605 facts, 538 pairs, 22 Haiku calls, $0.01 to $0.03 over two dry runs, 4 contradictions and 7 supersedes, all review lines (3 of them same-day siblings).

### What the backlog is

| | 2026-10-11 |
|---|---|
| Inbox files in `inbox/dreamer@<host>` | 3,643 |
| Of those already curated or found empty | 1,549 (1,413 gave 1,723 facts, 1.22 per note; 136 gave none) |
| Pending notes (all writers) | 2,108 (dreamer 2,094) |
| Dreamer notes written per day | 1,327 (10-09), 989 (10-10), 1,446 in the last 24 h |
| Curator: notes per run, 6 batches | 508 (84 to 88 per batch) |
| Curator: cost | $4.96 a run, $0.83 a batch, about one cent a note; 6.3 min a batch |

"2292 notes to 605 facts" in the run line is pending against made: the run read 508 of the 2,292. Promotion is 1.19 facts per note that day: dreamer notes are one fact each already, and the split keeps 91 percent of them. The backlog grows by about 800 notes a day.

### Levers measured

Duplicates (no model, `curate.Restated`): of the 2,094 pending dreamer notes, 120 share 0.6 or more of their description terms with a live curated fact (37 share 0.8 or more), and 84 with an earlier pending note. Most of the high-overlap pairs differ in a number, a path or a negation (a new port, "no longer needs"), which is an update the conflict check must see. With those guards, and scope, body and owner-words guards, 23 of 2,108 (1.1 percent) go: all six read in a sample are restatements. Duplication is not what fills the inbox.

Value (Sonnet 5.5 as labeler: durable for another agent's next task, or true only inside the task it came from): three samples of 150 pending dreamer notes were 18.0, 31.3 and 32.7 percent task-only (median 31.3). Notes filed under a worker worktree's name (21 percent of pending) were 46 percent task-only, the rest 22 percent.

Dreamer prompt. v3 is v2 plus one rule: "Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree." v4 adds "A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task." Each sample is 30 transcript windows (the middle full window of a session touched in the last 48 hours), both prompts on the same windows, Haiku 5.5, labels by Sonnet 5.5 on the mixed set:

| sample | prompt | facts | durable | task-only |
|---|---|---|---|---|
| A (seed 5) | v2 | 137 | 93 | 44 |
| A | v3 | 113 | 103 | 10 |
| B (seed 6) | v2 | 164 | 121 | 43 |
| B | v3 | 130 | 117 | 13 |
| C (seed 5, two hours later) | v2 | 97 | 77 | 20 |
| C | v4 | 73 | 71 | 2 |
| D (seed 6, two hours later) | v2 | 235 | 151 | 84 |
| D | v4 | 181 | 165 | 16 |

Recall on the 80 truth gaps (section 4, run with `--bare`, Haiku extractor and judge, all 80 items, two runs per prompt):

| prompt | yes | at least partial |
|---|---|---|
| v2 | 43, 40 | 60, 61 |
| v3 | 40, 41 | 58, 55 |
| v4 | 41, 39 | 58, 58 |

v3 lost three gaps in both of its runs that v2 found in both: a fix (close and reopen a PR with no check runs) and two owner rules that came up inside a task. v4 finds them again; one gap goes the other way for each of v2 and v4 in both runs, and the rest moves within a prompt's own run-to-run spread (v2's own yes count is 43 and 40). v4 ships: facts down 25 and 23 percent, task-only facts down 90 and 81 percent, durable facts 77 to 71 and 151 to 165, recall 58 against 60.5 at least partial. Labels are one model's reading, with a lenient rule (unsure is durable).

Repo of a session: the dreamer took the basename of the session's cwd, so 21 percent of pending notes were filed under a worktree name and 12 percent under the home directory's name or `projects/client` (tacit's subdirectory). `dream.RepoOfCwd` maps worktrees and subdirectories to their repo and the home directory to no repo.

Batch size: the split's cost is per note (input and output both grow with the notes) and its time is output-bound (about 100 facts in 6.3 minutes), so larger batches change neither; they only risk the output limit. Not changed.

### Budget

Intake after v4 and the restated drop: 1,327 x 0.76 x 0.99, about 1,000 notes a day (740 to 1,090 over the three daily figures), 11.7 batches. `--max-batches` goes from 6 to 16 (1,360 notes a day, about 101 minutes), with `--budget 2h30m` on the split phase and the unit's timeout at 5 h.

| | before | after |
|---|---|---|
| Notes in per day | 990 to 1,450 | 740 to 1,090 |
| Notes curated per day | 508 | up to 1,360 |
| Backlog | grows about 800 a day | 2,085 clears in about 6 days (3.4 to 7.6) |
| Curator cost per day | $4.96 | $13.30 while clearing, then about $9.70 ($7.30 to $10.60) |

## The repo of a session that starts outside one (2026-10-18)

Measured on a second owner's machine, where agents start in the home directory or a folder of checkouts and reach their repos by path: 116 frozen transcripts (Claude Code and Codex), 283 dream windows. The repo of every window was read with tacit's own `dream.Windows` on main and on this change, with the machine's real home directory; no model is called for it. A Sonnet 5.5 judge then read each window and the repository it was filed under: "is most of this window's work about this repository?" (yes, mixed, no).

| window filed under | main | this change |
|---|---|---|
| a repository | 121 | 96 |
| no project (`user`, `machines/<host>`) | 125 | 187 |
| a folder of checkouts that is no repository (`projects/<folder>`) | 33 | 0 |
| the home directory's name (a cwd recorded as `file:///home/u`) | 4 | 0 |

Main names a repository by the cwd's base name when the cwd is in no repository: 37 windows went to pseudo-projects, and 39 more to names such as `on`, `re` or a URL slug (Codex project folders, file URLs). This change gives 57 windows a repository their cwd did not give, from their paths, and leaves the rest to no project.

| repository of a window | right | mixed | wrong |
|---|---|---|---|
| main, the cwd's repository (39 windows sampled) | 12 | 6 | 21 |
| this change, inferred from paths (all 57) | 34 | 11 | 12 |

Long sessions move between repositories, so a 60k-character window often is not about the repository the session started in; the judge is strict, but it is the same judge for both rows. Rules tried on the same windows before this one: every path once with a simple majority, per delta (23 right, 7 mixed, 10 wrong of 40 sampled) and per window (18, 9, 13 of 40); the shipped rule counts a path the agent works in (`cd`, `git -C`, an edit's target) three times and asks for three quarters of the window's count.

Why it matters: the same store with its pseudo-project scopes moved to `user` (no other change, same ranker and floors) delivered the truth fact offline for 79 of 254 situations against 60 with the local selector, and 92 against 66 with Jev. Measured on a store whose pseudo-project scopes came partly from the harness: the evaluation dreamed with a substitute home directory, under which the real home was no home, so these numbers bound what fixing folder-named scopes is worth rather than measure this change's own delivery. The prompt-time inference is not measured offline (the check carries no transcript). Not measured either: whether any of the 62 more windows that now get no project held facts that belong to one; they reach every session through `user` instead of being hidden in a wrong project.


## Dreamed rulings keep the owner's words; dreamer and curator models (2026-10-18)

Measured on a second owner's machine (116 frozen transcripts, 254 truth situations, eval/SCHEMA.md).

**Rulings lost their owner.** Curation keeps a ruling as `kind: user` only when a quoted phrase in the note is found in the owner's turns (Rulings, The curator). Prompt v4 asked for rulings "quoted or closely paraphrased", and the dreamer paraphrased: of 85 dreamed owner notes, 69 held no quoted phrase, 5 a phrase too short to prove the rule, 3 one not found in the owner's turns, 8 would verify. Curating the same 287 notes with either model kept 4 rulings as the owner's of 85. Prompt v5 asks for the owner's exact words in `quote`, and the note body ends with the quote (`The owner said: "..."` in the run measured here; `Quoted from the session: "..."` since the review, so an unverified quote is not stated as theirs): on a full re-dream of the history, 373 of 374 owner notes carry their words (2 of 513 before).

**Dreamer model.** 89 digest windows of 60k characters around a truth situation's evidence (53 owner rulings, 12 machine facts, 10 past mistakes, 8 design decisions, 6 history items), prompt v4, a Sonnet 5.5 judge on "do the extracted facts state the truth fact":

| dreamer | truth fact written down | rulings | facts per window | dollars per window |
|---|---|---|---|---|
| Haiku 5.5 | 64% | 39 / 53 | 23.9 | 0.010 |
| Sonnet 5.5 | 72% | 42 / 53 | 14.6 | 0.118 |
| Opus 5.5 | 78% | 43 / 53 | 20.2 | 0.270 |
| Opus 5.5, prompt v5 | 74% | 42 / 53 | 21.0 | 0.279 |

One run per row. v5 against v4 on Opus: 3 fewer windows with the fact stated and 2 more partly (22 against 20), inside what one run can tell apart; v5 is not chosen for recall but for the quote. An Opus window took 51 s at the median, so a run at the 40-call cap is about 34 minutes of calls, inside the 45-minute batch bound.

**What curation keeps of v5 rulings.** The full re-dream (Opus 5.5, v5, 3,106 notes) curated with Sonnet 5.5 in 8 partitions, merged and finished: 254 of the 374 owner notes became 283 facts kept as the owner's (`owner_quote` backed by their turns), 151 more facts from them filed as observed. On v4 notes the same check kept 4 of 85. Are the kept ones their rulings? A Sonnet 5.5 judge on a seeded sample of 60 kept facts, shown the fact and its quote: 56 the quote states the rule, 4 the quote is theirs but the rule is an agent's reading or a stretch of it (a project's description from a request to build it, a standing rule from a one-time complaint), 0 unrelated. The agent read the 4 and agreed; the 56 were not hand-labelled. So `ownerwords.ProvesRuling`, calibrated on 31 facts, now carries almost every dreamed ruling and lets about 1 in 15 agent readings through as the owner's.

**Curator model.** The same 287 pending notes (v4 notes, mostly without quotes) on two identical store copies, `tacit curate --model` each; a blind pairwise judge (Sonnet 5.5, A/B in random order) on the 170 sampled notes: Opus better on 138, Sonnet on 22, tie 10 (owner notes 66 / 14 / 5). Cost per note: Sonnet $0.0040, Opus $0.0107. Why Sonnet lost: on a seeded 40 of the 138, the same judge shown both curations named what makes Opus's better: Sonnet's leaves out a fact or a rule the note has on 28, misstates one or gives it the wrong kind or scope on 7, wording only on 5 (owner notes 13, 3, 0). The losses are lost facts, not style. Not measured: the curator models on v5 notes; the kept-rulings numbers above are Sonnet's.

The owner chose Opus for both. For this history: a full rebuild about $119, about $79 a month at its pace (a Sonnet curator: $93 and $62). The re-dream with Opus cost $36.56 for 3,106 notes.

## The prompt selector in live sessions (2026-10-18)

Measured on the second owner's machine with the rebuilt store of the section above (Opus 5.5 dreamer with prompt v5, Sonnet 5.5 curator, repo inference). There are 24 live tasks from the owner's history, each run 3 times (72 cells per arm). Every cell is a fresh `claude -p` session on Opus 5.5 with the hooks of one arm, and a Sonnet 5.5 judge scores the answer against the truth fact (yes 1, partial 0.5). The two arms use the same build and store; only `utility.provider` differs.

| arm | judged | decision tasks | read tasks | tool calls | input tokens | seconds |
|---|---|---|---|---|---|---|
| local selection | 0.82 | 0.77 | 0.88 | 1.6 | 61k | 26 |
| Jev, threshold 0.70 | 0.80 | 0.73 | 0.88 | 1.7 | 66k | 30 |

Per task, local scored higher on 2 tasks, Jev on 2, and 20 tied. The median per-task difference in tool calls and input tokens is 0. Across the offline and live runs, Jev answered 304 calls and missed the 2 s deadline once. Offline, on the 254 truth situations, Jev delivered the truth fact for 77 and the local selection for 72. That does not carry over to the sessions. Jev's own cost is not in these numbers. So no provider is the default and the docs recommend leaving it off; the selector benchmark in docs/selectors.md still ranks the models for anyone who wants one.

## Prompt delivery: a stemmed index (2026-10-19)

Measured offline on the second owner's machine: the rebuilt store of the sections above, 254 truth situations, each session in a git checkout named after its repo, and the bench's Sonnet 5.5 judge (yes, partial, no on "does this context give the agent the fact"). A context identical to main's kept main's verdict, and every changed context was judged once. On 15 contexts judged twice with no change, 2 verdicts flipped, so a difference of 2 or 3 is within what one run can tell apart.

| build | yes | partial | no | rulings yes / partial / no (59) | truth target in the context | prompt lines | median context chars |
|---|---|---|---|---|---|---|---|
| main | 100 | 81 | 73 | 15 / 16 / 28 | 116 | 602 | 2,507 |
| porter stemming, one count per stem | 106 | 79 | 69 | 16 / 15 / 28 | 123 (+11, -4) | 626 | 2,518 |

The index stems with Porter, and a query term counts once per stem, so "amend" and "amended" are one match for the two-match gate. 150 prompt blocks change. Where the truth fact entered or left the context, the verdict rose 9 times and fell 3; where it did not, 12 rose and 9 fell, which is the judge's noise. "Amended" now finds the facts about `cr` re-runs after an amend, and "rotate" finds "rotation". The Go stemmer gives SQLite's stem on all 236,064 words of `/usr/share/dict/words`. Porter also stems a half-typed prefix ("deploy"* becomes "deploi"*), so the app's search-as-you-type reads an unstemmed copy of the index (the index grows from 12.8 to 14.6 MB here). The relationship route matches words as written and gets every spelling. The hook's warm time per prompt went from 181 to 191 ms at the median.

Measured on the same ladder and not shipped:

- **A ruling line** when the three facts hold none (score 10 or more): 105 yes against 106. It adds a line on 19 prompts, mostly rulings on the task's subject, but only 2 are the truth target.
- **Rare-term anchors** (any term in at most 50 summaries anchors the coherence gate): +1 yes, 3 fewer no, inside the noise, and about a third of its 22 new lines are off topic.
- **No repo mention of three letters or fewer**: 3 contexts change, and the real short repo names `pi` and `lpt` stop counting.
- **Body text on thick lines**: 105 yes against 107, and a redaction pass of about 350 ms on every prompt with a block.

Rulings stay at 15 to 16 of 59. Their main loss is vocabulary: the ruling is stored in general words and the situation names the task. On the same corrected bench NeptuneMemory delivers 75 (rulings 26), and with its episodes 69 (rulings 22).

## Dreamer models and prompts after v5 (2026-10-19)

The 89 windows of the dreamer comparison above, prompt v5 unless named, the same Sonnet 5.5 judge on "do the extracted facts state the truth fact". GPT models were called on Amazon Bedrock's OpenAI-compatible endpoint with reasoning effort high.

| dreamer | truth fact written down | rulings | facts per window | median seconds per window |
|---|---|---|---|---|
| Opus 5.5, v5 (two runs) | 74%, 76% | 42, 46 / 53 | 21 | 50 |
| GPT-6 Astra | 48% | 28 / 53 | 31 | 143 |
| GPT-6.1 Sol | 45% | 27 / 53 | 31 | 112 |
| Opus 5.5, v6 | 62% | 33 / 53 | 19 | 49 |

Opus stays the dreamer. Both GPT models write more facts and miss more of the ones that matter.

**One-off instructions kept as rulings.** Of the 283 facts curation kept as the owner's on the full v5 re-dream, a Sonnet 5.5 judge asked "a rule for future sessions, or an instruction for the task in progress turned into a rule" called 91 standing and 190 one-off ("set codex cli to run on bedrock", "lets create it as draft"). The quotes are the owner's, so the ruling check passes them. No code rule separates them: "no rule word in the quote" caught 11 of 22 one-offs on a development half and also 60 of 120 standing rulings. Prompt v6 told the dreamer what a ruling is and that an instruction for the task in progress is not one, however firmly said. On the 89 windows (full outputs, the same judge), owner facts went from 3.8 to 2.4 per window: one-off ones 156 to 61, standing ones 166 to 139. But truth rulings fell from 46 to 33 of 53. A missed ruling costs more than a stored one-off, which seldom matches a later prompt, so v5 stays.

**Situation-first ruling descriptions.** The truth rulings that are stored but not delivered mostly fail on vocabulary: the ruling is in general words and the situation names the task. One more curator rule ("a ruling's description starts with the situation it was said in, in the note's own words") was tested on two copies of the rebuilt store, both re-curating the 374 dreamed owner notes with Opus 5.5 (244 descriptions start with "When" against 83). Offline delivery on the 59 ruling situations: 18 yes against 16, inside the noise. The curator sees only the note, and the note seldom names the situation; it is not shipped.

## A rule slot and rare anchors in live sessions (2026-10-19)

Offline, both changes were inside the noise ("Prompt delivery: a stemmed index"). The gap analysis behind them had predicted the live tasks they fix: a ruling outranked by three facts, and a coherence gate vetoed by one incidental rare word. Measured live on the second owner's machine: 24 tasks from their history, 3 runs each, Opus 5.5 in a fresh `claude -p` per cell, with only the cell's own settings. The store and judge are the earlier sections'; the scoring is yes 1, partial 0.5. The arms are release v0.3.0, and v0.3.0 plus the two changes.

| arm | judged | decision tasks | read tasks | tool calls | input tokens | seconds |
|---|---|---|---|---|---|---|
| v0.3.0 | 0.83 | 0.76 | 0.92 | 1.2 | 46k | 19 |
| + rule slot, rare anchors | 0.90 | 0.91 | 0.88 | 1.0 | 43k | 16 |

Per task, the changes scored higher on 5, lower on 3, and tied on 16. A sign test on 8 tasks cannot tell that apart from chance (p = 0.7), but the gains are the predicted ones. A secret-scan ruling and a parked-model decision each rose by two of three runs, and the cr-amend case and two more by one. Each of the three losses is one run of three. In the same harness NeptuneMemory with its episodes scores 0.76, after rerunning 18 of its cells that had received the owner's own hooks (with cwd = home, Claude Code read the home settings as project settings). Shipped on the live result, with the offline null recorded above.

