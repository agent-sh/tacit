#!/usr/bin/env bash
# First run of tacit on a Mac (Apple Silicon, macOS), one script, safe to rerun.
#
#   scripts/mac-setup.sh --server URL [--read-only] [--with-dream] [--no-app] [--dry-run]
#
# Writer mode (the default, for a Mac on the tailnet): checks the tools and
# the server, pulls and builds tacit and tacit-app, creates the writer keys
# and prints the line that enrolls them on the server, waits until the server
# takes them, clones the store, and installs the Claude Code hooks (and
# Codex's when ~/.codex exists), the follower (launchd) and
# ~/Applications/tacit.app, which is view only here: the Mac never holds the
# curator key, so marks and edits stay on the curator's machine.
#
# --read-only (a Mac that cannot join the tailnet): the store comes over the
# server's public path (Tailscale Funnel, https://<server>.<tailnet>.ts.net),
# which asks a key on every request and never takes a push. Creates the key
# reader@<host>, prints its enroll line, waits until the server takes it,
# clones with `tacit clone --read-only --as reader`, and installs the hooks, the follower
# and the app (view only). Nothing is ever pushed from this Mac. What the
# agents here add (tacit add, the MCP tool) stays in this Mac's local
# overlay (~/.local/share/tacit/local), redacted with scrump, shared by the
# harnesses here. Never a dreamer.
#
# --with-dream: also the dreamer every 4 h (writer mode only; needs the selected runner,
# scrump and the selected runner's provider variables in --env-file). The curator and
# the verifier run on the curator's machine only and are never installed here.
#
# No sudo. Nothing secret is printed: only public keys and paths.
set -euo pipefail

server="${TACIT_SERVER:-}"
# Where the enroll command runs (user@host for ssh). Unset: the script prints
# the server-side command for the operator to run there.
enroll_ssh="${TACIT_ENROLL_SSH:-}"
repo_url="https://github.com/agent-sh/tacit.git"
# scrump at the commit that added its text profile (avifenesh/scrump#40); no
# release has it yet. Bump deliberately, after checking redaction still holds.
scrump_rev="17cd5abc9ce19d4fdec51588a35ef73a644e36bb"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo="${TACIT_REPO:-$here}"
ref="main"
read_only=0
with_dream=0
with_app=1
dry_run=0
wait_s=900
env_file="$HOME/.config/tacit/env"
bin_dir="$HOME/.local/bin"
store="${TACIT_STORE:-$HOME/.local/share/tacit/store}"

usage() {
  sed -n '2,28p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  cat <<'EOF'
Options:
  --server URL        tacit server, required (or $TACIT_SERVER): its tailnet address,
                      or its public path (https://<server>.<tailnet>.ts.net) with --read-only
  --read-only         the public path with a reader key; never configures pushing
  --with-dream        also install the dreamer (launchd, every 4 h; writer mode only)
  --env-file PATH     the dreamer's KEY=VALUE file (default ~/.config/tacit/env)
  --no-app            skip tacit-app and ~/Applications/tacit.app
  --repo DIR          tacit checkout (default: the one holding this script)
  --ref BRANCH        branch to build (default main)
  --wait SECONDS      how long to wait for the key enrollment (default 900)
  --enroll-ssh HOST   ssh target where the enrollment command runs (or $TACIT_ENROLL_SSH);
                      without it the script prints the command to run on the server
  --dry-run           check everything, change nothing, print what would run
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --server) server="${2:?--server needs a URL}"; shift 2 ;;
    --read-only) read_only=1; shift ;;
    --with-dream) with_dream=1; shift ;;
    --env-file) env_file="${2:?--env-file needs a path}"; shift 2 ;;
    --no-app) with_app=0; shift ;;
    --repo) repo="${2:?--repo needs a directory}"; shift 2 ;;
    --ref) ref="${2:?--ref needs a branch}"; shift 2 ;;
    --wait) wait_s="${2:?--wait needs seconds}"; shift 2 ;;
    --enroll-ssh) enroll_ssh="${2:?--enroll-ssh needs a host}"; shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) echo "mac-setup: unknown option $1" >&2; usage >&2; exit 2 ;;
  esac
done
if [[ -z "$server" ]]; then
  echo "mac-setup: name the server: --server URL (or \$TACIT_SERVER), its tailnet address, or its public path (https://<server>.<tailnet>.ts.net) with --read-only" >&2
  exit 2
fi
server="${server%/}"
if [[ $read_only -eq 1 && $with_dream -eq 1 ]]; then
  echo "mac-setup: --with-dream writes to the store; it cannot go with --read-only" >&2
  exit 2
fi
[[ "$wait_s" =~ ^[0-9]+$ ]] || { echo "mac-setup: --wait wants whole seconds" >&2; exit 2; }

say() { printf '== %s\n' "$*"; }
warn() { printf 'mac-setup: warning: %s\n' "$*" >&2; }
die() { printf 'mac-setup: %s\n' "$*" >&2; exit 1; }
# run changes something: in --dry-run it is printed instead.
run() {
  if [[ $dry_run -eq 1 ]]; then
    printf '+ %s\n' "$*"
  else
    "$@"
  fi
}

# --- 1. this machine -------------------------------------------------------
say "checking this Mac"
[[ "$(uname -s)" == Darwin ]] || die "this script is for macOS; on Linux follow README.md (Client)"
[[ "$(uname -m)" == arm64 ]] || warn "built and tested for Apple Silicon (arm64); this is $(uname -m)"
if ! git --version >/dev/null 2>&1; then
  die "git does not run: install the Xcode command line tools (xcode-select --install, a system dialog, no sudo), then rerun"
fi
git_v=$(git --version | awk '{print $3}')
IFS=. read -r gmaj gmin _ <<<"$git_v"
if ((gmaj < 2 || (gmaj == 2 && gmin < 29))); then
  die "git $git_v is too old; the follower needs 2.29 or newer"
fi
if [[ $with_app -eq 1 ]] && ! xcode-select -p >/dev/null 2>&1; then
  die "the app build needs the Xcode command line tools: run xcode-select --install (no sudo), then rerun; or pass --no-app"
fi
command -v go >/dev/null 2>&1 || die "Go is missing: brew install go (any Go 1.21 or newer fetches the 1.27 toolchain go.mod asks for), then rerun"
say "$(go version)"
# os.Hostname names the key (<harness>@<host>). Without a fixed HostName,
# macOS can take one from the network, and the key id would change.
if command -v scutil >/dev/null 2>&1 && ! scutil --get HostName >/dev/null 2>&1; then
  lhn=$(scutil --get LocalHostName 2>/dev/null || true)
  warn "no fixed HostName: the host part of the key id can change with the network. Pin it yourself (needs your password): sudo scutil --set HostName ${lhn:-<name>}"
fi
host=$(hostname | cut -d. -f1 | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')

# --- 2. the server ---------------------------------------------------------
# The tailnet answers /healthz to any tailnet peer (200). The public path asks
# a key on every route, so 401 without one is the answer of a reachable server.
say "checking the server at $server"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$server/healthz" 2>/dev/null || true)
case "$code" in
  200) ;;
  401) [[ $read_only -eq 1 ]] || die "$server asks a key for every request: it is the public path, which only reads; rerun with --read-only, or pass --server with the tailnet address" ;;
  *)
    if [[ $read_only -eq 1 ]]; then
      die "cannot reach $server/healthz from this Mac (answer: ${code:-none}). The public path needs Tailscale Funnel on the server (server/deploy/funnel.sh)."
    fi
    die "cannot reach $server/healthz from this Mac. Pass --server with an address this Mac can reach, or use --read-only for the public path."
    ;;
esac

# --- 3. scrump (every write is redacted, and fails closed without it) -------
# Read-only too: the agents' notes go to this Mac's local overlay, redacted
# like any add.
scrump_ok() {
  # Built from pieces, as in digest.RedactionReady, so no secret-shaped literal sits here.
  local in out
  in="pass""word=""Zq7Vx9""Lm2Kp4"
  out=$(printf '%s' "$in" | PATH="$bin_dir:$PATH" scrump --profile text scrub - -o - 2>/dev/null) || return 1
  [[ ${#out} -eq ${#in} && "$out" == *"****"* && "$out" != *Zq7Vx9* ]]
}
if scrump_ok; then
  say "scrump with the text profile works"
else
  command -v cargo >/dev/null 2>&1 || die "scrump (text profile) is missing and cargo is not installed: brew install rustup && rustup-init -y, open a new shell, then rerun (scrump's release predates the text profile, so it builds from the commit that added it)"
  say "building scrump (text profile, pinned commit) into $bin_dir"
  run cargo install --locked --root "$HOME/.local" --git https://github.com/avifenesh/scrump --rev "$scrump_rev" scrump-cli
  if [[ $dry_run -eq 0 ]] && ! scrump_ok; then
    die "scrump installed but its text profile did not mask the canary"
  fi
fi

# --- 4. the code -----------------------------------------------------------
say "tacit source at $repo ($ref)"
if [[ -d "$repo/.git" || -f "$repo/.git" ]]; then
  run git -C "$repo" fetch --quiet origin "$ref"
  run git -C "$repo" checkout --quiet "$ref"
  run git -C "$repo" merge --ff-only --quiet "origin/$ref"
else
  run git clone --quiet --branch "$ref" "$repo_url" "$repo"
fi
say "building tacit into $bin_dir"
run mkdir -p "$bin_dir"
run go -C "$repo/client" build -o "$bin_dir/tacit" ./cmd/tacit
if [[ $with_app -eq 1 ]]; then
  say "building tacit-app (the window: Wails on WKWebView, needs cgo and the macOS SDK)"
  run env CGO_ENABLED=1 CGO_LDFLAGS="-framework UniformTypeIdentifiers" go -C "$repo/client" build -tags desktop,production -o "$bin_dir/tacit-app" ./cmd/tacit
fi
tacit="$bin_dir/tacit"
case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) warn "$bin_dir is not on your PATH; add it in ~/.zprofile: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
esac

# --- 5. keys ---------------------------------------------------------------
# Writer mode: one key per harness that writes. Read-only: one reader key,
# which the server lets fetch and never push.
if [[ $read_only -eq 1 ]]; then
  harnesses=(reader)
else
  harnesses=(claude)
  if [[ -d "$HOME/.codex" ]]; then harnesses+=(codex); fi
  if [[ $with_dream -eq 1 ]]; then harnesses+=(dreamer); fi
fi
key_ids=()
say "keys"
enroll_lines=()
for h in "${harnesses[@]}"; do
  if [[ $dry_run -eq 1 && ! -x "$tacit" ]]; then
    run "$tacit" keygen "$h"
    pub="ssh-ed25519 <public-key> $h@$host"
  else
    if ! pub=$("$tacit" pubkey "$h" 2>/dev/null); then
      if [[ $dry_run -eq 1 ]]; then
        run "$tacit" keygen "$h"
        pub="ssh-ed25519 <public-key> $h@$host"
      else
        pub=$("$tacit" keygen "$h")
      fi
    fi
  fi
  id=$(awk '{print $NF}' <<<"$pub")
  key_ids+=("$id")
  if [[ -n "$enroll_ssh" ]]; then
    enroll_lines+=("ssh $enroll_ssh \"/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll(\\\"$id\\\", \\\"$pub\\\")'\"")
  else
    enroll_lines+=("/opt/tacit/rel/bin/tacit_server rpc 'TacitServer.Auth.Keys.enroll(\"$id\", \"$pub\")'")
  fi
done
echo
if [[ -n "$enroll_ssh" ]]; then
  echo "Enroll this Mac's keys (public keys only):"
else
  echo "Enroll this Mac's keys: run on the server (public keys only):"
fi
for l in "${enroll_lines[@]}"; do echo "  $l"; done
echo

# --- 6. wait until the server takes the keys ------------------------------
# `tacit authcheck` asks the server with the key's token what git asks first
# (receive-pack's refs for a writer, upload-pack's for a reader) and sends
# nothing. Exit 0: taken; 1: not enrolled yet or the server is unreachable
# (wait); 3: waiting cannot help (another host, another key, a refused
# network), so stop with the server's answer.
if [[ $dry_run -eq 1 ]]; then
  for i in "${!harnesses[@]}"; do
    printf '+ wait up to %ss until the server takes %s (tacit authcheck %s)\n' "$wait_s" "${key_ids[$i]}" "${harnesses[$i]}"
  done
  enrolled="not checked (dry run)"
else
  deadline=$((SECONDS + wait_s))
  enrolled=yes
  for i in "${!harnesses[@]}"; do
    h=${harnesses[$i]}
    while :; do
      rc=0
      answer=$("$tacit" authcheck "$h" --url "$server" 2>&1) || rc=$?
      [[ $rc -eq 0 ]] && break
      if [[ $rc -ne 1 ]]; then
        die "the server will not take ${key_ids[$i]}: $answer"
      fi
      if ((SECONDS >= deadline)); then
        die "the server has not taken ${key_ids[$i]} after ${wait_s}s. Its last answer: $answer"
      fi
      printf '%s (%ss left)\n' "$answer" $((deadline - SECONDS))
      sleep 10
    done
    say "the server takes ${key_ids[$i]}"
  done
fi

# --- 7. the store ----------------------------------------------------------
want_origin="$server/git"
need_clone=1
# A read-only clone holds nothing of this Mac's (every push is refused and
# the agents' notes live outside it), so it can be replaced when clean.
replace_ro() {
  [[ -z "$(git -C "$store" status --porcelain)" ]] || die "$store is read-only but has local changes; look at them, then remove it and rerun"
  say "$1"
  run rm -rf "$store"
  need_clone=1
}
if [[ -d "$store/.git" ]]; then
  need_clone=0
  origin=$(git -C "$store" remote get-url origin 2>/dev/null || true)
  ro=$(git -C "$store" config --bool --get tacit.readonly 2>/dev/null || true)
  as=$(git -C "$store" config --get tacit.as 2>/dev/null || true)
  if [[ "$ro" != true ]]; then
    [[ "$origin" == "$want_origin" ]] || die "$store is a clone of $origin, not $want_origin; move it aside or pass --server $origin"
    if [[ $read_only -eq 1 ]]; then
      die "$store is a writer clone; to make this Mac read-only, push or drop what it holds, remove $store and rerun with --read-only"
    fi
    say "store at $store: syncing"
    run "$tacit" sync
  elif [[ $read_only -eq 0 ]]; then
    [[ "$origin" == "$want_origin" ]] || die "$store is a clone of $origin, not $want_origin; move it aside or pass --server $origin"
    replace_ro "switching the read-only clone to a writer clone"
  elif [[ "$origin" != "$want_origin" || "$as" != reader ]]; then
    replace_ro "re-cloning the read-only clone from $want_origin with the reader key"
  else
    say "store at $store: syncing"
    run "$tacit" sync
  fi
fi
if [[ $need_clone -eq 1 ]]; then
  if [[ $read_only -eq 1 ]]; then
    run "$tacit" clone --read-only --as reader "$want_origin"
  else
    run "$tacit" clone "$want_origin" claude
  fi
fi

# --- 8. hooks, follower, dreamer, app --------------------------------------
say "hooks"
run "$tacit" install claude
if [[ -d "$HOME/.codex" ]]; then
  run "$tacit" install codex
fi

uid=$(id -u)
load_agent() {
  local label=$1 plist="$HOME/Library/LaunchAgents/$1.plist"
  run launchctl bootout "gui/$uid/$label" 2>/dev/null || true
  run launchctl bootstrap "gui/$uid" "$plist"
}
# The follower signs the feed with the clone's reader key (tacit.as) on
# its own; nothing to pass here.
say "follower (launchd)"
run "$tacit" install follow
load_agent ai.tacit.follow

if [[ $with_dream -eq 1 ]]; then
  say "dreamer (launchd, every 4 h)"
  # The runner can come from YAML or the env file; Tacit resolves it at job startup.
  [[ -f "$env_file" ]] || warn "$env_file is missing: put the selected runner's provider variables there (KEY=VALUE, chmod 600; for example the Bedrock ones), or leave it empty if the selected runner is already logged in"
  run "$tacit" install dream --env-file "$env_file"
  load_agent ai.tacit.dream
fi

if [[ $with_app -eq 1 ]]; then
  say "app"
  run "$bin_dir/tacit-app" install app
fi

# --- 9. status -------------------------------------------------------------
echo
say "status"
if [[ $read_only -eq 1 ]]; then
  echo "mode:      read-only (public path, reader key; nothing pushed; agents' notes stay in ~/.local/share/tacit/local)"
else
  echo "mode:      writer"
fi
echo "server:    $server (reachable)"
if [[ $dry_run -eq 0 ]]; then
  echo "store:     $store at $(git -C "$store" rev-parse --short HEAD 2>/dev/null || echo '?')$([[ "$(git -C "$store" config --bool --get tacit.readonly 2>/dev/null)" == true ]] && echo ', read-only')"
  state=$(launchctl print "gui/$uid/ai.tacit.follow" 2>/dev/null | awk -F'= ' '/^\tstate/{print $2; exit}')
  echo "follower:  ${state:-not loaded} (log ~/Library/Logs/tacit/follow.log)"
else
  echo "store:     $store (dry run)"
fi
echo "hooks:     claude$([[ -d "$HOME/.codex" ]] && echo ', codex (trust them once in /hooks)')"
echo "keys:      ${key_ids[*]} enrolled: $enrolled"
if [[ $read_only -eq 1 ]]; then
  echo "dreamer:   never on a read-only Mac"
else
  echo "dreamer:   $([[ $with_dream -eq 1 ]] && echo 'every 4 h (log ~/Library/Logs/tacit/dream.log)' || echo 'not installed (opt-in: --with-dream)')"
fi
echo "app:       $([[ $with_app -eq 1 ]] && echo "$HOME/Applications/tacit.app (view only: no curator key here; edit from the curator's machine)" || echo 'not installed')"
echo "curator, verifier: the curator's machine only, not installed here"
