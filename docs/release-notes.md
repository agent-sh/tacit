Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release adds two ways to score what the hooks deliver: agents mark which lines helped (#143), and a judge labels a sample of delivered lines after each dream run (#145). Episode recall is faster and its clip lands where the search words meet (#146).

- **Self-marks.** About 1 in 20 prompt blocks that print an agent's note end with an ask: when the request is done, say which lines helped, through the MCP tool `tacit_mark`. The ask never blocks, and the block's log entry records the chance it had of asking. A mark counts only for a block that asked, once per block, and only for the agent's lines that block printed. Marks move no score until they are calibrated. Set `TACIT_DELIVERY_MARK_RATE=0` to turn the asks off.
- **The dreamer's judge.** After each full dream run that succeeds, a model reads a sample of the delivered prompt and push lines with hindsight: the request, the line, and what the session did next. It labels each line relevant, confused and novel.
  - Route: `claude -p` on your Claude subscription, never Bedrock. The child process keeps only a short list of environment variables, so no Bedrock, AWS or gateway setting reaches it, and it runs with no tools, hooks or MCP servers. Everything it reads is redacted first.
  - Volume: about 36 lines a day, and each line gets one draw. Each label logs its chance of being labelled, plus the model that answered and a digest of the prompt.
  - Rate limits: a rate limit skips the pass, and the next dream run tries again.
  - Settings: `TACIT_DELIVERY_JUDGE=off` turns it off; `TACIT_DELIVERY_JUDGE_DAILY` and `TACIT_DELIVERY_JUDGE_MODEL` set the cap and the model.
- **Measured.** `eval/delivery/judge_agreement.py` checked the judge against the hand grades. The rule was written before the run, and both the relevant and useful labels passed it (eval/DELIVERY.md, "Scoring each delivery", step 5).
- **Recall.** A small `find`, `around` or `show` now starts one scrump redaction instead of two, and the clip is centred where the search words meet. On the synthetic recall bench: 112 of 112 tasks right (up from 108), and 1 MiB sessions at 617 ms (down from 993 ms). Redaction still fails closed.
- **Eval tooling.** `eval/delivery/signals.py` measures cheap outcome signals against the hand grades (#141). The README shows recall against grep on synthetic sessions anyone can regenerate.

Upgrading: install the new binary. The judge needs `claude` on the PATH, logged in to your subscription, wherever `tacit dream` runs. This release changes no index, store format or protocol, and the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
