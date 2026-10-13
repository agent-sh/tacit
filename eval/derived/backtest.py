#!/usr/bin/env python3
"""Backtest behind the thresholds in client/internal/derive/analyze.go.

Reads plain git history of local checkouts and answers three questions:

  1. window     How many recent code commits should the hot-spot ranking read?
                Rank files by touch count over the K commits before a split
                point, then see how many of the top 10 are touched again in the
                next 100 commits. Mean over split points.
  2. changeset  How many code files may a commit touch before it is a mass edit
                (rename, format sweep, vendor drop) and stops counting?
                Distribution of code files per commit, and the share above the
                chosen limit.
  3. cochange   When file f is in a commit, how often is partner g in it too,
                for rules "g changed with f at least M times and in at least
                C of f's commits" learned on the window? Precision over the
                150 commits after the split.

Usage: backtest.py --repos REPO [REPO ...] [--projects DIR]
  --projects defaults to $TACIT_PROJECTS, else ~/projects; each repo is a
  checkout directory under it.
Stdlib only. Needs git. The code-file rule below must match
client/internal/derive/code.go (a Go test holds that file's rule equal to the
pathspecs it hands to git; keep this copy in step by hand).
"""
import argparse, collections, itertools, os, statistics, subprocess

EXT = ("rs go c h cc cpp cxx hpp hh cu cuh zig java kt kts scala clj swift cs dart py rb php lua ex exs erl hrl ml mli hs "
       "ts tsx js jsx mjs cjs css scss html svelte vue sh bash zsh fish ps1 sql proto graphql toml yaml yml cmake gradle nix tf sol").split()
NAMES = ["Makefile", "Dockerfile", "justfile", "CMakeLists.txt"]
SKIP_DIRS = ["vendor", "node_modules", "third_party", "dist", "build", "target"]
SKIP_SUFFIX = [".min.js", ".min.css", ".pb.go", ".pb.cc", ".pb.h", "_pb2.py"]
SPECS = (["*." + e for e in sorted(EXT)] + [":(glob)**/" + n for n in sorted(NAMES)]
         + [":(exclude,glob)**/%s/**" % d for d in sorted(SKIP_DIRS)]
         + [":(exclude,glob)**/*" + s for s in SKIP_SUFFIX])

MAXCS = 30       # chosen limit under test in (2)
MIN_TOUCHES = 3
TOP = 10


def git(d, *a):
    return subprocess.run(["git", "-C", d, *a], capture_output=True, text=True, check=True).stdout


def load(projects, repo, n=8000):
    """Newest-first code-touching non-merge commits: (total code files, [files still tracked])."""
    d = os.path.join(projects, repo)
    tracked = set(git(d, "ls-files", "-z").split("\0"))
    out = git(d, "-c", "core.quotePath=false", "log", "-%d" % n, "--no-merges", "--name-only", "--format=tformat:%x01%ct", "--", *SPECS)
    commits = []
    for blk in out.split("\x01")[1:]:
        names = [l for l in blk.split("\n")[1:] if l]
        commits.append((len(set(names)), [l for l in names if l in tracked]))
    return commits


def hot(commits, k):
    c, last = collections.Counter(), {}
    for i, (total, files) in enumerate(commits[:k]):
        if total > MAXCS:
            continue
        for f in set(files):
            c[f] += 1
            last.setdefault(f, i)
    ranked = sorted((f for f in c if c[f] >= MIN_TOUCHES), key=lambda f: (-c[f], last[f], f))
    return ranked[:TOP]


def q(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p))]


def window(all_commits, ks=(50, 100, 200, 400), H=100):
    means = {}
    for repo, commits in all_commits.items():
        tot, maxk = len(commits), max(ks)
        if tot < H + maxk + 10:
            print("  window %-12s too short (%d code commits)" % (repo, tot))
            continue
        splits = list(range(H, tot - maxk, max(10, (tot - maxk - H) // 16)))
        res, sizes = {k: [] for k in ks}, {k: [] for k in ks}
        for s in splits:
            touched = set(f for _, files in commits[s - H:s] for f in files)
            for k in ks:
                top = hot(commits[s:], k)
                sizes[k].append(len(top))
                if top:
                    res[k].append(sum(f in touched for f in top) / len(top))
        print("  window %-12s splits=%2d " % (repo, len(splits)) + " ".join("K=%d:%.3f(len %.1f)" % (k, statistics.mean(res[k]), statistics.mean(sizes[k])) for k in ks))
        for k in ks:
            means.setdefault(k, []).append(statistics.mean(res[k]))
    print("  window mean over repos: " + " ".join("K=%d:%.3f" % (k, statistics.mean(v)) for k, v in means.items()))


def changeset(all_commits):
    for repo, commits in all_commits.items():
        sizes = [t for t, _ in commits]
        print("  changeset %-12s n=%d code files/commit p50=%d p90=%d p95=%d p99=%d  share over %d: %.3f"
              % (repo, len(sizes), q(sizes, .5), q(sizes, .9), q(sizes, .95), q(sizes, .99), MAXCS, sum(s > MAXCS for s in sizes) / len(sizes)))


def cochange(all_commits, H=150, K=400):
    for repo, commits in all_commits.items():
        tot = len(commits)
        if tot < H + K + 10:
            print("  cochange %-12s too short" % repo)
            continue
        splits = list(range(H, tot - K, max(10, (tot - K - H) // 10)))
        for maxcs in (15, 30, 60):
            for minco, minconf in ((2, .3), (3, .3), (3, .5), (5, .3), (5, .5)):
                tp = n = 0
                for s in splits:
                    cnt, co = collections.Counter(), collections.Counter()
                    for total, files in commits[s:s + K]:
                        fs = sorted(set(files))
                        if total > maxcs:
                            continue
                        for f in fs:
                            cnt[f] += 1
                        for a, b in itertools.permutations(fs, 2):
                            co[(a, b)] += 1
                    rules = collections.defaultdict(list)
                    for (a, b), c in co.items():
                        if c >= minco and c / cnt[a] >= minconf:
                            rules[a].append(b)
                    for total, files in commits[s - H:s]:
                        if total > maxcs:
                            continue
                        fs = set(files)
                        for a in fs:
                            for b in rules.get(a, ()):
                                n += 1
                                tp += b in fs
                print("  cochange %-12s maxcs=%2d minco=%d minconf=%.1f rule applications=%6d precision=%.3f" % (repo, maxcs, minco, minconf, n, tp / n if n else float("nan")))


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--repos", nargs="+", required=True, help="checkout names under --projects")
    ap.add_argument("--projects", default=os.path.expanduser(os.environ.get("TACIT_PROJECTS", "~/projects")))
    ap.add_argument("--only", choices=["window", "changeset", "cochange"])
    a = ap.parse_args()
    data = {r: load(a.projects, r) for r in a.repos}
    if a.only in (None, "window"):
        window(data)
    if a.only in (None, "changeset"):
        changeset(data)
    if a.only in (None, "cochange"):
        cochange(data)
