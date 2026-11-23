Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release makes delivery answer for itself. Agents mark what they were given, the marks move what is delivered next, and the store drops what no session needs.

- **One mark per turn.** A Stop hook (Claude Code and Codex) asks once, at the end of a turn that was given tacit facts, for one `tacit_mark` call: each fact used, not relevant or wrong. It never asks twice for the same lines, never loops on `stop_hook_active`, and never asks a headless run. Mark entries keep the old `helped`/`not_helped` keys and add `wrong`.
- **Outcomes move delivery.** Marks and judge labels are tallied per repository. A fact with more bad verdicts than used ones is demoted there, one with three bad and none used stops being delivered there and stays searchable, and one marked used more than not ranks up. The owner's own lines are never moved.
- **No blocks in headless runs.** Subagents, `claude -p`, `codex exec` and throwaway runs get no start, prompt or compaction block. `TACIT_HEADLESS=1` or `0` forces either way. The MCP tools stay available.
- **Rules before the command.** The PreToolUse hook no longer delivers context. It denies a shell command once per session when a fact forbids that whole command, and lets the retry through. The command is parsed as a shell reads it, so strings, comments and here-documents never match. The hook costs a few milliseconds.
- **Repository starts.** Another organization's standards no longer start sessions of an unrelated repository.
- **An off switch.** With `~/.config/tacit/off` (or `$XDG_CONFIG_HOME/tacit/off`) present, hooks print nothing, dream, curate and maintain exit with one line naming the reason, and the MCP tools answer that tacit is off. `tacit doctor` reports it.
- **Doctor checks keys against the host name.** A machine whose host name changed fails doctor when a role it runs has no key under the new name.
- **One-time words are not rulings.** An approval to go on, a chore list or an instruction for the task in hand is never minted as a ruling, preference or decision. `tacit rulings one-time` audits the store.
- **One fact per rule.** `tacit dedupe --near` merges facts that say the same rule in other words, keeping the one with the owner's words, then the earliest.
- **Expire.** `tacit expire` retires facts whose paths are gone or whose project the registry marks dead, and notes the new path of a moved one. Facts holding the owner's words become questions instead. `tacit curate --recheck-conflicts --rulings` sends older facts a newer ruling contradicts to the verifier.
- **Quiet types.** The dreamer measures each type's yield from the local delivery log and stops writing agent facts of types that are almost never delivered. The owner's words are written whatever their type.
- **Fixes.** `tacit_add` without a name or type, curator repairs of mechanical refusals, and timers that no longer carry `Persistent=`.

Upgrading: install the new binary on every machine, then run `tacit install claude` and `tacit install codex` to add the Stop hook and the narrower PreToolUse matcher. No store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
