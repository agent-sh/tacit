package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

func TestHermesWindowsReadOwnerSessionsOnceByMessageID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := hermestest.Create(t, path,
		hermestest.Session{ID: "phone", Source: "mobile", StartedAt: 1},
		hermestest.Session{ID: "nightly", Source: "cron", StartedAt: 2},
	)
	st.Add(hermestest.Message{Session: "phone", Role: "user", Content: "old turn that must never be replayed", Timestamp: 1})
	src := []Source{{DB: path, Format: digest.Hermes}}
	state := &State{Offsets: map[string]int64{}}
	if ws, _ := Windows(src, state, true); len(ws) != 0 || state.Offsets[hermes.Key(path, "phone")] != 1 {
		t.Fatalf("fresh start must only record cursors: %v %v", ws, state.Offsets)
	}
	if _, ok := state.Offsets[hermes.Key(path, "nightly")]; ok {
		t.Fatal("an automation session got a cursor")
	}
	pad := strings.Repeat("x", minNewBytes)
	st.Add(
		hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Report: disk at 91%\"]\n\nprune the docker images, never the volumes", Timestamp: 5},
		hermestest.Message{Session: "phone", Role: "assistant", Content: "Pruning.", ToolCalls: `[{"function":{"name":"terminal","arguments":"{\"command\":\"docker volume prune\"}"}}]`, Timestamp: 6},
		hermestest.Message{Session: "phone", Role: "tool", ToolName: "terminal", Content: `{"output": "permission denied", "exit_code": 1}`, Timestamp: 7},
		hermestest.Message{Session: "phone", Role: "assistant", Content: pad, Timestamp: 8},
		hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Report: disk at 91%\"]\n\nprune the docker images, never the volumes", Timestamp: 5}, // a compaction copy
		hermestest.Message{Session: "nightly", Role: "user", Content: "check the disk", Timestamp: 9},
		hermestest.Message{Session: "nightly", Role: "assistant", Content: "Disk fine. " + pad, Timestamp: 10},
	)
	ws, _ := Windows(src, state, false)
	if len(ws) != 1 {
		t.Fatalf("windows %d", len(ws))
	}
	w := ws[0]
	joined := strings.Join(w.Lines, "\n")
	if w.File != hermes.Key(path, "phone") || w.Session != "phone" || w.Start != 1 || w.End != 6 {
		t.Fatalf("window %s %s %d..%d", w.File, w.Session, w.Start, w.End)
	}
	if strings.Count(joined, "U: prune the docker images, never the volumes") != 1 || !strings.Contains(joined, "E: terminal: permission denied") || strings.Contains(joined, "Report: disk") || strings.Contains(joined, "old turn") {
		t.Fatalf("lines:\n%s", joined)
	}
	state.Offsets[w.File] = w.End
	if ws, _ := Windows(src, state, false); len(ws) != 0 {
		t.Fatal("nothing new should mean no window")
	}

	// A seen session waits for minNewBytes of new content.
	st.Add(
		hermestest.Message{Session: "phone", Role: "user", Content: "and keep the build cache", Timestamp: 11},
		hermestest.Message{Session: "phone", Role: "assistant", Content: "Kept.", Timestamp: 12},
	)
	if ws, _ := Windows(src, state, false); len(ws) != 0 {
		t.Fatalf("a small delta of a seen session was read: %d windows", len(ws))
	}

	// An excluded session is never read, though it is new and long enough.
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	if err := Exclude("laptop"); err != nil {
		t.Fatal(err)
	}
	st.AddSession(hermestest.Session{ID: "laptop", Source: "desktop", StartedAt: 20})
	st.Add(
		hermestest.Message{Session: "laptop", Role: "user", Content: "ship the hotfix tonight", Timestamp: 21},
		hermestest.Message{Session: "laptop", Role: "assistant", Content: pad, Timestamp: 22},
	)
	if ws, _ := Windows(src, state, false); len(ws) != 0 {
		t.Fatalf("an excluded session was read: %d windows", len(ws))
	}

	// The store went back (restored from a backup): the cursor restarts at the
	// session's last id, and what comes after it is read.
	key := hermes.Key(path, "phone")
	state.Offsets[key] = 1000
	if ws, _ := Windows(src, state, false); len(ws) != 0 || state.Offsets[key] != 10 {
		t.Fatalf("after a restore: %d windows, cursor %d", len(ws), state.Offsets[key])
	}
	st.Add(
		hermestest.Message{Session: "phone", Role: "user", Content: "rebuild the index after a restore", Timestamp: 30},
		hermestest.Message{Session: "phone", Role: "assistant", Content: pad, Timestamp: 31},
	)
	ws, _ = Windows(src, state, false)
	if len(ws) != 1 || ws[0].Start != 10 || !strings.Contains(strings.Join(ws[0].Lines, "\n"), "U: rebuild the index after a restore") {
		t.Fatalf("after a restore, new rows: %+v", ws)
	}
}

func TestPiWindowsAndASourceAddedLater(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, "claude", "*.jsonl")
	os.MkdirAll(filepath.Dir(claude), 0o755)
	cf := filepath.Join(filepath.Dir(claude), "c1.jsonl")
	os.WriteFile(cf, []byte(`{"type":"user","message":{"content":"x"}}`+"\n"), 0o644)
	state := &State{Offsets: map[string]int64{}}
	Windows([]Source{{Glob: claude, Format: digest.Claude}}, state, true)

	// pi appears after the dreamer started: its history is not replayed, but
	// a session it writes from then on is read from the start.
	dir := filepath.Join(home, "pi", "--home-u--")
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "2026-10-01T10-00-00-000Z_01aa.jsonl")
	os.WriteFile(old, []byte(piSession("01aa", "2026-10-01T10:00:00.000Z", "2026-10-01T10:00:05.000Z")), 0o644)
	srcs := []Source{{Glob: claude, Format: digest.Claude}, {Glob: filepath.Join(home, "pi", "*", "*.jsonl"), Format: digest.Pi}}
	if ws, _ := Windows(srcs, state, false); len(ws) != 0 || state.Offsets[old] == 0 {
		t.Fatalf("a new source was replayed: %d windows, %v", len(ws), state.Offsets)
	}
	typed := filepath.Join(dir, "2026-10-02T10-00-00-000Z_02bb.jsonl")
	headless := filepath.Join(dir, "2026-10-02T11-00-00-000Z_03cc.jsonl")
	os.WriteFile(typed, []byte(piSession("02bb", "2026-10-02T10:00:00.000Z", "2026-10-02T10:00:05.000Z")), 0o644)
	os.WriteFile(headless, []byte(piSession("03cc", "2026-10-02T10:00:00.000Z", "2026-10-02T10:00:00.040Z")), 0o644)
	ws, _ := Windows(srcs, state, false)
	got := map[string]string{}
	for _, w := range ws {
		got[w.Session] = strings.Join(w.Lines, "\n")
	}
	// A program's prompt is no owner line: the headless session is read as
	// an agent's, its prompt another agent's (P:).
	if len(ws) != 2 || !strings.HasPrefix(got["02bb"], "U: keep the cache warm before a release") || !strings.HasPrefix(got["03cc"], "P: keep the cache warm before a release") {
		t.Fatalf("pi windows: %d %q", len(ws), got)
	}
}

// piSession is a synthetic pi session: header and startup entry at header,
// the first prompt at first.
func piSession(id, header, first string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"` + header + `","cwd":"/home/u"}`,
		`{"type":"model_change","id":"a1","parentId":null,"timestamp":"` + header + `","provider":"p","modelId":"m"}`,
		`{"type":"message","id":"b1","parentId":"a1","timestamp":"` + first + `","message":{"role":"user","content":[{"type":"text","text":"keep the cache warm before a release"}]}}`,
		`{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-10-02T10:00:09.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Warming it. ` + strings.Repeat("y", minNewBytes) + `"}]}}`,
	}, "\n") + "\n"
}
