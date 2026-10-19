Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release is about owner rulings and repeated facts. Measurements are in [eval/VALUE.md](https://github.com/agent-sh/tacit/blob/main/eval/VALUE.md).

- **Rulings given inside a task.** Some of the owner's quoted words are instructions for the task at hand ("lets create it as draft", "1. drop it 3. fix it"). They stay the owner's ruling, protected as before. At the prompt they are plain lines, without the RULE label or the ruling slot. At session start they come right after the standing rules. The rule reads the owner's own wording: a task verb or a reply as the opener, a numbered answer, or a question, and no generalizing word. On held-out labels it marks 25 of 89 one-off instructions and 2 of 51 standing rules.
- **One copy of a fact.** The curator no longer writes an entry that only restates a curated fact of its scope. `tacit dedupe [--dry-run]` retires the copies a store already holds, with `superseded_by` the fact kept, and never touches rulings, facts with the owner's words, or answered pairs.
- **Search from inside a Claude Code session.** `tacit install claude` also registers tacit's MCP server (`--no-mcp` to skip), so an agent can open a fact in full or search mid-task. It costs about 43 tokens per session with deferred tools.
- **`TACIT_STATE_DIR`** moves the per-machine state, so a second store on one machine keeps its own cursors without changing `HOME`.

Upgrading: the search index rebuilds once (schema 8). Rerun `tacit install claude` to add the MCP server.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
