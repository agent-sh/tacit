Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release makes every logged mark count, and makes the model runner read Codex profiles as Codex does (#179).

- **Every logged mark counts.** A mark's verdicts reached the outcomes only when the process that took the mark recorded them, so an MCP server still running an older binary logged marks that never moved delivery. Before the hooks read the outcomes, the delivery log's marks are now matched against the verdicts by block and fact, and the missing ones are appended under the asked block's scope. It reads at most 8 MiB of the log a call and resumes where it stopped, so no hook waits on it. A verdict already recorded is never written twice.
- **Codex profiles merge deeply.** A profile that set one nested provider key, such as a region, replaced the base configuration's whole provider table and lost its endpoint, so verify runs reached the wrong service. Profile tables now merge into the base configuration at every depth, as in Codex.

Upgrading: install the new binary on every machine whose hooks run, and restart long-running `tacit mcp` servers when convenient. No hook, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
