#!/usr/bin/env python3
"""Decide whether `tailscale funnel --https=443 TARGET` is safe on this node.

    tailscale serve status --json | funnel_guard.py HOST:443 TARGET

Funnel is allowed per host:port (AllowFunnel["HOST:443"]), not per mount, so
turning it on publishes every handler on 443 of HOST to the internet. Safe
only when 443 serves nothing yet, or serves exactly "/" -> TARGET (a rerun).
The serve config (ipn.ServeConfig) keys TCP by port ("443") and Web and
AllowFunnel by "HOST:443"; Foreground holds the configs of foreground
`tailscale serve` sessions, which publish the same way, and Services holds
VIP services on other addresses, which this node's 443 does not touch.

Exit 0: safe (prints "fresh" or "already"). Exit 1: refused, with the reason.
Exit 2: the input is not a serve config.
"""
import json
import sys


def check(cfg, hostport, target):
    port = hostport.rsplit(":", 1)[1]
    configs = [cfg] + list((cfg.get("Foreground") or {}).values())
    mapped = False
    for c in configs:
        tcp = (c.get("TCP") or {}).get(port) or {}
        if tcp.get("TCPForward"):
            return False, f"port {port} forwards raw TCP to {tcp['TCPForward']}"
        if tcp.get("TerminateTLS"):
            return False, f"port {port} terminates TLS for {tcp['TerminateTLS']}"
        web = (c.get("Web") or {}).get(hostport) or {}
        handlers = web.get("Handlers") or {}
        if not handlers:
            continue
        if c is not cfg:
            return False, f"a foreground serve session has handlers on {hostport}: {sorted(handlers)}"
        if handlers != {"/": {"Proxy": target}}:
            return False, f"{hostport} already serves {json.dumps(handlers, sort_keys=True)}; Funnel would publish all of it"
        mapped = True
    return True, "already" if mapped else "fresh"


def main():
    if len(sys.argv) != 3 or ":" not in sys.argv[1]:
        print("usage: funnel_guard.py HOST:443 TARGET < serve-status.json", file=sys.stderr)
        return 2
    raw = sys.stdin.read().strip()
    try:
        cfg = json.loads(raw) if raw else {}
    except json.JSONDecodeError as e:
        print(f"not a serve config: {e}", file=sys.stderr)
        return 2
    if not isinstance(cfg, dict):
        print("not a serve config: not an object", file=sys.stderr)
        return 2
    ok, why = check(cfg, sys.argv[1], sys.argv[2])
    print(why if ok else "refused: " + why)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
