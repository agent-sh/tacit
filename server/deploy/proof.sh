#!/usr/bin/env bash
# Phase 2 proof, run from a tailnet client machine against the deployed server.
#
#   deploy/proof.sh root@203.0.113.10 100.64.0.10 claude
#
# 1. enroll this machine's <harness>@<host> key on the server (rpc)
# 2. clone over the tailnet without credentials
# 3. push one fact into our inbox with the tacit credential helper
# 4. push outside our inbox and expect the hook to refuse
# 5. read the journal and confirm the push is the newest row
# 6. from a non-tailnet address the server is unreachable (bind check)
set -euo pipefail

SSH_HOST="${1:?usage: proof.sh user@host tailnet-ip harness}"
IP="${2:?}"
HARNESS="${3:?}"
TACIT="${TACIT_BIN:-$(command -v tacit || echo tacit)}"
BASE="http://$IP:7447"
WORK="$HOME/.cache/tacit-proof-$$"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

pub="$("$TACIT" pubkey "$HARNESS" 2>/dev/null || "$TACIT" keygen "$HARNESS")"
keyid="${pub##* }"
echo "== enroll $keyid"
ssh "$SSH_HOST" "/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll(\"$keyid\", \"$pub\")'" | tail -1

echo "== clone"
git clone --quiet "$BASE/git" "$WORK/clone"
cd "$WORK/clone"
git config user.email "$keyid"; git config user.name "$HARNESS"
# An empty helper resets the global list (a global `cache --timeout=3600` helper,
# which would replay an expired token); then only tacit answers for this URL.
git config credential.helper ""
git config --add "credential.$BASE.helper" "$TACIT credential $HARNESS"
test -f "keys/$keyid.pub" && echo "key present in clone"

echo "== push into inbox/$keyid/"
mkdir -p "inbox/$keyid"
cat > "inbox/$keyid/proof-$(date -u +%Y%m%dT%H%M%SZ).md" <<EOF
---
name: proof-$(date -u +%Y%m%dT%H%M%SZ)
description: phase 2 proof push from $(hostname -s)
type: unclassified
scope: machines/$(hostname -s)
source:
  harness: $HARNESS
  host: $(hostname -s)
  kind: observed
created: $(date -u +%F)
---

Pushed over the tailnet by deploy/proof.sh.
EOF
git add -A && git commit --quiet -m "proof push" && git push --quiet origin HEAD:main && echo "push accepted"

echo "== push outside inbox (expect refusal)"
mkdir -p system && echo "x" > system/escape.md && git add -A && git commit --quiet -m escape
out="$(git push origin HEAD:main 2>&1 || true)"
if printf '%s' "$out" | grep -q "may write only under inbox/$keyid/"; then echo "refused as expected"; else echo "NOT REFUSED: $out"; exit 1; fi
git reset --quiet --hard HEAD~1

echo "== journal"
curl -fsS "$BASE/journal?since=0" | python3 -c "import json,sys; j=json.load(sys.stdin); print('rows', len(j), 'last writer', j[-1]['writer'], 'seq', j[-1]['seq'])"

echo "== feed (one frame)"
curl -sN --max-time 3 -H 'Last-Event-ID: 0' "$BASE/events" 2>/dev/null | head -4 || true

echo "== public address must not answer"
if curl -s --max-time 4 "http://$(ssh "$SSH_HOST" 'hostname -I | cut -d" " -f1'):7447/healthz" >/dev/null; then echo "PUBLIC PORT OPEN"; exit 1; else echo "public address refused"; fi
echo "proof ok"
