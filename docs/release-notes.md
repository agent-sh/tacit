Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release gives a session its facts back after a compaction, a resume, a clear or a fork (#127, #128).

- **Facts again after a compaction.** A compaction's summary keeps only part of what a session was given. SessionStart now answers a compaction with the start block from the store as it is, then the facts that match the summary. The delivered set becomes what the context holds, so the hooks can give a dropped fact again.
- **Resume, clear and fork.** A resumed session hears what was retired or replaced since it last saw it. A cleared session gets a start's block. A fork gets a start's block too; before, it got nothing.
- **No repeat after a Claude Code compaction.** Claude Code writes the summary after the start hook. A PreToolUse push that finds the summary written reads it once, so it does not give again a fact the summary holds.
- **Install.** The SessionStart matcher is now `startup|resume|clear|compact|fork`. `tacit install` widens an earlier tacit-only entry in place.
- **Tests.** `TestOpenIndexSurvivesPrune` no longer reads the machine's own notes, and the client suite's timeout is 30 minutes.

Upgrading: run `tacit install` once to widen the hook matcher. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
