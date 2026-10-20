package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// selectTurns is a delta worth dreaming: an owner turn, an error and enough
// padding to clear minNewBytes.
func selectTurns(word string) string {
	return `{"type":"user","message":{"content":"run the ` + word + ` fixture check"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Running it."},{"type":"tool_use","name":"Bash","input":{"command":"make check"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"` + word + `: fixture not found"}]}}` + "\n" +
		strings.Repeat(" ", minNewBytes) + "\n"
}

// --idle takes a transcript that went quiet, or whose new bytes hold a
// compaction, and leaves one still being written with its cursor where it
// was; a run without --idle takes all three.
func TestIdleSelectsQuietAndCompactedTranscripts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, content string, age time.Duration) string {
		p := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now.Add(-age), now.Add(-age))
		return p
	}
	quiet := write("quiet", selectTurns("quiet"), 21*time.Minute)
	busy := write("busy", selectTurns("busy"), 2*time.Minute)
	compacted := write("compacted", selectTurns("compacted")+`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`+"\n", 30*time.Second)
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	st := func() *State {
		return &State{Offsets: map[string]int64{quiet: 0, busy: 0, compacted: 0}, Sources: []string{src[0].root()}}
	}

	s := st()
	ws, err := WindowsFor(src, s, false, Select{Idle: 20 * time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, w := range ws {
		why[w.File] = w.Why
	}
	if len(why) != 2 || why[quiet] != "idle" || why[compacted] != "compacted" {
		t.Fatalf("idle selection: %v", why)
	}
	if s.Offsets[busy] != 0 {
		t.Fatalf("a transcript still being written moved its cursor: %d", s.Offsets[busy])
	}

	// The busy one is taken once it has been quiet long enough.
	if ws, _ := WindowsFor(src, st(), false, Select{Idle: 20 * time.Minute, Now: now.Add(19 * time.Minute)}); len(ws) != 3 {
		t.Fatalf("after 21 quiet minutes: %d windows", len(ws))
	}
	// Without --idle nothing changes: every transcript with new bytes.
	if ws, _ := Windows(src, st(), false); len(ws) != 3 {
		t.Fatalf("no selection: %d windows", len(ws))
	}
}

// --session reads one transcript and moves no other cursor. A source with no
// cursor yet is not read, so a later full run still starts it at its end.
func TestSessionReadsOneTranscript(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a", "one.jsonl")
	b := filepath.Join(dir, "a", "two.jsonl")
	c := filepath.Join(dir, "b", "three.jsonl")
	for _, p := range []string{a, b, c} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(selectTurns(filepath.Base(p))), 0o644)
	}
	seen := Source{Glob: filepath.Join(dir, "a", "*.jsonl"), Format: digest.Claude}
	unseen := Source{Glob: filepath.Join(dir, "b", "*.jsonl"), Format: digest.Claude}
	st := &State{Offsets: map[string]int64{a: 0, b: 0}, Sources: []string{seen.root()}}
	ws, _ := WindowsFor([]Source{seen, unseen}, st, false, Select{Only: a})
	if len(ws) != 1 || ws[0].File != a {
		t.Fatalf("windows: %+v", ws)
	}
	if len(st.Offsets) != 2 || st.Offsets[b] != 0 || len(st.Sources) != 1 {
		t.Fatalf("other cursors touched: %v %v", st.Offsets, st.Sources)
	}
	if ws, _ := WindowsFor([]Source{seen, unseen}, st, false, Select{Only: c}); len(ws) != 0 || len(st.Offsets) != 2 {
		t.Fatalf("a source with no cursors was read: %v %v", ws, st.Offsets)
	}
}

// A Hermes session is quiet by its newest message, and compacted when it
// ended by compression or a reset (its conversation goes on in a child).
func TestIdleHermesSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := hermestest.Create(t, path,
		hermestest.Session{ID: "quiet", Source: "mobile", StartedAt: 1},
		hermestest.Session{ID: "busy", Source: "desktop", StartedAt: 10},
		hermestest.Session{ID: "squeezed", Source: "telegram", StartedAt: 20},
	)
	now := time.Now()
	pad := strings.Repeat("x", minNewBytes)
	at := func(age time.Duration) float64 { return float64(now.Add(-age).Unix()) }
	for _, s := range []struct {
		id  string
		age time.Duration
	}{{"quiet", time.Hour}, {"busy", time.Minute}, {"squeezed", time.Minute}} {
		db.Add(
			hermestest.Message{Session: s.id, Role: "user", Content: "check the " + s.id + " fixture", Timestamp: at(s.age + time.Second)},
			hermestest.Message{Session: s.id, Role: "assistant", Content: "Checked. " + pad, Timestamp: at(s.age)},
		)
	}
	db.End("squeezed", "compression")
	src := []Source{{DB: path, Format: digest.Hermes}}
	st := &State{Offsets: map[string]int64{}, Sources: []string{src[0].root()}}
	ws, _ := WindowsFor(src, st, false, Select{Idle: 20 * time.Minute, Now: now})
	why := map[string]string{}
	for _, w := range ws {
		why[w.Session] = w.Why
	}
	if len(why) != 2 || why["quiet"] != "idle" || why["squeezed"] != "compacted" {
		t.Fatalf("hermes idle selection: %v", why)
	}
	if _, moved := st.Offsets[hermes.Key(path, "busy")]; moved {
		t.Fatal("a session still being written got a cursor")
	}
	if ws, _ := WindowsFor(src, st, false, Select{Only: "busy"}); len(ws) != 1 || ws[0].Session != "busy" {
		t.Fatalf("one hermes session: %+v", ws)
	}
}
