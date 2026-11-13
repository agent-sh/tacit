Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release quiets the hooks: a tool push keyed on a command is off by default, and a fact the session's own instruction files already carry is no longer delivered (#164, #165).

- **Command pushes are off.** A pre-tool push keyed on a command fired on every call of a program, and of the lines graded as delivered most changed nothing. A push keyed on a path the call writes still fires. `delivery.push_commands: on` (or `TACIT_DELIVERY_PUSH_COMMANDS=on`) brings the command push back.
- **Facts the instructions already carry are held.** A rule, standard, decision or past mistake whose words an instruction file covers widely stays out of the prompt block, the tool push and the area push: at least three quarters of its name and description words, three fifths of its name's, and at least four words matched. A fact the file only talks around still arrives. Derived facts are never held. The held facts show in the delivery log as `dedup`.
- **Which files count.** Each session repository's `AGENTS.md`, plus the files in `delivery.dedup_files` (a colon-separated list of paths and globs; `~/` is your home). `delivery.dedup: off` turns the gate off. A missing or unreadable file is skipped.
- **A lighter gate.** The files are read once per hook call, and only when a fact could be held. A file that names a fact's hyphenated name literally holds its name words.

Upgrading: install the new binary on every machine whose hooks deliver facts, and list that machine's instruction files in `delivery.dedup_files` (without the list only the repository's `AGENTS.md` counts). No index, store format or protocol change; the server needs no deploy.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
