#!/usr/bin/env bash
# Build the release in Docker on the target host and install it under systemd.
#
#   deploy/deploy.sh root@203.0.113.10
#
# Steps on the host: rsync the source to /opt/tacit/src, docker build the
# release stage, extract to /opt/tacit/rel.new, swap into /opt/tacit/rel,
# install the unit, restart, check /healthz over the tailnet IP.
set -euo pipefail

HOST="${1:?usage: deploy.sh user@host}"
BUILDER_IMAGE="${BUILDER_IMAGE:-hexpm/elixir:1.20.4-erlang-28.5.0.7-ubuntu-noble-20260917}"
SRC="$(cd "$(dirname "$0")/.." && pwd)"

ssh "$HOST" mkdir -p /opt/tacit/src
rsync -az --delete \
  --exclude _build --exclude deps --exclude .test-data --exclude .elixir_ls \
  "$SRC/" "$HOST:/opt/tacit/src/"

ssh "$HOST" bash -s "$BUILDER_IMAGE" <<'REMOTE'
set -euo pipefail
BUILDER_IMAGE="$1"
cd /opt/tacit/src

id tacit >/dev/null 2>&1 || useradd --system --home /var/lib/tacit --shell /usr/sbin/nologin tacit
install -d -o tacit -g tacit -m 0750 /var/lib/tacit /var/lib/tacit/tmp
# Retired: the pre-receive hook no longer keeps a log (the journal covers accepted pushes).
rm -f /var/lib/tacit/hook.log

docker build --quiet --build-arg BUILDER_IMAGE="$BUILDER_IMAGE" --target release -t tacit-server-release:latest -f Dockerfile.release . >/dev/null
rm -rf /opt/tacit/rel.new
cid=$(docker create tacit-server-release:latest /nonexistent)
docker cp "$cid:/tacit_server" /opt/tacit/rel.new
docker rm "$cid" >/dev/null
chown -R root:root /opt/tacit/rel.new
chmod -R a+rX /opt/tacit/rel.new
# The distribution cookie must not be readable by other users on this shared host.
chown root:tacit /opt/tacit/rel.new/releases/COOKIE
chmod 0640 /opt/tacit/rel.new/releases/COOKIE

install -m 0644 deploy/tacit-server.service /etc/systemd/system/tacit-server.service
systemctl daemon-reload
systemctl stop tacit-server.service 2>/dev/null || true
# A stale epmd from an earlier run may be bound to all interfaces; the release restarts it on loopback.
for e in /opt/tacit/rel/erts-*/bin/epmd; do [ -x "$e" ] && "$e" -kill >/dev/null 2>&1 || true; done
rm -rf /opt/tacit/rel.old
[ -d /opt/tacit/rel ] && mv /opt/tacit/rel /opt/tacit/rel.old
mv /opt/tacit/rel.new /opt/tacit/rel
systemctl enable --now tacit-server.service
sleep 2
systemctl is-active tacit-server.service
ip=$(tailscale ip -4)
curl -fsS "http://$ip:7447/healthz" && echo
# The public listener answers on loopback and refuses a request without a key.
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:7448/healthz")
[ "$code" = 401 ] || { echo "public listener on 127.0.0.1:7448 answered $code, want 401" >&2; exit 1; }
echo "public listener: 401 without a key"
REMOTE
