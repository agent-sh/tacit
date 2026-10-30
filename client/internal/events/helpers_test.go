package events

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// Fixtures here are synthetic: the fields as each harness writes them
// (Claude Code 2.1.30x, Codex 0.16x, pi 1.x, Hermes's state.db), the values
// made up.

// write writes lines as a transcript, one per line, and returns its path.
func write(t *testing.T, path string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// compress rolls a transcript into its .jsonl.zst copy and removes it, as
// Codex does, and returns the copy's path.
func compress(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	defer enc.Close()
	if err := os.WriteFile(path+".zst", enc.EncodeAll(b, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return path + ".zst"
}

// readAll reads a transcript's events from offset from.
func readAll(t *testing.T, path string, f digest.Format, from int64) ([]Event, int64) {
	t.Helper()
	var evs []Event
	next, err := ReadFile(path, f, from, func(e Event) error {
		evs = append(evs, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs, next
}

// byLine groups events by the line they came from, numbered from 0, given
// the lines the transcript was written with.
func byLine(t *testing.T, lines []string, evs []Event) map[int][]Event {
	t.Helper()
	starts := map[int64]int{}
	var off int64
	for i, l := range lines {
		starts[off] = i
		off += int64(len(l)) + 1
	}
	out := map[int][]Event{}
	for _, e := range evs {
		i, ok := starts[e.Offset]
		if !ok {
			t.Fatalf("event at offset %d, which starts no line: %+v", e.Offset, e)
		}
		if e.Index != len(out[i]) {
			t.Fatalf("line %d: event %d has index %d", i, len(out[i]), e.Index)
		}
		out[i] = append(out[i], e)
	}
	return out
}

// want is what a test checks of an event; empty fields are not checked,
// except Who, Kind and Text, which always are.
type want struct {
	Kind      Kind
	Who       digest.Who
	Why       string
	Text      string
	InputKind InputKind
	Prior     string
	ToolID    string
	ToolName  string
	IsError   bool
}

// check compares a line's events with what it should give, in order.
func check(t *testing.T, line int, got []Event, ws ...want) {
	t.Helper()
	if len(got) != len(ws) {
		t.Errorf("line %d: %d events, want %d:\n%s", line, len(got), len(ws), dump(got))
		return
	}
	for i, w := range ws {
		g := got[i]
		bad := g.Kind != w.Kind || g.Who != w.Who || g.Text != w.Text || g.IsError != w.IsError ||
			(w.Why != "" && g.Why != w.Why) || g.InputKind != w.InputKind || g.Prior != w.Prior ||
			(w.ToolID != "" && g.ToolID != w.ToolID) || (w.ToolName != "" && g.ToolName != w.ToolName)
		if bad {
			t.Errorf("line %d event %d:\n got %s\nwant %+v", line, i, dump([]Event{g}), w)
		}
	}
}

func dump(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString("  {" + string(e.Kind) + " who=" + string(e.Who) + " why=" + e.Why + " in=" + string(e.InputKind) + " prior=" + e.Prior +
			" tool=" + e.ToolID + "/" + e.ToolName + " err=" + map[bool]string{true: "yes", false: "no"}[e.IsError] + " text=" + strings.ReplaceAll(e.Text, "\n", `\n`) + " output=" + e.ToolOutput + "}\n")
	}
	return b.String()
}
