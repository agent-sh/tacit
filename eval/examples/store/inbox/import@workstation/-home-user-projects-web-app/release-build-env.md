---
name: release-build-env
description: "Release build notes"
type: unclassified
scope: projects/web-app
source:
  harness: import
  host: workstation
  kind: observed
created: 2026-10-01
---

npm run build must run with NODE_ENV=production. A dev build shipped once and doubled the bundle size.
