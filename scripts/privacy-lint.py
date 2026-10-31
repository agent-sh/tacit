#!/usr/bin/env python3
"""Fail on the owner's personal content in the repository's tracked files.

The repository is public, and personal content never enters it (AGENTS.md).
This checks the shapes it came in as:

  owner-quote   words or a rule attributed to the owner with a date, as in
                "(the owner, YYYY-MM-DD: ...)" or 'Owner YYYY-MM-DD: "..."'
  tailnet-ip    an address in Tailscale's ranges (100.64.0.0/10 and
                fd7a:115c:a1e0::/48) other than the placeholders below
  tailnet-name  a MagicDNS name, <host>.tail<hex>.ts.net, of a real tailnet
  home-path     a home directory with a real user name: /home/<user>,
                /Users/<user>, C:\\Users\\<user>, and Claude Code's encoded
                project folder -home-<user>-; placeholder users are allowed
  denied        a word from the private deny list (host names, project
                names): $TACIT_PRIVACY_DENY (one per line, or comma separated)
                or --deny FILE. The list never enters the repository; CI
                reads it from a secret, and without it this rule is skipped.

A finding prints its file, line and rule, never the text it matched, so a CI
log does not publish what it found. --show prints the text, for a local run.
A line that holds "privacy-lint: allow" is skipped: use it only for a
synthetic fixture that needs one of these shapes.

Exit status: 0 clean, 1 findings, 2 usage or I/O error.
"""

import argparse
import os
import re
import subprocess
import sys

# Users that stand for nobody in particular, in examples and tests.
PLACEHOLDER_USERS = {
    "a", "alice", "bob", "dana", "example", "me", "name", "owner", "projects",
    "runner", "someone", "someone-else", "tacit-test-no-such-home", "u",
    "ubuntu", "user", "username", "users", "you",
}

# Tailscale addresses used as placeholders in tests and docs.
PLACEHOLDER_IPS = {
    "100.64.0.0", "100.64.0.0/10", "100.64.0.10", "100.127.255.255",
    "100.100.0.7", "100.100.0.8", "100.100.0.9", "100.100.0.10",
    "100.100.1.1", "100.100.1.2", "100.100.1.3", "100.100.100.100",
    "fd7a:115c:a1e0::/48", "fd7a:115c:a1e0::2a", "fd7a:115c:a1e0::5",
    "fd7a:115c:a1e0:ffff:ffff:ffff:ffff:ffff",
}
PLACEHOLDER_TAILNETS = {"tail1234"}

# Public names that hold a denied word: the redactor's repository.
ALLOWED_SPANS = re.compile(r"(?:https://)?(?:github\.com/)?avifenesh/scrump\b", re.I)

OWNER_QUOTE = [
    re.compile(r"\bthe owner(?:'s (?:rule|ruling|words|answer|decision))?, (?:on )?20\d\d-\d\d-\d\d\b", re.I),
    re.compile(r"\bowner,? \(?20\d\d-\d\d-\d\d\)?\s*:\s*[\"“'‘][^\s\"”'’]", re.I),
    re.compile(r"\(owner,? 20\d\d-\d\d-\d\d\b", re.I),
]
TAILNET_IP = re.compile(
    r"(?<![\w.])(100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}(?:/\d{1,2})?)(?![\w.])"
    r"|(?<![\w:])(fd7a:115c:a1e0:[0-9a-f:]*(?:/\d{1,3})?)(?![\w:])",  # privacy-lint: allow (the pattern itself)
    re.I,
)
TAILNET_NAME = re.compile(r"\b(tail[0-9a-f]{4,})\.ts\.net\b", re.I)
HOME_PATH = re.compile(
    r"(?<![\w.~-])/(?:home|Users)/([A-Za-z0-9][A-Za-z0-9_.-]*)"
    r"|(?i:\bC:\\{1,2}Users\\{1,2})([A-Za-z0-9][A-Za-z0-9_.-]*)"
    r"|(?<![\w])-(?:home|Users)-([A-Za-z0-9][A-Za-z0-9_]*)-",
)
SKIP_EXT = {".png", ".jpg", ".jpeg", ".gif", ".icns", ".ico", ".woff", ".woff2", ".ttf", ".zst", ".gz", ".zip"}
ALLOW_MARK = "privacy-lint: allow"


def tracked_files(root):
    out = subprocess.run(["git", "-C", root, "ls-files", "-z"], capture_output=True, check=True)
    return [p for p in out.stdout.decode("utf-8", "surrogateescape").split("\0") if p]


def load_deny(path):
    raw = os.environ.get("TACIT_PRIVACY_DENY", "")
    if path:
        with open(path, encoding="utf-8") as f:
            raw += "\n" + f.read()
    words = set()
    for line in raw.replace(",", "\n").splitlines():
        w = line.strip()
        if w and not w.startswith("#"):
            words.add(w.lower())
    if not words:
        return None
    alts = "|".join(re.escape(w) for w in sorted(words, key=len, reverse=True))
    return re.compile(r"(?<![A-Za-z0-9_-])(?:" + alts + r")(?![A-Za-z0-9_-])", re.I)


def findings(line, deny):
    for rx in OWNER_QUOTE:
        for m in rx.finditer(line):
            yield "owner-quote", m.group(0)
    for m in TAILNET_IP.finditer(line):
        ip = (m.group(1) or m.group(2)).lower()
        if ip not in PLACEHOLDER_IPS:
            yield "tailnet-ip", ip
    for m in TAILNET_NAME.finditer(line):
        if m.group(1).lower() not in PLACEHOLDER_TAILNETS:
            yield "tailnet-name", m.group(0)
    for m in HOME_PATH.finditer(line):
        user = next(g for g in m.groups() if g)
        if user.lower() not in PLACEHOLDER_USERS:
            yield "home-path", m.group(0)
    if deny is not None:
        for m in deny.finditer(ALLOWED_SPANS.sub(" ", line)):
            yield "denied", m.group(0)


def scan(root, paths, deny, show):
    found = 0
    for rel in paths:
        if os.path.splitext(rel)[1].lower() in SKIP_EXT:
            continue
        full = os.path.join(root, rel)
        try:
            with open(full, "rb") as f:
                data = f.read()
        except (IsADirectoryError, FileNotFoundError):
            continue
        if b"\0" in data[:8192]:
            continue
        text = data.decode("utf-8", "replace")
        for n, line in enumerate(text.splitlines(), 1):
            if ALLOW_MARK in line:
                continue
            for rule, what in findings(line, deny):
                found += 1
                print(f"{rel}:{n}: {rule}" + (f": {what}" if show else ""))
    return found


def main():
    ap = argparse.ArgumentParser(description="Fail on personal content in tracked files (AGENTS.md).")
    ap.add_argument("paths", nargs="*", help="files to scan (default: every tracked file)")
    ap.add_argument("--root", default=".", help="repository root (default: .)")
    ap.add_argument("--deny", help="file with private words to refuse, one per line")
    ap.add_argument("--show", action="store_true", help="print the matched text (local runs only)")
    a = ap.parse_args()
    try:
        deny = load_deny(a.deny)
        paths = a.paths or tracked_files(a.root)
    except (OSError, subprocess.CalledProcessError) as e:
        print(f"privacy-lint: {e}", file=sys.stderr)
        return 2
    if deny is None:
        print("privacy-lint: no private deny list here; the denied rule is skipped", file=sys.stderr)
    found = scan(a.root, paths, deny, a.show)
    if found:
        print(f"privacy-lint: {found} finding(s). Personal content never enters the repository (AGENTS.md).", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
