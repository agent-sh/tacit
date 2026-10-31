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
Private prompts, fact text, labels, corpus, raw outputs and the counts measured
on them stay off the public repository. `delivery/hook_replay.py` supplies the
generic replay procedure.

Each prompt runs through the actual binary three times with an empty session
ID and isolated index/overlay, so delivery dedupe cannot suppress repetitions
and live sessions are untouched. Outputs were deterministic. Latencies below
are medians of per-cell medians after index warmup, on a shared loaded machine.
No model or GPU was used. This replaces the previously suggested dreamer
re-evaluation: extraction recall cannot validate prompt-delivery thresholds.

## Results

Both groups gave the same picture:

| Arm | Delivered lines | Labelled useful | Unexpected | Positive queries hit | No-memory queries injecting |
|---|---|---|---|---|---|
| Installed baseline | most | most | most | all | many |
| Summary coherence and repo gates, floor 15 | fewer | slightly fewer | fewer | most | few |
| Same gates, repo-less floor 20 | fewest | fewer | fewest | fewer | none |

The selected policy favors precision for unsolicited memory in repo-less
sessions. It loses useful recall: the baseline hits every positive request, and
the selected policy misses some. This is a cost, not a free improvement.
Explicit search remains available and retains its previous broad retrieval
behavior. Known-project requests keep floor 15; computed facts keep floor 12.

A sweep of repo-less floors 15 through 20 used the real tokenizer and boosted
query scores. Higher floors progressively suppressed more weak matches; 20
was the first tested floor with no injections on either no-memory group.
The chosen floor was then run through the actual candidate binary, not just a
score simulation. Floors are specific to this corpus and should be revisited
with fresh labelled traffic. Warm-hook median latency rose from about 16 ms
to about 91-92 ms, within the existing query budget.

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

Rule: when code can decide, code decides, and a scorer (the opt-in Jev selector or any model) judges only the rest. Four deterministic rules were tried as a pre-filter on the same frozen candidate pools as [JEV.md](JEV.md) (one frozen store snapshot, 6 curated + 1 derived candidates, original prompt). They read only the prompt and candidate metadata, never labels. Labels: authored primary labels plus the blinded review on dev and calibration. The rules were written after looking at dev/calibration errors, so the sealed test set was opened once, with every threshold frozen beforehand.

| Rule | Dev + calibration drops | Sealed test drops | Shipped |
|---|---|---|---|
| R1: a pure text operation on a literal the prompt supplies needs no memory | wrong facts only, no useful one | none (it did not fire) | yes |
| R2: drop a fact whose only link is a topic the user excluded ("not X", "do not Y") | none (never fired) | none | no: no measured gain |
| R3: derived hot-spot facts only when the prompt names a file or asks to change code | wrong facts only, no useful one | a wrong fact and **a useful one** | no |
| R4: a repo-anchored prompt keeps only that repo's facts plus user/system standards | wrong or unlabelled facts only, no useful one | wrong or unlabelled facts and **useful ones** | no |

On the sealed test (primary labels, frozen calibration thresholds), Jev kept as many useful facts as today's selection and no wrong one; behind R1-R4, Jev and Amazon Nova Micro on Bedrock both kept fewer useful facts than without them. R3 and R4 removed useful facts on unseen prompts, so they are not shipped. R1 ships as part of the harness-turn check: a prompt with the word "literal" and an operation (upper/lowercase form, reverse, spell, length, count characters/letters/words) gets no facts and no scorer call. Either word alone does not trigger it (`TestPromptHookSkipsSelfContainedTextOperations`).

## Real traffic, graded (2026-10-30)

The precision checks above use authored prompts. This one grades what the hooks printed in the author's own sessions and replays it with `delivery/labelled_replay.py` (method and fidelity: [LABELLED-REPLAY.md](delivery/LABELLED-REPLAY.md)). A line is useful (R) when it bears on the request, weak (W) when it is on the topic but not needed, noise (N) otherwise; one rater, with a blind second rater on a sample of the lines (moderate agreement). Release 0.9.14 was replayed at each call's own store commit, over owner prompts and tool-call pushes in two windows, one of 3 days and one of 14 days. In every set a minority of the printed lines were useful and noise was the largest share.

Recall: fewer than half of the owner prompts of the 3 days that a fact in the store answered got one. Those prompts are the recall probe the sections below use.

Where the prompt block's noise comes from, on the 3 days (an analysis build that logs why each line got in, not shipped): most lines are ranked lines, much of them noise; the action line is mostly noise; the metadata line is all noise; the rule slot prints few lines. Ranked lines whose only coherence was the anchor (no two name matches, no phrase) are mostly noise: the anchor is the request's least frequent known word, and in a request of common words that is a word like "out", which many facts hold.

## Pushes keyed on a bare program (2026-10-30)

A fact's `applies_to.commands` entry of one word (`ssh`, `python3`, `git`, `sed`) matched every call of that program. On the graded pushes of the 3 days, those lines were rarely useful and mostly noise, while pushes keyed on longer commands, and most of all on edits by path, were far more often useful. Separately, an area written with the tools pattern `Bash` pushed at the first shell command of every session, and none of those lines was useful.

The rule now: a command of one word matches only when a word of the fact's name or description, other than the command's own, is in the command line too, not counting the words of the session's repository names, folder and home (every fact of the repository shares them); and an area's tools patterns match MCP tools only. Measured with `delivery/labelled_replay.py` at each call's commit, every line either arm printed graded (14-day cases whose checkout is gone left out), against release 0.9.14 over both windows:

| arm | result |
|---|---|
| areas match MCP tools only | fewer lines and less noise, every useful line kept |
| one-word command needs a second word | fewer lines and less noise, nearly every useful line kept |
| **both, the session's repository and folder words not counted (shipped)** | **the least noise of the arms that keep nearly every useful line** |
| every command needs a second word, areas MCP only | fewer lines, useful lines lost |
| one-word commands, the second word in at most 50 facts' summaries, areas MCP only | the least noise, many useful lines lost |

Over all logged pushes of the 14 days whose checkout still exists, the shipped rule prints fewer lines than release 0.9.14. The prompt block is unchanged (its metadata line names commands by the prompt's words). The target set before measuring, noise at or below 25% with nearly all useful pushes kept, is not met: the stricter rules reach it only by dropping useful pushes, whose second word is a common one. What is left of the noise is mostly two-word commands that are still generic (`git diff`, `gh pr`) and second words that are incidental.

After the review (same cases, every changed line graded by hand): the session's repository names, folders and home are left out word by word however many there are (`search.Terms` stopped near 24 terms, so in a workspace of many repositories the folder words were not left out), and a one-word command that is a script's path or file name (`scripts/release.sh`, `deploy.sh`) needs no second word. Against the rule above, over the 3-day, 14-day and all-pushes cases, a few lines were added (some of them useful) and a few removed (none useful), and the shipped arm's results barely moved. The fact's own name and description are read whole too (no line changes on these cases). Counting every word of the command line as well (a heredoc's prose, a script's text) added mostly noise lines: the command's words stay its first terms.

## Echoes of the request (2026-10-30)

A scheduled job's prompt (a daily review, a maintenance job) got its own instructions back: the dreamer had filed them as facts from earlier runs, and every run's prompt matched them word for word. Nearly every line the graded scheduled-review blocks of the 14 days got was graded weak because it restated the prompt.

The rule: after the selection, a line whose name and description words the prompt holds for at least 7 in 10 of them is left out, and nothing takes its place. Measured with `delivery/labelled_replay.py` at each prompt's commit:

| share of the line's words in the prompt | scheduled review | headless prompts | other program prompts | owner prompts, 3 days and 14 days |
|---|---|---|---|---|
| off (release 0.9.14) | every echo printed | baseline | baseline | baseline |
| 0.8 | every echo still printed | a few lines fewer | unchanged | unchanged |
| **0.7 (shipped)** | **most echoes gone, the useful line kept** | **a few lines fewer** | **unchanged** | **unchanged** |
| 0.6 | nearly every echo gone, the useful line lost | fewer | a heartbeat prompt lost its useful line | unchanged |
| 0.5 | nearly every echo gone, the useful line lost | fewer still | a heartbeat prompt's and a side note's useful lines lost | unchanged |

At 0.6 the useful lines lost were facts that share the request's words and add to them: a log's row schema the review needed, a script's location. The ungraded lines 0.7 removes say back the first sentence of a maintenance job's prompt. No owner prompt line changes at any share from 0.5 to 0.8, and the recall probe does not move. Filtering the candidates before the selection instead let others take the echoes' places (new lines in the scheduled blocks), so the filter runs after it.

After the review, three guards: a line needs 5 distinct words or more (a short rule answers a request in the request's own words: "never force push to main" for "can I force push to main?"), a line that says no (`never`, `not`, `don't`, ...) where the request does not is kept, and the request is read whole (`search.Terms` stopped near 24 terms, so a long scheduled prompt's later words were not seen). On the same cases this leaves out a few more lines, all restatements; no useful line is lost, owner prompts lose only a noise line, and the probe does not move. The filter also covers every line the hook prints (the rule slot, the derived fact, the metadata and action lines), as it did; tests now pin its place after the selection and the share.

Not done here: the dreamer side, a fact made only from a program-written prompt. It needs dreamer runs to measure; the delivery rule removes the echo where it shows.

## Short requests (2026-10-30)

A request of fewer than 3 distinct words of 3 letters or more ("deploy now?", "fix the cache") names too little to pick a rule or an action for. It now gets neither the rule slot nor the action line; its ranked and metadata lines stay. Measured with `delivery/labelled_replay.py` on the graded prompt cases (cases whose checkout is gone left out), against #72's head: in each set (owner prompts of the 3 days, owner prompts of the 14 days, every prompt block of the 14 days) it prints a few lines fewer, mostly noise, and loses no useful line.

Over the prompt cases of every origin (gone checkouts left out), only short requests change: noise lines fall (the 95% bootstrap interval over cases is below zero), useful lines do not change, and a weak line goes. The recall probe does not move.

## Tried and not shipped (2026-10-30)

These changes were measured against release 0.9.14 and not shipped; their code is not in the repository. Prompt anchors and recall were measured with `delivery/labelled_replay.py` on the graded prompt cases (owner prompts only; for the 14 days, cases whose checkout is gone left out). The RULE line gate and the "used" detector were measured with scripts of their own, on the labelled lines. Noise is the share of graded lines graded noise; a change can print lines that were never graded, and those are noted where they matter.

**Prompt anchors.** Target, set before measuring: noise at or below 25%, at least 90% of the useful lines the release prints kept. Three changes, alone and together, against release 0.9.14 over both windows:

| arm | useful lines | noise share | recall probe |
|---|---|---|---|
| a word in more than 1% of the owner's own turns is no anchor, no name match, no phrase | many lost | barely moved | lower |
| the same at 0.5% | more lost | unchanged | lower |
| a machine's fact needs a host, tool or path word matched | many lost | slightly lower | lower |
| a request of fewer than 3 distinct words gets no rule slot and no action line | none lost | slightly lower | unchanged |

The last one shipped as its own change ("Short requests", above): it removes noise and no useful line. The owner's-words arms lose useful lines because a common word of the owner's also stopped counting as a name match: nearly all the useful lines they lost on the 3 days had come in on two name matches. From the lines whose only coherence was the anchor, they removed almost only noise.

So a fourth arm keeps the owner's common words as name matches and phrases and only stops them anchoring: no anchor on a word in more than 0.2% of the owner's turns. Every line it adds was graded by hand. Over the two windows it prints fewer lines with a little less noise, and loses almost no useful line. It is not shipped: the gain is small next to one rater's noise, and the hook would need a table of the owner's word counts, built on each machine from their turns and kept current. The machine-match arm loses many useful lines.

**Recall.** No target was set. The question was whether more of the probe's owner prompts that a stored fact answers get it. Adding the end of the agent's previous message to the query lowered recall in every form tried: always (its last 1,200 characters, which also dropped most of the useful lines), always with its last 400 characters, only after a request that points back ("it", "that", "this") or a short one, and only for a short request. Matching repository names fuzzily against the catalog changed no line. The previous message brings its own subject into the query.

**RULE lines.** Target, set before measuring: on the graded RULE lines, at least two thirds of the one-off instructions demoted (a question, a hedge, a time-bound order, status narration, a dated approval), and under 4% of the standing rules lost. The gate tried is a set of word and phrase patterns for those shapes. It fell short of the demotion target, while staying within the limit on standing rules. The patterns were written from these same lines, so the result is in-sample, and it held unevenly on the script's two halves. Most one-offs left are plain imperatives that read like standing rules.

**Relevance learned from use.** The plan was to stop unless a no-model detector of use agreed with the graded labels. The detector: a delivered fact was used when the agent opened it, or when one of its distinctive tokens (a path, a command, a flag, an identifier) shows up in the agent's next tool calls and was not in the context before. The sessions were read through tacit's own event reader (`client/pkg/evidence`). Over the graded prompt and tool-call lines, at the default settings it calls few lines used and finds few of the useful ones, and its agreement with the labels is near chance (Cohen's kappa near zero). Over 54 settings (how many calls after the delivery, owner turns crossed, the agent's own text, words rare in the store's descriptions), it stays there. For most useful lines the token test has nothing to find: the fact changed what the agent decided, not a token it typed. So no learner was replayed and no idle step was built.

**Owner loop: closing old questions.** Not built. SPEC gives closing an agent's question to the owner. Most of the questions agents asked in the review files were still open, some of them older than a week.
