#!/usr/bin/env python3
"""Generate the client's real agent configuration in a disposable profile."""

import argparse
import json
import os
import shutil
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    root = args.out.resolve()
    # Refuse reuse so a local run cannot replace an existing agent profile.
    root.mkdir(parents=True, exist_ok=False)
    config = root / "tacit.yaml"
    config.write_text("{}\n", encoding="utf-8")
    env = {key: value for key, value in os.environ.items()
           if not key.startswith("TACIT_")}
    env.update(HOME=str(root), USERPROFILE=str(root),
               APPDATA=str(root / "AppData" / "Roaming"),
               XDG_CONFIG_HOME=str(root / ".config"))
    for harness in ("claude", "codex"):
        result = subprocess.run(
            [str(binary), "--config", str(config), "install", harness],
            env=env, check=True, capture_output=True, text=True, timeout=30,
        )
        if harness == "codex":
            # The installer prints ready-to-paste TOML after its hook notice.
            marker = "[mcp_servers.tacit]"
            _, found, body = result.stdout.partition(marker)
            if not found:
                raise RuntimeError("Codex installer did not print its MCP configuration")
            (root / ".codex" / "config.toml").write_text(
                marker + body, encoding="utf-8",
            )
    for path in (root / ".claude" / "settings.json", root / ".codex" / "hooks.json"):
        hooks = json.loads(path.read_text(encoding="utf-8"))["hooks"]
        if not all(hooks.get(event) for event in ("SessionStart", "UserPromptSubmit")):
            raise RuntimeError(f"Missing generated hooks in {path}")
    # agnix 0.57 recognizes hook JSON at .claude/settings.json. Codex uses
    # the same event format but its hooks.json path is not yet recognized.
    # Lint a byte-for-byte copy through that validator as well.
    mirror = root / "codex-hooks" / ".claude" / "settings.json"
    mirror.parent.mkdir(parents=True)
    shutil.copyfile(root / ".codex" / "hooks.json", mirror)
    print(f"Generated Claude hooks, Codex hooks and Codex MCP configuration in {root}")


if __name__ == "__main__":
    main()
