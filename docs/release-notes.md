Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release lets the dreamer's judge run on a machine that reaches Bedrock through a local service, and makes Fable 5.2 its default model.

- **The loopback route (#161).** The judge ran only where its environment held a bearer token or AWS credentials. A machine that keeps the token in a local service, and points Claude Code at that service's loopback endpoint with the auth skipped, ran the dreamer but skipped the judge with `no Bedrock route here`. That is now a route of its own: `CLAUDE_CODE_SKIP_BEDROCK_AUTH` on, and `ANTHROPIC_BEDROCK_BASE_URL` an `http` or `https` address of 127.0.0.0/8 or `::1`. The host is written as an address; no name counts, `localhost` included. On this route the judge drops every credential, so the endpoint is sent none. An endpoint anywhere else is refused as before.
- **Fable 5.2 is the default judge.** The judge's model defaults to `global.anthropic.claude-fable-5-2`. A changed model is a new judge, so it was measured under the rule before the default changed (eval/DELIVERY.md). A `TACIT_DELIVERY_JUDGE_MODEL` setting that names this model can be removed.
- **The same note twice (#162).** A finding's notes were made one by their time and text, so the same words sent twice within a second showed as one note. A note is now its record in the decision log.
- **The offline eval reads one judge.** With two judges under one prompt, a pass that logged no label was counted for both. It now counts only for the judge whose model the pass asked.
- **Tests on an older git (#160).** One test counted git commands as a git from 2.44 runs them and failed on an older one.

Upgrading: install the new binary on every machine that runs the dreamer. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
