Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

- **One repo identity.** A path names a repo only through a real git root. Symlinks, worktrees and clones whose origin matches a checkout resolve to that checkout, and their names become aliases. Folder names alone no longer make a repo; folders outside home or in the temp dir count once two sessions edited or committed there.
- **A browsable store.** Every curated folder has a generated `_index.md`. `tacit tree` and the MCP tool `tacit_tree(path, depth)` show the folders a session may read, from the root (depth 2) or any folder (depth 3).
- **Metadata is checked when it is written.** Scope, `applies_to` repos and paths, the fact type and owner-ruling wording are checked in the curator, `tacit add` and `tacit_add`. A refused curator answer goes back to the model once with the reasons; code never rewrites it. Absolute paths inside the fact's repo are made repo-relative. Notes about a repo not yet known wait in the inbox for up to 14 days.
- **Machines share their repo catalogs** through the store (names, aliases and remote URLs, no paths). A local-layer machine keeps its own catalog local.
- **Repo-first delivery.** Session start brings the repo's rules and decisions; the prompt block keeps up to 5 lines close to the best match; a short push before an edit or command brings a fact tied to that file or command. `tacit_search` filters by scope, type, rulings, path and command. `tacit_list` is replaced by `tacit_tree` and `tacit_search`.
- **`memory_md: off`** stops generating the flat `MEMORY.md`. It stays on by default.

Upgrading: upgrade every machine that reads the store before the writer's next curate. Older binaries read `_index.md` as a broken fact. Rerun `tacit install claude` and `tacit install codex` to add the PreToolUse hook.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
