Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **A model's safety refusal runs the fallback.** When a model's safety filter refuses a request (Claude: "safeguards flagged this session"), the configured fallback is tried once, as for capacity and outage failures. A fallback on another provider can then answer a dream window or a curation batch that would otherwise be refused at every run.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
