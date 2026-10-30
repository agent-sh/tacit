package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// readState reads a transcript's events from a State.
func readState(t *testing.T, path string, f digest.Format, from State, limit int64) ([]Event, State) {
	t.Helper()
	var evs []Event
	st, err := ReadFileState(path, f, from, limit, func(e Event) error {
		evs = append(evs, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs, st
}

// saved is st as it comes back from storage.
func saved(t *testing.T, st State) State {
	t.Helper()
	b, err := st.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	return back
}

func marshal(t *testing.T, st State) []byte {
	t.Helper()
	b, err := st.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A read from the State saved at any line, saved and loaded back, gives
// exactly the events a read from the start gives from there, and ends in the
// State a read from the start ends in. A read that stops at a line (limit)
// gives the events before it and keeps what the reader needs, so the read
// from its State starts at its cursor (Kept): what a line depends on in the
// lines before it comes from the State, not from those lines.
func TestReadFromAnyLineWithItsSavedStateIsTheFullRead(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	for _, c := range resumeFixtures(t, home) {
		whole, full := readState(t, c.path, c.f, State{}, 0)
		if !full.Kept() {
			t.Fatalf("%s: a read to the end keeps nothing", c.name)
		}
		end := marshal(t, full)
		var off int64
		for i, l := range c.lines {
			var from State
			if i > 0 {
				var before []Event
				before, from = readState(t, c.path, c.f, State{}, off)
				var want []Event
				for _, e := range whole {
					if e.Offset < off {
						want = append(want, e)
					}
				}
				if from.Offset != off || !from.Kept() || !reflect.DeepEqual(before, want) {
					t.Fatalf("%s: a read stopped at line %d (offset %d) ends at %d, kept %v:\n%s\nwant\n%s", c.name, i, off, from.Offset, from.Kept(), dump(before), dump(want))
				}
			}
			from = saved(t, from)
			if i > 0 && !from.Kept() {
				t.Fatalf("%s: line %d: the saved State lost what it kept", c.name, i)
			}
			rest, next := readState(t, c.path, c.f, from, 0)
			var want []Event
			for _, e := range whole {
				if e.Offset >= off {
					want = append(want, e)
				}
			}
			if !reflect.DeepEqual(rest, want) {
				t.Fatalf("%s: from the State at line %d (offset %d):\n%s\nwant\n%s", c.name, i, off, dump(rest), dump(want))
			}
			if got := marshal(t, next); !bytes.Equal(got, end) {
				t.Fatalf("%s: from the State at line %d, the read ends in\n%s\nwant\n%s", c.name, i, got, end)
			}
			off += int64(len(l)) + 1
		}
	}
}

// filler is a line of about 1 KiB that no reader gives an event for or keeps
// anything of: Claude Code's file history snapshot.
func filler(i int) string {
	return fmt.Sprintf(`{"type":"file-history-snapshot","messageId":"m%06d","snapshot":{"note":%q}}`, i, strings.Repeat("x", 960))
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// replaceInFile changes old to new, of the same length, in place.
func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != len(new) || bytes.Count(b, []byte(old)) != 1 {
		t.Fatalf("replace %q: not one place of the same length", old)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A State saved while a transcript grows resumes at its cursor: the read
// after an append gives the events a read from the start gives from there,
// and the lines before the cursor are not read again (a call changed before
// the cursor after the save shows in a read from the start, not in the read
// from the State). While a transcript is shorter than 256 KiB, its head, which
// decides who wrote it, changes with each append, and a saved State is read
// as a bare cursor.
func TestSavedStateFollowsAGrowingTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	first := cl(0, `"type":"user","uuid":"u0","promptId":"p0",`+typedMarks+`,"message":{"role":"user","content":"keep the cache warm before a release"}`)
	call := cl(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_77","name":"Bash","input":{"command":"make warm"}}]}`)
	result := cl(2, `"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_77","type":"tool_result","content":"no target warm","is_error":true}]}`)
	turn := cl(3, `"type":"user","uuid":"u3","promptId":"p3",`+typedMarks+`,"message":{"role":"user","content":"then add the target"}`)

	for _, long := range []bool{true, false} {
		lines := []string{first}
		if long {
			for i := range 300 { // the head is whole before the call
				lines = append(lines, filler(i))
			}
		}
		lines = append(lines, call)
		path := write(t, filepath.Join(home, ".claude", "projects", "-home-u", map[bool]string{true: "long", false: "short"}[long]+".jsonl"), lines...)
		_, st := readState(t, path, digest.Claude, State{}, 0)
		st = saved(t, st)
		appendLines(t, path, result, turn)

		rest, next := readState(t, path, digest.Claude, st, 0)
		whole, _ := readAll(t, path, digest.Claude, st.Offset)
		if !reflect.DeepEqual(rest, whole) || len(rest) != 2 || rest[0].ToolName != "Bash" || !rest[0].IsError || rest[1].Text != "then add the target" {
			t.Fatalf("long %v: after the append:\n%s\nwant\n%s", long, dump(rest), dump(whole))
		}

		// The call changes after the save: a read from the start reads it, a
		// read from the State does not, unless the head changed with it.
		replaceInFile(t, path, `"name":"Bash"`, `"name":"Grep"`)
		again, _ := readState(t, path, digest.Claude, st, 0)
		fresh, _ := readAll(t, path, digest.Claude, st.Offset)
		if fresh[0].ToolName != "Grep" {
			t.Fatalf("long %v: a read from the start names the result %q", long, fresh[0].ToolName)
		}
		want := "Bash"
		if !long {
			want = "Grep" // the head changed: read as a bare cursor
		}
		if again[0].ToolName != want {
			t.Fatalf("long %v: the read from the State names the result %q, want %q", long, again[0].ToolName, want)
		}
		if !next.Kept() || next.Offset != st.Offset+int64(len(result)+len(turn)+2) {
			t.Fatalf("long %v: the read ends at %d, kept %v", long, next.Offset, next.Kept())
		}
	}
}

// What a State does at its edges: one at the end reads nothing and comes
// back as it was; a read stopped by emit returns a bare cursor at the line it
// stopped in; a State saved in another format loads as a bare cursor, bytes
// that are no State do not load; a State of another format's reader is read
// as a bare cursor.
func TestStateEdges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lines := claudeOwnerSession()
	path := write(t, filepath.Join(home, ".claude", "projects", "-home-u", "s.jsonl"), lines...)
	whole, end := readState(t, path, digest.Claude, State{}, 0)
	if evs, again := readState(t, path, digest.Claude, end, 0); len(evs) != 0 || !reflect.DeepEqual(again, end) {
		t.Fatalf("at the end: %d events, %+v", len(evs), again)
	}

	stop := errors.New("stop")
	n := 0
	st, err := ReadFileState(path, digest.Claude, State{}, 0, func(Event) error {
		if n == 3 {
			return stop
		}
		n++
		return nil
	})
	if !errors.Is(err, stop) || st.Kept() || st.Offset != whole[3].Offset {
		t.Fatalf("stopped at event 3: %+v %v (its line %d)", st, err, whole[3].Offset)
	}

	_, mid := readState(t, path, digest.Claude, State{}, whole[5].Offset)
	b := marshal(t, mid)
	other := bytes.Replace(b, []byte(`"v":1`), []byte(`"v":99`), 1)
	var back State
	if err := back.UnmarshalBinary(other); err != nil || back.Kept() || back.Offset != mid.Offset {
		t.Fatalf("another format: %+v %v", back, err)
	}
	if err := back.UnmarshalBinary([]byte("not a state")); err == nil {
		t.Fatal("bytes that are no State loaded")
	}

	// A Codex reader's State on a Claude Code transcript.
	cpath := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019a-own.jsonl"), codexOwnerSession()...)
	_, cst := readState(t, cpath, digest.Codex, State{}, 0)
	foreign := State{Offset: mid.Offset, kept: cst.kept}
	rest, _ := readState(t, path, digest.Claude, foreign, 0)
	want, _ := readAll(t, path, digest.Claude, mid.Offset)
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("a Codex State on a Claude Code transcript:\n%s\nwant\n%s", dump(rest), dump(want))
	}
}

// A Hermes session followed row by row: each read from the State the last
// one saved gives the rows added since, as a read from the start gives them,
// and reads only those (a call changed before the cursor after the save shows
// in a read from the start, not in the read from the State).
func TestReadHermesFromItsSavedStateReadsOnlyNewRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state.db")
	st := hermestest.Create(t, path, hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100})
	rows := []hermestest.Message{
		{Session: "phone", Role: "user", Content: "prune the docker images", Timestamp: 101},
		{Session: "phone", Role: "assistant", Content: "Pruning.", Finish: "tool_calls", Timestamp: 102,
			ToolCalls: `[{"id":"call_h1","call_id":"call_h1","type":"function","function":{"name":"terminal","arguments":"{}"}}]`},
		{Session: "phone", Role: "tool", ToolCallID: "call_h1", Content: `{"output": "permission denied", "exit_code": 1}`, Timestamp: 103},
		{Session: "phone", Role: "assistant", Content: "It failed.", Finish: "stop", Timestamp: 104},
		{Session: "phone", Role: "user", Content: "use sudo then", Timestamp: 110},
	}
	db, err := hermes.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	session := func() hermes.Session {
		ss, err := db.Sessions(ctx)
		if err != nil || len(ss) != 1 {
			t.Fatalf("sessions %v %v", ss, err)
		}
		return ss[0]
	}
	read := func(from State) ([]Event, State) {
		var evs []Event
		next, err := ReadHermesState(ctx, db, session(), from, func(e Event) error {
			evs = append(evs, e)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return evs, next
	}
	var followed []Event
	var at State
	for i, m := range rows {
		st.Add(m)
		evs, next := read(saved(t, at))
		whole, _ := read(State{})
		var want []Event
		for _, e := range whole {
			if e.Offset > at.Offset {
				want = append(want, e)
			}
		}
		if !reflect.DeepEqual(evs, want) || !next.Kept() {
			t.Fatalf("row %d: from the State after %d:\n%s\nwant\n%s", i, at.Offset, dump(evs), dump(want))
		}
		followed, at = append(followed, evs...), next
	}
	if whole, _ := read(State{}); !reflect.DeepEqual(followed, whole) {
		t.Fatalf("followed row by row:\n%s\nwant\n%s", dump(followed), dump(whole))
	}

	// A call renamed before the cursor, then its result: only a read from
	// the start sees the new name.
	at = saved(t, at)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	st.Add(hermestest.Message{Session: "phone", Role: "assistant", Content: "Trying again.", Finish: "tool_calls", Timestamp: 111,
		ToolCalls: `[{"id":"call_h2","call_id":"call_h2","type":"function","function":{"name":"terminal","arguments":"{}"}}]`})
	_, at = read(at)
	if _, err := raw.Exec(`update messages set tool_calls = replace(tool_calls, '"terminal"', '"shell"') where tool_calls like '%call_h2%'`); err != nil {
		t.Fatal(err)
	}
	st.Add(hermestest.Message{Session: "phone", Role: "tool", ToolCallID: "call_h2", Content: `{"output": "ok", "exit_code": 0}`, Timestamp: 112})
	evs, _ := read(saved(t, at))
	whole, _ := read(State{})
	if len(evs) != 1 || evs[0].ToolName != "terminal" || whole[len(whole)-1].ToolName != "shell" {
		t.Fatalf("the read from the State named the result %+v; from the start %+v", evs, whole[len(whole)-1])
	}
	if evs, again := read(at); len(evs) != 1 || !again.Kept() {
		t.Fatalf("read twice from one State: %d events, kept %v", len(evs), again.Kept())
	}
}
