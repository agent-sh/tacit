Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **A machine-local layer.** A machine whose store clone is read-only can now remember its own sessions without sending anything to the shared store. Turn it on with `local_layer: on`; it is off by default.
  - Its sessions are dreamed and curated into its local overlay: the rulings check against the owner's turns on this machine, conflicts and duplicates against both layers.
  - The results are delivered on this machine only, marked "(this machine)".
  - A local fact that passes the same evidence check as the shared store's is a ruling on this machine.
  - Nothing of it is written to the clone or pushed.
- **Clearer curator failures.** A split or rulings answer that fails validation is logged by entry, field and rule, never by its text. A scope whose repo or host is written in capitals is lower-cased instead of rejecting the whole answer.

Upgrading: to use the local layer on a read-only machine, set `local_layer: on` and rerun `tacit install dream`. SPEC.md "Local layer" has the commands to turn it off again.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
