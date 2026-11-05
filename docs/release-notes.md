Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release moves the dreamer's judge onto the dreamer's Bedrock route (#153).

- **Judge on Bedrock.** At the end of a full dream run, the judge calls `claude -p` on the same Bedrock route the dream run uses, with the model `global.anthropic.claude-opus-5-5` (`TACIT_DELIVERY_JUDGE_MODEL` overrides it). Previously it ran on a subscription login.
  - The call keeps only what a process needs plus the Bedrock route's settings. No endpoint, proxy, gateway key or token, custom header, other config directory, plugin directory, skipped auth or other provider's switch reaches it.
  - On the bearer or key route the call reads no AWS config file. On a profile route, a config that names an `endpoint_url` refuses the call.
- **Skipped without a route.** A machine whose dreamer runs through another gateway, with no Bedrock route, skips the judge. It draws nothing, sends no case anywhere, logs a `judge-skip` entry, and the dream run says why.
- **Errors.** Only the route's own errors end a pass without counting against the drawn lines: the CLI's API error status 401, 403, 429 or 529, or, when it gives no status, its words for a throttle, an overload or AWS credentials that do not load or have expired. Any other error counts against the lines.
- **Measured.** The Bedrock judge was measured against the hand grades under the preregistered rule (eval/DELIVERY.md, step 5, amendment). Its `relevant` labels pass; its `useful` labels do not, so they move nothing, and the offline evaluation's net value promotes nothing from this judge yet.

Upgrading: install the new binary. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
