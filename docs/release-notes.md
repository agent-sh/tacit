Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release closes a gap in how questions on a name two files hold are closed (#126) and fixes a flaky test (#125).

- **A moot question on a name is closed by code.** When a file of a name two files hold is later retired, replaced by another fact or gone, an agent's question on it is closed as moot by the curator, with the reason, as the other moot closures are (`tacit questions moot` runs it alone; `tacit verify` runs it before it sends anything). The owner's questions and any with a note of theirs stay theirs, a file that failed to parse closes nothing, and a closure whose files are both live again is reopened by the curator unless the owner answered first.
- **A test no longer depends on a wall clock.** The PreToolUse hook still prints nothing after 400 ms. The tests of what it prints now run it under a minute, so a loaded machine or the race detector cannot turn their answer into the empty one. Nothing in behavior changes.

Upgrading: no index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
