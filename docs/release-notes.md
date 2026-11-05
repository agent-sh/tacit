Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release lets a day's delivery log hold a full working day, and keeps the judge's entries on a full day (#155).

- **A larger day file.** The delivery log's day file now grows to 64 MiB, up from 10 MiB. On a working machine a normal day passed the old cap by early evening. Most of a day is push entries that delivered nothing, which the propensities need, and every entry after the cap was dropped. 64 MiB is several times a normal day. The default window of 31 files still bounds a runaway hook loop under 2 GiB.
- **The judge's entries keep their place.** The judge's label and pass entries may go 8 MiB past the cap, so a day the hooks filled never loses a label or a pass's record of its draw.
- **A label the log cannot take.** It counts against the judge's daily cap and as a failed try, and waits for a later pass. After three tries it is given up and named in the pass's lost and failed lines.
- **A lighter pool read.** The judge's pool decodes only prompt and push moments and keeps only their delivered lines, so a larger day costs it little.

Upgrading: install the new binary on every machine whose hooks log deliveries, and on the one that runs the dreamer's judge (each machine keeps its own log). No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
