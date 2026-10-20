"""tacit: Hermes memory provider over the shared tacit fact store.

Installed by `tacit install hermes`. It calls the same two hooks Claude Code and
Codex use, so every harness gets the same facts by the same rules:

- system_prompt_block: `tacit hook session-start` (owner rules and the facts for
  this host, at most 10 lines), computed once per session in the background.
- prefetch: `tacit hook prompt` with the user's message (at most 3 facts above
  the match floor, each fact once per session).
- on_pre_compress: `tacit hook pre-compact`, which starts a dream of this
  session in the background and returns at once, so what the compaction is
  about to drop from the context is in the inbox soon after.
- on_memory_write: Hermes' own memory writes go to the tacit inbox as
  hermes@<host> notes, through `tacit add` (which redacts with scrump), so the
  curator sees them like any other harness's notes.

It never touches MEMORY.md / USER.md; the built-in memory tool keeps working.
Search, fetch and add tools come from the tacit MCP server, not from here.
Lines are rescanned with Hermes' threat scanner before use (fail closed).
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import subprocess
import threading
from pathlib import Path
from typing import Any, Dict, List, Optional

from agent.memory_provider import MemoryProvider, RecallStatus, is_trivial_prompt

logger = logging.getLogger(__name__)

_TACIT = os.environ.get("TACIT_BIN", @TACIT_BIN@)
_SETTINGS = @TACIT_SETTINGS@
_STORE = Path(os.path.expanduser(os.environ.get("TACIT_STORE", @TACIT_STORE@)))
_START_WAIT = 12.0   # the hook's own sync budget is 8 s
_PROMPT_WAIT = 4.0   # the hook's query budget is 2 s


def _command(args: List[str]) -> List[str]:
    return [_TACIT, *(["--config", _SETTINGS] if _SETTINGS else []), *args]


def _run(args: List[str], payload: Dict[str, Any], timeout: float) -> str:
    try:
        out = subprocess.run(
            _command(args), input=json.dumps(payload), capture_output=True,
            text=True, timeout=timeout, check=False,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        logger.debug("tacit %s failed: %s", args, exc)
        return ""
    return out.stdout.strip()


def _session_key(session_id: str) -> str:
    """tacit keys per-session dedupe on a slug; Hermes ids carry underscores."""
    return "hermes-" + re.sub(r"[^a-z0-9-]+", "-", str(session_id).lower()).strip("-")


def _scan(block: str) -> str:
    """Drop lines Hermes' threat scanner flags. No scanner, no facts."""
    if not block:
        return ""
    try:
        from tools.threat_patterns import scan_for_threats
    except Exception:
        logger.warning("tacit: Hermes threat scanner unavailable; facts not loaded")
        return ""
    kept = [line for line in block.splitlines() if not scan_for_threats(line, scope="strict")]
    return "\n".join(kept) if any(line.startswith("- ") for line in kept) else ""


class TacitProvider(MemoryProvider):
    def __init__(self) -> None:
        self._session = ""
        self._session_id = ""
        self._cwd = ""
        self._context = "primary"
        self._block = ""
        self._start: Optional[threading.Thread] = None
        self._gen = 0
        self._lock = threading.Lock()
        self._mirrored: set = set()
        self._last_count = 0

    @property
    def name(self) -> str:
        return "tacit"

    def is_available(self) -> bool:
        return os.access(_TACIT, os.X_OK) and (_STORE / ".git").exists()

    def unavailable_reason(self) -> str:
        return f"tacit is not installed ({_TACIT}) or has no store at {_STORE}; run `tacit init` locally or `tacit clone` for a server."

    def initialize(self, session_id: str, **kwargs) -> None:
        self._session = _session_key(session_id)
        self._session_id = str(session_id)
        # Hermes passes the session directory as cwd; agent_workspace is a name.
        cwd = str(kwargs.get("cwd") or "")
        self._cwd = cwd if os.path.isabs(cwd) and os.path.isdir(cwd) else os.getcwd()
        self._context = str(kwargs.get("agent_context") or "primary")
        # Cron and subagent runs get per-prompt facts only: no sync, no wait.
        if self._context == "primary":
            self._load_start()

    def _load_start(self) -> None:
        with self._lock:
            self._gen += 1
            gen = self._gen
            payload = {"session_id": self._session, "cwd": self._cwd, "source": "startup"}

            def work() -> None:
                block = _scan(_run(["hook", "session-start"], payload, _START_WAIT))
                with self._lock:
                    if gen == self._gen:  # a newer session's load wins
                        self._block = block

            self._block = ""
            self._start = threading.Thread(target=work, name="tacit-start", daemon=True)
            self._start.start()

    def system_prompt_block(self) -> str:
        with self._lock:
            start = self._start
        if start is not None:
            start.join(_START_WAIT)
        with self._lock:
            return self._block

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        self._last_count = 0
        if not query or is_trivial_prompt(query):
            return ""
        with self._lock:
            payload = {"session_id": self._session, "cwd": self._cwd, "prompt": query}
        block = _scan(_run(["hook", "prompt"], payload, _PROMPT_WAIT))
        self._last_count = sum(1 for line in block.splitlines() if line.startswith("- "))
        return block

    def recall_status(self):
        return RecallStatus(provider_label="tacit facts", count=self._last_count) if self._last_count else None

    def on_session_switch(self, new_session_id: str, *, parent_session_id: str = "", reset: bool = False,
                          rewound: bool = False, **kwargs) -> None:
        with self._lock:
            self._session = _session_key(new_session_id)
            self._session_id = str(new_session_id)
        if reset and self._context == "primary":
            self._load_start()

    def on_pre_compress(self, messages) -> str:
        # The dreamer reads the session from state.db by its id; the hook
        # only starts it. Nothing goes into the summary prompt.
        if self._context == "primary":
            with self._lock:
                payload = {"hermes_session_id": self._session_id, "cwd": self._cwd}
            if payload["hermes_session_id"]:
                _run(["hook", "pre-compact"], payload, _PROMPT_WAIT)
        return ""

    def on_memory_write(self, action: str, target: str, content: str,
                        metadata: Optional[Dict[str, Any]] = None) -> None:
        if self._context != "primary" or action not in ("add", "replace"):
            return
        text = (content or "").strip()
        if len(text) < 20:
            return
        digest = hashlib.sha1(text.encode("utf-8")).hexdigest()[:10]
        if digest in self._mirrored:
            return
        self._mirrored.add(digest)
        name = f"hermes-{'user' if target == 'user' else 'memory'}-{digest}"
        # Full text: tacit add redacts first and clips the description after.
        desc = " ".join(text.split())
        args = ["add", "--harness", "hermes", "--type", "user" if target == "user" else "unclassified",
                "--scope", "user", "--kind", "observed", "--desc", desc, "--body", text, name]

        def work() -> None:
            try:
                out = subprocess.run(_command(args), capture_output=True, text=True, timeout=120, check=False)
                if out.returncode != 0:
                    logger.warning("tacit add failed: %s", out.stderr.strip()[:300])
            except (OSError, subprocess.SubprocessError) as exc:
                logger.warning("tacit add failed: %s", exc)

        threading.Thread(target=work, name="tacit-add", daemon=True).start()

    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        return []


def register(ctx) -> None:
    ctx.register_memory_provider(TacitProvider())
