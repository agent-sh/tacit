Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release keeps the owner's personal content out of the repository and out of what the binary prints and sends. The docs, the eval notes, code comments, tests and a few example strings are generic now, and a CI lint keeps them so (#113, #114, #115, #116). Nothing else in behavior changes.

- **Generic examples in the binary.** The `--folders` and `--max-batches` help, the `tacit_add` schema example and two examples in the assessment prompt use generic names. The probe tool's description and the stale verifier's prompt now name the rule directories a run adds, which is what the probe reads. `--max-batches` names the batch's real limit, 45,000 characters of notes.
- **A lint for personal content.** `scripts/privacy-lint.py` fails on words attributed to the owner with a date, Tailscale addresses and tailnet names other than placeholders, home directories of real users (also as Claude Code encodes a project folder), and the words of a private deny list (`--deny FILE`, `TACIT_PRIVACY_DENY`, or privacy-deny.txt in tacit's config directory; the list is never committed). It checks tracked paths and contents, and prints file, line and rule, never the text. CI runs it on every pull request, every push to main and once a day. AGENTS.md states the rule: personal content never enters the repository, fixtures are synthetic, and evidence stays on the machine that made it.
- **Docs and eval notes.** SPEC.md, the README and docs/ state each rule and mechanism without the measurements of one owner's store. eval/VALUE.md and the other eval notes keep each measurement's question, method, decision rule, the direction of its result and the decision; the numbers measured on that store stay with its owner.

Upgrading: no index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
