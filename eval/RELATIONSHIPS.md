# Weighted relationships for prompt retrieval

This adds a second, explainable eligibility route to the existing BM25 prompt
retrieval. It does not lower the BM25 floors or change normal explicit search.
It does not change a fact's authority. Retrieval evidence is not authorization
to run a command or follow instructions from a fact.

## Observations and scoring

Index schema 6 stores distinct summary terms and ordered adjacent term pairs.
The name and description are separate phrase fields. Stop words and one-letter
words remain phrase boundaries, so `add a new` does not become `add new`.
Hyphens and punctuation delimit tokens. No body cooccurrence creates a relation.

The evidence corpus contains active curated facts only. It excludes retired,
superseded, inbox, local-overlay, derived-scope and `result_ref` facts. Postings
share the FTS build transaction and HEAD/overlay cache lifecycle. Queries read
only matching postings and documents, not the whole store's text.

For a known query term `t`, its information weight is
`log((N + 1) / (df(t) + 1))`. Unknown query terms have zero weight. A candidate's
coverage is matched information divided by total known query information.
A candidate needs a strict majority, not a tuned score multiplier.

An eligible relation is a query pair also observed in the candidate summary.
Its support-discounted lift is `(pair_df - 1) * N / (df(a) * df(b))`. It must be
greater than one. Subtracting this fact's support prevents a singleton from
supporting itself. The denominator corrects for the marginal popularity of
both words. This is an association heuristic, not a causal or statistical
significance claim. Different documents are not necessarily independent authors
or origins; this implementation does not deduplicate lineage.

At least one supported pair term must also occur in the fact ID or a stored
`applies_to` command, path or symbol. This binds prose evidence to an identifiable
fact or recorded location. Pairs are query-local observations, not a graph of
arbitrary word combinations. No synonym model, LLM, GPU or service is involved.

## Routing and guards

A candidate satisfying coverage, corrected relation support and location
binding may clear the hook floor through evidence rather than BM25 alone.
BM25 still orders candidates; existing coherence remains a separate route.
Eligibility is applied before the candidate limit.

When neither cwd nor the prompt identifies a repository, this direct evidence
can retrieve a curated project fact or a system/user/current-host fact despite
a nonmatching capture repository in `applies_to.repos`. It does not add that
repo to the query globally. A known current repo or an explicitly named repo
keeps its existing restrictions; foreign-machine scopes stay inaccessible.
A host match alone still cannot override a repo restriction.

SessionStart does not use this route. Derived retrieval stays separate with its
own floor. The route does not include inbox or overlay notes, change rulings,
or backfill weaker results after session deduplication. Explicit search without
`--prompt` keeps its broad behavior.

## Diagnostics and locations

`tacit search --prompt --json <request>` runs prompt selection without reading
or writing session deduplication state. It uses cwd repo aliases unless `--repo`
is specified, respects `--host`, and uses the hook's fixed limits rather than
`--limit` or `--inbox`. Normal `tacit search --json` is unchanged.

Each curated hit's `Evidence` reports corpus size, per-term df/IDF/match status,
weighted coverage, supported pairs and discounted lifts, location binding and
whether this route qualified. `Locations` contains the indexed fact ID, actual
store-relative fact path, scope and recorded applicability. A body `[[link]]`
adds a one-hop pointer only if it resolves uniquely to an active curated fact.
Dangling, ambiguous, retired and derived links are not resolved. Links do not
promote their targets into the delivery set or trigger recursive expansion.
Recorded applicability is not a claim that a filesystem path exists now.

## Development replay

The existing 60 private cases were replayed unchanged against one clean pinned
store with the actual binaries and isolated indices/overlays. Both 30-case
groups were already inspected during development; neither is held out. Final
runs used three repetitions and were deterministic. Each group has 15 positive
and 15 no-memory cases. Labels were not expanded for new results.

| Arm | Group | Lines | Useful | Unexpected | Positive hit / 15 | No-memory injection / 15 |
|---|---|---:|---:|---:|---:|---:|
| Main 0732543 | A | 24 | 16 | 8 | 10 | 0 |
| Main 0732543 | B | 27 | 17 | 10 | 12 | 0 |
| Weighted relationships | A | 30 | 19 | 11 | 12 | 0 |
| Weighted relationships | B | 31 | 20 | 11 | 13 | 0 |
| Uniform known-term weights | A | 30 | 18 | 12 | 12 | 0 |
| Uniform known-term weights | B | 31 | 20 | 11 | 13 | 0 |
| No marginal correction | A | 32 | 19 | 13 | 12 | 1 |
| No marginal correction | B | 32 | 20 | 12 | 13 | 1 |

Ablations used one repetition, separate compiled binaries and the same corpus.
Uniform weighting changes only the coverage weights. The uncorrected arm
retains two-document support and location binding but removes the lift > 1
test. It causes two no-memory false triggers; the marginal correction matters.
IDF adds one labelled useful line on this set, not another positive query hit.

Positive coverage improves from 22/30 to 25/30, while no-memory injections stay
at 0/30. Unexpected lines increase from 18 to 22, so this is not a claim of
unchanged line-level precision. Warm median hook latency is about 107-118 ms
versus 91 ms for main on the shared machine. The initial whole-corpus runtime
prototype took about 330 ms and was replaced with indexed postings.

An observed synthetic regression fails on main and passes here: an explicitly
named service can resolve its current-host fact from an unknown cwd despite
historical capture-repo metadata. Additional tests cover exact IDF, corrected
pair support, stop-word boundaries, pointer resolution, corpus exclusions,
known-repo/foreign-host guards, derived isolation and session no-backfill.
All Go packages were tested with `GOMAXPROCS=2 go test -p 2 ./...`.

Remaining limits: lexical phrases miss paraphrases and inflections; corpus
composition changes IDF; duplicate-origin facts can inflate support; broad
phrases can still add unwanted lines on positive requests. Majority coverage
is a design rule, not a validated optimum. No new heldout set was available to
the implementation author. A separately prepared evaluation follows; no
task-level outcome study was run. Do not generalize these numbers to production
traffic.

## Independently prepared evaluation and post-evaluation correction

A separate evaluator prepared 36 cases without seeing candidate source, output
or the development-60 cases: 24 positive requests and 12 no-memory requests.
Eight were bounded excerpts of actual user messages; 28 were independently
written targeted requests. All used a home-directory cwd. Frozen primary labels
preceded baseline replay; the files were hashed and unchanged. There was no
exact normalized prompt overlap with the development set. This is a small,
purposive sample, not a random traffic estimate.

| Arm | Lines | Primary useful | Unexpected | Positive hit / 24 | No-memory injection / 12 |
|---|---:|---:|---:|---:|---:|
| Main 0732543 | 55 | 19 | 36 | 17 | 0 |
| Frozen candidate, first exposure | 56 | 22 | 34 | 18 | 0 |
| Candidate after scope correction | 56 | 21 | 35 | 18 | 0 |

The first exposure found an explicit-project scope leak: a supported generic
phrase admitted a different project's fact from a home cwd despite the request
naming its target repo. A new synthetic regression failed before the correction
and passes afterward. The final route now fills missing repo context only;
it never overrides a repo explicitly named in the request. The final row is
therefore post-evaluation, not an untouched heldout result. No threshold was
tuned on these cases and no labels were changed for the candidate.

Before candidate exposure, the evaluator also documented 11 useful omissions in
the baseline labels. Against that additive, baseline-informed label set, useful
lines are 30 to 31 and positive hits 19 to 20; uncredited lines stay 25. Both
candidate versions have those same supplementary counts. Frozen primary and
supplementary results remain separate: an unexpected result is not necessarily
irrelevant, and redundant useful facts do not prove additional task value.

All final replays used three deterministic repetitions. Post-correction warm
median latency was about 115-119 ms on development cases and 129 ms on the
independent cases. Three isolated cold-index hook runs measured a median of
2.63 seconds versus 1.05 seconds for main. Both returned real fact blocks on
this shared machine; cold-build growth and the harness timeout remain risks
for larger stores. Full Go client tests pass after the scope correction.

