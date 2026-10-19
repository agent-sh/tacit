Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release is about what the dreamer keeps and what reaches each prompt. Measurements are in [eval/VALUE.md](https://github.com/agent-sh/tacit/blob/main/eval/VALUE.md).

- **Rulings keep the owner's words.** Dream prompt v5 copies the owner's exact words for each ruling, so curation can check them against the owner's own turns. On one owner's history, curation kept 254 of 374 dreamed owner notes as the owner's own rulings, against 4 of 85 before.
- **Opus by default** for dreaming, curation and verification, with Haiku as the contradiction judge. Opus dreamed more of the facts that matter than Sonnet, Haiku, GPT-6 Astra and GPT-6.1 Sol. Opus also curated better than Sonnet, mostly by not dropping facts. A 60k-character window costs about $0.27 to dream.
- **The repository from what the session works in.** A session started in the home directory or a folder of checkouts now gets its repository from the paths it works in, not from the folder's name.
- **More of the store reaches the prompt.** The index stems words, so "amended" finds "amend". Offline delivery went from 100 to 106 of 254 situations.
- **Curation and dreaming keep moving.** A batch with an invalid answer is split until the bad note is found, and it is set aside after 3 runs. One overlong transcript line is skipped, not retried forever. The dreamer's output is checked against a JSON schema.
- **The prompt selector is optional and off by default.** In live sessions, Jev did not beat the local selection, so the docs recommend leaving it off.

Upgrading: the search index rebuilds once (schema 7). Notes dreamed before v5 carry no owner quote, and curation files most of their rulings as observed. Existing `models` settings keep working; only the generic defaults changed.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
