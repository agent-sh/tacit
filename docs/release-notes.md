Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release asks for marks less often (#178).

- **About one turn in ten.** The Stop hook asked for a mark on every turn that was given facts, which cost the agent a call nearly every turn. Now each such turn is drawn on its own with a one-in-ten chance, from a hash of the session and the turn's count, so the marks are a sample of all turns rather than mostly first ones. A turn whose ask could not be logged keeps its place, so the next turn draws again.

Upgrading: install the new binary on every machine whose hooks run. No hook, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
