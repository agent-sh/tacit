---
name: pkill-self-match
description: "Shell pitfalls in the agent's Bash tool"
type: unclassified
scope: user
source:
  harness: import
  host: workstation
  kind: observed
created: 2026-10-01
---

pkill -f and pgrep -f match the Bash tool's own shell because the pattern is in its command line; the call kills itself (exit 144). Use a bracket pattern like pkill -f '[w]orker.py' or kill by PID.
