#!/usr/bin/env python3
"""Draws how-it-works-{light,dark}.svg, the README's How it works diagram,
in tacit's app tokens (client/cmd/tacit/reviewui/app.css).

    python3 -I docs/images/diagram.py
"""

import os

THEMES = {
    "light": {"surface": "#fbfbf8", "border": "#dfe0d9", "card": "#ffffff", "frame": "#f5f6f2", "ink": "#1c1d1a",
              "ink2": "#55574f", "muted": "#6b6d64", "edge": "#8a8c83", "edge2": "#b9bbb2", "accent": "#2b52b8", "on": "#ffffff"},
    "dark": {"surface": "#1c1f24", "border": "#2b2f35", "card": "#23272d", "frame": "#15171b", "ink": "#e7e5de",
             "ink2": "#b0b0a8", "muted": "#8f9089", "edge": "#7d7f79", "edge2": "#4a4e55", "accent": "#6d8ff0", "on": "#15171b"},
}
FONT = "'Atkinson Hyperlegible Next', 'Atkinson Hyperlegible', system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif"
W, H = 880, 476


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def card(t, x, y, w, h, title, lines, num=None, stroke=None):
    o = ['<rect x="%d" y="%d" width="%d" height="%d" rx="6" fill="%s" stroke="%s"/>' % (x, y, w, h, t["card"], stroke or t["border"])]
    tx = x + 16
    if num:
        o.append('<circle cx="%d" cy="%d" r="10" fill="%s"/>' % (x + 24, y + 24, t["accent"]))
        o.append('<text x="%d" y="%d" font-size="13" font-weight="600" text-anchor="middle" style="fill:%s">%s</text>' % (
            x + 24, y + 28.5, t["on"], num))
        tx = x + 42
    o.append('<text x="%d" y="%d" font-size="15" font-weight="600">%s</text>' % (tx, y + 29, esc(title)))
    for i, line in enumerate(lines):
        o.append('<text class="s" x="%d" y="%d" font-size="13">%s</text>' % (x + 16, y + 52 + 18 * i, esc(line)))
    return o


def arrow(t, pts, both=False, label=None, lx=None, ly=None, anchor="middle"):
    d = "M" + " L".join("%d,%d" % p for p in pts)
    o = ['<path d="%s" fill="none" stroke="%s" stroke-width="1.5" marker-end="url(#a)"%s/>' % (
        d, t["edge"], ' marker-start="url(#a)"' if both else "")]
    if label:
        o.append('<text class="m" x="%d" y="%d" font-size="12" text-anchor="%s">%s</text>' % (lx, ly, anchor, esc(label)))
    return o


def diagram(theme):
    t = THEMES[theme]
    o = ['<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="t d">' % (W, H, W, H),
         '<title id="t">How tacit works</title>',
         '<desc id="d">Agents send notes to collect, and a dreamer reads their transcripts; collect writes both to a git store; curate turns them into facts there; '
         'deliver gives matching facts back to agents through hooks and MCP, at session start and per prompt; check and organize maintain the store. '
         'All of it runs in the local client; an optional shared server syncs stores between machines, '
         'and then one machine curates the shared store.</desc>',
         '<defs><marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
         '<path d="M0,0 L10,5 L0,10 z" fill="%s"/></marker></defs>' % t["edge"],
         '<style>text{font-family:%s;fill:%s}.s{fill:%s}.m{fill:%s}</style>' % (FONT, t["ink"], t["ink2"], t["muted"]),
         '<rect x="0.5" y="0.5" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>' % (W - 1, H - 1, t["surface"], t["border"])]
    # Agents, outside the client: a stronger edge, since they sit on the
    # outer surface.
    # One actor with two ports (notes out, facts in): its words sit between them.
    o.append('<rect x="24" y="68" width="124" height="360" rx="6" fill="%s" stroke="%s"/>' % (t["card"], t["edge2"]))
    o.append('<text x="40" y="196" font-size="15" font-weight="600">Agents</text>')
    for i, line in enumerate(["Claude Code,", "Codex,", "Hermes,", "Grok, pi,", "Muse, agy"]):
        o.append('<text class="s" x="40" y="%d" font-size="13">%s</text>' % (220 + 18 * i, esc(line)))
    # The local client: its frame and the five steps, drawn 12px left of
    # their grid.
    o.append('<g transform="translate(-12,0)">')
    o.append('<rect x="200" y="24" width="500" height="428" rx="8" fill="%s" stroke="%s"/>' % (t["frame"], t["edge2"]))
    o.append('<text class="m" x="220" y="50" font-size="13" font-weight="600">Local client (Go)</text>')
    o += card(t, 220, 68, 210, 104, "Collect", ["Inbox notes, imports,", "a dreamer that reads", "redacted transcripts"], num="1")
    o += card(t, 470, 68, 210, 104, "Curate", ["One fact per file, with", "its source and scope;", "doubts go to review"], num="2")
    o += card(t, 220, 208, 460, 64, "Store", ["A git repository: facts, inbox notes and their history"])
    o += card(t, 220, 308, 140, 120, "Deliver", ["Local index;", "hooks and MCP:", "facts at start", "and per prompt"], num="3")
    o += card(t, 380, 308, 140, 120, "Check", ["A verifier checks", "facts on code,", "git and docs;", "asks you the rest"], num="4")
    o += card(t, 540, 308, 140, 120, "Organize", ["Folder records:", "what each is,", "its status,", "an overview"], num="5")
    o += arrow(t, [(160, 120), (220, 120)])
    o += arrow(t, [(325, 172), (325, 208)])
    o += arrow(t, [(575, 172), (575, 208)], both=True)
    o += arrow(t, [(290, 272), (290, 308)])
    o += arrow(t, [(450, 272), (450, 308)], both=True)
    o += arrow(t, [(610, 272), (610, 308)], both=True)
    o += arrow(t, [(220, 368), (160, 368)])
    o += arrow(t, [(680, 240), (724, 240)], both=True)
    o.append('<text class="m" x="182" y="110" font-size="12" text-anchor="middle">notes</text>')
    o.append('<text class="m" x="182" y="358" font-size="12" text-anchor="middle">facts</text>')
    o.append("</g>")
    # Shared server, optional.
    o.append('<rect x="712" y="160" width="144" height="164" rx="6" fill="%s" stroke="%s" stroke-dasharray="4 3"/>' % (t["surface"], t["edge"]))
    o.append('<text x="728" y="186" font-size="15" font-weight="600">Shared server</text>')
    for i, line in enumerate(["Optional (Elixir):", "git transport,", "signed writers,", "a change feed;", "the curator runs", "on one machine"]):
        o.append('<text class="s" x="728" y="%d" font-size="13">%s</text>' % (210 + 18 * i, esc(line)))
    o.append("</svg>")
    return "\n".join(o) + "\n"


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    for theme in THEMES:
        with open(os.path.join(here, "how-it-works-%s.svg" % theme), "w") as f:
            f.write(diagram(theme))
