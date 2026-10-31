Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release fixes how `tacit verify` and the Codex runner take a stream to the provider that drops (#119), and describes one workspace layout generically (#120).

- **A dropped stream fails only its batch.** A verifier batch's marks are pushed when the batch ends, so a Codex stream that drops in another batch (`stream disconnected before completion: Transport error: ...`) loses nothing settled. A `tacit verify` run whose only errors were such drops, and that settled or asked a finding, now exits 0 and says so. The findings left stay open for the next run. With a drop, a run that settled and asked nothing, a dry run, and a run with any other error, before or after the drop or in a stale batch, still exit 1. A finding left without a verdict it can take, or with a mark the page refused, still counts against that finding and fails no run. A drop is no reason to fall back, as before.
- **A Codex retry notice is not a failure.** Codex prints each stream retry as an `error` event (`Reconnecting... 1/5 (...)`) and goes on. Tacit failed the call at the first one, so an answer Codex recovered and gave was lost, in every job routed to Codex. Now the turn's own end decides. A failed turn reports its own error, not the first notice, so a capacity failure after a retry falls back as configured.
- **Generic wording.** The README, SPEC.md, the eval notes, code comments and tests describe a workspace whose packages are each a checkout under `src/` by that shape, as a multi-package workspace.

Upgrading: no index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
