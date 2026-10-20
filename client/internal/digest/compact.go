package digest

import (
	"bytes"
	"encoding/json"
)

// compactKeys are the bytes a line must hold before it is decoded: the
// field each harness writes on the entry that marks a compaction.
var compactKeys = map[Format][]byte{
	Claude: []byte(`ompact`), // isCompactSummary, compact_boundary
	Codex:  []byte(`"compacted"`),
	Pi:     []byte(`"compaction"`),
}

// Compacted reports whether transcript lines (JSONL, whole lines) hold the
// entry a harness writes when it compacts the conversation, read on the
// entry itself, never in text inside it:
//
//   - Claude Code: the summary turn (`"isCompactSummary": true`) or the
//     system entry before it (`"subtype": "compact_boundary"`);
//   - Codex: the `"type": "compacted"` entry with the replacement history;
//   - pi: the `"type": "compaction"` entry.
//
// Hermes compacts by ending the session and continuing it in a child; that
// is read from the session row (hermes.Continued), not from lines. After a
// compaction the agent no longer has what came before it in context, so the
// dreamer does not wait for the session to go quiet.
func Compacted(b []byte, f Format) bool {
	key := compactKeys[f]
	if key == nil {
		return false
	}
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if !bytes.Contains(line, key) {
			continue
		}
		var e struct {
			Type             string `json:"type"`
			Subtype          string `json:"subtype"`
			IsCompactSummary bool   `json:"isCompactSummary"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch f {
		case Claude:
			if e.IsCompactSummary || e.Type == "system" && e.Subtype == "compact_boundary" {
				return true
			}
		case Codex:
			if e.Type == "compacted" {
				return true
			}
		case Pi:
			if e.Type == "compaction" {
				return true
			}
		}
	}
	return false
}
