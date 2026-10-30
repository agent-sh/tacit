# Replaying graded deliveries

`labelled_replay.py` scores a delivery rule on the hook calls that really happened: each one replayed through an actual binary, at the store commit the hook read then, with the facts the session already had. A candidate's lines are scored on grades a person gave the lines tacit printed at the time. It answers "on real traffic, how many useful lines does this rule keep, and how much noise does it print", without a model and without touching the live store.

Store contents, prompts, grades and outputs stay private. Nothing in the repository holds them.

## Cases

`deliveries.py` turns Claude Code transcripts into cases:

```sh
python3 -I eval/delivery/deliveries.py --store /abs/store-clone --out /abs/private/cases \
    [--labels /abs/private/labels.tsv] TRANSCRIPT...
```

One case per prompt block (UserPromptSubmit) and per tool-call block (PreToolUse) the transcripts recorded:

- the prompt, or the tool call and its input;
- the cwd the hook saw. Claude Code records the shell's directory on the prompt's or the call's own record; the hook's record can hold another one, and a push of a repository's fact then has no repository to come from;
- `seen`: the names every earlier block of the session printed, and `area:<folder>` for an area pushed. Give it all of a session's transcripts, its subagents' files too, or the sets come out short and the replay prints lines the session already had;
- `transcript`: the last 512 KiB before the prompt, or up to the call, for the prompt hook's activity rule;
- `commit`: the clone's last first-parent commit at or before the hook ran;
- `delivered`, `rules`: what it printed, and `labels` from a TSV of case id, fact name and grade (R useful, W weak, N noise). A case id is a hash of its transcript, event and time, so grades keep their case across runs.

A cwd that is gone and was a Claude Code agent worktree (`<repo>/.claude/worktrees/<name>`) gets a stand-in linked worktree of the same repository under `--out`, and an edit's file path moves into it; a command's text is left as it was. Any other cwd that is gone is noted (`cwd_note: gone`): its case replays outside a repository, so leave it out of a comparison (`score --exclude gone`). A probe is a case with `expected`, the facts that would have answered it; add those by hand, before looking at any arm.

## Running arms

```sh
python3 -I eval/delivery/labelled_replay.py run --store /abs/store-clone --cases cases.jsonl \
    --out /abs/private/run --index-cache /abs/private/index --simulate-start \
    --arm base=/abs/tacit-main --arm cand=/abs/tacit-candidate,TACIT_DELIVERY_X=on
python3 -I eval/delivery/labelled_replay.py score --cases cases.jsonl --out /abs/private/run \
    [--grades extra.tsv] [--exclude gone]
python3 -I eval/delivery/labelled_replay.py todo --cases cases.jsonl --out /abs/private/run > to-grade.tsv
```

- **The commit.** Worktrees of the clone are checked out at each case's commit, in time order. The clone is never pushed; point its remote at nothing (`git remote set-url origin /nonexistent`) so a hook's sync cannot move a worktree.
- **The index.** A binary prunes every other commit's index from the directory it builds in, so each worker and arm has its own directory holding only the commit being replayed, hard-linked from `--index-cache`. Indexes are built with `tacit hook index` (the hooks' own build budget) and kept across runs: once they existed, a pass over 237 cases at about 100 commits took 26 seconds. After each commit the runner checks no hook started a build.
- **The session.** Each call gets a fresh state directory (`TACIT_STATE_DIR`) whose session file holds the case's `seen`, an empty overlay and a config file with defaults only. No dreamer state means the prompt hook's window trigger starts nothing. `--state-file NAME=PATH` puts a file in every call's state directory.
- **SessionStart.** Newer Claude Code does not record the SessionStart block. With `--simulate-start` a session whose block is missing gets it from the first arm, at the commit of the session's first case, from the transcript's first cwd, and its facts join `seen`.
- An arm is a binary and environment settings. Arms run on the same cases, so a comparison holds the machine, the store and the session fixed.

## Scores

Per label set (and per prompt origin), for each arm: lines printed, how many carry each grade, the lines no grade covers, the noise share of graded lines with a Wilson interval, `useful_kept` (the useful lines of the time the arm still prints), blocks with a useful line and blocks that are all noise, `reproduced` (lines of the time the arm prints) and, for probes, how many get one of their expected facts.

Lines an arm prints that the session did not get have no grade. `todo` lists them shuffled, without the arm's name, with the request and the line; grade them, add the file with `--grades`, and score again. A comparison is final only with every line graded.

## Fidelity

Run the shipped binary as the first arm. On the delivery code of its own release, the replay should print what the session got:

- On the author's store, 3 days of owner prompts (93 blocks, 240 graded lines, releases 0.9.9 to 0.9.14, whose delivery code is identical): 234 of 240 lines reproduced, and 4 lines printed that the session did not get. The 6 lost lines are two blocks of one session that started in a notes folder: the prompt hook's activity rule found no repository at the time (it gives up after 300 ms, and the machine was loaded), and on a quiet machine it finds one.
- 122 tool calls of the same days with a push: 124 of 124 lines reproduced. Before the cwd came from the call's own record, 7 were lost.
- 14 days of owner prompts and pushes, delivered by releases 0.2 to 0.9.14: 160 of 171 and 64 of 65 lines, with 11 and 0 lines no grade covered. Older delivery code printed slightly different blocks.

## Limits

- A grade says whether a line was useful to the request, as a reader judged it from the request and the turn before; it is not what the agent did with it. The author's grades: one rater; a blind second rater on 87 lines of the 14 days (kappa 0.63) and 140 of the 3 days (0.56 for prompt lines, 0.54 for pushes); the first rater on 100 lines again (0.90).
- `seen` comes from the transcripts given, and the simulated SessionStart from the first arm at a later commit than the session's start.
- Time budgets: the prompt hook's activity rule (300 ms) and the PreToolUse hook (400 ms) can cut a live call short on a loaded machine and not in a replay.
- Only Claude Code transcripts are read. Codex and Hermes record their hook output differently.
