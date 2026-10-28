Tacit keeps memory for coding agents in Markdown and git. Use a local store or share one across machines.

This release clears folder overviews that lint reports as broken, and stops new ones from piling up.

- **Overviews that are no longer due are removed.** A folder overview is generated, like the folder index. A folder gets one only while it has a live record and at least 3 live facts. When every fact of a folder was retired, or its record went away, the overview stayed. It went on citing facts that were gone, and lint L19 reported it. On every full curate, the overview step now removes these first: no model call, at most 50 a run, in one commit. Facts are never removed, and git history keeps the old overviews.
- **Broken overviews are rewritten first.** An overview that cites a fact the store no longer has (a removed derived fact, such as a module map or a results fact) is now the first one rewritten in a run. Before, the order added a tier to the share of changed facts, and a share can pass 1. On the rig, projects/hqmtp came 35th, so the limit of 10 a run never reached it. The tier now decides alone: broken overviews first, then forced changes, then folders with no overview, then the rest. If the model's rewrite is refused twice, the overview is removed, and a later run writes the folder a new one.
- **The curator removes only generated files.** The curator's locked write can now remove files. It removes only a folder's `_overview.md` or `_index.md` under a scope root. It refuses any other removal (a fact, a record, `derived/`, the inbox) before it writes anything.

Measured on a read-only copy of the rig's store (2026-10-28), with a fake model for the rewrites, pushed through the server's real pre-receive hook as the curator:
- **Lint.** Before: 7 L19 findings in 4 overviews (recipe-lab, tiyuvta-gateway and valkey-glide, whose folders have no live facts, and hqmtp, which cites a results fact that was removed). After one overview step: 0 findings of any kind.
- **What changed.** One commit removed 12 overviews: 11 of folders with no live facts and 1 (relay) with 2 live facts. projects/hqmtp was rewritten first, and 7 more overviews after it. The diff from the store's head deletes those 12 `_overview.md` files and nothing else.
- **Delivery.** The 276-case replay gives this release's output byte for byte the same as 0.9.12's.

Upgrading: no index or store format change. The full curate's line is now `curate: overviews: N written, N removed, N error(s)`. A removal commit is `curator: overview: removed N stale overview(s): <folder> (<why>), ...`. An older binary reads the store as before. It leaves a stale overview in place but does not need it.

Builds for Linux, macOS and Windows on amd64 and arm64 include the CLI, the browser app and a compatible scrump redactor. Git 2.29+ is required. [Setup and platform validation](https://github.com/agent-sh/tacit/blob/main/docs/portable.md).
