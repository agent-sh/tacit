# Methods

tacit stores conclusions an agent can use in a later session. It collects them from agent notes, existing memory files, transcript deltas and git. It records where each came from, delivers a small relevant subset, and checks whether they still hold.

The [README](../README.md) shows the system and its measured results. [SPEC.md](../SPEC.md) is the exact contract. This guide explains the choices and how to evaluate them on another store.

## One fact, one file

A curated fact is Markdown with YAML frontmatter. Its name is its identity; its description is the line delivered in a hook block. The body supplies the detail. Scope says where it applies, and source says who wrote or computed it.

For example, this invented fact appears in the runnable sample:

```yaml
---
name: go-toolchain
description: "demo-api pins its Go toolchain in go.mod; do not lower it"
type: design_decision
scope: projects/demo-api
source:
  harness: curator
  host: workstation
  kind: observed
created: 2026-10-01
---

The toolchain line in go.mod is the supported version.
```

This says what to preserve and where to look. The agent still reads go.mod. Facts are not a replacement for reading code.

Scopes are `system`, `user`, `projects/<repo>`, `machines/<host>` and `derived/<repo>`. Curated file directories match their scopes. An inbox note carries the scope its writer believes is right; the curator can narrow or correct it. Superseded and retired facts remain in git but leave normal retrieval.

One fact per entry matters because retrieval delivers descriptions. A memory file that bundles many facts behind one description can rank well while hiding the one fact the agent needs. The curator split improved offline delivery on the measured store; [VALUE.md](../eval/VALUE.md) gives the comparison and limits.

## Collection and curation

Agents write only to `inbox/<harness>@<host>/`. The server enforces that boundary. A read-only client keeps new notes in a local overlay instead of pushing them.

`tacit add` records an agent note. `tacit import` converts existing harness memory files with provenance. `tacit dream` reads new transcript deltas, makes compact redacted digests, and asks a model for reusable candidate facts. The dreamer's output is unreviewed inbox material. Redaction must work before agent notes or digests leave the machine.

`tacit curate` first sets aside plain restatements it can recognize without a model. A model then splits pending notes into individual entries, with a real type, scope and source. A contradiction judge compares new entries with nearby existing facts and same-scope facts from that run. A failed conflict check stops the batch before it is written.

Automatic supersession is restricted to closely related observations. Contradictions, uncertain supersessions and conflicts with owner rulings become findings. The curator generates `MEMORY.md`, lints the changes and pushes as the curator key. It does not silently discard an owner's ruling to make the store consistent.

## Proving an owner's ruling

An agent writing `kind: user` does not make its sentence the owner's decision. After the rulings audit, a ruling needs evidence: an app contribution from the owner, a qualifying quote found in an owner turn and recorded in the proof ledger, or the owner's recorded decision to keep the fact.

The quote check normalizes case, whitespace and punctuation. It checks that the quoted statement supports the rule. Questions, negated statements, harness notices, subagent messages and compaction summaries cannot stand in for the owner's statement. A shorter backed phrase can be recorded as the owner's words without making the whole fact a ruling.

The proof ledger preserves evidence when the original transcript disappears. The audit records its migration marker in the store. Existing and server-created unaudited stores use the legacy definition based on `type: user_standard` or `kind: user`. A new `tacit init` local store starts with evidence-based rulings while empty. For unaudited stores, run `tacit rulings audit` on the curator's machine to switch; see [setup](setup.md#initialize-ruling-proof) and [Rulings](../SPEC.md#rulings). An empty or unreadable owner corpus cannot prove a new ruling, and the audit refuses to enable the switch on that basis.

The guard is shared by curation, verification, staleness checks, results and link repair. An agent cannot retire or supersede a protected ruling. The owner reviews the proposed change in the app.

## Retrieval and delivery

The client builds a local SQLite FTS5 index over names, descriptions and bodies. The cache is keyed by store HEAD, schema version and local-overlay state. The ranker does not call a model.

Query terms form an OR query. BM25 weights are 6 for name, 6 for description and 1 for body. Eligible scopes are system, user, the current project and the current host. Repo and host scopes get a small score boost. Normal hook retrieval excludes shared inbox notes, retired facts and superseded facts. A read-only machine's local notes are eligible but remain unreviewed; they are capped in the start block. A conflict pair is never delivered together.

The hooks use two delivery moments:

| Moment | Selection |
|---|---|
| Session start | Query by repo name, keep up to 10 matches above the start floor, order rulings before project, host and general facts. No computed facts. |
| User prompt | Keep up to 3 curated matches above the prompt floor, then remove facts already delivered in this session. No backfill with weaker matches. A separate query can add the best computed fact above its own floor. |

Harness-injected turns and prompts with fewer than two searchable terms do not trigger prompt delivery. Hook output contains names and descriptions; `tacit fact <name>` supplies the detail. Explicit `tacit search` and the MCP search tool can include computed facts. Hooks return no output when the store or network is unavailable.

The score floors and caps came from the measured store and prompt samples, not a universal relevance guarantee. Larger blocks cost more without helping in those runs. Dense retrieval and fusion did not beat lexical retrieval there, so the client has no embeddings. The Python offline rankers in `eval/delivery/` explore lexical IDF, dense ranking and fusion; they are not the shipped Go hook implementation.

## Facts from git

`tacit derive` computes hot spots and module maps from a repo's configured branch or remote default branch. Hot spots count file changes and co-change partners over a window of code commits. Module maps summarize the tree. Each output records the commit it read. The same input commit produces the same bytes.

`tacit results` reads eligible committed measurement files, groups compatible rows and records values with their source commits. It distinguishes measurements that can be printed in a hook from references an agent should open. It does not establish that a result transfers to another model, machine or workload.

`tacit stale` looks for deletion of subjects named by facts. This narrower trigger was useful in the measured store; merely changing a line that shares a name was not. Deletions can flag staleness, but changes in a value whose name survives can escape this check.

These steps compute evidence. They do not choose what the owner wants or invent measurements. The review page's git timeline helps the owner inspect a finding. Adding that timeline to the verifier did not improve agreement in the paired runs, so it is not part of that verifier input.

## Verification and review

`tacit verify` takes open findings and gives a model read-only access to the store, repo checkouts and configured source directories. File access is restricted; shell-like probes run through tacit's validated MCP probe. Secret paths are excluded, and model output is redacted before it is stored.

A verdict records evidence and the model used. Clear observation changes can be applied; changes to protected rulings and unresolved intent go to the owner. The configured fallback runs for capacity failures such as throttling, not for lost model access or arbitrary errors. Explicitly configured missing source directories stop the model call. Claude auto-memory is included when it exists.

The owner can inspect the competing facts, sources, dates and git timeline, then keep, edit, retire or mark a conflict. The decision is recorded. That human decision is the backstop: the held-out verifier sample missed most of the retirements the owner made. The verifier's purpose is to settle evidenced cases before the owner sees the remainder.

## How to evaluate another store

Start with the [synthetic sample](../eval/README.md#run-the-sample). It checks that the schemas and offline commands work together. Its output is a wiring example, not evidence that tacit improves another workload.

For a real evaluation:

1. Make redacted session digests. Write a truth set of visible detours where an existing or missing fact would have helped. Record scope, evidence and whether delivery must be automatic or an explicit query can find it. Follow [SCHEMA.md](../eval/SCHEMA.md).
2. Pin the corpus and baseline. Grep over existing memory files separates absent knowledge from knowledge retrieval missed. Report the number of truth items, coverage gaps and hit@k. Preserve the corpus manifest and the exact command.
3. Compare retrieval choices on that fixed corpus. Separate tuning prompts from held-out situations. Offline hits show whether a block carries a fact; they do not show that the agent uses it.
4. Run matched tasks with the same model and prompt: bare, ranked block and oracle. The oracle deliberately supplies the truth fact, measuring the value of delivery when retrieval cannot miss it. Compare block caps and curated versus unsplit stores. To evaluate deployed behavior, test the shipped Go hooks separately; the offline Python ranker is not a substitute.
5. Measure outcomes as well as retrieval. Keep task correctness, ruling violations, tool calls, tokens, latency and cost. Repeat runs and retain each cell, not just the aggregate. Cache state can dominate per-call dollars, so token and tool counts matter too.
6. Evaluate the dreamer against facts that were never written, and the verifier against owner-labelled findings. Report missed facts and wrong retirements as well as successes. Keep held-out agreement separate from tuning agreement.

[eval/README.md](../eval/README.md) maps the scripts, flags, dependencies and model calls. [VALUE.md](../eval/VALUE.md) keeps the original measurements, sample sizes and negative results. Original store contents, transcripts and task answers stay private. You can repeat the methods on your own data; you cannot recreate those exact results from the synthetic sample.

## Choosing the model runner

Dreaming, splitting, judging and verification use independently configurable agent CLIs. Claude and its measured role models remain the defaults. Muse and agy also expose memory through MCP, alongside the existing integrations. Changing a runner does not change the prompts, owner-proof rules, redaction or review requirements. Quality on another model needs its own evaluation; the published measurements do not transfer automatically. See [configuration and validation limits](portable.md#model-runners).
