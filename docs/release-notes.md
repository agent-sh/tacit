Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release reads two more harnesses. Measurements are in [eval/VALUE.md](https://github.com/agent-sh/tacit/blob/main/eval/VALUE.md).

- **Hermes.** The dreamer and the owner-words corpus read the owner's Hermes conversations: mobile, Telegram, desktop, CLI, TUI and web UI.
  - The store is opened read-only (`~/.hermes/state.db`).
  - Left out: automation (cron, one-shot runs, subagents, delegations Hermes marks, kanban) and text the harness injects into a turn (compaction summaries, notices, out-of-band frames, pasted attachments).
  - A voice message's transcript counts as the owner's words.
  - Turn it off with `hermes_db: off` or `TACIT_HERMES_DB=off`.
- **pi.** pi sessions are read the same way. A headless run (`pi -p`, an RPC or SDK driver) is told apart by how fast its first turn arrives.
- **No replay on upgrade.** A source the dreamer has never seen starts at its current end, so an existing install does not dream months of old history at once.

Rulings from Hermes and pi chats can now be checked against the owner's own words there.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
