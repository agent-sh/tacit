#!/usr/bin/env python3
"""How often the pre-receive secret scan refuses keys, and fact names.

Reads the hook's expressions from server/priv/hooks/pre-receive (or --hook
FILE): secret_re, and each family's <family>_token_re and <family>_key_re (sk,
slack, tskey). It runs them with grep the way the hook does (LC_ALL=C; a line
is refused when it matches secret_re, or holds a family's token that matches
that family's key_re) over random keys of each shape providers issue and over
fact names with a segment that starts like a key. Every key is random; none
is real. The key shapes follow gitleaks' rules (anthropic.go, openai.go,
slack.go), TruffleHog's detectors (deepseek, openrouter, langfuse, tailscale),
LiteLLM's key generator and Tailscale's key reference. --compare adds another
regex as a column. --hook reads another copy of the hook, for example the one
a change replaces: `git show <base>:server/priv/hooks/pre-receive > old`.

    python3 -I eval/secrets/token_rules.py [--n 100000] [--seed 1] [--hook FILE] [--compare REGEX]

No model, no network. Needs GNU grep. client/internal/secretscan holds the
same expressions (its test keeps them equal), so one engine measures both.
"""
import argparse
import os
import random
import re
import string
import subprocess
import sys
import uuid

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HOOK = os.path.join(ROOT, "server", "priv", "hooks", "pre-receive")

B64URL = string.ascii_letters + string.digits + "-_"
ALNUM = string.ascii_letters + string.digits
UPPER_ALNUM = string.ascii_uppercase + string.digits
HEX = "0123456789abcdef"
LOWER = string.ascii_lowercase + string.digits
DIGITS = string.digits

# Words for fact names: lowercase letters, digits and hyphens is the whole
# alphabet of a fact name ([a-z0-9][a-z0-9-]*).
WORDS = """only bang commands plain flags hook pipeline cache notes locale fallback order
rule lint check build deploy port proxy route model judge curator dreamer review
store server client token budget retry backoff batch split index search fact
folder record owner session agent prompt reply merge rebase branch commit tag
release install config timer unit service socket queue worker pool driver
kernel memory disk network tunnel mirror backup restore schema format parser
writer reader scope repo project machine host region bucket image layer cluster
learn skill tool command shell script path file line table column field value
error failure timeout limit stale fresh draft final quick full idle daily weekly
auth api client scim webhook bot user app refresh expires reusable ephemeral tagged
""".split()

FAMILIES = ("sk", "slack", "tskey")


def pick(rnd, alphabet, n):
    return "".join(rnd.choice(alphabet) for _ in range(n))


def digits(rnd, lo, hi):
    return pick(rnd, DIGITS, rnd.randint(lo, hi))


def ts_id(rnd):
    # The key id Tailscale shows: k and six letters or digits, then CNTRL
    # (kPjmma7CNTRL), or seven of them (abcDEF1CNTRL in its key reference).
    return rnd.choice(["k" + pick(rnd, ALNUM, 6), pick(rnd, ALNUM, 7)]) + "CNTRL"


def shapes(rnd):
    return {
        "anthropic api03": lambda: "sk-ant-api03-" + pick(rnd, B64URL, 93) + "AA",
        "anthropic admin01": lambda: "sk-ant-admin01-" + pick(rnd, B64URL, 93) + "AA",
        "anthropic oat01": lambda: "sk-ant-oat01-" + pick(rnd, B64URL, 93) + "AA",
        "openai project 74": lambda: "sk-proj-" + pick(rnd, B64URL, 74) + "T3BlbkFJ" + pick(rnd, B64URL, 74),
        "openai project 58": lambda: "sk-proj-" + pick(rnd, B64URL, 58) + "T3BlbkFJ" + pick(rnd, B64URL, 58),
        "openai svcacct": lambda: "sk-svcacct-" + pick(rnd, B64URL, 74) + "T3BlbkFJ" + pick(rnd, B64URL, 74),
        "openai admin": lambda: "sk-admin-" + pick(rnd, B64URL, 58) + "T3BlbkFJ" + pick(rnd, B64URL, 58),
        "openai legacy": lambda: "sk-" + pick(rnd, ALNUM, 20) + "T3BlbkFJ" + pick(rnd, ALNUM, 20),
        "legacy 48 alnum": lambda: "sk-" + pick(rnd, ALNUM, 48),
        "DeepSeek (32 lowercase alnum)": lambda: "sk-" + pick(rnd, LOWER, 32),
        "openrouter": lambda: "sk-or-v1-" + pick(rnd, HEX, 64),
        "langfuse": lambda: "sk-lf-" + str(uuid.UUID(int=rnd.getrandbits(128), version=4)),
        "litellm (22 base64url)": lambda: "sk-" + pick(rnd, B64URL, 22),
        "unknown, 16 base64url": lambda: "sk-" + pick(rnd, B64URL, 16),
        "unknown, 18 base64url": lambda: "sk-" + pick(rnd, B64URL, 18),
        "unknown, 20 base64url": lambda: "sk-" + pick(rnd, B64URL, 20),
        "unknown, 32 base64url": lambda: "sk-" + pick(rnd, B64URL, 32),
        "unknown, 48 base64url": lambda: "sk-" + pick(rnd, B64URL, 48),
        # Tailscale: tskey-<type>-<key id>-<secret> (key prefixes reference;
        # TruffleHog's tskey-[a-z]+-[0-9A-Za-z_]+-[0-9A-Za-z_]+), and the
        # untyped tskey-<key id>-<secret> of 2021.
        "tailscale auth": lambda: "tskey-auth-" + ts_id(rnd) + "-" + pick(rnd, ALNUM, 32),
        "tailscale api": lambda: "tskey-api-" + ts_id(rnd) + "-" + pick(rnd, ALNUM, 33),
        "tailscale client, scim, webhook": lambda: "tskey-" + rnd.choice(["client", "scim", "webhook"]) + "-" + ts_id(rnd) + "-" + pick(rnd, ALNUM, 32),
        "tailscale untyped (2021)": lambda: "tskey-" + ts_id(rnd) + "-" + pick(rnd, ALNUM, rnd.randint(26, 32)),
        "unknown tskey, 16 lowercase hex": lambda: "tskey-" + pick(rnd, HEX, 16),
        "unknown tskey, 24 lowercase hex": lambda: "tskey-" + pick(rnd, HEX, 24),
        # Slack (gitleaks' slack.go): bot, user, legacy bot, legacy workspace,
        # legacy, configuration access and refresh, app-level.
        "slack bot xoxb": lambda: "xoxb-%s-%s-%s" % (digits(rnd, 10, 13), digits(rnd, 10, 13), pick(rnd, ALNUM, 24)),
        "slack user xoxp": lambda: "xoxp-%s-%s-%s-%s" % (digits(rnd, 10, 13), digits(rnd, 10, 13), digits(rnd, 10, 13), pick(rnd, HEX, 32)),
        "slack legacy bot xoxb": lambda: "xoxb-%s-%s" % (digits(rnd, 8, 14), pick(rnd, ALNUM, rnd.randint(18, 26))),
        "slack legacy workspace xoxa-2": lambda: "xoxa-2-%s-%s-%s-%s" % (digits(rnd, 9, 9), digits(rnd, 11, 11), digits(rnd, 13, 13), pick(rnd, HEX, 18)),
        "slack legacy workspace, 12 alnum": lambda: rnd.choice(["xoxa-", "xoxr-"]) + rnd.choice(["", pick(rnd, DIGITS, 1) + "-"]) + pick(rnd, ALNUM, 12),
        "slack legacy xoxs": lambda: "xoxs-%s-%s-%s-%s" % (digits(rnd, 10, 12), digits(rnd, 9, 12), digits(rnd, 10, 12), pick(rnd, HEX, 10)),
        "slack config access xoxe.xoxp": lambda: "xoxe.xox" + rnd.choice("bp") + "-1-" + pick(rnd, ALNUM, rnd.randint(163, 166)),
        "slack config refresh xoxe": lambda: "xoxe-1-" + pick(rnd, ALNUM, 146),
        "slack app-level xapp": lambda: "xapp-1-A%s-%s-%s" % (pick(rnd, UPPER_ALNUM, 10), digits(rnd, 13, 13), pick(rnd, HEX, 64)),
    }


def in_line(rnd, key):
    return rnd.choice(['export API_KEY="%s"', "API_KEY=%s", '{"token": "%s"}', "the key is %s here", "%s"]) % key


def words(rnd, k):
    return "-".join(rnd.choice(WORDS) for _ in range(k))


def num(rnd):
    return str(rnd.randrange(1, 999)).zfill(rnd.choice([1, 2, 3]))


def sk_name(rnd):
    return rnd.choice([
        words(rnd, rnd.randint(1, 2)) + "-cc-sk-" + num(rnd) + "-" + words(rnd, rnd.randint(2, 6)),
        words(rnd, rnd.randint(1, 3)) + "-sk-" + words(rnd, rnd.randint(3, 7)),
        "sk-" + words(rnd, rnd.randint(3, 8)),
        words(rnd, rnd.randint(1, 3)) + "-sk-" + num(rnd) + "-" + words(rnd, rnd.randint(2, 5)) + "-v" + str(rnd.randint(1, 9)),
    ])


def prefixed_name(rnd, prefix):
    """A fact name with a segment that starts like a key: tskey-auth-expires-in-90-days."""
    return rnd.choice([
        prefix + "-" + words(rnd, rnd.randint(2, 7)),
        words(rnd, rnd.randint(1, 3)) + "-" + prefix + "-" + words(rnd, rnd.randint(2, 6)),
        words(rnd, rnd.randint(1, 2)) + "-" + prefix + "-" + num(rnd) + "-" + words(rnd, rnd.randint(2, 5)) + "-v" + str(rnd.randint(1, 9)),
        prefix + "-" + words(rnd, rnd.randint(1, 3)) + "-2026-" + str(rnd.randint(1, 12)).zfill(2) + "-" + str(rnd.randint(1, 28)).zfill(2),
    ])


def name_line(rnd, name):
    return rnd.choice(["- [[%s]]: a fact", "- [%s](%s.md) - a fact", "%s", "name: %s"]).replace("%s", name)


def grep(args, text):
    out = subprocess.run(["grep", "-a"] + args, input=text, capture_output=True, text=True,
                         env=dict(os.environ, LC_ALL="C"))
    if out.returncode not in (0, 1):
        sys.exit("grep failed: " + out.stderr)
    return out.stdout


def hits(regex, lines):
    """Lines one regex matches (a --compare column)."""
    return int(grep(["-E", "-c", "--", regex], "\n".join(lines) + "\n").strip() or 0)


def hook_hits(rules, lines):
    """Lines the hook refuses: secret_re, or a family's token that is a key."""
    text = "\n".join(lines) + "\n"
    numbers = lambda out: {l.split(":", 1)[0] for l in out.splitlines() if l}
    refused = numbers(grep(["-E", "-n", "--", rules["secret_re"]], text))
    for fam in FAMILIES:
        if fam + "_token_re" not in rules:
            continue
        tokens = grep(["-E", "-o", "-n", "--", rules[fam + "_token_re"]], text)
        if tokens:
            refused |= numbers(grep(["-E", "--", rules[fam + "_key_re"]], tokens))
    return len(refused)


def read_rules(path):
    hook = open(path).read()
    rules = {}
    m = re.search(r"^secret_re='([^']*)'$", hook, re.M)
    if not m:
        sys.exit("secret_re not found in " + path)
    rules["secret_re"] = m.group(1)
    for fam in FAMILIES:
        tok = re.search(r"^" + fam + r"_token_re='([^']*)'$", hook, re.M)
        key = re.search(r"^" + fam + r"_key_re='([^']*)'$", hook, re.M)
        if bool(tok) != bool(key):
            sys.exit(fam + ": a token expression without its key expression in " + path)
        if tok:
            rules[fam + "_token_re"], rules[fam + "_key_re"] = tok.group(1), key.group(1)
    return rules


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--n", type=int, default=100000, help="samples per shape")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--hook", default=HOOK, help="the pre-receive hook to read the expressions from")
    ap.add_argument("--compare", action="append", default=[], metavar="REGEX", help="another regex to measure beside the hook's")
    a = ap.parse_args()
    rules = read_rules(a.hook)
    compare = [("compare %d" % (i + 1), r) for i, r in enumerate(a.compare)]
    rnd = random.Random(a.seed)
    rows = [(name, [in_line(rnd, gen()) for _ in range(a.n)]) for name, gen in shapes(rnd).items()]
    rows.append(("fact names with sk-", [name_line(rnd, sk_name(rnd)) for _ in range(a.n)]))
    rows.append(("fact names with tskey-", [name_line(rnd, prefixed_name(rnd, rnd.choice(["tskey", "tskey-auth", "tskey-api", "tskey-client"]))) for _ in range(a.n)]))
    rows.append(("fact names with xox?- or xapp-", [name_line(rnd, prefixed_name(rnd, rnd.choice(["xoxb", "xoxp", "xoxa", "xoxr", "xoxs", "xoxe", "xapp"]))) for _ in range(a.n)]))
    print("| " + " | ".join(["sample", "n", "hook"] + [n for n, _ in compare]) + " |")
    print("|" + "---|" * (3 + len(compare)))
    for name, lines in rows:
        cells = ["%.3f%%" % (100.0 * hook_hits(rules, lines) / len(lines))]
        cells += ["%.3f%%" % (100.0 * hits(r, lines) / len(lines)) for _, r in compare]
        print("| %s | %d | %s |" % (name, len(lines), " | ".join(cells)))


if __name__ == "__main__":
    main()
