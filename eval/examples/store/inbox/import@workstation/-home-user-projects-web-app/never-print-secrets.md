---
name: never-print-secrets
description: "Secrets handling"
type: unclassified
scope: user
source:
  harness: import
  host: workstation
  kind: observed
created: 2026-10-01
---

Never print a secret value in an answer or a log. Write it to a file with mode 600 and give the path.
