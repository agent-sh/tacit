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
to about 91-92 ms on the one machine measured, within the existing query
budget there.

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

Over all logged pushes of the 14 days whose checkout still exists, the shipped rule prints fewer lines than release 0.9.14. The prompt block is unchanged (its metadata line names commands by the prompt's words). The target set before measuring, noise at or below 25% with at least six in seven of the useful pushes kept, is not met: only the strictest arm (the second word in at most 50 facts' summaries) gets under the noise bound, on the 3 days only, and it does so by dropping many useful pushes, whose second word is a common one. What is left of the noise is mostly two-word commands that are still generic (`git diff`, `gh pr`) and second words that are incidental.

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

## Scoring each delivery (2026-11-04)

Issue #129: learn, from what happens after each delivery, which facts help for which moments, as web search learns from clicks, and let it rank later deliveries. tacit pushes facts rather than answering a click, so the work is credit assignment: which outcome to charge to which delivered line. The steps, each its own change and each shipped only on a measurement: the log; one-line titles the agent opens on demand, so the opening is the click; cheap outcome signals, validated against the graded cases; random hold-outs that measure each fact's effect; offline estimates of a new ranker from the logged propensities; and last, a learned ranker that adds a bounded boost or penalty over BM25 and the gates.

**1. The delivery log** (SPEC.md, "Delivery log"). Each moment a hook chose what to deliver is a line of a private log on the machine: the request's terms (never its text, and only the terms the store's own index holds), the transcript's path and size, the policy, every candidate with the features it was chosen on (score, rank, share of the best, matched terms, tier, scope, provenance, age, review, repository tie, echo share), what chose it or the gate that left it out, the probability the policy had of showing it, and whether the block showed nothing. Each step an agent takes through the store (a tree folder, a search, a fact read whole) is an entry of its own.

Prior art it follows:

- Learning-to-rank systems log the features at query time, next to what was shown, and join the outcomes to them later, so a model trains on the values the ranker saw, not on values recomputed after the corpus moved (the OpenSearch and Elasticsearch LTR plugins' feature logging; the LETOR and MSLR feature sets are the same kind of per-candidate query-document features: BM25, term coverage, field matches).
- Feedback from use is biased by what was shown: a line never printed can never be used, and a line printed first is read more (Joachims, Swaminathan and Schnabel, "Unbiased Learning-to-Rank with Biased Feedback", WSDM 2017; Chuklin, Markov and de Rijke, "Click Models for Web Search", 2015). Logging the candidates the gates dropped, with their rank, is what lets a later model see the counterfactual.
- Off-policy estimates of a new policy on logged data (inverse propensity scoring, doubly robust) need the probability the logging policy had of each action, the "show nothing" action included (Li, Chu, Langford and Wang, "Unbiased Offline Evaluation of Contextual-bandit-based News Article Recommendation Algorithms", WSDM 2011; Dudik, Langford and Li, "Doubly Robust Policy Evaluation and Learning", ICML 2011). Today's policy is deterministic, so every propensity is 1 or 0 and such an estimate can only cover what the policy already does; the field is there so that an exploring policy's moments are usable from its first day.
- A step through the tree is the store's clickstream: what the agent opened, how deep it went, whether it read a fact whole or saw it listed and left (the dwell and bounce of click models).

A live bench in a repository that ties many facts to itself filled a day's file within the hour: each start entry held every one of them. The start entry now holds its pool's head, and no entry more than 100 candidates. Cost, on a loaded machine: writing a block's line of 50 candidates takes about 45 to 65 microseconds (`BenchmarkDeliveryLogWrite`, 5 runs), and looking a request's 24 terms up in an index of a few thousand facts about 0.1 to 0.3 ms; both run after the answer is printed.

**2. Titles and open-on-demand** (SPEC.md, "Titles"). With `delivery.titles` on, a hook prints an agent's note as a title: its name, whose it is and its type, with no description. The agent opens the facts it needs with `tacit_fact`, and that opening is the click the later steps learn from: an open after a title says the title looked worth it, and what follows the open says whether it was. The owner's rules, rulings, statements, notes holding their words and notes they confirmed stay whole. The log marks each printed line's form (`title` or `full`).

Prior art: progressive disclosure in agent context, where only a name and a one-line description stay loaded and the body is read on demand (Agent Skills; Anthropic, "Effective context engineering for AI agents", 2025: lightweight identifiers, loaded just in time); in click models, the snippet decides whether a result is examined and clicked, and the click with what follows it is the feedback (Chuklin, Markov and de Rijke, 2015).

Measured before it can be the default, with the decision rule set before any run: the live A/B (`ab/run.py`, arms `hooks:full` and `hooks:titles+TACIT_DELIVERY_TITLES=on`, on a build where titles were off by default; with titles the default, the full arm is `hooks:full+TACIT_DELIVERY_TITLES=off`: the same build's shipped hooks and MCP server in place of the user's settings, each arm with its own state directory), on the private read-mostly and decision tasks, three reps, interleaved. Titles becomes the default only if (a) the titles arm passes at least as many cells as the full arm, (b) no task passes in two or more fewer reps under titles, and (c) its input tokens (the sum over tasks of each task's median over reps) are at most 10% above the full arm's. Reported with it: facts opened. Otherwise titles stays off and its code is removed.

Result (three reps of every task, both arms interleaved; the numbers stay with the owner). A first run measured an earlier head, and the head that ships was measured again with isolated arms (`hooks:full+TACIT_DELIVERY_TITLES=off` against `hooks:titles`, each with its own state directory and an allowlisted environment). In both runs all three conditions held: the titles arm passed at least as many cells, no task lost two or more reps, and its input tokens stayed within the bound. Both runs cost less with titles and made more tool calls, nearly all of the extra calls opening facts, which agents with whole lines rarely did; in the second run the delivery log's opens matched the agents' own calls. By the rule, titles are the default, and only where they were measured: the start block, the prompt block and the push before a shell command, in a harness with tacit's MCP server. The start again after a compaction, the summary's selection and a resume's block keep whole lines (a headless run never compacts or resumes), and so does the push before an edit (the tasks made no edit); #139 covers an A/B that forces those moments. The first run also found the log's start-entry bug fixed in #136.

**3. Cheap outcome signals, validated before they move a score.** What happens after a delivery says something about it, without a model: whether a compaction's summary kept the line, whether the agent needed it again after one dropped it, whether the agent carried it forward unprompted, whether the owner restated it, whether the agent opened it. `delivery/signals.py` reads each graded line's session transcript after its moment and computes each signal, then measures its agreement with the hand grades.

Prior art: in click models, what follows the click (dwell, a return to the results, the last click) is the evidence of satisfaction, not the click alone (Chuklin, Markov and de Rijke, 2015; the DBN model, Chapelle and Zhang, WWW 2009); a memory's utility learned from the outcomes that follow its use, not from its retrieval (MemRL, arXiv 2601.03192).

Decision rule, as first written before the first run: a positive signal (summary kept, needed again, carried forward, opened) may raise a fact's score only if, on the graded lines where it fires, the share graded useful is at least twice the base rate of useful lines and the lower end of its 95% interval (Wilson, as first written; amended below) is above the base rate. A negative signal (summary dropped, since removed: below) may lower a score only if the share graded noise where it fires clears the same two bars against the base rate of noise. The owner's restatement is no score: it flags a fact for the curator (relevant but ineffective). Each signal's agreement is reported (the numbers stay with the owner); a signal that fails moves nothing. Under the rule as first written, the first run passed three of the positive signals. Its review found defects that made those passes unsound: a subagent's deliveries were scored against the parent's transcript, rulings were held by their words where the hooks hold them by name only, the window ran to the end of the session, and per-line intervals treated the clustered lines of one session as independent. The amended rule replaces it (its session minimum came after the second run and changed no verdict): the interval is a bootstrap over sessions; a signal is judged only when it fires on at least 10 distinct (session, fact) units in at least 5 sessions; a line's window ends at the next block of the session that delivers the same fact again (the transcript's own hook output) or 24 hours after it; a ruling is held by its name only; a subagent's delivery is left out and counted. Limits: in these transcripts no window ended early, since no graded fact was delivered again in a recorded block and most of the Claude Code versions behind them record no SessionStart block; so every window ran its full 24 hours, and what follows a compaction may answer a re-delivery the transcript does not show (the report counts such windows). A delivery that several label sets grade is one row, and one graded two ways is left out. The restatement detector reads only the owner's turns that made a case, a small share of their turns without silent cases; and when noise is half the graded lines or more, twice its base rate is out of reach, so the negative bar can hardly be met: the next rule should set a lift that can be, and judge a summary's drop against the lines that had a compaction in their window rather than all lines.

Result, on the graded prompt and push lines of real sessions whose transcripts are still on the machine, the owner's prompts and programs' (headless, scheduled) alike (the numbers stay with the owner): no signal is validated. The positive signals (a summary keeping the line, the agent opening it, needing it again, carrying it forward) each fire on fewer units and sessions than the amended rule needs, so they are unmeasured on these sessions, not failed. A negative: a summary dropping the line, meant as a sign the line did not matter, fired on nearly every line that had a compaction in its window, since summaries rarely hold a delivered fact; the share graded noise there was about the base rate. It measured that a compaction happened, not the line, so it is removed from the harness (a drop is still the start of the window for "needed again"). The restatement detector never fired, so this says nothing yet about restatements. So none of them moves a score yet. They are not negatives: they lack data, and `signals.py` is what reruns them. Rerun when the graded sessions hold, for a signal, at least 10 distinct (session, fact) units in at least 5 sessions where it fires (the amended rule's minimum); the harness reports both counts per signal. Since titles (step 2), agents open facts far more often, so the opening signal will have more to measure; and the hold-outs (step 4) measure a fact's effect without grades. 

**Self-marks** (SPEC.md, "Self-marks"). On about 1 in 20 prompt blocks that print an agent's note, the block asks the agent to say, when the request is done, which of its lines helped, with `tacit_mark`. The ask never blocks, its probability is logged with the block, and the marks are log entries of their own. A self-mark is explicit and cheap, but it is the agent's report of itself: in search, what a user says about a result and what the user's behaviour shows often differ (Joachims, Granka, Pan, Hembrooke and Gay, "Accurately Interpreting Clickthrough Data as Implicit Feedback", SIGIR 2005), and a model's report of its own reasoning is no direct reading of it. So marks move no score until they are calibrated: against the hand grades where a marked block was graded, against the dreamer's judgment, and against the hold-outs' measured effects. Calibration waits for marks to accumulate; the rate is set so a working machine gathers them within weeks.

**4. Hold-outs** (SPEC.md, "Hold-outs"). On about 1 in 20 prompt blocks that print an agent's note, one of those notes, drawn uniformly, is withheld and denied for the rest of the session, and the log records the share, the withheld fact and each line's propensity. A fact's arm in a session is decided at the first prompt block it reaches, so that block's propensity is the session's. The owner's own lines are never withheld. What follows in sessions that got a fact of a kind and in sessions denied one, read from momentum's failure events, measures the kind's effect without grades, and later validates the cheap signals and the self-marks.

Prior art: randomized hold-outs as the basis of an unbiased estimate when the logging policy is otherwise deterministic, with each action's logged propensity (Li, Chu, Langford and Wang, WSDM 2011; the explore share of contextual bandits); the value of a memory as the effect of showing it over not (MemRL, arXiv 2601.03192).

The share is 0.05: one line in about 1 in 20 eligible blocks, a small cost to any one session. Preregistered, before any data:
- **Kinds.** Rule, decision, past mistake, machine fact: an agent's note of type `user_standard`, `design_decision`, `past_mistake` or `machine_fact`. Only these are ever withheld (never the owner's lines, never a fact computed from git, never another type). In a store the rulings audit has not run on, every `user_standard` reads as a ruling, so the rule kind is empty there and is reported empty.
- **Unit, class and window.** The session, per kind. A session's class for kind K is decided at its first prompt block whose entry holds an eligible line of kind K (a candidate of kind K with `p < 1`): "denied" for K when that block withheld a line of kind K (`held_out`), "got" otherwise. Only sessions with such a block are in K's analysis; a session whose first block for K came after the fact was given elsewhere, or that never had one, is not. Later draws in the session do not change its class (they can only move a "got" session toward denied, which biases the difference toward zero). Sessions whose withheld fact the session's `.given` also holds (a push at the same moment gave it) are dropped and counted. The outcome window is the whole session.
- **Propensity.** The class's probability at that block, from its logged entry: `q·k_K/k` for denied and `1 − q·k_K/k` for got, with `q` the block's `holdout_q`, `k` its eligible lines and `k_K` those of kind K.
- **Outcome.** Momentum's failure events of the session, read-only from its store through `momentum evidence events --session <id> --source mistakes --json`: events of kind `tool-error` with actor `tool` (a classifier's refusal is logged as a tool error with actor `harness` and is not counted), `turn-error`, `refusal`, and the owner's `correction` and `interrupt` (actor `owner`); heuristic rows (`Heuristic`) are counted apart and not in the outcome. The count is raw events per session.
- **When to read.** Only once K holds at least 30 denied sessions.
- **The read.** For each kind, the difference in mean failure events per session between denied and got sessions, each session weighted by the inverse of its class's propensity, with a two-sided 95% interval from a bootstrap over sessions. No per-fact claim. It reports the interval; no decision follows from it until the interval excludes zero.

**5. The dreamer's judge** (SPEC.md, "The dreamer's judge"). After a full dream run, a model reads a sample of the lines the prompt and push hooks delivered with hindsight (the request, what the session did after) and labels each relevant, confused and novel. It runs on the owner's subscription, never Bedrock, about 36 lines a day, and logs each label with the probability its line had of being drawn (each line has one draw), and each pass's draw. Prior art: a language model as a relevance assessor in place of human judges (Thomas, Spielman, Craswell and Mitra, "Large Language Models can Accurately Predict Searcher Preferences", SIGIR 2024; Faggioli et al., "Perspectives on Large Language Models for Relevance Judgment", ICTIR 2023), which also found that a model's labels must be checked against human ones before use, and that their agreement varies with the prompt and the collection; and judging with hindsight, from what followed (the click models' satisfied click, Chuklin, Markov and de Rijke, "Click Models for Web Search", 2015).

`delivery/judge_agreement.py` finds each hand-graded line's moment in its session's transcript and has `tacit delivery-judge` build the case as the dream pass does (with the line as delivered, at the case's commit), judge it and measure the agreement. The graded lines are prompt and push lines, so a pass covers the judge's labels of those; it judges no start lines. The rule, written before the first run:
- **Labels compared.** `relevant` against on the topic (useful R or weak W) versus noise (N); `useful`, relevant and novel both yes, against useful (R) versus not (W or N). `confused` has no grade to compare with and is reported only.
- **Measure.** Cohen's kappa over the lines the judge decided, with a 95% interval by a bootstrap over sessions. Unsure answers are left out and counted.
- **To pass.** At least 200 decided lines in at least 30 sessions; kappa at least 0.41 (the moderate band, which the blind second rater reached against the first on these grades: a judge that passes agrees with the grades about as well as a second person does); the interval's lower end at least 0.21 (above slight agreement even at the low end); at most 1 in 5 answers unsure.
- **One run, one judge.** A judge is its model (as the CLI reports it) and its prompt (the digest each label carries), both fixed before the run; the graded lines play no part in writing them, so the run is held out. The harness refuses verdicts from more than one judge. A changed prompt or model is a new judge, measured again from the start, and only labels of a judge that passed are used.
- **What a pass allows.** A label that passes may be used as a training label for the ranker, with its logged probability; a label that fails moves nothing, and the result is reported as a negative. The judge keeps running either way: its labels are kept for a better rule, and for calibrating the self-marks.

First run, under that rule, on the graded prompt and push lines (the judge `claude-opus-5-5` with prompt digest `349414a790aa`): both `relevant` and `useful` pass every bar (lines, sessions, kappa, the interval's low end, unsure share); the numbers stay with the owner. So that judge's `relevant` and `useful` labels may train the ranker, each with its logged `p`; `confused` stays unmeasured. A changed prompt or model is measured again before its labels are used.

