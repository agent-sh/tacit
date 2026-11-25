Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release delivers fewer prompt lines and asks agents to grade only the lines given for their turn (#180).

- **Higher prompt floors.** A ranked prompt line now needs a score of 30 in a known repository and 25 without one (were 15 and 20). On graded prompt lines, the agents' marks and the dreamer's judge, lines under those scores were mostly noise; in a repository the repository's own rules and decisions still reach the session through the start block and the repository boost. The compaction summary's selection keeps 15 and 20.
- **The metadata line is off by default.** The prompt block's line for a fact whose `applies_to` paths or commands the prompt names was nearly all noise on graded blocks. `delivery.prompt_metadata: on` brings it back.
- **The Stop hook asks only for the turn's own lines.** It asked for a mark of every line a hook printed, the start block and the blocks after a compaction or a resume included, which are background for the session. It now asks for the prompt block's lines and a deny's. The summary's selection after a compaction counts as standing in the outcomes, like the start block.

Upgrading: install the new binary on every machine whose hooks run. No store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
