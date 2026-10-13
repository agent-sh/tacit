---
name: upload-async
description: "Upload endpoint design"
type: unclassified
scope: projects/demo-api
source:
  harness: import
  host: workstation
  kind: observed
created: 2026-10-01
---

Uploads return 202 and the worker queue processes them. Synchronous processing was dropped on 2026-03-02 because large files hit the 30 s gateway timeout.
