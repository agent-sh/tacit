# tacit server

Owns the bare store repository and the order of its pushes. Contract: `../SPEC.md`. Design: `../docs/PLAN.md` step 2.

- Git smart HTTP (`/git/...`) by driving `git upload-pack` and `git receive-pack --stateless-rpc`.
- Reads need a tailnet identity (tailscaled LocalAPI WhoIs). Writes also need a signed token for `<harness>@<host>`; the harness must be on the list in `lib/tacit_server/auth/key_id.ex`. A `reader@<host>` key reads and never pushes.
- The public path (`TACIT_PUBLIC_PORT`, loopback only, reached through Tailscale Funnel): a reader key token on every route, one 401 body for every failure, no push for any key, feed streams end after 300 s (SPEC.md "Public path").
- Every push runs under one FIFO lease and the pre-receive hook (`priv/hooks/pre-receive`): inbox scope, no merges, symlinks or secrets.
- `state.db` holds the journal (`/journal`, `/events`) and replay nonces. It is disposable: at start the journal is reconciled with the repo and rebuilt if the file is missing or damaged (`lib/tacit_server/state/rebuild.ex`).

## Test

```
mix test
```

Needs `git` and a free port 17447. Tests start the real server on loopback and use a real git client.

## Deploy

`deploy/deploy.sh root@host` builds the release in Docker on the host and installs `deploy/tacit-server.service`. `deploy/funnel.sh root@host on|off|status` turns the public path on (Funnel 443 -> 127.0.0.1:7448) or off; it refuses unless 443 serves nothing yet or exactly / -> 127.0.0.1:7448 (`deploy/funnel_guard.py`). `deploy/proof.sh` runs the phase 2 proof from a tailnet client.

Enroll a key (on the host): `/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll("claude@workstation", "ssh-ed25519 AAAA...")'`
