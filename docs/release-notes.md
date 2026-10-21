Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **Better search.** Two-letter words such as "pi" or "PR" now count in `tacit_search`, and the session's own repo facts weigh more, so machine notes no longer crowd them out. Each answer ends with the `tacit_tree` call for the repo's folder.
- **A pointer to the rest.** When the session-start block shows only part of the repo's facts, a last line gives the count and the tree call (`delivery.tree_pointer`, on by default).
- **Command pushes after a runner.** A fact tied to a command now also matches when the command runs through a runner, e.g. `npx vitest` or `sudo make lint`.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
