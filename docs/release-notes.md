Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release asks agents to grade every line they were given again, and adds an optional relevance gate on the prompt block (#181).

- **The Stop hook asks for every line again.** The start block, the blocks after a compaction or a resume and the summary's selection are in the mark ask again, so every line a session was given is graded and counted. A standing line's "not relevant" still does not count against it in the outcomes.
- **An optional relevance gate.** With `gate:` set, a model scores each line of the prompt block for the request and the session's repository after the block's own selection, and the lines under the threshold are left out; nothing takes their place. Bedrock (a bearer token, or a Bedrock endpoint on this machine's loopback with the auth skipped) or an OpenAI-compatible endpoint. The deadline defaults to 1.5s; a failure or a late answer leaves the block as it was. Measured offline on graded prompt blocks: [Relevance gate](https://github.com/agent-sh/tacit/blob/main/docs/selectors.md#relevance-gate).
- **Redaction before flattening.** A text of several lines sent to a model (the gate's request, the judge's case) is masked as a whole and line by line before it is flattened, so a secret on the line after an open setting is masked. The judge's case builder is version 2, a new judge digest.
- **The blocks after a compaction** were measured against what the next compaction's summary keeps; no cut kept those lines while removing noise, so they are unchanged.

Upgrading: install the new binary on every machine whose hooks run. No store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
