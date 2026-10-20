package digest

import "testing"

// Each harness's compaction entry, as it writes it (synthetic copies of the
// shapes in ~/.claude/projects, ~/.codex/sessions and pi's session manager),
// and the same words where they are only text: inside a turn, a tool result
// that quotes a transcript, or another harness's format.
func TestCompactedReadsEachHarnessMarker(t *testing.T) {
	turn := `{"type":"user","message":{"role":"user","content":"ok"}}` + "\n"
	cases := []struct {
		name string
		f    Format
		line string
		want bool
	}{
		{"claude boundary", Claude, `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"auto","preTokens":900}}`, true},
		{"claude summary", Claude, `{"type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation."},"isVisibleInTranscriptOnly":true,"isCompactSummary":true}`, true},
		{"claude quoted in a tool result", Claude, `{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"type\":\"system\",\"subtype\":\"compact_boundary\",\"isCompactSummary\":true}"}]}}`, false},
		{"claude summary false", Claude, `{"type":"user","isCompactSummary":false,"message":{"content":"compact the logs"}}`, false},
		{"codex compacted", Codex, `{"timestamp":"2026-10-15T15:33:32.905Z","type":"compacted","payload":{"message":"","replacement_history":[]}}`, true},
		{"codex word in a message", Codex, `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"\"compacted\" is the event"}]}}`, false},
		{"pi compaction", Pi, `{"type":"compaction","id":"c1","parentId":"b9","summary":"Earlier: deploy checks.","firstKeptEntryId":"b5","tokensBefore":9000}`, true},
		{"pi summary message", Pi, `{"type":"message","id":"b9","message":{"role":"compactionSummary","summary":"\"compaction\" earlier"}}`, false},
		{"codex marker read as claude", Claude, `{"type":"compacted","payload":{}}`, false},
		{"claude marker read as pi", Pi, `{"type":"system","subtype":"compact_boundary"}`, false},
		{"hermes has no lines", Hermes, `{"type":"compaction"}`, false},
	}
	for _, c := range cases {
		if got := Compacted([]byte(turn+c.line+"\n"+turn), c.f); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if Compacted([]byte(turn+turn), Claude) {
		t.Error("plain turns read as a compaction")
	}
}
