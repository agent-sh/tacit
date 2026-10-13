---
name: node-version
description: "web-app needs the Node version in .nvmrc"
type: machine_fact
scope: projects/web-app
source:
  harness: curator
  host: workstation
  kind: observed
created: 2026-10-01
---

Use the version in .nvmrc; older Node versions fail the build.
