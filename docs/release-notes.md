Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release bundles scrump 0.2.1, whose start-up is now measured in milliseconds (#150, #151), and adds an offline evaluation of delivery policies (#149).

- **scrump 0.2.1.** The bundled redactor, and the one the Mac setup builds, moves from scrump 0.2.0 to 0.2.1. It compiles a rule only where one of its keywords occurs, so start-up on empty input drops from about 0.29 s to 0.012 s, with byte-identical output on scrump's differential suite. Every `tacit episode` call and every redacted write starts scrump, so this is their latency floor. Installing v0.9.30 puts scrump 0.2.0 back; installing this release brings 0.2.1. Building scrump from source needs rustc 1.89.
- **Offline evaluation.** `eval/delivery/offline.py` grades a delivery policy on past moments without running it, as useful and noise lines per moment.
  - Inputs: the hooks' logged delivery probabilities, and the dreamer's judge labels with the probability each line had of being judged.
  - Estimators: inverse propensity, its self-normalized form, and a doubly robust estimate, each with its support and bootstrap intervals over sessions.
  - The rule was written before any data (eval/DELIVERY.md, "Scoring each delivery", step 6). An offline result never ships a ranking change; at most it promotes a candidate to a live interleaving test.
- **Judge log.** Each judge pass now logs its pool's window, the models that answered, the moments it could not read, and why each drawn line came to no label. The offline evaluation needs these to know which moments every pass covered. A pass now also checks that each transcript opens before it draws.

Upgrading: install the new binary. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
