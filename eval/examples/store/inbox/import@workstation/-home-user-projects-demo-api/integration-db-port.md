---
name: integration-db-port
description: "Integration test setup for demo-api"
type: unclassified
scope: projects/demo-api
source:
  harness: import
  host: workstation
  kind: observed
created: 2026-10-01
---

Run make db-up before the integration tests. The test database listens on port 55432, not 5432. The compose file is deploy/test-db.yml.
