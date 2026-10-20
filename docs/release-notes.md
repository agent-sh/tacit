Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **`stale_workers`** (`TACIT_STALE_WORKERS`) sets how many facts the curator's stale step checks at once. The step runs its own git commands for each fact, tens of thousands per run on a large store; set `1` on a machine that does other work. The default stays 4.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
