#!/usr/bin/env python3
"""What the idle dreamer costs and saves, on this machine's own history.

Reads, read-only, every top-level Claude Code, Codex and pi transcript
written in the last N days (line times and byte offsets, and the compaction
entries SPEC.md "When a transcript is dreamed" names), and replays two
schedules over them:

  now:  a full dream every 4 hours;
  idle: the same 4-hour dreams, plus a run every 10 minutes that takes a
        transcript quiet for 20 minutes or whose delta holds a compaction,
        plus the PreCompact hook's dream at each Claude or Codex compaction.

An event is one transcript delta of more than 4,000 bytes taken by one run:
about one model call (a window is up to 60,000 characters of digest). Both
schedules read the same bytes, so the idle schedule's extra cost is its extra
calls times the prompt overhead every call carries. The delay is
byte-weighted: for each byte, the time from its write to the run that took it.

Then it reads the dreamer's and the curator's meter logs (dreamer.log,
curator.log in the state directory) for the cost per run and the cheapest
calls per role and model, which is that fixed overhead.

  python3 eval/dream/idle_schedule.py [--days 4] [--runs 24]

Hermes sessions are not replayed (their times are in the store, not in
files). Transcripts listed in dream-exclude (tacit's own model calls) are
left out. Prints JSON; writes nothing.
"""
import argparse
import bisect
import datetime as dt
import glob
import json
import os
import statistics

MIN_NEW = 4000
IDLE = 20 * 60


def state_dir():
    return os.environ.get("TACIT_STATE_DIR") or os.path.expanduser("~/.local/state/tacit")


def stamp(v):
    if isinstance(v, (int, float)):
        return v / 1000 if v > 1e11 else v
    try:
        return dt.datetime.fromisoformat(str(v).replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def compacted(e, fmt):
    if fmt == "claude":
        return e.get("isCompactSummary") is True or (e.get("type") == "system" and e.get("subtype") == "compact_boundary")
    if fmt == "codex":
        return e.get("type") == "compacted"
    return e.get("type") == "compaction"


def transcripts(start):
    home = os.path.expanduser("~")
    excl = set()
    p = os.path.join(state_dir(), "dream-exclude")
    if os.path.exists(p):
        excl = set(open(p).read().split())
    out = []
    for pattern, fmt in ((".claude/projects/*/*.jsonl", "claude"), (".codex/sessions/*/*/*/*.jsonl", "codex"), (".pi/agent/sessions/*/*.jsonl", "pi")):
        for f in glob.glob(os.path.join(home, pattern)):
            name = os.path.basename(f)[:-6]
            if name in excl or name.split("_")[-1] in excl:
                continue
            try:
                if os.path.getmtime(f) < start:
                    continue
            except OSError:
                continue
            rows, off, last = [], 0, None
            with open(f, "rb") as fh:
                for raw in fh:
                    off += len(raw)
                    t, c = None, False
                    try:
                        e = json.loads(raw)
                        t, c = stamp(e.get("timestamp")), compacted(e, fmt)
                    except (ValueError, AttributeError):
                        pass
                    # Times as written can step back (a resumed session); the
                    # replay needs them in file order.
                    t = max(t or 0, last or 0) or None
                    if t is not None:
                        last = t
                        rows.append((t, off, c))
            if rows:
                out.append((fmt, [r[0] for r in rows], [r[1] for r in rows], [r[1] for r in rows if r[2]]))
    return out


def replay(files, start, end, idle):
    events, delays = [], []
    days = (end - start) / 86400
    ticks = [(start + i * 4 * 3600, "full") for i in range(int(days * 6) + 1)]
    if idle:
        ticks += [(start + i * 600, "idle") for i in range(int(days * 144) + 1)]
    for fmt, times, offs, comps in files:
        def size(t):
            i = bisect.bisect_right(times, t)
            return offs[i - 1] if i else 0
        cur = size(start)
        hooks = [(times[offs.index(o)], "hook") for o in comps] if idle and fmt != "pi" else []
        for t, kind in sorted(ticks + [h for h in hooks if h[0] >= start]):
            if t > end:
                break
            now = size(t)
            if now - cur <= MIN_NEW:
                continue
            if kind == "idle":
                i = bisect.bisect_right(times, t)
                quiet = t - (times[i - 1] if i else 0) >= IDLE
                if not quiet and not any(cur < o <= now for o in comps):
                    continue
            events.append(kind)
            i, prev = bisect.bisect_right(offs, cur), cur
            while i < len(offs) and offs[i] <= now:
                delays.append((t - times[i], offs[i] - prev))
                prev = offs[i]
                i += 1
            cur = now
    return events, delays


def weighted_median_hours(pairs):
    pairs.sort()
    total, acc = sum(b for _, b in pairs), 0
    for d, b in pairs:
        acc += b
        if acc >= total / 2:
            return round(d / 3600, 2)
    return None


def meters(name, runs):
    p = os.path.join(state_dir(), name)
    if not os.path.exists(p):
        return None
    rows = [json.loads(l) for l in open(p) if l.strip()][-runs:]
    calls = [c for r in rows for c in (r.get("model_calls") or {}).get("calls", []) if not c.get("failure")]
    floor = {}
    for c in calls:
        key = c["role"] + " " + c["route"]["model"]
        cheapest = floor.get(key)
        if cheapest is None or (c.get("input_tokens") or 0) < cheapest["input_tokens"]:
            floor[key] = {"input_tokens": c.get("input_tokens") or 0, "cost_usd": round(c.get("cost_usd") or 0, 4)}
    return {
        "runs": len(rows),
        "from": rows[0]["at"] if rows else None,
        "to": rows[-1]["at"] if rows else None,
        "cost_usd": round(sum(r.get("cost_usd") or 0 for r in rows), 2),
        "median_cost_usd": round(statistics.median(r.get("cost_usd") or 0 for r in rows), 3) if rows else None,
        "calls": len(calls),
        "cheapest_call": floor,
        "last": {k: rows[-1].get(k) for k in ("windows", "calls", "notes", "facts", "cost_usd")} if rows else None,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--days", type=float, default=4)
    ap.add_argument("--runs", type=int, default=24, help="meter lines read from each log")
    a = ap.parse_args()
    end = dt.datetime.now(dt.timezone.utc).timestamp()
    start = end - a.days * 86400
    files = transcripts(start)
    out = {"days": a.days, "transcripts": len(files), "compactions": sum(len(f[3]) for f in files)}
    for name, idle in (("now", False), ("idle", True)):
        ev, delays = replay(files, start, end, idle)
        out[name] = {"events": len(ev), "per_day": round(len(ev) / a.days, 1), "by_run": {k: ev.count(k) for k in ("full", "idle", "hook") if ev.count(k)}, "delay_hours": weighted_median_hours(delays)}
    out["dreamer_log"] = meters("dreamer.log", a.runs)
    out["curator_log"] = meters("curator.log", 4)
    print(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()
