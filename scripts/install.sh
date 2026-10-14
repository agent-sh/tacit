#!/bin/sh
# Install the CLI/browser app and bundled text redactor from a public release.
set -eu
release=latest
bin_dir="${HOME}/.local/bin"
base="${TACIT_RELEASE_BASE_URL:-https://github.com/agent-sh/tacit/releases}"
fail() { printf '%s\n' "install: $*" >&2; exit 1; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || fail '--version needs a tag'; release=$2; shift 2 ;;
    --bin-dir) [ "$#" -ge 2 ] || fail '--bin-dir needs a directory'; bin_dir=$2; shift 2 ;;
    --help|-h) printf '%s\n' 'Usage: sh install.sh [--version v0.1.0] [--bin-dir ~/.local/bin]'; exit 0 ;;
    *) fail "unknown option: $1" ;;
  esac
done
command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v tar >/dev/null 2>&1 || fail 'tar is required'
case "$(uname -s)" in Linux) platform=linux ;; Darwin) platform=darwin ;; *) fail 'use install.ps1 on Windows' ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail 'supported architectures are amd64 and arm64' ;; esac
if [ "$release" = latest ]; then
  url=$(curl -fLsS --retry 2 --connect-timeout 10 --max-time 120 -o /dev/null -w '%{url_effective}' "$base/latest")
  release=${url##*/}
fi
case "$release" in v[0-9]*.[0-9]*.[0-9]*) ;; *) fail 'version must be a v-prefixed release tag' ;; esac
case "$release" in *[!A-Za-z0-9.+-]*) fail 'unsafe release tag' ;; esac
cache="${XDG_CACHE_HOME:-${HOME}/.cache}"
mkdir -p "$cache"
task=$(mktemp -d "$cache/tacit-install.XXXXXX")
trap 'rm -rf "$task"' EXIT HUP INT TERM
name="tacit-$release-$platform-$arch.tar.gz"
curl -fLsS --retry 2 --connect-timeout 10 --max-time 120 -o "$task/$name" "$base/download/$release/$name"
curl -fLsS --retry 2 --connect-timeout 10 --max-time 120 -o "$task/SHA256SUMS" "$base/download/$release/SHA256SUMS"
expected=$(awk -v name="$name" '$2 == name {print $1}' "$task/SHA256SUMS")
[ "${#expected}" -eq 64 ] || fail 'missing or invalid archive checksum'
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$task/$name" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$task/$name" | awk '{print $1}')
else
  fail 'sha256sum or shasum is required'
fi
[ "$actual" = "$expected" ] || fail 'archive checksum mismatch'
tar -xzf "$task/$name" -C "$task"
source="$task/tacit-$release-$platform-$arch"
[ -f "$source/tacit" ] && [ -f "$source/scrump" ] || fail 'archive lacks required executables'
[ "$("$source/tacit" --version)" = "tacit $release" ] || fail 'archive version mismatch'
canary='TACIT_PROBE_TOKEN=fixture-value-12345'
masked=$(printf '%s' "$canary" | "$source/scrump" --profile text scrub - -o -) || fail 'bundled scrump failed'
[ "$masked" != "$canary" ] && [ "${#masked}" -eq "${#canary}" ] || fail 'bundled scrump lacks working text masking'
mkdir -p "$bin_dir"
# Stage next to the destination so each executable is replaced atomically.
for executable in scrump tacit; do
  staged=$(mktemp "$bin_dir/.$executable.XXXXXX")
  cp "$source/$executable" "$staged"
  chmod 755 "$staged"
  mv -f "$staged" "$bin_dir/$executable"
done
printf '%s\n' "Installed tacit $release and scrump in $bin_dir" "Add $bin_dir to PATH, then run: tacit init"
