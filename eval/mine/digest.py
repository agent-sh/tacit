#!/usr/bin/env python3
"""Compact per-session digests from Claude Code and Codex transcripts.

Keeps only what the truth-set miners need: human turns, harness-injected
turns (labelled apart), tool errors, which files and commands were touched,
and short assistant conclusions. Raw tool output is dropped and secrets are
redacted. Output goes under $TACIT_MINE_OUT, else ~/.cache/tacit-mine (disk,
not /tmp). Transcripts are read from ~/.claude/projects and ~/.codex/sessions,
or from $TACIT_MINE_CLAUDE and $TACIT_MINE_CODEX.

The header names the session, its cwd, start time, transcript and `entry`:
how it was started (Claude Code `entrypoint`: cli and claude-desktop are a
person, sdk-cli and sdk-ts are headless or SDK runs whose user turns an agent
wrote; Codex `originator`: codex_exec is headless). `tacit rulings audit`
counts only a person's U: lines as the owner's words.

Line labels: U: human text, H: text injected by the harness (notifications,
command echoes, permission frames), A: assistant text, T: tool call, E: tool
error. Miners must not mint user standards from H: lines.

Set TACIT_MINE_SUBAGENTS=1 to also digest <project>/<session>/subagents/*.jsonl
(they can far outnumber the sessions; slow).
"""
import json
import os
import re
import sys
from pathlib import Path

OUT = Path(os.path.expanduser(os.environ.get("TACIT_MINE_OUT", str(Path.home() / ".cache/tacit-mine"))))
U_MAX, A_MAX, E_MAX, T_MAX, H_MAX = 700, 300, 240, 140, 200

# Harness-injected user text. Claude Code wraps its own injections in tags;
# Codex prefixes them with ">>>" frames or "Host notice".
CLAUDE_HARNESS = re.compile(r"^\s*<(task-notification|local-command-stdout|local-command-caveat|command-name|command-message|command-args|system-reminder|agent-message|bash-input|bash-stdout|bash-stderr)\b")
CODEX_HARNESS = re.compile(r"^\s*(>>>|Host notice|<environment_context>|# AGENTS\.md|\[Recorded tool history\]|<turn_aborted>|<permissions|<collaboration)")

SECRETS = [
    re.compile(r"\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b"),
    re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}\b"),
    re.compile(r"\bsk-(?:proj-|ant-)?[A-Za-z0-9_-]{16,}\b"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}\b"),
    re.compile(r"\btskey-[A-Za-z0-9-]{10,}\b"),
    re.compile(r"\bhf_[A-Za-z0-9]{20,}\b"),
    re.compile(r"(?i)\b(bearer)\s+[A-Za-z0-9._-]{20,}"),
    re.compile(r"(?i)\b([a-z_]*(?:token|secret|password|passwd|api[_-]?key|private[_-]?key)[a-z_]*)\s*[=:]\s*['\"]?[^\s'\"]{8,}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b"),
]


def redact(s):
    for rx in SECRETS:
        s = rx.sub(lambda m: (m.group(1) + "=" if m.lastindex else "") + "[REDACTED]", s)
    return s


def clip(s, n):
    s = re.sub(r"\s+", " ", redact(s or "")).strip()
    return s if len(s) <= n else s[: n - 1] + "…"


def tool_line(name, inp):
    if not isinstance(inp, dict):
        return clip(f"T: {name}", T_MAX)
    key = inp.get("file_path") or inp.get("path") or inp.get("command") or inp.get("cmd") or inp.get("pattern") or inp.get("query") or inp.get("url") or ""
    return clip(f"T: {name} {key}", T_MAX)


def blocks_text(c):
    """Flatten a content value (str, list of blocks, dict) to text."""
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return " ".join(blocks_text(x) for x in c)
    if isinstance(c, dict):
        return str(c.get("text") or c.get("content") or c.get("output") or "")
    return str(c) if c is not None else ""


def user_text_line(txt, harness_rx):
    if harness_rx.search(txt):
        return "H: " + clip(txt, H_MAX)
    return "U: " + clip(txt, U_MAX)


ERR_RX = re.compile(r"(?i)\b(error|failed|not found|no such|denied|traceback|panic|exited with code [1-9]\d*|exit status [1-9]\d*)\b")


def claude_session(path):
    lines, meta = [], {}
    for raw in open(path, errors="replace"):
        try:
            o = json.loads(raw)
        except json.JSONDecodeError:
            continue
        t = o.get("type")
        if t not in ("user", "assistant"):
            continue
        if not meta:
            meta = {"cwd": o.get("cwd"), "session": o.get("sessionId"), "ts": o.get("timestamp"), "branch": o.get("gitBranch"), "entry": o.get("entrypoint")}
        m = o.get("message") or {}
        content = m.get("content")
        if t == "user":
            meta_turn = bool(o.get("isMeta"))
            if isinstance(content, str):
                if content.strip():
                    lines.append("H: " + clip(content, H_MAX) if meta_turn else user_text_line(content, CLAUDE_HARNESS))
            elif isinstance(content, list):
                for b in content:
                    bt = b.get("type")
                    if bt == "text" and b.get("text", "").strip():
                        lines.append("H: " + clip(b["text"], H_MAX) if meta_turn else user_text_line(b["text"], CLAUDE_HARNESS))
                    elif bt == "tool_result" and b.get("is_error"):
                        lines.append("E: " + clip(blocks_text(b.get("content")), E_MAX))
        else:
            if isinstance(content, list):
                for b in content:
                    bt = b.get("type")
                    if bt == "text" and b.get("text", "").strip():
                        lines.append("A: " + clip(b["text"], A_MAX))
                    elif bt == "tool_use":
                        lines.append(tool_line(b.get("name"), b.get("input")))
    return meta, lines


def codex_session(path):
    lines, meta = [], {}
    for raw in open(path, errors="replace"):
        try:
            o = json.loads(raw)
        except json.JSONDecodeError:
            continue
        p = o.get("payload") or {}
        if o.get("type") == "session_meta":
            meta = {"cwd": p.get("cwd"), "session": p.get("id"), "ts": p.get("timestamp"), "entry": p.get("originator")}
            continue
        if o.get("type") != "response_item":
            continue
        pt = p.get("type")
        if pt == "message":
            role = p.get("role")
            for b in p.get("content") or []:
                txt = b.get("text") or ""
                if not txt.strip():
                    continue
                if role == "user":
                    lines.append(user_text_line(txt, CODEX_HARNESS))
                elif role == "assistant":
                    if txt.startswith("[Recorded tool history]"):
                        continue
                    lines.append("A: " + clip(txt, A_MAX))
        elif pt in ("function_call", "custom_tool_call"):
            args = p.get("arguments") or p.get("input") or ""
            try:
                args = json.loads(args) if isinstance(args, str) else args
            except json.JSONDecodeError:
                pass
            lines.append(tool_line(p.get("name"), args if isinstance(args, dict) else {"cmd": str(args)}))
        elif pt in ("function_call_output", "custom_tool_call_output"):
            s = blocks_text(p.get("output"))
            if ERR_RX.search(s[:400]):
                lines.append("E: " + clip(s, E_MAX))
    return meta, lines


def write(dst, meta, lines, src):
    if not lines:
        return False
    dst.parent.mkdir(parents=True, exist_ok=True)
    with open(dst, "w") as f:
        f.write(f"# session {meta.get('session')}\n# cwd {meta.get('cwd')}  ts {meta.get('ts')}  src {src}  entry {meta.get('entry')}\n\n")
        f.write("\n".join(lines) + "\n")
    return True


def main():
    n = 0
    projects = Path(os.path.expanduser(os.environ.get("TACIT_MINE_CLAUDE", str(Path.home() / ".claude/projects"))))
    if projects.is_dir():
        for proj in projects.iterdir():
            for f in proj.glob("*.jsonl"):
                meta, lines = claude_session(f)
                if write(OUT / "claude" / proj.name / (f.stem + ".md"), meta, lines, str(f)):
                    n += 1
            if os.environ.get("TACIT_MINE_SUBAGENTS") == "1":
                for f in proj.glob("*/subagents/*.jsonl"):
                    meta, lines = claude_session(f)
                    dst = OUT / "claude" / proj.name / f"{f.parents[1].name}--sub-{f.stem}.md"
                    if write(dst, meta, lines, str(f)):
                        n += 1
    sessions = Path(os.path.expanduser(os.environ.get("TACIT_MINE_CODEX", str(Path.home() / ".codex/sessions"))))
    if sessions.is_dir():
        for f in sessions.rglob("*.jsonl"):
            meta, lines = codex_session(f)
            if write(OUT / "codex" / (f.stem + ".md"), meta, lines, str(f)):
                n += 1
    print(f"wrote {n} digests under {OUT}")


if __name__ == "__main__":
    sys.exit(main())
