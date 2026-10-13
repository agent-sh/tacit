---
name: upload-async
description: Upload endpoint design
type: project
---

Uploads return 202 and the worker queue processes them. Synchronous processing was dropped on 2026-03-02 because large files hit the 30 s gateway timeout.
