package events

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
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

var errStop = errors.New("stop")

// resumes reports whether a read from st starts at its cursor: a read whose
// emit stops at the first event returns st itself only then, and a bare
// cursor after a pass over the lines before it.
func resumes(t *testing.T, path string, f digest.Format, st State) bool {
	t.Helper()
	got, err := ReadFileState(path, f, st, 0, func(Event) error { return errStop })
	if !errors.Is(err, errStop) {
		t.Fatalf("no event after the cursor to stop at: %v", err)
	}
	return got.Kept()
}

// A read from the State saved at any line, saved and loaded back, gives
// exactly the events a read from the start gives from there, and ends in the
// State a read from the start ends in. A read that stops at a line (limit)
// gives the events before it and keeps what the reader needs, so the read
// from its State starts at its cursor: what a line depends on in the lines
// before it comes from the State, not from those lines.
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
			if i > 0 && !resumes(t, c.path, c.f, from) {
				t.Fatalf("%s: the State saved at line %d is read as a bare cursor", c.name, i)
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
// anything of, in each format.
func filler(f digest.Format, i int) string {
	note := strings.Repeat("x", 960)
	switch f {
	case digest.Codex:
		return cx(1, "event_msg", fmt.Sprintf(`{"type":"token_count","info":{"n":%d,"note":%q}}`, i, note))
	case digest.Pi:
		return piEntry(6, fmt.Sprintf(`"type":"label","id":"l%06d","label":%q`, i, note))
	}
	return fmt.Sprintf(`{"type":"file-history-snapshot","messageId":"m%06d","snapshot":{"note":%q}}`, i, note)
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

// growing is a session that grows past a saved State in one format: its first
// lines, a call, a reply after it (the line before the cursor), and what comes
// after the save (the call's result, then a turn).
type growing struct {
	name, path           string
	f                    digest.Format
	first                []string
	call, reply          string
	result, turn         string
	tool, renamed        string // the call's tool, and a name of the same length
	replyText, replyEdit string // a word of the reply, and one of the same length
}

func growingSessions(home string) []growing {
	return []growing{
		{
			name: "claude", f: digest.Claude, path: filepath.Join(home, ".claude", "projects", "-home-u", "grow.jsonl"),
			first:  []string{cl(0, `"type":"user","uuid":"u0","promptId":"p0",`+typedMarks+`,"message":{"role":"user","content":"keep the cache warm before a release"}`)},
			call:   cl(1, `"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_77","name":"Bash","input":{"command":"make warm"}}]}`),
			reply:  cl(1, `"type":"assistant","uuid":"a2","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Warming now."}]}`),
			result: cl(2, `"type":"user","uuid":"u3","message":{"role":"user","content":[{"tool_use_id":"toolu_77","type":"tool_result","content":"no target warm","is_error":true}]}`),
			turn:   cl(3, `"type":"user","uuid":"u4","promptId":"p4",`+typedMarks+`,"message":{"role":"user","content":"then add the target"}`),
			tool:   `"name":"Bash"`, renamed: `"name":"Grep"`, replyText: "Warming now.", replyEdit: "Warming off.",
		},
		{
			name: "codex", f: digest.Codex, path: filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019g-grow.jsonl"),
			first:  []string{codexMeta("019g-grow", `"source":"cli","thread_source":"user",`), cx(1, "event_msg", `{"type":"task_started","turn_id":"t1"}`), cxUser(1, "make the cache tests pass")},
			call:   cxCall(2, "exec_command", "call_g1", `{"cmd":"go test ./cache"}`),
			reply:  cx(2, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running now."}]}`),
			result: cxOutput(3, "call_g1", "Process exited with code 1\nOutput:\nFAIL\n"),
			turn:   cxUser(4, "and with -race"),
			tool:   `"name":"exec_command"`, renamed: `"name":"exec_commanx"`, replyText: "Running now.", replyEdit: "Running off.",
		},
		{
			name: "pi", f: digest.Pi, path: filepath.Join(home, ".pi", "agent", "sessions", "--home-u--", "2026-10-22T08-00-00-000Z_01gr.jsonl"),
			first: []string{`{"type":"session","version":3,"id":"01gr","timestamp":"2026-10-22T08:00:00.000Z","cwd":"/home/u"}`,
				piMsg(5, "u1", map[string]any{"role": "user", "content": piText("keep the cache warm")})},
			call:   piMsg(8, "a2", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "toolUse", "content": []map[string]any{{"type": "toolCall", "id": "call_p1", "name": "bash", "arguments": map[string]string{"command": "make warm"}}}}),
			reply:  piMsg(8, "a3", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "toolUse", "content": piText("Warming now.")}),
			result: piMsg(9, "r4", map[string]any{"role": "toolResult", "toolCallId": "call_p1", "content": piText("no target warm"), "isError": true}),
			turn:   piMsg(12, "u5", map[string]any{"role": "user", "content": piText("then add the target")}),
			tool:   `"name":"bash"`, renamed: `"name":"bosh"`, replyText: "Warming now.", replyEdit: "Warming off.",
		},
	}
}

// A State saved while a transcript grows resumes at its cursor, in every
// format, plain or compressed after the save: the read after an append gives
// the events a read from the start gives from there, and a line before the
// cursor and past the transcript's first 256 KiB is not read again (a call
// renamed there after the save shows in a read from the start, not in the
// read from the State). A change to the line before the cursor, or to any
// byte of a session shorter than 256 KiB, makes the read a bare cursor's,
// with the pass.
func TestSavedStateFollowsAGrowingTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	for _, c := range growingSessions(home) {
		for _, long := range []bool{true, false} {
			for _, packed := range []bool{false, true} {
				if packed && (c.f != digest.Codex || !long) {
					continue // Codex compresses its rollouts
				}
				name := fmt.Sprintf("%s long=%v compressed=%v", c.name, long, packed)
				path := strings.Replace(c.path, ".jsonl", fmt.Sprintf("-%v-%v.jsonl", long, packed), 1)
				lines := append([]string{}, c.first...)
				if long {
					for i := range 300 { // the call is past the first 256 KiB
						lines = append(lines, filler(c.f, i))
					}
				}
				lines = append(lines, c.call, c.reply)
				write(t, path, lines...)
				_, st := readState(t, path, c.f, State{}, 0)
				st = saved(t, st)
				appendLines(t, path, c.result, c.turn)

				// A State inside a copy of the file names the bytes before its
				// cursor, not the ones after it: a line after it changes, and
				// the read from it still starts at its cursor.
				ipath := strings.Replace(path, ".jsonl", "-inside.jsonl", 1)
				write(t, ipath, append(lines, c.result, c.turn)...)
				var callAt int64
				for _, l := range lines[:len(lines)-2] {
					callAt += int64(len(l)) + 1
				}
				_, inside := readState(t, ipath, c.f, State{}, callAt)
				replaceInFile(t, ipath, c.replyText, c.replyEdit)
				irest, _ := readState(t, ipath, c.f, inside, 0)
				iwant, _ := readAll(t, ipath, c.f, callAt)
				if inside.Offset != callAt || !resumes(t, ipath, c.f, inside) || !reflect.DeepEqual(irest, iwant) {
					t.Fatalf("%s: the State at the call (%d), after a line after it changed:\n%s\nwant\n%s", name, inside.Offset, dump(irest), dump(iwant))
				}

				rest, next := readState(t, path, c.f, st, 0)
				whole, _ := readAll(t, path, c.f, st.Offset)
				if !reflect.DeepEqual(rest, whole) || len(rest) != 2 || rest[0].Kind != KindToolResult || rest[0].ToolName != strings.Split(c.tool, `"`)[3] || !rest[0].IsError {
					t.Fatalf("%s: after the append:\n%s\nwant\n%s", name, dump(rest), dump(whole))
				}
				if !next.Kept() || !resumes(t, path, c.f, st) {
					t.Fatalf("%s: the saved State is not resumed", name)
				}

				// The call is renamed after the save: a read from the start
				// reads it, a read from the State does not, unless the State
				// names its bytes (a short session).
				replaceInFile(t, path, c.tool, c.renamed)
				if packed {
					compress(t, path)
				}
				again, _ := readState(t, path, c.f, st, 0)
				fresh, _ := readAll(t, path, c.f, st.Offset)
				want := strings.Split(c.renamed, `"`)[3]
				if fresh[0].ToolName != want {
					t.Fatalf("%s: a read from the start names the result %q", name, fresh[0].ToolName)
				}
				if long {
					want = strings.Split(c.tool, `"`)[3]
				}
				if again[0].ToolName != want || resumes(t, path, c.f, st) != long {
					t.Fatalf("%s: the read from the State names the result %q, want %q", name, again[0].ToolName, want)
				}

				// The line before the cursor changes: read as a bare cursor.
				if !packed {
					replaceInFile(t, path, c.replyText, c.replyEdit)
					if resumes(t, path, c.f, st) {
						t.Fatalf("%s: a State whose line before the cursor changed was resumed", name)
					}
					if again, _ := readState(t, path, c.f, st, 0); again[0].ToolName != strings.Split(c.renamed, `"`)[3] {
						t.Fatalf("%s: after the line before the cursor changed, the result is %q", name, again[0].ToolName)
					}
				}
			}
		}
	}
}

// A State kept while a Codex rollout's or a pi session's first line was still
// being written is the start: what the readers take from that line (the
// session's id, cwd and parent) comes from it once it is whole.
func TestStateBeforeTheFirstLineIsTheStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	for _, c := range growingSessions(home) {
		if c.f == digest.Claude {
			continue // its lines carry the session's fields
		}
		path := strings.Replace(c.path, ".jsonl", "-first.jsonl", 1)
		first := c.first[0]
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(first[:len(first)/2]), 0o600); err != nil {
			t.Fatal(err)
		}
		evs, st := readState(t, path, c.f, State{}, 0)
		if len(evs) != 0 || st.Offset != 0 || st.Kept() {
			t.Fatalf("%s: half a first line: %d events, %+v", c.name, len(evs), st)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(first[len(first)/2:] + "\n" + strings.Join(append(c.first[1:], c.call), "\n") + "\n")
		f.Close()
		got, _ := readState(t, path, c.f, st, 0)
		want, _ := readAll(t, path, c.f, 0)
		if !reflect.DeepEqual(got, want) || len(got) == 0 || got[0].Session == strings.TrimSuffix(filepath.Base(path), ".jsonl") {
			t.Fatalf("%s: after the first line was whole:\n%s\nwant\n%s", c.name, dump(got), dump(want))
		}
	}
}

// What a State does at its edges: one at the end reads nothing and comes
// back as it was; a read stopped by emit at the first event from a State
// returns that State, and later a bare cursor at the line it stopped in; a
// State saved in another format loads as a bare cursor, bytes that are no
// State do not load; a State inside a caller's JSON keeps what it kept; a
// State of another format's reader on the same file is read as a bare
// cursor; one State read twice gives the same events.
func TestStateEdges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	lines := claudeOwnerSession()
	path := write(t, filepath.Join(home, ".claude", "projects", "-home-u", "s.jsonl"), lines...)
	whole, end := readState(t, path, digest.Claude, State{}, 0)
	if evs, again := readState(t, path, digest.Claude, end, 0); len(evs) != 0 || !reflect.DeepEqual(again, end) {
		t.Fatalf("at the end: %d events, %+v", len(evs), again)
	}

	n := 0
	st, err := ReadFileState(path, digest.Claude, State{}, 0, func(Event) error {
		if n == 3 {
			return errStop
		}
		n++
		return nil
	})
	if !errors.Is(err, errStop) || st.Kept() || st.Offset != whole[3].Offset {
		t.Fatalf("stopped at event 3: %+v %v (its line %d)", st, err, whole[3].Offset)
	}
	_, mid := readState(t, path, digest.Claude, State{}, whole[5].Offset)
	if back, err := ReadFileState(path, digest.Claude, mid, 0, func(Event) error { return errStop }); !errors.Is(err, errStop) || !reflect.DeepEqual(back, mid) {
		t.Fatalf("stopped at the first event from a State: %+v %v", back, err)
	}

	b := marshal(t, mid)
	other := bytes.Replace(b, []byte(`"v":1`), []byte(`"v":99`), 1)
	var back State
	if err := back.UnmarshalBinary(other); err != nil || back.Kept() || back.Offset != mid.Offset {
		t.Fatalf("another format: %+v %v", back, err)
	}
	if err := back.UnmarshalBinary([]byte("not a state")); err == nil {
		t.Fatal("bytes that are no State loaded")
	}
	type record struct {
		Path  string
		State State
	}
	raw, err := json.Marshal(record{path, mid})
	if err != nil {
		t.Fatal(err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil || !rec.State.Kept() || !bytes.Equal(marshal(t, rec.State), b) {
		t.Fatalf("a State in a caller's JSON: %s, %v", raw, err)
	}

	// The same file read as pi, to the same line: its State is not Claude
	// Code's.
	_, asPi := readState(t, path, digest.Pi, State{}, mid.Offset)
	if asPi.Offset != mid.Offset || !asPi.Kept() {
		t.Fatalf("the read as pi ends at %d", asPi.Offset)
	}
	rest, _ := readState(t, path, digest.Claude, asPi, 0)
	want, _ := readAll(t, path, digest.Claude, mid.Offset)
	if !reflect.DeepEqual(rest, want) || resumes(t, path, digest.Claude, asPi) {
		t.Fatalf("a pi reader's State on a Claude Code transcript:\n%s\nwant\n%s", dump(rest), dump(want))
	}

	// One State read twice: the second read has what the first used up (the
	// end event of a patch, before its output).
	cpath := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019a-own.jsonl"), codexOwnerSession()...)
	var off int64
	for _, l := range codexOwnerSession()[:10] {
		off += int64(len(l)) + 1
	}
	_, cst := readState(t, cpath, digest.Codex, State{}, off)
	first, _ := readState(t, cpath, digest.Codex, cst, 0)
	second, _ := readState(t, cpath, digest.Codex, cst, 0)
	if !reflect.DeepEqual(first, second) || first[0].ToolID != "call_p1" || !first[0].IsError {
		t.Fatalf("one State read twice:\n%s\nthen\n%s", dump(first), dump(second))
	}
}

// A Hermes session followed row by row: each read from the State the last
// one saved gives the rows added since, as a read from the start gives them,
// and reads only those (a call renamed before the row at the cursor after the
// save shows in a read from the start, not in the read from the State). A
// changed row at the cursor, a session whose rows Hermes wrote again, another
// session's State and a transcript's State are read as a bare cursor; one
// State read twice gives the same events.
func TestReadHermesFromItsSavedStateReadsOnlyNewRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state.db")
	st := hermestest.Create(t, path,
		hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100},
		hermestest.Session{ID: "desk", Source: "desktop", CreatedSource: "desktop", StartedAt: 200})
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
	session := func(id string) hermes.Session {
		ss, err := db.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no session %s in %v", id, ss)
		return hermes.Session{}
	}
	read := func(id string, from State) ([]Event, State) {
		var evs []Event
		next, err := ReadHermesState(ctx, db, session(id), from, func(e Event) error {
			evs = append(evs, e)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return evs, next
	}
	hermesResumes := func(id string, from State) bool {
		got, err := ReadHermesState(ctx, db, session(id), from, func(Event) error { return errStop })
		if !errors.Is(err, errStop) {
			t.Fatalf("no row after the cursor to stop at: %v", err)
		}
		return got.Kept()
	}
	var followed []Event
	var at, afterStop State // afterStop: the State after a reply that stopped
	for i, m := range rows {
		st.Add(m)
		if i > 0 && !hermesResumes("phone", saved(t, at)) {
			t.Fatalf("row %d: the State after %d is read as a bare cursor", i, at.Offset)
		}
		evs, next := read("phone", saved(t, at))
		whole, _ := read("phone", State{})
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
		if m.Finish == "stop" {
			afterStop = next
		}
	}
	if whole, _ := read("phone", State{}); !reflect.DeepEqual(followed, whole) {
		t.Fatalf("followed row by row:\n%s\nwant\n%s", dump(followed), dump(whole))
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// A call, a reply after it, the State; the call renamed, then its result.
	st.Add(hermestest.Message{Session: "phone", Role: "assistant", Content: "Trying again.", Finish: "tool_calls", Timestamp: 111,
		ToolCalls: `[{"id":"call_h2","call_id":"call_h2","type":"function","function":{"name":"terminal","arguments":"{}"}}]`},
		hermestest.Message{Session: "phone", Role: "assistant", Content: "Waiting for it.", Timestamp: 112})
	_, at = read("phone", at)
	at = saved(t, at)
	exec(`update messages set tool_calls = replace(tool_calls, '"terminal"', '"shell"') where tool_calls like '%call_h2%'`)
	st.Add(hermestest.Message{Session: "phone", Role: "tool", ToolCallID: "call_h2", Content: `{"output": "ok", "exit_code": 0}`, Timestamp: 113})
	evs, _ := read("phone", at)
	whole, _ := read("phone", State{})
	if len(evs) != 1 || evs[0].ToolName != "terminal" || whole[len(whole)-1].ToolName != "shell" {
		t.Fatalf("the read from the State named the result %+v; from the start %+v", evs, whole[len(whole)-1])
	}
	twice, _ := read("phone", at)
	if !reflect.DeepEqual(evs, twice) {
		t.Fatalf("one State read twice:\n%s\nthen\n%s", dump(evs), dump(twice))
	}

	// What a read from a State adds is not the State's: a call read after
	// the cursor, then gone, names nothing in the next read from it.
	_, end := read("phone", State{})
	st.Add(hermestest.Message{Session: "phone", Role: "assistant", Content: "Once more.", Finish: "tool_calls", Timestamp: 114,
		ToolCalls: `[{"id":"call_h9","call_id":"call_h9","type":"function","function":{"name":"terminal","arguments":"{}"}}]`},
		hermestest.Message{Session: "phone", Role: "tool", ToolCallID: "call_h9", Content: `{"output": "ok", "exit_code": 0}`, Timestamp: 115})
	if evs, _ := read("phone", end); len(evs) != 3 || evs[2].ToolName != "terminal" {
		t.Fatalf("a call and its result after the cursor: %s", dump(evs))
	}
	exec(`delete from messages where tool_calls like '%call_h9%'`)
	if evs, _ := read("phone", end); len(evs) != 1 || evs[0].ToolName != "" {
		t.Fatalf("a result whose call is gone, read again from the same State: %s", dump(evs))
	}
	exec(`delete from messages where tool_call_id = 'call_h9'`)

	// The row at the cursor keeps its text and gets another finish reason:
	// the next turn's prior is the new one, as from the start.
	st.Add(hermestest.Message{Session: "phone", Role: "assistant", Content: "Done for now.", Finish: "stop", Timestamp: 116})
	_, upTo := read("phone", State{})
	exec(`update messages set finish_reason = 'incomplete' where content = 'Done for now.'`)
	st.Add(hermestest.Message{Session: "phone", Role: "user", Content: "and the volumes", Timestamp: 117})
	if evs, _ := read("phone", upTo); len(evs) != 1 || evs[0].Prior != "incomplete" || hermesResumes("phone", upTo) {
		t.Fatalf("after the finish reason of the row at the cursor changed: %s", dump(evs))
	}

	// The row at the cursor changes: read as a bare cursor.
	exec(`update messages set content = 'Waiting for it!' where content = 'Waiting for it.'`)
	if hermesResumes("phone", at) {
		t.Fatal("a State whose row at the cursor changed was resumed")
	}
	if evs, _ := read("phone", at); evs[0].ToolName != "shell" {
		t.Fatalf("after the row at the cursor changed: %+v", evs)
	}

	// Hermes writes the session's rows again (/retry, /undo, /compress): the
	// turn it wrote again has no reply before it.
	st.Add(hermestest.Message{Session: "desk", Role: "user", Content: "list the open PRs", Timestamp: 201},
		hermestest.Message{Session: "desk", Role: "assistant", Content: "Three.", Finish: "stop", Timestamp: 202})
	_, dst := read("desk", State{})
	exec(`delete from messages where session_id = 'desk'`)
	st.Add(hermestest.Message{Session: "desk", Role: "user", Content: "list the open PRs", Timestamp: 201})
	again, _ := read("desk", dst)
	fresh, _ := read("desk", State{Offset: dst.Offset})
	if !reflect.DeepEqual(again, fresh) || len(again) != 1 || again[0].Prior != "" {
		t.Fatalf("a session written again:\n%s\nwant\n%s", dump(again), dump(fresh))
	}

	// Another session's State, and a transcript's, each with a record before
	// the next turn: from the start or after a row, they are read as a bare
	// cursor.
	st.Add(hermestest.Message{Session: "desk", Role: "assistant", Content: "Two now.", Finish: "stop", Timestamp: 203})
	// The session's own State moved to its start is the start.
	moved := afterStop
	moved.Offset = 0
	if got, _ := read("phone", moved); !reflect.DeepEqual(got, followedTo(t, read, "phone")) {
		t.Fatalf("a State moved to the start:\n%s", dump(got))
	}
	for name, foreign := range map[string]State{"another session's": afterStop, "a transcript's": transcriptState(t)} {
		if foreign.kept.Prior == "" {
			t.Fatalf("%s State has no record before a turn to carry", name)
		}
		for _, off := range []int64{0, 1} {
			foreign.Offset = off
			got, _ := read("desk", foreign)
			want, _ := read("desk", State{Offset: off})
			if !reflect.DeepEqual(got, want) || off > 0 && hermesResumes("desk", foreign) {
				t.Fatalf("%s State at %d on a Hermes session:\n%s\nwant\n%s", name, off, dump(got), dump(want))
			}
		}
	}
}

// followedTo is a Hermes session read from its start.
func followedTo(t *testing.T, read func(string, State) ([]Event, State), id string) []Event {
	t.Helper()
	evs, _ := read(id, State{})
	return evs
}

// transcriptState is a Claude Code transcript's State after a turn's end
// (turn_duration, line 10 of claudeOwnerSession).
func transcriptState(t *testing.T) State {
	t.Helper()
	lines := claudeOwnerSession()
	path := write(t, filepath.Join(t.TempDir(), "t.jsonl"), lines...)
	var off int64
	for _, l := range lines[:11] {
		off += int64(len(l)) + 1
	}
	_, st := readState(t, path, digest.Claude, State{}, off)
	return st
}

// Every field a reader carries from one line to the next is kept in a State,
// rebuilt by the replay, set again by each read or scratch for one line; a
// field added to a reader, to the owner's words' TurnReader or to the local
// command rule fails here until it is placed, and a change to what is kept or
// replayed bumps keptVersion.
func TestStateCoversEveryField(t *testing.T) {
	const (
		kept     = "kept in the State"
		replayed = "rebuilt by the replay"
		perRead  = "set again by each read"
		scratch  = "scratch for one line"
	)
	for name, c := range map[string]struct {
		v      any
		fields map[string]string
	}{
		"base": {base{}, map[string]string{"harness": perRead, "session": kept, "subagent": kept, "parent": kept, "cwd": kept,
			"model": kept, "verdict": perRead, "prior": kept, "calls": kept}},
		"claudeReader": {claudeReader{}, map[string]string{"base": kept, "turns": replayed, "cmds": replayed, "replay": kept}},
		"codexReader": {codexReader{}, map[string]string{"base": kept, "turns": replayed, "asked": replayed, "replay": kept,
			"noted": scratch, "failed": kept, "open": kept, "active": kept}},
		"piReader":     {piReader{}, map[string]string{"base": kept, "turns": perRead, "working": kept}},
		"hermesReader": {hermesReader{}, map[string]string{"base": kept, "turns": perRead}},
		// Pastes are the prompt history's, read by each read; asked and cmds
		// follow the lines the replay reads again.
		"ownerwords.TurnReader": {ownerwords.TurnReader{}, map[string]string{"Pastes": perRead, "asked": replayed, "cmds": replayed}},
		"digest.ClaudeCommands": {digest.ClaudeCommands{}, map[string]string{"caveat": replayed, "prompt": replayed}},
	} {
		typ := reflect.TypeOf(c.v)
		var got, want []string
		for i := range typ.NumField() {
			got = append(got, typ.Field(i).Name)
		}
		for f := range c.fields {
			want = append(want, f)
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s has fields %v; this test places %v: place each new one, and bump keptVersion if it is kept or replayed", name, got, want)
		}
	}
	if k := reflect.TypeOf(digest.CodexAsked{}).Kind(); k != reflect.Map {
		t.Errorf("digest.CodexAsked is a %s: the replay relies on Note only adding options by question", k)
	}
}
