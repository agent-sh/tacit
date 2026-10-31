# Truth set schema

A truth set is one JSON object per line. Each line is a situation from a real session where a stored fact would have cut a detour, plus the fact itself. Retrieval is scored against this file; grep over the fact files is the baseline.

Your own set goes in `eval/truth.jsonl` (gitignored: it is mined from your transcripts). The author's set is not published. `eval/examples/truth.sample.jsonl` holds eight invented items in this schema.

```json
{
  "id": "t-0001",
  "source_id": "miner-a-0004",
  "situation": {
    "repo": "demo-api",
    "host": "workstation",
    "paths": ["internal/db/migrate.go"],
    "command": "go test ./internal/db/...",
    "intent": "one line: what the agent was trying to do"
  },
  "query": "the question the agent could have asked, or null if nobody would ask (push-only)",
  "fact": "one or two sentences: the fact that would have cut the detour",
  "fact_type": "user_standard | design_decision | past_mistake | machine_fact | code_change | hot_spot | structure | history",
  "mode": "push | pull",
  "evidence": {
    "session": "digest path or session id",
    "quote": "short quote from the digest showing the detour or the correction",
    "cost": "what it cost: N tool calls, a user correction, a wrong result, a repeated mistake"
  },
  "existing_memory": "path of a memory file that already holds this fact, or null"
}
```

Rules:

- `fact` is a conclusion someone else can act on, never a transcript or raw output.
- `mode: push` when the fact should arrive from the situation alone (file touched, host, command). `mode: pull` when only an explicit question would surface it; then `query` is required.
- `host` is the machine the session ran on, not a remote it talked to.
- A fact derivable from the code or from `git log` in one command is not a truth item (it is code reading, not memory). Hot spots, co-change pairs and structure are allowed because they are aggregates.
- Prefer situations where the detour is visible in the digest: a user correction, a repeated tool error, a rediscovery of something an older session already knew.
- Convert relative dates to absolute, full `YYYY-MM-DD`; take the day from the transcript timestamp when the text does not give it. Do not invent paths; copy them from the digest (a path may no longer exist on disk; that is history, not an error).
- `source_id` is the miner's own id, kept for tracing back to the mining slice. `id` is assigned at merge.
- Digest lines labelled `H:` are harness-injected text (notifications, command echoes, permission frames), not the owner. Never mint a `user_standard` from an `H:` line.
- `existing_memory` must name a file that states this fact, not one that merely shares a keyword. Verify by reading it.
