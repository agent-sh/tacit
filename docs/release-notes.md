Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release tightens prompt delivery, retries curator failures without a quick-run retry loop, and lets the delivery judge read Hermes sessions directly from their message store (#167, #168).

- **A closer prompt match.** Common title words no longer make a fact relevant by themselves: title coverage needs a rare term too. A selected block drops a semantic restatement of another line, while distinct numbered facts, curated statements and owner evidence stay protected. The delivery log records these drops as `near_duplicate`.
- **Hook latency.** Prompt, start and push receipts can carry `elapsed_ms`, measured from opening the index through writing the block's log.
- **A quick failure waits for the full curator.** A note-specific lone quick-curate failure is held for the full run instead of being retried by every quick run. Full runs keep their strike and set-aside rules. The local layer follows the same rule.
- **Automated Hermes prompts search their request.** With no repository or workspace, a non-primary Hermes context searches the final 600 runes of its prompt rather than a repeated scheduling or delegation preamble. The full prompt still controls eligibility and repository detection. Session and agent-context metadata now reach the hook in primary and automated contexts.
- **Hermes deliveries can be judged.** Receipts record a Hermes session and message-id boundary. The existing sampled judge reads the session through the shared Hermes event reader, with gateway frames, tool calls and compaction handled as in the dreamer. The offline evaluator accepts the same receipts when their boundary is valid and their block was not unread.

Upgrading: install the new binary on every machine whose hooks deliver facts or whose curator or dreamer runs. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
