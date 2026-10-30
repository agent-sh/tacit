package events

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// A cursor is a byte offset: a read from an event's Next gives exactly the
// events after its line; a line still being written waits; a stop in emit
// returns the offset of the line it stopped in.
func TestReadFileResumesFromCursors(t *testing.T) {
	home := t.TempDir()
	lines := []string{
		cl(0, `"type":"user","uuid":"u0",`+typedMarks+`,"message":{"role":"user","content":"keep the cache warm before a release"}`),
		cl(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"On it."},{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"make warm"}}]}`),
		cl(2, `"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_01","type":"tool_result","content":"warmed"}]}`),
		cl(3, `"type":"assistant","uuid":"a3","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Warm."}]}`),
	}
	path := write(t, filepath.Join(home, ".claude", "projects", "-home-u", "s.jsonl"), lines...)
	whole, end := readAll(t, path, digest.Claude, 0)
	if len(whole) != 5 {
		t.Fatalf("events: %s", dump(whole))
	}
	for i, e := range whole {
		rest, next := readAll(t, path, digest.Claude, e.Next)
		var after []Event
		for _, w := range whole[i+1:] {
			if w.Offset >= e.Next {
				after = append(after, w)
			}
		}
		if next != end || len(rest) != len(after) {
			t.Fatalf("from %d: %d events, want %d", e.Next, len(rest), len(after))
		}
		for j := range rest {
			if rest[j].Offset != after[j].Offset || rest[j].Index != after[j].Index || rest[j].Text != after[j].Text || rest[j].Session != after[j].Session {
				t.Fatalf("from %d, event %d: %+v, want %+v", e.Next, j, rest[j], after[j])
			}
		}
		// The model of the session's first reply is known before a reply is read.
		if len(rest) > 0 && rest[0].Model != "claude-opus-5-5" {
			t.Errorf("from %d: model %q", e.Next, rest[0].Model)
		}
	}
	// A tool result read from a cursor past its call names no tool: the
	// pairing is by ToolID.
	if rest, _ := readAll(t, path, digest.Claude, whole[2].Next); rest[0].ToolID != "toolu_01" || rest[0].ToolName != "" {
		t.Errorf("result read alone: %+v", rest[0])
	}

	// A line still being written is left for the next read, then read whole.
	part := cl(4, `"type":"user","uuid":"u4",`+typedMarks+`,"message":{"role":"user","content":"and the docs"}`)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(part[:len(part)/2])
	evs, next := readAll(t, path, digest.Claude, end)
	if len(evs) != 0 || next != end {
		t.Fatalf("half a line: %d events, cursor %d (want %d)", len(evs), next, end)
	}
	f.WriteString(part[len(part)/2:]) // whole, its newline not written yet
	f.Close()
	evs, next = readAll(t, path, digest.Claude, end)
	if len(evs) != 1 || evs[0].Text != "and the docs" || next != end+int64(len(part)) {
		t.Fatalf("whole line without its newline: %s cursor %d", dump(evs), next)
	}

	// A stop in emit: the offset of the line it stopped in.
	stop := errors.New("stop")
	n := 0
	cursor, err := ReadFile(path, digest.Claude, 0, func(e Event) error {
		if n++; n == 3 { // the tool call, the line's second event
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || cursor != whole[1].Offset {
		t.Fatalf("stopped in line 1: cursor %d, %v", cursor, err)
	}

	if _, err := ReadFile(path, digest.Claude, next+1, func(Event) error { return nil }); !errors.Is(err, ErrPastEnd) {
		t.Fatalf("past the end: %v", err)
	}
	if _, err := ReadFile(path, digest.Hermes, 0, func(Event) error { return nil }); err == nil {
		t.Fatal("a Hermes store read as a transcript")
	}
}
