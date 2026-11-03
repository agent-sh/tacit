package digest

import (
	"bytes"
	"encoding/json"
	"strings"
)

// compactKeys are the bytes a line must hold before it is decoded: the
// field each harness writes on the entry that marks a compaction.
var compactKeys = map[Format][]byte{
	Claude: []byte(`ompact`), // isCompactSummary, compact_boundary
	Codex:  []byte(`"compacted"`),
	Pi:     []byte(`"compaction"`),
}

// CompactSummary is the summary of the last compaction entry in transcript
// lines (JSONL; a tail read may leave the first line cut, which does not
// decode), read on the entry as Compacted reads it: Claude Code's summary
// turn (`isCompactSummary`, its message's text), Codex's `compacted` entry
// (its payload's message), pi's `compaction` entry (its summary). ok is
// false when the lines hold no such entry, or its summary holds no text.
func CompactSummary(b []byte, f Format) (string, bool) {
	key := compactKeys[f]
	if key == nil {
		return "", false
	}
	lines := bytes.Split(b, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if !bytes.Contains(line, key) {
			continue
		}
		var e struct {
			Type             string `json:"type"`
			IsCompactSummary bool   `json:"isCompactSummary"`
			Message          struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Payload struct {
				Message json.RawMessage `json:"message"`
			} `json:"payload"`
			Summary json.RawMessage `json:"summary"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		var raw json.RawMessage
		switch {
		case f == Claude && e.IsCompactSummary:
			raw = e.Message.Content
		case f == Codex && e.Type == "compacted":
			raw = e.Payload.Message
		case f == Pi && e.Type == "compaction":
			raw = e.Summary
		default:
			continue
		}
		s := strings.TrimSpace(summaryText(raw))
		return s, s != ""
	}
	return "", false
}

// summaryText is the text of a summary as a harness records it: a string,
// or a list of content blocks whose text blocks are joined.
func summaryText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
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
