---
name: integration-db-port
description: Integration test setup for demo-api
type: project
---

Run make db-up before the integration tests. The test database listens on port 55432, not 5432. The compose file is deploy/test-db.yml.
