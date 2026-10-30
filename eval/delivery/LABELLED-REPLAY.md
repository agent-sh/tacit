# Replaying graded deliveries

`labelled_replay.py` scores a delivery rule on the hook calls that really happened: each one replayed through an actual binary, at the store commit the hook read then, with the facts the session already had. A candidate's lines are scored on grades a person gave the lines tacit printed at the time. It answers "on real traffic, how many useful lines does this rule keep, and how much noise does it print", without a model and without touching the live store.

Store contents, prompts, grades and outputs stay private. Nothing in the repository holds them: keep `--out` and every case, grade and result file in a private directory outside the checkout (the examples use `/abs/private`).

## Cases

`deliveries.py` turns Claude Code transcripts into cases:

```sh
python3 -I eval/delivery/deliveries.py --store /abs/store-clone --out /abs/private/cases \
    [--labels /abs/private/labels.tsv] [--silent] TRANSCRIPT...
```

One case per prompt block (UserPromptSubmit) and per tool-call block (PreToolUse) the transcripts recorded:

- the prompt, or the tool call and its input, and for a prompt its origin: typed, queued (typed while the agent worked), meta (a harness's turn) or headless (an SDK entrypoint);
- the cwd the hook saw. Claude Code records the shell's directory on the prompt's or the call's own record; the hook's record can hold another one, and a push of a repository's fact then has no repository to come from;
- `seen`: the names every earlier block of the session printed, and `area:<folder>` for an area pushed, including blocks that make no case because their prompt or call is not in the transcript. Give it all of a session's transcripts, its subagents' files too, or the sets come out short and the replay prints lines the session already had;
- `transcript`: the last 512 KiB before the prompt, or up to the call, for the prompt hook's activity rule. The hook reads a transcript only under the home directory, so `--out` must be under it (`run` warns otherwise);
- `commit`: the clone's last first-parent commit at or before the hook ran;
- `delivered`, `rules`: what it printed, and `labels` from a TSV of case id, fact name and grade (R useful, W weak, N noise). A case id is a hash of its transcript's real path, event and time, so grades keep their case across runs.

Claude Code records no hook output when the hook printed nothing, so without `--silent` a call tacit stayed silent on is no case, and a rule that adds lines there is never seen. `--silent` adds every prompt and every tool call of a tool the PreToolUse hook matches, with nothing delivered (`silent: true`): their lines are all ungraded until `todo` lists them. A rule that only removes lines is measured fully without them.

A cwd that is gone and was a Claude Code agent worktree (`<repo>/.claude/worktrees/<name>`) gets a stand-in linked worktree of the same repository under `--out`, and an edit's file path moves into it (a command's text is left as it was); the session's first cwd gets the same. Any other cwd that is gone is noted (`cwd_note: gone`): its case replays outside a repository, so leave it out of a comparison (`--exclude gone`). A probe is a case with `expected`, the facts that would have answered it; add those by hand, before looking at any arm.

## Running arms

```sh
python3 -I eval/delivery/labelled_replay.py run --store /abs/store-clone --cases /abs/private/cases/cases.jsonl \
    --out /abs/private/run --index-cache /abs/private/index --simulate-start \
    --arm base=/abs/tacit-main --arm cand=/abs/tacit-candidate,TACIT_DELIVERY_X=on
python3 -I eval/delivery/labelled_replay.py score --cases /abs/private/cases/cases.jsonl --out /abs/private/run \
    [--grades /abs/private/extra.tsv] [--exclude gone]
python3 -I eval/delivery/labelled_replay.py compare --cases /abs/private/cases/cases.jsonl --out /abs/private/run \
    --arms base,cand [--grades /abs/private/extra.tsv]
python3 -I eval/delivery/labelled_replay.py todo --cases /abs/private/cases/cases.jsonl --out /abs/private/run \
    > /abs/private/to-grade.tsv
```

- **The commit.** Worktrees of the clone are checked out at each case's commit, in time order, and checked again after each commit's calls. The clone is never pushed, and `run` refuses one whose origin a hook's sync could reach: point it at nothing first (`git remote set-url origin /nonexistent`).
- **The index.** A binary prunes every other commit's index from the directory it builds in, so each worker and arm has its own directory holding only the commit being replayed, hard-linked from `--index-cache`. Indexes are built with `tacit hook index` (the hooks' own 5-minute build budget) and kept across runs: once they existed, a pass over 237 cases at about 100 commits took 26 seconds. The cache is keyed by schema and commit, as the binary names its files; empty it when the index code changes without a schema bump.
- **The session.** Each call gets a fresh state directory (`TACIT_STATE_DIR`) whose session file holds the case's `seen`, an empty overlay and a config file with defaults only. No dreamer state means the prompt hook's window trigger starts nothing; a call that leaves a build or dream log in its state directory, or changes the index directory, stops the run. `--state-file NAME=PATH` puts a file in every call's state directory.
- **SessionStart.** Newer Claude Code does not record the SessionStart block. With `--simulate-start` a session whose block is missing gets it from the first arm, at the commit of the session's first case, from the session's first cwd, and its facts join `seen`.
- **Arms.** An arm is a binary and environment settings (`NAME=/abs/binary,KEY=VAL,...`). Arms run on the same cases, so a comparison holds the machine, the store and the session fixed. An arm name keeps its binary and settings within one `--out` (`arms.json`): resuming with another refuses, since the results would mix.

## Scores

`score`, per label set (and per prompt origin, and the silent calls apart), for each arm: lines printed, how many carry each grade, the lines no grade covers, the noise share of graded lines with a Wilson interval, `useful_kept` (the useful lines of the time the arm still prints, over the cases it ran), blocks with a useful line and blocks that are all noise, `reproduced` (lines of the time the arm prints), `missing` (cases it did not run), `failed` (calls that errored or ran past their hook's budget, where printing nothing may be the failure) and, for probes, how many get one of their expected facts.

Lines cluster by block and session, so the Wilson interval is too narrow for a difference between arms. `compare` pairs two arms case by case: the change in useful, noise and all lines, each with a bootstrap interval over cases.

Lines an arm prints that the session did not get have no grade. `todo` lists them shuffled, without the arm's name, with the request and the line; grade them, add the file with `--grades`, and score again. A comparison is final only with every line graded.

## Fidelity

Run the shipped binary as the first arm. On the delivery code of its own release, the replay should print what the session got:

- On the author's store, 3 days of owner prompts (93 blocks, 240 graded lines, releases 0.9.9 to 0.9.14, whose delivery code is identical): 234 of 240 lines reproduced, and 4 lines printed that the session did not get. The 6 lost lines are two blocks of one session that started in a notes folder: the prompt hook's activity rule found no repository at the time (it gives up after 300 ms, and the machine was loaded), and on a quiet machine it finds one.
- 122 tool calls of the same days with a push: 124 of 124 lines reproduced. Before the cwd came from the call's own record, 7 were lost.
- 14 days of owner prompts and pushes, delivered by releases 0.2 to 0.9.14: 160 of 171 and 64 of 65 lines, with 11 and 0 lines no grade covered. Older delivery code printed slightly different blocks.

## Limits

- A grade says whether a line was useful to the request, as a reader judged it from the request and the turn before; it is not what the agent did with it. The author's grades: one rater; a blind second rater on 87 lines of the 14 days (kappa 0.63) and 140 of the 3 days (0.56 for prompt lines, 0.54 for pushes); the first rater on 100 lines again (0.90).
- The commit is chosen by time: on a clone that follows the server it is what the hook read within seconds. The hooks' clone's reflog would give it exactly.
- `seen` comes from the transcripts given, and the simulated SessionStart from the first arm at a later commit than the session's start; a session that began with `/clear` or a resume got no start block live, and the simulated one hides lines it did get.
- Time budgets: the prompt hook's activity rule (300 ms) and the PreToolUse hook (400 ms) can cut a live call short on a loaded machine and not in a replay.
- Only Claude Code transcripts are read. Codex and Hermes record their hook output differently.
