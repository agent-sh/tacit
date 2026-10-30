Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release reads the paths a fact names the way SPEC says when it routes the fact to a repository: every checkout path counts, and paths that name no checkout of a project no longer count. It also labels Claude Code's interruption mark the same way in every session.

- **A checkout outside the usual roots is found.** Routing decides which repository a fact belongs to: an org's migration, the moves out of a folder that is no project, folds and assess's evidence. It read only paths under `~` and nine system roots (`/Users`, `/home`, `/opt`, `/etc`, `/var`, `/usr`, `/srv`, `/mnt`, `/Library`). A fact that named its checkout under `/tmp`, `/data`, `/root`, `/workspace` or `/Volumes` named no repository, though SPEC promised the absolute paths. Every `~` and absolute path is now read, and names the repository of its real git root on this machine (#91).
- **A task's scratch clone names nothing.** A checkout outside home or in a temp directory counts only as the catalog takes it, as Scopes says: work from 2 sessions, or a repository of its name that shares a remote with it, such as the same checkout through a bind mount or a clone of a fork. Before, any checkout counted, so a fact that named its project and a scratch clone pointed at two repositories and stayed where it was. A vendored clone or submodule whose name is no slug (`third_party/llama.cpp`) counts as the checkout holding it.
- **Another machine's path names a checkout only under a home's projects directory** (`~/projects`, `/root/projects`, `/home/<user>/projects`, `/Users/<user>/projects`). The fallback for paths with no checkout here matched `projects/<name>` anywhere. So Claude Code's memory folders, `~/.claude/projects/-home-<user>/memory/`, read as a repository named memory, which is an alias of tacit.
  - On the author's store, 147 facts pointed at tacit that way, and one pointed at darklanes through the store's own `projects/darklanes` folder.
  - One fact had been held by that false pointer in a folder that is no project; it now moves to its repository.
  - Routes such as `/api/projects/<name>` never name one.
- **Paths a process sees its own way are left out.** `/proc`, `/sys` and `/dev` resolve to what the reading process has open (`/proc/self/cwd`), and on macOS a lookup under `/net` mounts another host's export.
- **Rotation and the ancestor index still read places only.** Their reader takes `~` and the system roots, so a route never reads as a missing path or as folders inside a repository. SPEC now names the roots.
- **Claude Code's interruption mark is the harness's in every session.** `[Request interrupted by user]` was harness text in sessions the owner typed into. In sessions no person types into, it was an agent's prompt, and the dreamer's digest wrote it as `P:`. Those are subagent transcripts, sessions in an agent's directory, and Codex rollouts converted from Claude Code. One rule now decides it for every format: the digest writes `H:` and the event reader reports `harness_text` (#92).

On the author's store, the routing changes give 5 facts the repository their `/data/projects/...` path is in, take a false pointer from 148, and move one fact.

Upgrading: no index, store format or server change; the server needs no deploy. Install it where the curator, assess and the dreamer run. An older binary reads what this one writes, and keeps the old reading of paths and of the interruption mark.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
