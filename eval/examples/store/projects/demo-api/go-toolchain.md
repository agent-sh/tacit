---
name: go-toolchain
description: "demo-api pins its Go toolchain in go.mod; do not lower it"
type: design_decision
scope: projects/demo-api
source:
  harness: curator
  host: workstation
  kind: observed
created: 2026-10-01
---

The toolchain line in go.mod is the supported version.
