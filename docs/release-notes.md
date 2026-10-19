Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release delivers more owner rulings at the prompt. Measurements are in [eval/VALUE.md](https://github.com/agent-sh/tacit/blob/main/eval/VALUE.md).

- **A ruling line.** When none of the three facts a prompt gets is an owner ruling, the best owner ruling that passed the gates (score 10 or more) is a fourth line, once per session.
- **Rare words anchor a match.** Any matched word that appears in at most 50 fact names and descriptions (and at most 2% of the facts) satisfies the coherence gate, not only the single rarest one.

In live sessions on one owner's history (24 tasks, 3 runs each), v0.3.0 scored 0.83 and this release 0.90, and decision tasks went from 0.76 to 0.91, with fewer tool calls and tokens. Offline the change was inside the noise; the live gains are on the tasks it was designed for.

Also fixed: two verifier tests now match the prompts' neutral wording from v0.3.0.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
