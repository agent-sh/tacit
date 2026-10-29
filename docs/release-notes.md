Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release fixes four faults the Mac's local-layer rebuild ran into. Each one held work back without saying so.

- **A refused batch no longer blocks its notes.** When the model's safety filter refused a curate batch, tacit took it as a failure of the route. It backed off, and the next run sent the same batch again, so those notes were never curated. A refusal is about the notes, so the batch is now split like a malformed answer. The note refused alone is counted against that note and set aside after its strikes. The route keeps working for every other note.
- **The local reviewer keeps its verdicts.** On a machine with the local layer, an imported memory sits in its agent's folder (`local/claude@<host>/memory/...`). When the reviewer trimmed one, it wrote the trimmed note there, the overlay refused that path, and the whole step's verdicts were lost with it. The trimmed note is now written beside its note (`overlay.WriteNote`, which writes only into an agent's folder). A file the step cannot write is reported, and the verdicts are written all the same.
- **The local rotation stays in its layer.** When the model compressed facts on the local layer, it could name an agent's note as a member, and the write went to `curated/local/claude@...`, which failed the batch. Only the layer's own facts are compressed now. A member deleted since it was read, for example by dedupe, is skipped instead of failing the batch.
- **The reviewer line counts the notes it passes over.** Notes older than the reviewer's start are judged only with `--backfill`. The line now says how many there were (`N older than the reviewer (--backfill reads them)`), so "0 judged" no longer looks like nothing to do.

Upgrading: no index or store format change. An older binary reads what this one writes.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
