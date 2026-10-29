#!/usr/bin/env python3
"""How often the pre-receive secret scan refuses sk- keys, and fact names.

Reads secret_re from server/priv/hooks/pre-receive and runs it with grep -E,
the hook's own engine, over random keys of each shape providers issue and over
fact names with an sk- segment. Every key is random; none is real. The key
shapes follow gitleaks' rules (anthropic.go, openai.go) and the documented
formats of OpenRouter, Langfuse and LiteLLM. --compare adds another regex as a
column, for example the rule before v0.9.15.

    python3 -I eval/secrets/sk_rule.py [--n 100000] [--seed 1] [--compare REGEX]

No model, no network. Needs GNU grep. The client's copies of the rule are the
same string (their tests hold them equal), so one engine measures all three.
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
HEX = "0123456789abcdef"

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
""".split()


def pick(rnd, alphabet, n):
    return "".join(rnd.choice(alphabet) for _ in range(n))


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
        "32 hex (DeepSeek)": lambda: "sk-" + pick(rnd, HEX, 32),
        "openrouter": lambda: "sk-or-v1-" + pick(rnd, HEX, 64),
        "langfuse": lambda: "sk-lf-" + str(uuid.UUID(int=rnd.getrandbits(128), version=4)),
        "litellm (22 base64url)": lambda: "sk-" + pick(rnd, B64URL, 22),
        "unknown, 32 base64url": lambda: "sk-" + pick(rnd, B64URL, 32),
        "unknown, 48 base64url": lambda: "sk-" + pick(rnd, B64URL, 48),
    }


def in_line(rnd, key):
    return rnd.choice(['export API_KEY="%s"', "API_KEY=%s", '{"token": "%s"}', "the key is %s here", "%s"]) % key


def fact_name(rnd):
    words = lambda k: "-".join(rnd.choice(WORDS) for _ in range(k))
    num = str(rnd.randrange(1, 999)).zfill(rnd.choice([1, 2, 3]))
    return rnd.choice([
        words(rnd.randint(1, 2)) + "-cc-sk-" + num + "-" + words(rnd.randint(2, 6)),
        words(rnd.randint(1, 3)) + "-sk-" + words(rnd.randint(3, 7)),
        "sk-" + words(rnd.randint(3, 8)),
        words(rnd.randint(1, 3)) + "-sk-" + num + "-" + words(rnd.randint(2, 5)) + "-v" + str(rnd.randint(1, 9)),
    ])


def name_line(rnd, name):
    return rnd.choice(["- [[%s]]: a fact", "- [%s](%s.md) - a fact", "%s", "name: %s"]).replace("%s", name)


def hits(regex, lines):
    out = subprocess.run(["grep", "-E", "-c", "--", regex], input="\n".join(lines) + "\n",
                         capture_output=True, text=True, env=dict(os.environ, LC_ALL="C"))
    if out.returncode not in (0, 1):
        sys.exit("grep failed: " + out.stderr)
    return int(out.stdout.strip() or 0)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--n", type=int, default=100000, help="samples per shape")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--compare", action="append", default=[], metavar="REGEX", help="another regex to measure beside the hook's")
    a = ap.parse_args()
    m = re.search(r"^secret_re='([^']*)'$", open(HOOK).read(), re.M)
    if not m:
        sys.exit("secret_re not found in " + HOOK)
    rules = [("hook", m.group(1))] + [("compare %d" % (i + 1), r) for i, r in enumerate(a.compare)]
    rnd = random.Random(a.seed)
    rows = [(name, [in_line(rnd, gen()) for _ in range(a.n)]) for name, gen in shapes(rnd).items()]
    rows.append(("fact names with sk-", [name_line(rnd, fact_name(rnd)) for _ in range(a.n)]))
    print("| sample | n | " + " | ".join(n for n, _ in rules) + " |")
    print("|---|---|" + "---|" * len(rules))
    for name, lines in rows:
        cells = ["%.3f%%" % (100.0 * hits(r, lines) / len(lines)) for _, r in rules]
        print("| %s | %d | %s |" % (name, len(lines), " | ".join(cells)))


if __name__ == "__main__":
    main()
