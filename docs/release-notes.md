Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release prints agents' notes as titles the agent opens on demand, where a live A/B measured it (#137), and fixes the delivery log's start entry (#136).

- **Titles.** In a harness with tacit's MCP server, the start block, the prompt block and the push before a shell command print an agent's note as a title: its name, whose it is and its type, without the description. The agent opens what it needs with `tacit_fact`, and the delivery log records the opening and each line's form. The owner's rules, rulings, statements and the notes holding their words stay whole. The start again after a compaction, the summary's selection, a resume and the push before an edit keep whole lines until an A/B covers them (#139). `delivery.titles: off` prints whole lines everywhere.
- **Measured.** A live A/B on private tasks, with the decision rule set before the run, on the build that ships: the titles arm passed at least as many tasks, no task lost two or more reps, and input tokens stayed within 10%. It cost less, and the extra tool calls were facts opened (eval/DELIVERY.md, "Scoring each delivery", step 2).
- **Delivery log fix.** The start entry logged every fact a repository ties to itself and could fill the day's file within the hour. It now holds its pool's head, and no entry more than 100 candidates besides its delivered lines.
- **A/B harness.** `eval/ab/run.py` runs a build's real hooks and MCP server in isolated arms (`hooks:<name>[+KEY=VAL...]`).

Upgrading: run `tacit install claude` again so the prompt and push hooks get `--mcp`; until then only the start block is titled. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
