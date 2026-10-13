#!/usr/bin/env bash
# Turn the public path on or off: Tailscale Funnel on the server host,
# public port 443 -> the server's loopback-only public listener (7448).
#
#   deploy/funnel.sh root@203.0.113.10 on      # enable
#   deploy/funnel.sh root@203.0.113.10 off     # disable
#   deploy/funnel.sh root@203.0.113.10 status
#
# The public listener takes only reader keys on every route and refuses every
# push (server/lib/tacit_server/auth/plug.ex, git/http.ex), so what Funnel
# exposes is "signed readers fetch". The tailnet listener (7447 on the
# tailnet IP) is not touched. Needs the tailnet policy to allow Funnel for
# this node (the `funnel` node attribute); `tailscale funnel` says so when it
# does not.
#
# Funnel is allowed per host:port, not per mount: every handler on 443 goes
# public with it. So `on` reads the node's serve config and refuses unless
# 443 serves nothing yet or exactly "/" -> the public listener
# (deploy/funnel_guard.py, tested in test/tacit_server/funnel_guard_test.exs).
# `off` removes the 443 Funnel and its handler.
set -euo pipefail

HOST="${1:?usage: funnel.sh user@host on|off|status}"
ACTION="${2:?usage: funnel.sh user@host on|off|status}"
PUBLIC_PORT="${TACIT_PUBLIC_PORT:-7448}"
TARGET="http://127.0.0.1:$PUBLIC_PORT"
HERE="$(cd "$(dirname "$0")" && pwd)"

case "$ACTION" in
  status)
    ssh "$HOST" tailscale funnel status
    ;;
  on)
    # The listener must be up and must refuse a request without a key first.
    code=$(ssh "$HOST" curl -s -o /dev/null -w '%{http_code}' "$TARGET/healthz")
    [ "$code" = 401 ] || { echo "$TARGET/healthz on $HOST answered $code, want 401: deploy the server first" >&2; exit 1; }
    name=$(ssh "$HOST" tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')
    [ -n "$name" ] || { echo "cannot read the node's DNS name" >&2; exit 1; }
    ssh "$HOST" tailscale serve status --json | python3 "$HERE/funnel_guard.py" "$name:443" "$TARGET"
    ssh "$HOST" tailscale funnel --bg --https=443 "$TARGET"
    ssh "$HOST" tailscale funnel status
    echo "check from off the tailnet: curl -s -o /dev/null -w '%{http_code}' https://$name/healthz   (want 401)"
    ;;
  off)
    ssh "$HOST" tailscale funnel --https=443 off
    ssh "$HOST" tailscale funnel status || true
    ;;
  *)
    echo "usage: funnel.sh user@host on|off|status" >&2
    exit 2
    ;;
esac
