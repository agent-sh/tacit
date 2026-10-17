Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release is about which facts reach each prompt.

- **Choose a model to judge each prompt's facts (optional).** The utility selector asks a model whether each candidate fact adds something useful for the request, beyond topic overlap. Providers: `jev` (TypeSafe, recommended), `bedrock` (any Amazon Bedrock model; Amazon Nova Micro is the measured option for data that stays in AWS), `systemone` (any System One-compatible server, such as a self-hosted Strands Decider), and `openai` (any OpenAI-compatible chat endpoint). Each provider reads only its own key and sends it only to its own endpoint. A failed or slow model falls back to the lexical selection. [Setup and benchmark](https://github.com/agent-sh/tacit/blob/main/docs/selectors.md).
- **Code before the model.** A prompt that only asks for a text operation on a literal it supplies gets no facts and no model call. Three other code rules were measured and not shipped; the results are in eval/DELIVERY.md.
- **Stricter prompt delivery.** Facts need coherent matches in their name and description, unsolicited facts respect repository restrictions, and weighted relationship evidence can route a fact from a related project.
- **Evaluation tools.** Offline selection and CPU scorer tools are in `eval/selection` and `eval/cpu`, for measuring a selector on your own labelled prompts.
- **macOS.** The client test suite passes on macOS.

Upgrading: existing `utility.provider: jev` settings keep working unchanged. Every other provider needs its own `utility.threshold`.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
