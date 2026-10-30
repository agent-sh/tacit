Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release fixes which repository a fact's paths send it to. A checkout the catalog takes counts wherever it is, and a scratch clone or a Claude Code memory folder no longer sends a fact anywhere. Claude Code's interruption mark is now the harness's in every session, and `tacit doctor` checks scrump's records mode.

- **A checkout outside the usual roots is found.** Routing decides which repository a fact belongs to for an org's migration, the moves out of a folder that is no project, code's folds and assess's evidence. It read only paths under `~` and nine system roots (`/Users`, `/home`, `/opt`, `/etc`, `/var`, `/usr`, `/srv`, `/mnt`, `/Library`). A fact that named its checkout under `/tmp`, `/data`, `/root`, `/workspace` or `/Volumes` named no repository, though SPEC routes a fact by the absolute paths it names.
  - Every `~` and absolute path outside `/proc`, `/sys`, `/dev` and `/net` is now read, and names the repository of its real git root on this machine (#91).
  - Links in those kernel views resolve to what the reading process has open (`/proc/self/cwd`). On macOS a lookup under `/net` mounts another host's export.
  - On the author's machine `~/projects` is a bind mount of `/data/projects`. 5 facts that name a checkout by its `/data/projects/...` path now point at its repository.
- **A scratch clone the catalog does not take names nothing.** A checkout outside home or in a temp directory, and no main checkout of the projects directory, counts once 2 sessions worked in it, as Scopes says.
  - It also counts when the catalog's repository of its name shares a remote with it: the same checkout through a bind mount, or a clone of a fork.
  - Before, any checkout counted. So a fact that named its project and a task's scratch clone pointed at two repositories and stayed where it was.
  - A vendored clone or submodule whose name is no slug (`third_party/llama.cpp`) counts as the checkout holding it.
- **Another machine's path names a checkout only under a home's projects directory:** `~/projects`, `/root/projects`, `/home/<user>/projects` or `/Users/<user>/projects`. The fallback for a path with no checkout here matched `projects/<name>` anywhere. So a Claude Code memory folder, `~/.claude/projects/<folder>/memory/`, named the repository the catalog calls `memory`. A store's own `projects/<name>` folder named that project.
  - On the author's machine `memory` is another name of tacit. 147 facts pointed at tacit that way, and one pointed at darklanes through the store's `projects/darklanes` folder.
  - One of them had been held by that false pointer in a folder that is no project. It now moves to its repository.
  - Below a folder of checkouts, the second level counts by a repository's own name only, never by another name of it.
  - A route such as `/api/projects/<name>` names nothing.
- **Claude Code's interruption mark is the harness's in every session.** `[Request interrupted by user]` was harness text in sessions a person typed into. Sessions no person types into read it otherwise: subagent transcripts, sessions in an agent's directory and Codex rollouts converted from Claude Code.
  - There the event reader called it agent text. In the ones the dreamer reads, its digest wrote it as an agent's prompt, `P:`.
  - One rule now decides it for every format: the digest writes `H:` and the event reader reports `harness_text` (#92).
  - A dream window of an agent's session whose only prompt was the mark no longer counts as holding one, so the dreamer skips it.
- **`tacit doctor` checks scrump's records mode.** It sends #84's repro as two records and reports `scrump records`.
  - The line is ok when scrump keeps them apart, a warning when scrump has no records mode, and a failure when its answer is malformed.
  - Without records mode, batched texts go joined by a boundary, which still redacts.
  - The `scrump` check also fails a scrump that leaves a quoted value a cut left open unmasked, as scrump before 0.2.0 does.
  - The canary's error now says to install scrump 0.2.0 or later instead of rebuilding it from main. The dreamer, the MCP server and the other redacting commands return it too (0af0bd3).

Upgrading: no index, store format or server change. Install it on every machine that runs the curator, whose assessment and migrate steps route facts, and on every machine that dreams. A program that reads sessions through `client/pkg/evidence` gets the new label once it requires the client module at this release's commit. An older binary reads what this one writes, and keeps the old reading of paths and of the interruption mark. With a scrump before 0.2.0 first on PATH, `tacit doctor` now exits 1: its `scrump` check fails and `scrump records` warns. Redaction still runs with that scrump as before. The release archives bundle 0.2.0, and `scrump --version` shows which one is on PATH.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
