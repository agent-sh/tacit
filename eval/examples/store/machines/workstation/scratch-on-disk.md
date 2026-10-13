---
name: scratch-on-disk
description: "Scratch files go under ~/.cache, not /tmp"
type: machine_fact
scope: machines/workstation
source:
  harness: curator
  host: workstation
  kind: observed
created: 2026-10-01
---

/tmp is a RAM-backed tmpfs on the workstation; scratch goes under ~/.cache.
