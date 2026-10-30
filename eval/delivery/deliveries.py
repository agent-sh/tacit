#!/usr/bin/env python3
"""Turn the tacit blocks Claude Code transcripts recorded into replay cases
for labelled_replay.py. Read-only on transcripts and the store clone.

  deliveries.py --store CLONE --out DIR [--labels labels.tsv] [--silent] TRANSCRIPT...

Writes DIR/cases.jsonl and DIR/transcripts/<id>.jsonl: one case per prompt
block (UserPromptSubmit) and per tool-call block (PreToolUse) found, with

- the prompt, or the tool call and its input;
- the cwd the hook saw: the prompt's or the call's own record (Claude Code
  records the shell's directory there; the hook's record may hold another);
- the session's facts so far: every name an earlier block of the session
  printed, and "area:<folder>" for an area it pushed, also from blocks that
  make no case (their prompt or call is not in the transcript);
- the transcript as it stood (its last 512 KiB before the prompt, or up to
  the call), for the prompt hook's activity rule;
- the store commit the hook read: the clone's last first-parent commit at
  or before the hook ran;
- origin: typed, queued (a message typed while the agent worked), meta (a
  harness's turn) or headless (an SDK entrypoint);
- the names printed then, and their grades from --labels (TSV: case id,
  fact name, R|W|N), when given.

Claude Code records no hook output when the hook printed nothing, so a call
tacit stayed silent on makes no case. --silent adds them: every prompt and
every tool call of a tool the PreToolUse hook matches, with nothing
delivered (silent: true). A rule that can add lines needs them, or what it
adds where the hook said nothing is never seen.

A case's id is stable (a hash of its transcript's real path, event and
time), so labels written once keep their case across runs. A cwd that is
gone and was a Claude Code agent worktree (<repo>/.claude/worktrees/<name>)
gets a stand-in worktree of the same repository under DIR/stand, and an
edit's file path moves into it; any other cwd that is gone is noted
(cwd_note: gone), and its case replays outside a repository.
"""
import argparse
import bisect
import hashlib
import json
import os
import re
import subprocess
import sys
from datetime import datetime

TAIL = 512 * 1024
NAME = re.compile(r"^- (?:RULE )?([a-z0-9][a-z0-9-]*)")
WORKTREE = re.compile(r"^(.+?)/\.claude/worktrees/([^/]+)(/.*)?$")
# The tools `tacit install` matches PreToolUse on (toolPushTools).
PUSH_TOOLS = re.compile(r"^(Edit|Write|MultiEdit|NotebookEdit|Bash|apply_patch|mcp__.*)$")


def text_of(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(x.get("text", "") if isinstance(x, dict) and x.get("type") == "text" else (x if isinstance(x, str) else "") for x in content)
    return ""


def tacit_blocks(a):
    """The tacit blocks of a hook attachment: [text]."""
    out = []
    for key in ("stdout", "content"):
        v = a.get(key)
        if isinstance(v, list):
            v = "\n".join(x if isinstance(x, str) else text_of([x]) for x in v)
        if not isinstance(v, str) or "# tacit" not in v:
            continue
        s = v.strip()
        if s.startswith("{"):
            try:
                ctx = json.loads(s).get("hookSpecificOutput", {}).get("additionalContext", "")
                if "# tacit" in ctx:
                    out.append(ctx)
                    continue
            except ValueError:
                pass
        out.append(s[s.find("# tacit"):])
    seen, res = set(), []
    for b in out:
        if b.strip() not in seen:
            seen.add(b.strip())
            res.append(b)
    return res


def parse(block):
    names, rules, areas = [], [], []
    for line in block.splitlines():
        m = NAME.match(line)
        if m:
            names.append(m.group(1))
            if line.startswith("- RULE "):
                rules.append(m.group(1))
        a = re.search(r"^# more on .*`tacit_tree (\S+?)`", line)
        if a:
            areas.append("area:" + a.group(1))
    return names, rules, areas


def read(path):
    """One pass over a transcript: its records in order, with byte offsets."""
    recs, off = [], 0
    with open(path, "rb") as fh:
        for raw in fh:
            start, off = off, off + len(raw)
            try:
                recs.append((start, off, json.loads(raw)))
            except ValueError:
                continue
    return recs


def tsec(ts):
    return datetime.fromisoformat(ts.replace("Z", "+00:00")).timestamp()


def stand_in(cwd, out):
    """A stand-in linked worktree for a Claude Code agent worktree that is gone."""
    m = WORKTREE.match(cwd)
    if not m or not os.path.isdir(os.path.join(m.group(1), ".git")):
        return None
    main, name, rest = m.group(1), m.group(2), m.group(3) or ""
    key = hashlib.sha1(cwd[: len(cwd) - len(rest)].encode()).hexdigest()[:12]
    gitdir = os.path.join(out, "stand", ".gitdirs", key)
    root = os.path.join(out, "stand", key, name)
    os.makedirs(gitdir, exist_ok=True)
    os.makedirs(root + rest, exist_ok=True)
    for name_, text in (("HEAD", "ref: refs/heads/replay-stand-in\n"), ("commondir", os.path.join(main, ".git") + "\n")):
        with open(os.path.join(gitdir, name_), "w") as fh:
            fh.write(text)
    with open(os.path.join(root, ".git"), "w") as fh:
        fh.write("gitdir: " + gitdir + "\n")
    for mf in ("package.json", "Cargo.toml", "pyproject.toml", "go.mod"):
        if os.path.isfile(os.path.join(main, mf)):
            with open(os.path.join(main, mf), "rb") as src, open(os.path.join(root, mf), "wb") as dst:
                dst.write(src.read())
    return cwd[: len(cwd) - len(rest)], root, root + rest


def is_tool_result(rec):
    c = rec.get("message", {}).get("content")
    return isinstance(c, list) and any(isinstance(x, dict) and x.get("type") == "tool_result" for x in c)


def prompt_of(rec):
    """(text, origin) when rec is a turn that fires the prompt hook, else None."""
    if rec.get("type") == "attachment":
        qa = rec.get("attachment") or {}
        if qa.get("type") == "queued_command" and isinstance(qa.get("prompt"), str) and qa["prompt"].strip():
            return qa["prompt"], "queued"  # a message typed while the agent worked
        return None
    if rec.get("type") != "user" or is_tool_result(rec):
        return None
    if rec.get("entrypoint") in ("sdk-cli", "sdk-ts"):
        origin = "headless"
    elif rec.get("isMeta"):
        origin = "meta"  # a harness's turn fires the hook too
    else:
        origin = "typed"
    return text_of(rec.get("message", {}).get("content")), origin


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--store", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--labels")
    ap.add_argument("--silent", action="store_true", help="also every prompt and push-matched tool call the hook printed nothing for")
    ap.add_argument("transcripts", nargs="+")
    a = ap.parse_args()
    a.out = os.path.abspath(a.out)
    os.makedirs(os.path.join(a.out, "transcripts"), exist_ok=True)
    log = subprocess.check_output(["git", "-C", a.store, "log", "--first-parent", "--format=%H %ct", "HEAD"], text=True)
    commits = sorted((int(l.split()[1]), l.split()[0]) for l in log.splitlines() if l.strip())
    times = [t for t, _ in commits]
    labels = {}
    if a.labels:
        with open(a.labels) as fh:
            for l in fh:
                f = l.rstrip("\n").split("\t")
                if len(f) >= 3 and f[2] in ("R", "W", "N"):
                    labels.setdefault(f[0], {})[f[1]] = f[2]
    blocks = []
    for path in a.transcripts:
        path = os.path.realpath(path)
        recs = read(path)
        start_cwd = next((r.get("cwd") for _, _, r in recs if r.get("cwd")), None)
        by_uuid = {r.get("uuid"): (s, e, r) for s, e, r in recs if r.get("uuid")}
        calls = {}
        for s, e, r in recs:
            if r.get("type") == "assistant" and isinstance(r.get("message", {}).get("content"), list):
                for x in r["message"]["content"]:
                    if isinstance(x, dict) and x.get("type") == "tool_use":
                        calls[x.get("id")] = (e, r, x)
        base = {"file": path, "start_cwd": start_cwd}
        answered = set()  # the prompts and calls a tacit block answered
        for s, e, r in recs:
            att = r.get("attachment") or {}
            if r.get("type") != "attachment" or att.get("hookEvent") not in ("UserPromptSubmit", "PreToolUse", "SessionStart"):
                continue
            for block in tacit_blocks(att):
                names, rules, areas = parse(block)
                b = dict(base, session=r.get("sessionId"), ts=r.get("timestamp") or "", event=att["hookEvent"],
                         names=names, rules=rules, areas=areas, sidechain=bool(r.get("isSidechain")))
                if att["hookEvent"] == "PreToolUse":
                    c = calls.get(att.get("toolUseID"))
                    if not c:
                        # No case without its call, but the session got the names.
                        blocks.append(dict(b, case=False))
                        continue
                    answered.add(att.get("toolUseID"))
                    b.update(cut=c[0], cwd=c[1].get("cwd") or r.get("cwd"), tool_name=c[2].get("name"), tool_input=c[2].get("input"), key=att.get("toolUseID"))
                elif att["hookEvent"] == "UserPromptSubmit":
                    # the turn that fired the hook: up the parent chain to a prompt
                    cur, prompt = r.get("parentUuid"), None
                    for _ in range(80):
                        if cur not in by_uuid:
                            break
                        ps, pe, pr = by_uuid[cur]
                        p = prompt_of(pr)
                        if p:
                            prompt = (ps, pr, p[0], p[1])
                            break
                        cur = pr.get("parentUuid")
                    if not prompt:
                        blocks.append(dict(b, case=False))
                        continue
                    answered.add(prompt[1].get("uuid"))
                    b.update(cut=prompt[0], cwd=prompt[1].get("cwd") or r.get("cwd"), prompt=prompt[2], origin=prompt[3], key=prompt[1].get("uuid"))
                blocks.append(b)
        if not a.silent:
            continue
        for s, e, r in recs:
            p = prompt_of(r)
            if p and r.get("uuid") not in answered and r.get("timestamp"):
                blocks.append(dict(base, session=r.get("sessionId"), ts=r["timestamp"], event="UserPromptSubmit", names=[], rules=[], areas=[],
                                   sidechain=bool(r.get("isSidechain")), cut=s, cwd=r.get("cwd"), prompt=p[0], origin=p[1], key=r.get("uuid"), silent=True))
        for tid, (e, r, x) in calls.items():
            if tid not in answered and PUSH_TOOLS.match(x.get("name") or "") and r.get("timestamp"):
                blocks.append(dict(base, session=r.get("sessionId"), ts=r["timestamp"], event="PreToolUse", names=[], rules=[], areas=[],
                                   sidechain=bool(r.get("isSidechain")), cut=e, cwd=r.get("cwd"), tool_name=x.get("name"), tool_input=x.get("input"), key=tid, silent=True))
    blocks.sort(key=lambda b: b["ts"])
    started = {b["session"] for b in blocks if b["event"] == "SessionStart"}
    seen, out = {}, []
    for b in blocks:
        prior = seen.setdefault(b["session"], [])
        if b["event"] != "SessionStart" and b.get("case", True):
            cid = hashlib.sha1(f'{b["file"]}\0{b["event"]}\0{b["ts"]}\0{b["key"]}'.encode()).hexdigest()[:12]
            cut = os.path.join(a.out, "transcripts", cid + ".jsonl")
            with open(b["file"], "rb") as fh, open(cut, "wb") as dst:
                fh.seek(max(0, b["cut"] - TAIL))
                dst.write(fh.read(b["cut"] - max(0, b["cut"] - TAIL)))
            i = bisect.bisect_right(times, tsec(b["ts"])) - 1
            cwd, note, tool_input = b["cwd"], "", b.get("tool_input")
            if cwd and not os.path.isdir(cwd):
                s = stand_in(cwd, a.out)
                if s:
                    note, cwd = "stand-in", s[2]
                    if isinstance(tool_input, dict):
                        tool_input = {k: (v.replace(s[0], s[1]) if k in ("file_path", "notebook_path", "path") and isinstance(v, str) else v) for k, v in tool_input.items()}
                else:
                    note = "gone"
            start_cwd = b["start_cwd"]
            if start_cwd and not os.path.isdir(start_cwd):
                s = stand_in(start_cwd, a.out)
                start_cwd = s[2] if s else start_cwd
            lab = labels.get(cid, {})
            case = {"id": cid, "set": a.labels and "labelled" or "all", "event": "prompt" if b["event"] == "UserPromptSubmit" else "pre-tool",
                    "ts": b["ts"], "commit": commits[i][1] if i >= 0 else None, "cwd": cwd, "cwd_orig": b["cwd"], "cwd_note": note,
                    "session": b["session"], "sidechain": b["sidechain"], "transcript": cut, "prompt": b.get("prompt", ""),
                    "origin": b.get("origin", "tool"), "silent": bool(b.get("silent")),
                    "seen": list(prior), "start_recorded": b["session"] in started, "start_cwd": start_cwd,
                    "delivered": b["names"], "rules": b["rules"], "labels": {n: g for n, g in lab.items() if n in b["names"]}}
            if case["event"] == "pre-tool":
                case["tool_name"], case["tool_input"] = b["tool_name"], tool_input
            if case["commit"]:
                out.append(case)
        prior.extend(b["names"] + b["areas"])
    with open(os.path.join(a.out, "cases.jsonl"), "w") as fh:
        for c in out:
            fh.write(json.dumps(c) + "\n")
    print(f"{len(out)} cases ({sum(c['silent'] for c in out)} silent) from {len(a.transcripts)} transcripts", file=sys.stderr)


if __name__ == "__main__":
    main()
