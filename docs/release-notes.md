Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release fixes the dreamer's judge, which has recorded no label since v0.9.32 (#157).

- **The judge reads its answer.** The call read the CLI's output before the CLI had run, so every answer was empty and every pass failed with `the judge's answer is no JSON: unexpected end of JSON input`. After three such passes a batch of drawn lines was given up. The call now runs first and its output is read after.
- **Refusals are seen again.** A throttle the CLI printed on stderr was missed for the same reason, so a rate limit counted against the drawn lines' tries. It ends the pass without counting again. Refusal words are read from the CLI's own text, never from the model's answer.
- **Failures that are not the lines' hold them.** A call that cannot start (no `claude` on `PATH`, a model the route does not know), one the pass's deadline ends, or one whose CLI prints no result counts no try. Such a line is given up after 12 held calls, two days of dream runs. The deadline now ends the CLI's whole process tree.
- **More answer forms.** The CLI's result is found in its one JSON object, in the last result event of an array of events, or in the last result line after other output. A success result counts whatever the CLI's exit after it. The answer is the first JSON object with a `lines` key that has three valid words for every case, whatever prose, code fence or example is around it.
- **Why a call gave no labels.** A `judge-pass` entry now carries `answer_error`, a class and never the answer's text: `setup`, `timeout`, `no-output`, `call`, `declined`, `empty`, `truncated`, `prose`, `shape` or `other`. The next failure of this kind shows in the delivery log.

Upgrading: install the new binary on the machine that runs the dreamer's judge. No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
