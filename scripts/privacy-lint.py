#!/usr/bin/env python3
"""Fail on the owner's personal content in the repository's tracked files.

The repository is public, and personal content never enters it (AGENTS.md).
This checks the shapes it came in as:

  owner-quote   words or a rule attributed to the owner with a date, as in
                "(the owner, YYYY-MM-DD: ...)", 'Owner YYYY-MM-DD: "..."',
                "Owner YYYY-MM-DD: <clause>" with the clause written out,
                "the owner said on YYYY-MM-DD", "On YYYY-MM-DD the owner
                said"; a line is also read joined to the next one, so a
                wrapped attribution is caught
  tailnet-ip    an address in Tailscale's ranges (100.64.0.0/10 and
                fd7a:115c:a1e0::/48) other than the placeholders below
  tailnet-name  a MagicDNS name of a real tailnet, <host>.<tailnet>.ts.net,
                in either style (tail<hex> or two words)
  home-path     a home directory with a real user name: /home/<user>,
                /Users/<user>, C:\\Users\\<user>, and Claude Code's encoded
                project folder -home-<user>; placeholder users are allowed
  denied        a word of the private deny list (host names, project names),
                read from --deny FILE, $TACIT_PRIVACY_DENY (one per line, or
                comma separated) and $XDG_CONFIG_HOME/tacit/privacy-deny.txt
                (else ~/.config/tacit/privacy-deny.txt), every one that is
                there, merged. The list stays out of the
                repository; CI reads it from a secret. Without one the rule
                is skipped, unless --require-deny makes that an error.

Tracked paths are checked as well as contents, and a symbolic link is read as
its target's name, never followed. A finding prints its file, line and rule,
never the text it matched; a path that is itself a finding is printed as its
place in the list of tracked files; and the deny list's findings print only as
a count, since a line number would point at the word. --show prints all of it,
for a local run. A line that holds "privacy-lint: allow" is passed by the
shape rules, never by the deny list: use it only for a synthetic fixture that
needs one of the shapes.

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
PLACEHOLDER_TAILNETS = {"example", "tail1234"}

# Public repository names the deny list does not apply to.
ALLOWED_SPANS = re.compile(r"(?:https://)?(?:github\.com/)?(?:avifenesh/scrump|agent-sh/[A-Za-z0-9_.-]+)\b", re.I)

DATE = r"20\d\d-\d\d-\d\d"
OWNER_QUOTE = [
    re.compile(r"\bthe owner(?:'s (?:rule|ruling|words|answer|decision))?, (?:on )?" + DATE + r"\b", re.I),
    re.compile(r"\bowner,? \(?" + DATE + r"\)?\s*:\s*(?![<\"”'’\\]|$)\S", re.I),
    re.compile(r"\bowner,? \(?" + DATE + r"\)?\s*:\s*\\?[\"“'‘][^\s\"”'’\\]", re.I),
    re.compile(r"\(owner,? " + DATE + r"\b", re.I),
    re.compile(r"\bthe owner (?:said|decided|wrote|asked|ruled|approved|answered|agreed)(?: [a-z]+)? on " + DATE + r"\b", re.I),
    re.compile(r"\bon " + DATE + r",? the owner (?:said|decided|wrote|asked|ruled|approved|answered|agreed)\b", re.I),
]
TAILNET_IP = re.compile(
    r"(?<![\w.])(100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.\d{1,3}\.\d{1,3}(?:/\d{1,2})?)(?!\w|\.\d)"
    r"|(?<![\w:])(fd7a:115c:a1e0:[0-9a-f:]*(?:/\d{1,3})?)(?![\w:])",  # privacy-lint: allow (the pattern itself)
    re.I,
)
TAILNET_NAME = re.compile(r"(?<![\w-])([a-z0-9][a-z0-9-]*)\.ts\.net\b", re.I)
HOME_PATH = re.compile(
    r"(?<![\w.~-])/(?:home|Users)/([A-Za-z0-9][A-Za-z0-9_.-]*)"
    r"|(?i:\bC:\\{1,2}Users\\{1,2})([A-Za-z0-9][A-Za-z0-9_.-]*)"
    r"|(?<![\w])-(?:home|Users)-([A-Za-z0-9][A-Za-z0-9_]*)(?![A-Za-z0-9_])",
)
COMMENT_LEAD = re.compile(r"^\s*(?://+|#+|\*+|--|;+|>+)?\s*")
SKIP_EXT = {".png", ".jpg", ".jpeg", ".gif", ".icns", ".ico", ".woff", ".woff2", ".ttf", ".zst", ".gz", ".zip"}
ALLOW_MARK = "privacy-lint: allow"


class LintError(Exception):
    pass


def tracked_files(root):
    out = subprocess.run(["git", "-C", root, "ls-files", "-z"], capture_output=True, check=True)
    return [p for p in out.stdout.decode("utf-8", "surrogateescape").split("\0") if p]


def default_deny_file():
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    path = os.path.join(base, "tacit", "privacy-deny.txt")
    return path if os.path.isfile(path) else None


def load_deny(path):
    raw = os.environ.get("TACIT_PRIVACY_DENY", "")
    for p in (path, default_deny_file()):
        if p:
            try:
                with open(p, encoding="utf-8") as f:
                    raw += "\n" + f.read()
            except (OSError, UnicodeDecodeError) as e:
                raise LintError(f"cannot read the deny list: {e}") from e
    words = set()
    for line in raw.replace(",", "\n").splitlines():
        w = line.strip()
        if w and not w.startswith("#"):
            words.add(w.lower())
    if not words:
        return None
    alts = "|".join(re.escape(w) for w in sorted(words, key=len, reverse=True))
    return re.compile(r"(?<![A-Za-z0-9])(?:" + alts + r")(?![A-Za-z0-9])", re.I)


def shape_findings(line):
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
        user = next(g for g in m.groups() if g).rstrip(".")
        if user.lower() not in PLACEHOLDER_USERS:
            yield "home-path", m.group(0)


def deny_findings(line, deny):
    if deny is not None:
        for m in deny.finditer(ALLOWED_SPANS.sub(" ", line)):
            yield "denied", m.group(0)


def owner_quotes(text):
    return {m.group(0) for rx in OWNER_QUOTE for m in rx.finditer(text)}


def read_text(root, rel):
    """The text to check for a tracked path, or None when it is binary."""
    full = os.path.join(root, rel)
    if os.path.islink(full):
        return os.readlink(full)
    if os.path.splitext(rel)[1].lower() in SKIP_EXT or os.path.isdir(full):
        return None
    try:
        with open(full, "rb") as f:
            data = f.read()
    except FileNotFoundError:
        return None  # tracked but deleted in the work tree
    except OSError as e:
        raise LintError(f"cannot read {rel}: {e}") from e
    if b"\0" in data[:8192]:
        return None
    return data.decode("utf-8", "replace")


def scan(root, paths, deny, show):
    """Prints the findings and returns (shown findings, deny findings)."""
    shown = denied = 0

    def report(label, where, rule, what):
        nonlocal shown, denied
        if rule.startswith("denied") and not show:
            denied += 1
            return
        shown += 1
        print(f"{label}{where}: {rule}" + (f": {what}" if show else ""))

    for i, rel in enumerate(paths, 1):
        hits = list(shape_findings(rel)) + list(deny_findings(rel, deny))
        label = rel if show or not hits else f"<tracked path {i} withheld>"
        for rule, what in hits:
            report(label, "", rule + " in the path", what)
        text = read_text(root, rel)
        if text is None:
            continue
        lines = text.splitlines()
        for n, line in enumerate(lines, 1):
            for rule, what in deny_findings(line, deny):
                report(label, f":{n}", rule, what)
            if ALLOW_MARK in line:
                continue
            for rule, what in shape_findings(line):
                report(label, f":{n}", rule, what)
            if n < len(lines) and ALLOW_MARK not in lines[n]:
                joined = line.rstrip() + " " + COMMENT_LEAD.sub("", lines[n], count=1)
                for what in owner_quotes(joined) - owner_quotes(line) - owner_quotes(lines[n]):
                    report(label, f":{n}", "owner-quote (wrapped)", what)
    return shown, denied


def main():
    ap = argparse.ArgumentParser(description="Fail on personal content in tracked files (AGENTS.md).")
    ap.add_argument("paths", nargs="*", help="tracked files to scan (default: every tracked file)")
    ap.add_argument("--root", default=".", help="repository root (default: .)")
    ap.add_argument("--deny", help="file with private words to refuse, one per line")
    ap.add_argument("--require-deny", action="store_true", help="fail when no deny list is found (CI on the main repository)")
    ap.add_argument("--show", action="store_true", help="print the matched text and every path (local runs only)")
    a = ap.parse_args()
    try:
        deny = load_deny(a.deny)
        paths = a.paths or tracked_files(a.root)
        missing = [p for p in a.paths if not os.path.lexists(os.path.join(a.root, p))]
        if missing:
            raise LintError(f"no such file: {missing[0]}")
        if deny is None and a.require_deny:
            raise LintError("no private deny list: set TACIT_PRIVACY_DENY or pass --deny FILE")
        shown, denied = scan(a.root, paths, deny, a.show)
    except (LintError, OSError, subprocess.CalledProcessError) as e:
        print(f"privacy-lint: {e}", file=sys.stderr)
        return 2
    sys.stdout.flush()
    if deny is None:
        print("privacy-lint: no private deny list here; the denied rule is skipped", file=sys.stderr)
    if denied:
        print(f"privacy-lint: {denied} line(s) hold a word of the private deny list; run with the list and --show to see them", file=sys.stderr)
    if shown or denied:
        print(f"privacy-lint: {shown + denied} finding(s). Personal content never enters the repository (AGENTS.md).", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
