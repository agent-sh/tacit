Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **Hooks stay fast while the local layer changes.** A change to the machine's local layer no longer rebuilds the whole search index: the store's index is kept per commit and the local notes are added to a copy, in well under a second. Hooks never rebuild while you wait; they answer from the closest existing index and start one build in the background, so a new local note shows up a prompt or two later. Only one process builds a given index at a time.
- **Sync and follow** build the index under their own longer budget, the store part first.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
