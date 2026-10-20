Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release brings a session's facts into the store within the hour instead of up to a day later. Measurements are in [eval/VALUE.md](https://github.com/agent-sh/tacit/blob/main/eval/VALUE.md).

- **Dreaming as a session goes:**
  - The prompt hook starts a dream of the session as soon as its new text reaches about one dream window.
  - A pre-compact hook dreams the session before its context is compacted.
  - `tacit dream --idle` (a 10-minute job) dreams sessions idle for 20 minutes or compacted, which catches sessions closed with Ctrl-C or a crash.
  - Every dream reads only from where the last one stopped. The 4-hour dream stays as a backstop.
- **Quick curation.** `tacit curate --quick` turns the last day's new notes into facts, with the rulings and conflict checks, at most every 30 minutes. The whole-store steps (stale, derive, results, links) stay in the twice-daily full curate.
- **One job at a time.** Dreams and curates take locks: the scheduled dream and the full curate wait for them, and idle runs step aside. A window whose model call fails is retried by the scheduled run only, so it can't become a retry loop.
- **Quiet where there is nothing to do.** On a read-only clone, or without a dreamer or curator key, the new hooks and jobs do nothing.

Upgrading: rerun `tacit install dream` to add the idle job, and `tacit install claude` / `tacit install codex` for the pre-compact hook.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
