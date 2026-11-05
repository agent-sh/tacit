#!/usr/bin/env python3
"""Draws the recall benchmark's charts from recall-summary.csv as SVG, one
light and one dark file per chart (docs/images/recall-*-{light,dark}.svg).

The colors are tacit's own app tokens (client/cmd/tacit/reviewui/app.css and
DESIGN.md): ink on galley, and the query blue for tacit, the one series each
chart is about; by hand is the de-emphasis gray, ink-3. Both were checked
against each theme's surface for contrast (3:1 or more) and for separation
under color-vision deficiency. The table view is the CSV beside the charts,
and a table in README.md between its recall-table markers, which render()
rewrites when it is given the README.

    python3 -I eval/recall/charts.py docs/images/recall-summary.csv docs/images
"""

import csv
import json
import math
import os
import sys

THEMES = {
    "light": {"surface": "#fbfbf8", "border": "#dfe0d9", "ink": "#1c1d1a", "ink2": "#55574f", "muted": "#6b6d64",
              "grid": "#e6e7e0", "axis": "#b9bbb2", "tacit": "#2b52b8", "hand": "#6b6d64",
              # Whiskers: a step of each series toward the surface, still 3:1 on it.
              "tacit_w": "#6a8ade", "hand_w": "#909289"},
    "dark": {"surface": "#1c1f24", "border": "#2b2f35", "ink": "#e7e5de", "ink2": "#b0b0a8", "muted": "#8f9089",
             "grid": "#2b2f35", "axis": "#4a4e55", "tacit": "#6d8ff0", "hand": "#8f9089",
             "tacit_w": "#4d6bc4", "hand_w": "#6b6d68"},
}
FONT = "'Atkinson Hyperlegible Next', 'Atkinson Hyperlegible', system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif"
W, H = 880, 480
LEFT, RIGHT, TOP, BOTTOM = 72, 166, 172, 64
ARMS = [("tacit", "tacit", "tacit: episode find, then show"), ("by hand", "hand", "grep: the matching lines, by hand")]
# Where the two series' ranges meet (the context chart), each sits this far
# left or right of its size, so their whiskers stand apart.
DODGE = 9
# A range shorter than this (px) hides behind its marker: no whisker.
MIN_WHISKER = 12
WHISKERS = "Whiskers show the tasks' min to max; ranges smaller than a marker are not drawn."


def compact(v):
    if v >= 1e6:
        s = "%.1fM" % (v / 1e6)
    elif v >= 1e3:
        s = "%.1fK" % (v / 1e3) if v < 1e4 else "%dK" % round(v / 1e3)
    else:
        s = "%d" % round(v) if v >= 10 or v == int(v) else ("%.1f" % v)
    return s.replace(".0K", "K").replace(".0M", "M")


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def tick_label(metric, v):
    if metric == "latency_ms":
        return "%s ms" % compact(v) if v < 1000 else "%s s" % compact(v / 1000)
    return compact(v)


def end_label(metric, v):
    if metric == "latency_ms":
        return "%d ms" % round(v) if v < 1000 else "%.1f s" % (v / 1000)
    return compact(v) + " chars"


def line_chart(rows, metric, title, subtitle, unit, theme, ticks, dodge):
    t = THEMES[theme]
    sizes = sorted({int(r["mib"]) for r in rows})
    pw, ph = W - LEFT - RIGHT, H - TOP - BOTTOM
    lo, hi = math.log10(ticks[0]), math.log10(ticks[-1])

    def x(mib):
        a, b = math.log2(sizes[0]), math.log2(sizes[-1])
        if b == a:  # one size: the plot's middle
            return LEFT + pw / 2
        return LEFT + 20 + (math.log2(mib) - a) / (b - a) * (pw - 40)

    def y(v):
        return TOP + ph - (math.log10(max(v, ticks[0])) - lo) / (hi - lo) * ph

    o = ['<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="t d">' % (W, H, W, H),
         '<title id="t">%s</title><desc id="d">%s</desc>' % (esc(title), esc(" ".join(subtitle))),
         '<style>text{font-family:%s;fill:%s}.s{fill:%s}.m{fill:%s;font-variant-numeric:tabular-nums}</style>' % (FONT, t["ink"], t["ink2"], t["muted"]),
         '<rect x="0.5" y="0.5" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>' % (W - 1, H - 1, t["surface"], t["border"]),
         '<text x="24" y="38" font-size="18" font-weight="600">%s</text>' % esc(title),
         ]
    for i, line in enumerate(subtitle):
        o.append('<text class="s" x="24" y="%d" font-size="14">%s</text>' % (62 + 20 * i, esc(line)))
    # Legend: a line key with the marker shape each series uses.
    lx = 24
    for arm, key, label in ARMS:
        o.append(_key(lx, 114, t[key], key))
        o.append('<text class="s" x="%d" y="119" font-size="13">%s</text>' % (lx + 30, esc(label)))
        lx += 30 + 7 * len(label) + 36
    # Grid and y ticks.
    for v in ticks:
        yy = y(v)
        o.append('<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>' % (LEFT, LEFT + pw, yy, yy, t["grid"]))
        o.append('<text class="m" x="%d" y="%.1f" font-size="12" text-anchor="end">%s</text>' % (LEFT - 10, yy + 4, tick_label(metric, v)))
    o.append('<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="1"/>' % (LEFT, LEFT + pw, TOP + ph, TOP + ph, t["axis"]))
    for m in sizes:
        o.append('<text class="m" x="%.1f" y="%d" font-size="12" text-anchor="middle">%d</text>' % (x(m), TOP + ph + 20, m))
    o.append('<text class="m" x="%.1f" y="%d" font-size="12" text-anchor="middle">Transcript size, MiB (log scale)</text>' % (LEFT + pw / 2, TOP + ph + 44))
    o.append('<text class="m" x="24" y="%d" font-size="12">%s</text>' % (TOP - 20, esc(unit)))
    # Min to max as a whisker at each size, then the median line and its
    # markers (ringed in the surface, over the whiskers), then end labels.
    ends = []
    # The emphasis series is drawn last, on top.
    for arm, key, _ in reversed(ARMS):
        rs = sorted((r for r in rows if r["arm"] == arm), key=lambda r: int(r["mib"]))
        dx = dodge if arm == "tacit" else -dodge
        med = [(x(int(r["mib"])) + dx, y(float(r[metric + "_median"]))) for r in rs]
        for r in rs:
            wx, y0, y1 = x(int(r["mib"])) + dx, y(float(r[metric + "_min"])), y(float(r[metric + "_max"]))
            if y0 - y1 < MIN_WHISKER:
                continue
            o.append('<path d="M%.1f,%.1f V%.1f M%.1f,%.1f h6 M%.1f,%.1f h6" stroke="%s" stroke-width="1" fill="none"/>' % (
                wx, y0, y1, wx - 3, y0, wx - 3, y1, t[key + "_w"]))
        o.append('<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>' % (
            " ".join("%.1f,%.1f" % p for p in med), t[key]))
        for px, py in med:
            o.append(_marker(px, py, t[key], key, t["surface"]))
        ends.append((med[-1][1], end_label(metric, float(rs[-1][metric + "_median"])), key))
    # End labels at the last point, kept apart when the lines end close.
    ends.sort()
    if len(ends) == 2 and ends[1][0] - ends[0][0] < 18:
        mid = (ends[0][0] + ends[1][0]) / 2
        ends = [(mid - 9, ends[0][1], ends[0][2]), (mid + 9, ends[1][1], ends[1][2])]
    for yy, label, key in ends:
        name = "tacit" if key == "tacit" else "grep"
        o.append('<text x="%d" y="%.1f" font-size="13" font-weight="600">%s<tspan class="s" font-weight="400"> %s</tspan></text>' % (
            LEFT + pw + 12, yy + 4, label, name))
    o.append("</svg>")
    return "\n".join(o) + "\n"


def _marker(cx, cy, color, key, surface=None):
    ring = ' stroke="%s" stroke-width="2"' % surface if surface else ""
    if key == "tacit":
        return '<circle cx="%.1f" cy="%.1f" r="4.5" fill="%s"%s/>' % (cx, cy, color, ring)
    return '<rect x="%.1f" y="%.1f" width="9" height="9" fill="%s"%s/>' % (cx - 4.5, cy - 4.5, color, ring)


def _key(x0, cy, color, key):
    # One unbroken line through an unringed marker, so the key reads solid.
    return ('<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="%s" stroke-width="2" stroke-linecap="round"/>' % (x0, x0 + 22, cy, cy, color)
            + _marker(x0 + 11, cy, color, key))


def median(rows, arm, mib, metric):
    return float(next(r for r in rows if r["arm"] == arm and int(r["mib"]) == mib)[metric + "_median"])


def table(rows):
    """The table view: medians with min and max, per size and arm."""
    def cell(r, k, f):
        return "%s (%s to %s)" % (f(float(r[k + "_median"])), f(float(r[k + "_min"])), f(float(r[k + "_max"])))
    ms = lambda v: "%.1f ms" % v if v < 10 else "%d ms" % round(v) if v < 1000 else "%.1f s" % (v / 1000)
    chars = lambda v: "{:,}".format(round(v))
    out = ["| MiB | Arm | Right | Characters | Planted secrets | Calls | Time |", "|---|---|---|---|---|---|---|"]
    for r in sorted(rows, key=lambda r: (int(r["mib"]), r["arm"] != "tacit")):
        out.append("| %s | %s | %s of %s | %s | %s | %s | %s |" % (
            r["mib"], "grep" if r["arm"] == "by hand" else r["arm"], r["found_right"], r["tasks"], cell(r, "chars", chars),
            cell(r, "markers", lambda v: "%g" % v), cell(r, "calls", lambda v: "%g" % v), cell(r, "latency_ms", ms)))
    return "\n".join(out) + "\n"


def render(summary, out, readme=None):
    with open(summary) as f:
        rows = list(csv.DictReader(f))
    with open(os.path.join(os.path.dirname(summary), "recall-run.json")) as f:
        reps = json.load(f)["reps"]
    n = rows[0]["tasks"]
    top = max(int(r["mib"]) for r in rows)
    c_t, c_h = median(rows, "tacit", top, "chars"), median(rows, "by hand", top, "chars")
    l_t, l_h = median(rows, "tacit", top, "latency_ms"), median(rows, "by hand", top, "latency_ms")
    charts = [
        ("recall-context", "chars", "At %d MiB, grep's median task brings %.1fx tacit's context" % (top, c_h / c_t),
         ["Lower is better. Median of %s planted tasks per size." % n, WHISKERS],
         "Characters per task, all calls", DODGE),
        ("recall-latency", "latency_ms", "tacit takes %s at %d MiB; grep takes %s" % (end_label("latency_ms", l_t), top, end_label("latency_ms", l_h)),
         ["Lower is better. Median of %s planted tasks per size, each the median of %d launches." % (n, reps), WHISKERS],
         "Time per task, all calls", 0),
    ]
    for name, metric, title, sub, unit, dodge in charts:
        # Powers of ten that bound every value, min and max included.
        vals = [float(r[metric + k]) for r in rows for k in ("_min", "_max")]
        lo, hi = math.floor(math.log10(min(vals))), math.ceil(math.log10(max(vals)))
        ticks = [10 ** e for e in range(lo, max(hi, lo + 1) + 1)]
        for theme in THEMES:
            with open(os.path.join(out, "%s-%s.svg" % (name, theme)), "w") as f:
                f.write(line_chart(rows, metric, title, sub, unit, theme, ticks, dodge))
    if readme:
        with open(readme) as f:
            text = f.read()
        a, b = "<!-- recall-table -->\n", "<!-- /recall-table -->"
        if a in text and b in text:
            i, j = text.index(a) + len(a), text.index(b)
            with open(readme, "w") as f:
                f.write(text[:i] + "\n" + table(rows) + "\n" + text[j:])


if __name__ == "__main__":
    render(sys.argv[1], sys.argv[2], sys.argv[3] if len(sys.argv) > 3 else None)
