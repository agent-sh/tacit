Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release lets a session recall its own earlier history after a compaction (#130, #133, #134), and adds a private delivery log for scoring what the hooks deliver (#132).

- **Recall: `tacit episode` and the MCP tool `tacit_episode`.** After a compaction an agent loses ids, commands, paths and errors its session had. Recall reads the session's own transcript on this machine and answers an index first. `find <words>` lists one line per event, newest first, and pages back with `before`. `around <ref>` shows the events on each side of one. `show <ref>` shows one event whole. Claude Code, Codex and pi sessions are read; `session` names another session of this machine.
- **Redacted, fail closed.** Every answer is redacted by scrump; without it nothing is shown. `find` matches only redacted text, so it shows and counts nothing a secret alone holds. The session's own recall calls are left out, and tacit's own model runs are never read.
- **From a fact to its moment.** `fact` reads the session a fact came from, at its words. The dreamer now keeps, on this machine only, the part of the transcript each note was dreamed from (`dream-ranges.jsonl` in the state directory), so a fact made from a note finds its words in that part. A transcript on another machine is named by its host.
- **Delivery log.** Each hook moment's candidates, their features, what was shown and its probability, and the agent's steps through the store, as JSON lines in `delivery/<day>.jsonl` under the state directory. It is private: never in the store and never sent. It keeps a request's terms only when the store's index holds them, never the request's text. Turn it off with `delivery.log` or `TACIT_DELIVERY_LOG`.
- **Measured.** Replayed offline on real sessions, recall got back about as many targets as agents got by reading their transcripts by hand, in one call and a capped, redacted answer. A call takes about half a second on a short transcript (eval/VALUE.md).

Upgrading: no index, store format or protocol change; the server needs no deploy. The MCP server lists `tacit_episode` on its own; run `tacit install` again only if you skipped it for v0.9.27.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
