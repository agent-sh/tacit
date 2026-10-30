package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// unplaced is evs without their place (path and offsets), to compare the
// events of two transcripts that hold the same items on other lines.
func unplaced(evs []Event) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		e.Path, e.Offset, e.Next = "", 0, 0
		out[i] = e
	}
	return out
}

// A line the reader cannot decode gives no events and is counted, in every
// transcript format: a line a crash cut short, which the next write joined,
// and JSON that is not the record its format writes. The lines around them
// give the events they give without them: a result after them is still named
// by its call before them. A line that holds nothing (blank, or null) is not
// counted, nor a last line still being written. Each State counts the lines before its cursor however the
// read came there: from the start, line by line from saved States, from a
// bare cursor (its pass counts them), stopped by emit. A State at the end is
// returned as it is, and one whose count is damaged is read as a cursor.
func TestLinesThatDoNotParseAreCounted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	// JSON that is not the record its format writes: a field of another type,
	// a payload or a message that does not decode.
	shapes := map[digest.Format]string{
		digest.Claude: cl(1, `"type":"user","uuid":"u9","isMeta":"yes","message":{"role":"user","content":"a field of another type"}`),
		digest.Codex:  cx(2, "response_item", `{"type":"message","role":7,"content":[{"type":"output_text","text":"a payload of another shape"}]}`),
		digest.Pi:     piEntry(8, `"type":"message","id":"a9","message":"a message that is a string"`),
	}
	for _, c := range growingSessions(home) {
		t.Run(c.name, func(t *testing.T) {
			// What a crash leaves: half a line, and the next write on the same
			// line.
			cut := c.turn[:len(c.turn)/2] + c.reply
			if json.Valid([]byte(cut)) || !json.Valid([]byte(shapes[c.f])) {
				t.Fatal("the cut line is JSON, or the other shape is not")
			}
			good := append(append([]string{}, c.first...), c.call, c.reply, c.result)
			// Lines that hold nothing, blank or null, after the two that do
			// not parse.
			lines := append(append([]string{}, c.first...), c.call, cut, shapes[c.f], "", "null", c.reply, c.result)
			clean := write(t, strings.Replace(c.path, ".jsonl", "-clean.jsonl", 1), good...)
			path := write(t, strings.Replace(c.path, ".jsonl", "-unparsed.jsonl", 1), lines...)
			// starts are where each line starts, bad where the two that do not
			// parse do, end the transcript's end; the reply comes after the
			// lines that hold nothing.
			var starts, bad []int64
			var end int64
			for i, l := range lines {
				starts = append(starts, end)
				if i == len(c.first)+1 || i == len(c.first)+2 {
					bad = append(bad, end)
				}
				end += int64(len(l)) + 1
			}
			replyAt := starts[len(c.first)+5]
			before := func(cursor int64) int {
				n := 0
				for _, b := range bad {
					if b < cursor {
						n++
					}
				}
				return n
			}

			want, wst := readState(t, clean, c.f, State{}, 0)
			whole, st := readState(t, path, c.f, State{}, 0)
			if wst.Unparsed() != 0 || st.Unparsed() != 2 || st.Offset != end {
				t.Fatalf("%d lines that do not parse (want 2), the read ends at %d (want %d); without them %d", st.Unparsed(), st.Offset, end, wst.Unparsed())
			}
			if !reflect.DeepEqual(unplaced(whole), unplaced(want)) || whole[len(whole)-1].ToolName == "" {
				t.Fatalf("the lines around the ones that do not parse:\n%s\nwant\n%s", dump(whole), dump(want))
			}

			// Line by line, each read from the State the last one saved, which
			// it starts at (no pass): each State counts the lines before its
			// cursor, and the last is the whole read's.
			var parts []Event
			var at State
			for {
				evs, next := readState(t, path, c.f, saved(t, at), 1)
				if next.Offset == at.Offset {
					break
				}
				if !next.Kept() || next.Unparsed() != before(next.Offset) {
					t.Fatalf("the State at %d counts %d (kept %v), want %d", next.Offset, next.Unparsed(), next.Kept(), before(next.Offset))
				}
				// An event follows every cursor up to the last line's start.
				if next.Offset <= starts[len(starts)-1] && !resumes(t, path, c.f, saved(t, next)) {
					t.Fatalf("the State at %d is read as a bare cursor", next.Offset)
				}
				parts, at = append(parts, evs...), next
			}
			if !reflect.DeepEqual(parts, whole) || !bytes.Equal(marshal(t, at), marshal(t, st)) {
				t.Fatalf("read line by line, ending in %s (want %s):\n%s\nwant\n%s", marshal(t, at), marshal(t, st), dump(parts), dump(whole))
			}

			// From a bare cursor at any line: the events from there, and the
			// pass counts the lines before it, so the read ends in the State a
			// read from the start ends in.
			for _, s := range starts {
				rest, next := readState(t, path, c.f, State{Offset: s}, 0)
				var wantRest []Event
				for _, e := range whole {
					if e.Offset >= s {
						wantRest = append(wantRest, e)
					}
				}
				if !reflect.DeepEqual(rest, wantRest) || !bytes.Equal(marshal(t, next), marshal(t, st)) {
					t.Fatalf("from the cursor %d, ending in %s (want %s):\n%s\nwant\n%s", s, marshal(t, next), marshal(t, st), dump(rest), dump(wantRest))
				}
			}
			// Stopped by emit at the reply after them: a cursor at its line,
			// which counts them.
			stopped, err := ReadFileState(path, c.f, State{}, 0, func(e Event) error {
				if e.Offset >= replyAt {
					return errStop
				}
				return nil
			})
			if !errors.Is(err, errStop) || stopped.Kept() || stopped.Offset != replyAt || stopped.Unparsed() != 2 {
				t.Fatalf("stopped at the reply (%d): %v, cursor %d, kept %v, %d lines that do not parse", replyAt, err, stopped.Offset, stopped.Kept(), stopped.Unparsed())
			}
			// A State whose count is damaged (below 0) is read as a cursor,
			// which counts again.
			_, past := readState(t, path, c.f, State{}, replyAt)
			var damaged State
			if err := damaged.UnmarshalBinary(bytes.Replace(marshal(t, past), []byte(`"unparsed":2`), []byte(`"unparsed":-5`), 1)); err != nil || damaged.Unparsed() != -5 {
				t.Fatalf("a damaged count loads as %d: %v", damaged.Unparsed(), err)
			}
			if _, next := readState(t, path, c.f, damaged, 0); resumes(t, path, c.f, damaged) || next.Unparsed() != 2 {
				t.Fatalf("a damaged count was resumed, or counts %d", next.Unparsed())
			}
			// At the end a State is returned as it is: the saved one with its
			// count, a bare cursor with none.
			if evs, again := readState(t, path, c.f, saved(t, st), 0); len(evs) != 0 || again.Unparsed() != 2 {
				t.Fatalf("the State at the end: %d events, %d lines that do not parse", len(evs), again.Unparsed())
			}
			if _, again := readState(t, path, c.f, State{Offset: end}, 0); again.Unparsed() != 0 || again.Offset != end {
				t.Fatalf("a bare cursor at the end counts %d", again.Unparsed())
			}

			// A last line still being written is neither read nor counted, from
			// the State or from a cursor inside it; once whole it is read.
			appendRaw(t, path, c.turn[:len(c.turn)/2])
			evs, mid := readState(t, path, c.f, saved(t, st), 0)
			if len(evs) != 0 || mid.Offset != end || mid.Unparsed() != 2 {
				t.Fatalf("half a last line: %d events, cursor %d (want %d), %d lines that do not parse", len(evs), mid.Offset, end, mid.Unparsed())
			}
			if evs, inside := readState(t, path, c.f, State{Offset: end + 1}, 0); len(evs) != 0 || inside.Offset != end+1 || inside.Unparsed() != 2 {
				t.Fatalf("a cursor inside half a last line: %d events, cursor %d, %d lines that do not parse", len(evs), inside.Offset, inside.Unparsed())
			}
			appendRaw(t, path, c.turn[len(c.turn)/2:]+"\n")
			evs, st = readState(t, path, c.f, saved(t, mid), 0)
			if len(evs) == 0 || evs[0].Offset != end || evs[0].Kind != KindOwnerText || st.Unparsed() != 2 {
				t.Fatalf("the last line once whole, counting %d:\n%s", st.Unparsed(), dump(evs))
			}
			end = st.Offset

			// The harness stops in the middle of a line, and its next write goes
			// on the same line: that line is counted once it ends, and the line
			// after it is read.
			appendRaw(t, path, c.turn[:len(c.turn)/2])
			if evs, mid = readState(t, path, c.f, saved(t, st), 0); len(evs) != 0 || mid.Unparsed() != 2 {
				t.Fatalf("a line cut short and not yet ended: %d events, %d lines that do not parse", len(evs), mid.Unparsed())
			}
			appendRaw(t, path, c.reply+"\n"+c.turn+"\n")
			turnAt := end + int64(len(c.turn)/2+len(c.reply)) + 1
			evs, st = readState(t, path, c.f, saved(t, mid), 0)
			if st.Unparsed() != 3 || len(evs) == 0 || evs[0].Offset != turnAt || evs[0].Kind != KindOwnerText {
				t.Fatalf("after the joined line (%d lines that do not parse, want 3):\n%s", st.Unparsed(), dump(evs))
			}
			if _, fresh := readState(t, path, c.f, State{}, 0); !bytes.Equal(marshal(t, fresh), marshal(t, st)) {
				t.Fatalf("followed, the read ends in %s; from the start %s", marshal(t, st), marshal(t, fresh))
			}
		})
	}
}

// An assistant row whose tool calls do not parse is counted: its text is
// read, its calls are not, and the result after it names no tool. A row with
// no calls, or with calls that parse, is not counted. Each State counts the
// rows up to its cursor: row by row from saved States, and from a bare cursor,
// whose pass counts them.
func TestHermesRowsWhoseCallsDoNotParseAreCounted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state.db")
	store := hermestest.Create(t, path, hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100})
	rows := []hermestest.Message{
		/* 1 */ {Session: "phone", Role: "user", Content: "prune the docker images", Timestamp: 101},
		// Calls of another shape: the arguments an object, not a string of JSON.
		/* 2 */ {Session: "phone", Role: "assistant", Content: "Pruning.", Finish: "tool_calls", Timestamp: 102,
			ToolCalls: `[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":{"command":"docker image prune -f"}}}]`},
		/* 3 */ {Session: "phone", Role: "tool", ToolCallID: "call_h1", Content: `{"output": "permission denied", "exit_code": 1}`, Timestamp: 103},
		// Calls cut short.
		/* 4 */ {Session: "phone", Role: "assistant", Content: "Trying again.", Finish: "tool_calls", Timestamp: 104, ToolCalls: `[{"id":"call_h2","type":"func`},
		/* 5 */ {Session: "phone", Role: "assistant", Content: "Both failed.", Finish: "stop", Timestamp: 105},
		/* 6 */ {Session: "phone", Role: "assistant", Content: "With sudo.", Finish: "tool_calls", Timestamp: 106,
			ToolCalls: `[{"id":"call_h3","type":"function","function":{"name":"terminal","arguments":"{}"}}]`},
		/* 7 */ {Session: "phone", Role: "tool", ToolCallID: "call_h3", Content: `{"output": "ok", "exit_code": 0}`, Timestamp: 107},
	}
	bad := map[int64]bool{2: true, 4: true}
	db, err := hermes.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	session := func() hermes.Session {
		t.Helper()
		ss, err := db.Sessions(ctx)
		if err != nil || len(ss) != 1 {
			t.Fatalf("sessions %v %v", ss, err)
		}
		return ss[0]
	}
	read := func(from State) ([]Event, State) {
		t.Helper()
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

	// resumes reports whether a read from st starts after its cursor: one
	// stopped at its first event returns st only then.
	resumes := func(st State) bool {
		t.Helper()
		got, err := ReadHermesState(ctx, db, session(), st, func(Event) error { return errStop })
		if !errors.Is(err, errStop) {
			t.Fatalf("no row after the cursor to stop at: %v", err)
		}
		return got.Kept()
	}

	// Row by row, each read from the State the last one saved, which it
	// starts after (no pass).
	var followed []Event
	var at State
	for i, m := range rows {
		store.Add(m)
		if i > 0 && !resumes(saved(t, at)) {
			t.Fatalf("row %d: the State at %d is read as a bare cursor", i+1, at.Offset)
		}
		if i == len(rows)-1 {
			// A State whose count is damaged (below 0) is read as a cursor,
			// which counts again.
			var damaged State
			if err := damaged.UnmarshalBinary(bytes.Replace(marshal(t, at), []byte(`"unparsed":2`), []byte(`"unparsed":-5`), 1)); err != nil || damaged.Unparsed() != -5 {
				t.Fatalf("a damaged count loads as %d: %v", damaged.Unparsed(), err)
			}
			if _, next := read(damaged); resumes(damaged) || next.Unparsed() != 2 {
				t.Fatalf("a damaged count was resumed, or counts %d", next.Unparsed())
			}
		}
		evs, next := read(saved(t, at))
		n := 0
		for id := range bad {
			if id <= int64(i+1) {
				n++
			}
		}
		if next.Offset != int64(i+1) || !next.Kept() || next.Unparsed() != n {
			t.Fatalf("row %d: the State at %d counts %d (kept %v), want %d", i+1, next.Offset, next.Unparsed(), next.Kept(), n)
		}
		followed, at = append(followed, evs...), next
	}
	whole, st := read(State{})
	if st.Unparsed() != 2 || !reflect.DeepEqual(followed, whole) || !bytes.Equal(marshal(t, at), marshal(t, st)) {
		t.Fatalf("followed row by row, ending in %s (from the start %s):\n%s\nwant\n%s", marshal(t, at), marshal(t, st), dump(followed), dump(whole))
	}
	got := byRow(whole)
	check(t, 2, got[2], want{Kind: KindAgentText, Who: digest.Agent, Text: "Pruning."})
	check(t, 3, got[3], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_h1", IsError: true})
	check(t, 4, got[4], want{Kind: KindAgentText, Who: digest.Agent, Text: "Trying again."})
	check(t, 6, got[6], want{Kind: KindAgentText, Who: digest.Agent, Text: "With sudo."}, want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_h3", ToolName: "terminal"})
	check(t, 7, got[7], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_h3", ToolName: "terminal"})
	if name := got[3][0].ToolName; name != "" {
		t.Errorf("the result of a call that did not parse names %q", name)
	}

	// From a bare cursor after any row but the last, the pass counts the rows
	// before it. At the last row a State is returned as it is: the saved one
	// with its count, a bare cursor with none.
	for after := int64(0); after < int64(len(rows)); after++ {
		if _, next := read(State{Offset: after}); !bytes.Equal(marshal(t, next), marshal(t, st)) {
			t.Fatalf("after row %d the read ends in %s, want %s", after, marshal(t, next), marshal(t, st))
		}
	}
	if _, again := read(saved(t, st)); again.Unparsed() != 2 {
		t.Fatalf("the State at the last row counts %d", again.Unparsed())
	}
	if _, again := read(State{Offset: st.Offset}); again.Unparsed() != 0 {
		t.Fatalf("a bare cursor at the last row counts %d", again.Unparsed())
	}
}
