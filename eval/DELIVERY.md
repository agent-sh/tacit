# Prompt-delivery precision check

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
