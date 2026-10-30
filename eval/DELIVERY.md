# Prompt-delivery precision check

This records the summary/coherence policy at `0732543`. The subsequent
[weighted-relationship experiment](RELATIONSHIPS.md) retains those floors and
adds an evidence-backed route, with separate replay results and limitations.

## Scope and method

This is a diagnostic on one private operator store, not a general benchmark.
The installed baseline is commit `3601b08`. The corpus was pinned for all arms.
The case file has 60 hand-authored prompts: two groups of 30, each with 15
positive requests and 15 requests labelled as needing no stored fact.
The second group contains paraphrases, not independent production traffic.
Both groups were inspected during development. Do not call either held out.

The expected sets were augmented with equivalent useful baseline facts before
candidate replay. They were not expanded to include candidate-only results;
therefore `unexpected` is a diagnostic label, not proof every such line is
irrelevant. Labels concern contextual usefulness, not factual freshness.
Private prompts, fact text, labels, corpus and raw outputs stay off the public
repository. `delivery/hook_replay.py` supplies the generic replay procedure.

Each prompt runs through the actual binary three times with an empty session
ID and isolated index/overlay, so delivery dedupe cannot suppress repetitions
and live sessions are untouched. Outputs were deterministic. Latencies below
are medians of per-cell medians after index warmup, on a shared loaded machine.
No model or GPU was used. This replaces the previously suggested dreamer
re-evaluation: extraction recall cannot validate prompt-delivery thresholds.

## Results

| Arm | Group | Delivered lines | Labelled useful | Unexpected | Positive queries hit / 15 | No-memory queries injecting / 15 |
|---|---|---:|---:|---:|---:|---:|
| Installed baseline | A | 53 | 24 | 29 | 15 | 7 |
| Installed baseline | B | 60 | 24 | 36 | 15 | 9 |
| Summary coherence and repo gates, floor 15 | A | 36 | 21 | 15 | 13 | 2 |
| Summary coherence and repo gates, floor 15 | B | 41 | 20 | 21 | 13 | 3 |
| Same gates, repo-less floor 20 | A | 24 | 16 | 8 | 10 | 0 |
| Same gates, repo-less floor 20 | B | 27 | 17 | 10 | 12 | 0 |

The selected policy favors precision for unsolicited memory in repo-less
sessions. It loses useful recall: 22/30 positive requests hit, versus 30/30 in
the baseline. This is a cost, not a free improvement. Explicit search remains
available and retains its previous broad retrieval behavior. Known-project
requests keep floor 15; computed facts keep floor 12.

A sweep of repo-less floors 15 through 20 used the real tokenizer and boosted
query scores. Higher floors progressively suppressed more weak matches; 20
was the first tested floor with no injections on either no-memory group.
The chosen floor was then run through the actual candidate binary, not just a
score simulation. Floors are specific to this corpus and should be revisited
with fresh labelled traffic. Warm-hook median latency rose from about 16 ms
to about 91-92 ms, within the existing query budget on this machine.

## What changed and what remains

- Shared-body vocabulary no longer establishes relevance: coverage is over
  names and descriptions only, using FTS tokenization.
- Two matching summary terms also need coherence: the least-frequent known
  query term, two name terms, or a query-term phrase. Unknown words cannot
  veto the query. This is heuristic, not semantic understanding.
- A single underscored or mixed letter/digit identifier can qualify only when
  it appears in at most five indexed documents. Tests cover the boundary;
  this cutoff is a conservative design choice, not an optimized parameter.
- Repo restrictions are checked before candidate limits. An unrelated host
  match does not override them. Verified checkout aliases and explicit repo
  mentions allow targeted requests from a home-directory session.
- Nonempty but incorrectly narrow repo metadata can suppress genuinely
  reusable facts. This check did not rewrite the store to hide that problem.
- Empty or inaccurate metadata, long prompts, multilingual queries and
  conceptually similar but lexically different requests need broader future
  evaluation. The two paraphrase groups do not establish task-level gains.

The regression suite covers body-only matches, incidental summary words,
rare identifiers, cross-project restrictions, unknown-repo delivery, explicit
repo mentions, aliases, candidate starvation, floor boundaries and session
no-backfill behavior. Existing package tests remain part of validation.

## Code before the scorer (2026-10-17)

Rule: when code can decide, code decides, and a scorer (the opt-in Jev selector or any model) judges only the rest. Four deterministic rules were tried as a pre-filter on the same frozen candidate pools as [JEV.md](JEV.md) (one frozen store snapshot, 6 curated + 1 derived candidates, original prompt). They read only the prompt and candidate metadata, never labels. Labels: authored primary labels plus the blinded review on dev (19 queries) and calibration (12). The rules were written after looking at dev/calibration errors, so the sealed test set (20 queries) was opened once, with every threshold frozen beforehand.

| Rule | Dev + calibration drops | Sealed test drops | Shipped |
|---|---|---|---|
| R1: a pure text operation on a literal the prompt supplies needs no memory | 9 wrong, 0 useful (2 prompts) | none (it did not fire) | yes |
| R2: drop a fact whose only link is a topic the user excluded ("not X", "do not Y") | none (never fired) | none | no: no measured gain |
| R3: derived hot-spot facts only when the prompt names a file or asks to change code | 6 wrong, 0 useful | 1 wrong, **1 useful** | no |
| R4: a repo-anchored prompt keeps only that repo's facts plus user/system standards | 11 wrong or unlabelled, 0 useful | 14 wrong or unlabelled, **2 useful** | no |

Sealed test, primary labels, frozen calibration thresholds (useful / wrong / unlabelled picks, of 6 findable useful): today's selection 5 / 2 / 12; Jev 5 / 0 / 1; Jev behind R1-R4 3 / 0 / 3; Amazon Nova Micro on Bedrock 4 / 0 / 1, behind R1-R4 3 / 0 / 5. R3 and R4 removed useful facts on unseen prompts, so they are not shipped. R1 ships as part of the harness-turn check: a prompt with the word "literal" and an operation (upper/lowercase form, reverse, spell, length, count characters/letters/words) gets no facts and no scorer call. Either word alone does not trigger it (`TestPromptHookSkipsSelfContainedTextOperations`).

## Real traffic, graded (2026-10-30)

The precision checks above use authored prompts. This one grades what the hooks printed in the author's own sessions and replays it with `delivery/labelled_replay.py` (method and fidelity: [LABELLED-REPLAY.md](delivery/LABELLED-REPLAY.md)). A line is useful (R) when it bears on the request, weak (W) when it is on the topic but not needed, noise (N) otherwise; one rater, with a blind second rater on 227 lines (kappa 0.54 to 0.63). Release 0.9.14 replayed at each call's own store commit:

| set | calls | lines | useful | weak | noise |
|---|---:|---:|---:|---:|---:|
| owner prompts, 3 days (10-27 to 10-30) | 93 | 238 | 55 (23%) | 62 | 117 (50%, 44-56) |
| tool-call pushes, 3 days | 122 | 124 | 30 (24%) | 39 | 55 (44%, 36-53) |
| owner prompts, 14 days | 68 | 171 | 39 | 36 | 85 (53% of 160 graded) |
| tool-call pushes, 14 days | 65 | 65 | 13 | 18 | 34 (52%) |

Recall: of 22 owner prompts of the 3 days that a fact in the store answered, 10 got one.

Where the prompt block's noise comes from, on the 3 days (an analysis build that logs why each line got in, not shipped): the ranked lines are 201 of the 238 and 45% noise; the action line 28 lines, 2 useful and 20 noise; the metadata line 5, all noise; the rule slot 4, 1 useful. Ranked lines whose only coherence was the anchor (no two name matches, no phrase) are 58 lines, 10 useful and 37 noise: the anchor is the request's least frequent known word, and in a request of common words that is a word like "out" (in 1,392 facts).

## Pushes keyed on a bare program (2026-10-30)

A fact's `applies_to.commands` entry of one word (`ssh`, `python3`, `git`, `sed`) matched every call of that program. On the graded pushes of the 3 days, those lines were 9% useful and 63% noise, against 36% and 26% for longer commands and 62% and 8% for edits by path. Separately, an area written with the tools pattern `Bash` pushed at the first shell command of every session: 7 lines, none useful.

The rule now: a command of one word matches only when a word of the fact's name or description, other than the command's own, is in the command line too, not counting the words of the session's repository names, folder and home (every fact of the repository shares them); and an area's tools patterns match MCP tools only. Measured with `delivery/labelled_replay.py` at each call's commit, every line either arm printed graded (14-day cases whose checkout is gone left out):

| arm | 3 days: lines | useful kept | noise | 14 days: lines | useful kept | noise |
|---|---:|---:|---:|---:|---:|---:|
| release 0.9.14 | 124 | 30 of 30 | 55 (44%) | 65 | 13 of 14 | 34 (52%) |
| areas match MCP tools only | 117 | 30 | 49 (42%) | 64 | 13 | 33 (52%) |
| one-word command needs a second word | 111 | 29 | 44 (40%) | 60 | 13 | 25 (42%) |
| **both, the session's repository and folder words not counted (shipped)** | **102** | **29** | **36 (35%)** | **57** | **13** | **23 (40%)** |
| every command needs a second word, areas MCP only | 91 | 25 | 36 (40%) | 52 | 12 | 20 (38%) |
| one-word commands, the second word in at most 50 facts' summaries, areas MCP only | 67 | 22 | 16 (24%) | 38 | 11 | 14 (38%) |

Over all 536 logged pushes of the 14 days whose checkout still exists, the shipped rule prints 458 lines where release 0.9.14 prints 523 (12% fewer). The prompt block is unchanged (its metadata line names commands by the prompt's words). The target set before measuring, noise at or below 25% with 12 of the 14 useful kept, is not met: the stricter rules reach 24% only by dropping a quarter of the useful pushes, whose second word is a common one. What is left of the noise is mostly two-word commands that are still generic (`git diff`, `gh pr`) and second words that are incidental.

After the review (same cases, every changed line graded by hand, 13 of them new): the session's repository names, folders and home are left out word by word however many there are (`search.Terms` stopped near 24 terms, so in a workspace of many repositories the folder words were not left out), and a one-word command that is a script's path or file name (`scripts/release.sh`, `deploy.sh`) needs no second word. Against the rule above, over the 3-day, 14-day and all-pushes cases: 9 lines added (2 useful, 2 weak, 5 noise), 7 removed (3 weak, 4 noise); in the table, 3 days stays 102 lines, 29 useful, 36 noise, and 14 days becomes 56 lines (one weak line fewer), 13 useful, 23 noise. The fact's own name and description are read whole too (no line changes on these cases). Counting every word of the command line as well (a heredoc's prose, a script's text) added 19 lines (3 useful, 2 weak, 14 noise) and removed 13: the command's words stay its first terms.

## Echoes of the request (2026-10-30)

A scheduled job's prompt (the tool-failure review, the wiki's daily maintenance) got its own instructions back: the dreamer had filed them as facts from earlier runs, and every run's prompt matched them word for word. Of the 34 lines the 7 graded scheduled-review blocks of the 14 days got, 32 were graded weak because they restated the prompt, 1 useful and 1 noise.

The rule: after the selection, a line whose name and description words the prompt holds for at least 7 in 10 of them is left out, and nothing takes its place. Measured with `delivery/labelled_replay.py` at each prompt's commit:

| share of the line's words in the prompt | scheduled review: lines (useful) | headless prompts: lines | other program prompts | owner prompts, 3 days and 14 days |
|---|---:|---:|---|---|
| off (release 0.9.14) | 34 (1) | 24 | | 238 and 171 lines |
| 0.8 | 34 (1) | 22 | unchanged | unchanged |
| **0.7 (shipped)** | **5 (1)** | **21** | **unchanged** | **unchanged** |
| 0.6 | 2 (0) | 19 | a harness heartbeat lost its 1 useful line | unchanged |
| 0.5 | 1 (0) | 16 | the heartbeat's and a side note's useful lines lost | unchanged |

At 0.6 the two useful lines lost were facts that share the request's words and add to them: the failure log's row schema the review needed, a heartbeat script's location. The lines 0.7 removes that had no grade (2, both on the wiki maintainer's prompt) say back its first sentence. No owner prompt line changes at any share from 0.5 to 0.8, and the 22-prompt recall probe stays at 10. Filtering the candidates before the selection instead let others take the echoes' places (28 new lines in the 7 scheduled blocks), so the filter runs after it.

After the review, three guards: a line needs 5 distinct words or more (a short rule answers a request in the request's own words: "never force push to main" for "can I force push to main?"), a line that says no (`never`, `not`, `don't`, ...) where the request does not is kept, and the request is read whole (`search.Terms` stopped near 24 terms, so a long scheduled prompt's later words were not seen). On the same cases this leaves out 6 more lines, all restatements: 4 weak, 1 noise, 1 ungraded; no useful line is lost, owner prompts lose 1 noise line, and the probe stays at 10 of 22. The filter also covers every line the hook prints (the rule slot, the derived fact, the metadata and action lines), as it did; tests now pin its place after the selection and the share.

Not done here: the dreamer side, a fact made only from a program-written prompt. It needs dreamer runs to measure; the delivery rule removes the echo where it shows.
