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

// rewired is st saved with old replaced by new, and loaded back: a State as
// another version saved it, or a damaged one.
func rewired(t *testing.T, st State, pairs ...string) State {
	t.Helper()
	b := marshal(t, st)
	for i := 0; i+1 < len(pairs); i += 2 {
		if !bytes.Contains(b, []byte(pairs[i])) {
			t.Fatalf("no %s in %s", pairs[i], b)
		}
		b = bytes.Replace(b, []byte(pairs[i]), []byte(pairs[i+1]), 1)
	}
	var back State
	if err := back.UnmarshalBinary(b); err != nil {
		t.Fatal(err)
	}
	return back
}

// asVersion1 is st as version 1 saved it: the same, with no count.
func asVersion1(t *testing.T, st State) State {
	t.Helper()
	return rewired(t, st, fmt.Sprintf(`"v":%d`, keptVersion), `"v":1`, fmt.Sprintf(`"unparsed":%d,`, st.Unparsed()), "")
}

// A line the reader cannot decode gives no events and is counted, in every
// transcript format: a line a crash cut short, which the next write joined,
// and JSON that is not the record its format writes. The lines around them
// give the events they give without them: a result after them is still named
// by its call before them. A line that holds nothing (blank, or null) is not
// counted, nor a last line still being written. Each State counts the lines
// before its cursor however the read came there: from the start, line by
// line from saved States, from a bare cursor (its pass counts them), stopped
// by emit and read on, from a State whose line before the cursor changed. At
// the end a State a read returned is returned as it is, and a bare cursor, a
// State saved before the count or a damaged one is read with the pass.
func TestLinesThatDoNotParseAreCounted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	// JSON that is not the record its format writes: a field the reader reads
	// of another type, a payload or a message that does not decode.
	shapes := map[digest.Format]string{
		digest.Claude: cl(1, `"type":"user","uuid":"u9","isMeta":"yes","message":{"role":"user","content":"a field of another type"}`),
		digest.Codex:  cx(2, "response_item", `{"type":"message","role":7,"content":[{"type":"output_text","text":"a payload of another shape"}]}`),
		digest.Pi:     piEntry(8, `"type":"message","id":"a9","message":"a message that is a string"`),
	}
	for _, c := range growingSessions(home) {
		t.Run(c.name, func(t *testing.T) {
			// What a crash leaves: half a line, and the line written again
			// after it, on the same line.
			cut := c.turn[:len(c.turn)/2] + c.turn
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
			// parse do, end the transcript's end.
			var starts, bad []int64
			var end int64
			for i, l := range lines {
				starts = append(starts, end)
				if i == len(c.first)+1 || i == len(c.first)+2 {
					bad = append(bad, end)
				}
				end += int64(len(l)) + 1
			}
			replyAt, resultAt := starts[len(c.first)+5], starts[len(c.first)+6]
			before := func(cursor int64) int {
				n := 0
				for _, b := range bad {
					if b < cursor {
						n++
					}
				}
				return n
			}
			from := func(whole []Event, off int64) []Event {
				var out []Event
				for _, e := range whole {
					if e.Offset >= off {
						out = append(out, e)
					}
				}
				return out
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
				if next.Offset <= resultAt && !resumes(t, path, c.f, saved(t, next)) {
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
				if !reflect.DeepEqual(rest, from(whole, s)) || !bytes.Equal(marshal(t, next), marshal(t, st)) {
					t.Fatalf("from the cursor %d, ending in %s (want %s):\n%s\nwant\n%s", s, marshal(t, next), marshal(t, st), dump(rest), dump(from(whole, s)))
				}
			}

			// Stopped by emit at the reply after them: a cursor at its line,
			// which counts them. Read on from it, each is counted once.
			stopped, err := ReadFileState(path, c.f, State{}, 0, func(e Event) error {
				if e.Offset >= replyAt {
					return errStop
				}
				return nil
			})
			if !errors.Is(err, errStop) || stopped.Kept() || stopped.Offset != replyAt || stopped.Unparsed() != 2 {
				t.Fatalf("stopped at the reply (%d): %v, cursor %d, kept %v, %d lines that do not parse", replyAt, err, stopped.Offset, stopped.Kept(), stopped.Unparsed())
			}
			if rest, next := readState(t, path, c.f, saved(t, stopped), 0); !reflect.DeepEqual(rest, from(whole, replyAt)) || !bytes.Equal(marshal(t, next), marshal(t, st)) {
				t.Fatalf("read on from the stop, ending in %s (want %s)", marshal(t, next), marshal(t, st))
			}

			// A State whose line before the cursor changed is read as a
			// cursor, and its pass counts the lines once.
			_, atResult := readState(t, path, c.f, State{}, resultAt)
			atResult = saved(t, atResult)
			if atResult.Offset != resultAt || atResult.Unparsed() != 2 {
				t.Fatalf("the State at the result: %d, %d lines that do not parse", atResult.Offset, atResult.Unparsed())
			}
			replaceInFile(t, path, c.replyText, c.replyEdit)
			_, fresh := readState(t, path, c.f, State{}, 0)
			if _, next := readState(t, path, c.f, atResult, 0); resumes(t, path, c.f, atResult) || !bytes.Equal(marshal(t, next), marshal(t, fresh)) {
				t.Fatalf("after the line before the cursor changed, the read ends in %s; from the start %s", marshal(t, next), marshal(t, fresh))
			}
			replaceInFile(t, path, c.replyEdit, c.replyText)
			// The same past the transcript's first 256 KiB, where the head is
			// unchanged and only the line before the cursor tells.
			long := append([]string{}, c.first...)
			for i := range 300 {
				long = append(long, filler(c.f, i))
			}
			long = append(long, c.call, cut, shapes[c.f], c.reply, c.result)
			lpath := write(t, strings.Replace(c.path, ".jsonl", "-unparsed-long.jsonl", 1), long...)
			var lresultAt int64
			for _, l := range long[:len(long)-1] {
				lresultAt += int64(len(l)) + 1
			}
			_, lat := readState(t, lpath, c.f, State{}, lresultAt)
			lat = saved(t, lat)
			if lat.Offset != lresultAt || lat.Unparsed() != 2 || lat.Offset <= headBytes {
				t.Fatalf("the long State at the result: %d, %d lines that do not parse", lat.Offset, lat.Unparsed())
			}
			replaceInFile(t, lpath, c.replyText, c.replyEdit)
			_, lfresh := readState(t, lpath, c.f, State{}, 0)
			if _, next := readState(t, lpath, c.f, lat, 0); resumes(t, lpath, c.f, lat) || !bytes.Equal(marshal(t, next), marshal(t, lfresh)) {
				t.Fatalf("past the head, after the line before the cursor changed, the read ends in %s; from the start %s", marshal(t, next), marshal(t, lfresh))
			}

			// A count below 0, or one that is no whole number, is damage: the
			// State loads as a bare cursor.
			for _, damage := range []string{`"unparsed":-5`, `"unparsed":"two"`, `"unparsed":2.5`, `"unparsed":null`} {
				if d := rewired(t, st, `"unparsed":2`, damage); d.Kept() || d.Unparsed() != 0 || d.Offset != st.Offset {
					t.Fatalf("%s loads as %+v", damage, d)
				}
			}
			// At the end a State a read returned is returned as it is. A bare
			// cursor, one saved before the count (version 1) and a damaged one
			// are read with the pass, so the State returned holds the count.
			// ReadFile, which gives no count, reads nothing there.
			if evs, again := readState(t, path, c.f, saved(t, st), 0); len(evs) != 0 || !bytes.Equal(marshal(t, again), marshal(t, st)) {
				t.Fatalf("the saved State at the end: %d events, %s", len(evs), marshal(t, again))
			}
			for name, s := range map[string]State{"a bare cursor": {Offset: end}, "a version 1 State": asVersion1(t, st), "a damaged State": rewired(t, st, `"unparsed":2`, `"unparsed":-5`)} {
				if s.Kept() {
					t.Fatalf("%s at the end loads as kept", name)
				}
				if evs, again := readState(t, path, c.f, s, 0); len(evs) != 0 || !bytes.Equal(marshal(t, again), marshal(t, st)) {
					t.Fatalf("%s at the end: %d events, %s (want %s)", name, len(evs), marshal(t, again), marshal(t, st))
				}
			}
			if evs, next := readAll(t, path, c.f, end); len(evs) != 0 || next != end {
				t.Fatalf("ReadFile at the end: %d events, cursor %d", len(evs), next)
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

// A read during which the transcript grows past the head it began with
// returns a bare cursor, which counts the lines before it that did not parse;
// the read from it counts them again in its pass, once.
func TestCountWhileTheTranscriptGrows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := growingSessions(home)[0]
	cut := c.turn[:len(c.turn)/2] + c.turn
	path := write(t, strings.Replace(c.path, ".jsonl", "-grows.jsonl", 1), append(append([]string{}, c.first...), cut, c.call)...)
	grown := false
	st, err := ReadFileState(path, c.f, State{}, 0, func(Event) error {
		if !grown {
			grown = true
			appendLines(t, path, c.reply, c.result)
		}
		return nil
	})
	fi, serr := os.Stat(path)
	if err != nil || serr != nil || !grown || st.Kept() || st.Offset != fi.Size() || st.Unparsed() != 1 {
		t.Fatalf("a read while the transcript grew: %+v %v %v", st, err, serr)
	}
	if _, again := readState(t, path, c.f, saved(t, st), 0); !again.Kept() || again.Unparsed() != 1 || again.Offset != st.Offset {
		t.Fatalf("the read from its cursor: %+v", again)
	}
}

// A read that fails on its input after lines it read returns a cursor that
// counts the lines before it that did not parse; one that fails in its pass,
// before the cursor it started from, returns that cursor with its count. The
// input: a compressed copy of several blocks whose checksum is wrong, so the
// first blocks decode and the last fails.
func TestCountWhenTheReadFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := growingSessions(home)[1] // Codex compresses its rollouts
	cut := c.turn[:len(c.turn)/2] + c.turn
	lines := append(append([]string{}, c.first...), cut)
	var cutEnd int64
	for _, l := range lines {
		cutEnd += int64(len(l)) + 1
	}
	for i := range 400 { // more than one block of 128 KiB
		lines = append(lines, filler(c.f, i))
	}
	callAt := cutEnd
	for _, l := range lines[len(lines)-400:] {
		callAt += int64(len(l)) + 1
	}
	lines = append(lines, c.call, c.result)
	plain := write(t, strings.Replace(c.path, ".jsonl", "-fails.jsonl", 1), lines...)
	_, whole := readState(t, plain, c.f, State{}, 0)
	zst := compress(t, plain)
	// A cursor at the call, in the last block, where a read stopped by emit
	// left it: it counts the cut line.
	late, err := ReadFileState(plain, c.f, State{}, 0, func(e Event) error {
		if e.Offset >= callAt {
			return errStop
		}
		return nil
	})
	if !errors.Is(err, errStop) || late.Kept() || late.Offset != callAt || late.Unparsed() != 1 {
		t.Fatalf("stopped at the call (%d): %v, %+v", callAt, err, late)
	}
	b, err := os.ReadFile(zst)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff // the frame's checksum
	if err := os.WriteFile(zst, b, 0o600); err != nil {
		t.Fatal(err)
	}
	failed, err := ReadFileState(plain, c.f, State{}, 0, func(Event) error { return nil })
	if err == nil || failed.Kept() || failed.Offset < cutEnd || failed.Offset >= callAt || failed.Unparsed() != 1 {
		t.Fatalf("a read whose input failed: %v, %+v (the cut line ends at %d, the call starts at %d, the whole read ends at %d)", err, failed, cutEnd, callAt, whole.Offset)
	}
	if inPass, err := ReadFileState(plain, c.f, late, 0, func(Event) error { return nil }); err == nil || inPass.Kept() || inPass.Offset != callAt || inPass.Unparsed() != 1 {
		t.Fatalf("a read that failed in its pass: %v, %+v", err, inPass)
	}
}

// The payload of a Codex record the reader reads that does not decode makes
// its line count: an event_msg that is no object, an end event whose call id
// is a number, a turn_context, and a compacted entry, which still marks its
// compaction. A field the reader does not read, of another type, counts
// nothing: the record is read as before.
func TestCodexRecordsThatDoNotParseAreCounted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := growingSessions(home)[1]
	n := len(c.first) + 1
	lines := append(append([]string{}, c.first...), c.call,
		/* n */ cx(2, "event_msg", `"a task_complete of another shape"`),
		/* n+1 */ cx(2, "event_msg", `{"type":"exec_command_end","call_id":7,"exit_code":1}`),
		/* n+2 */ cx(2, "turn_context", `"a context of another shape"`),
		/* n+3 */ cx(2, "compacted", `"a summary of another shape"`),
		/* n+4 */ cx(2, "event_msg", `{"type":"elicitation_request","id":7,"server_name":"docs"}`),
		/* n+5 */ cx(2, "turn_context", `{"turn_id":"t2","cwd":"/home/u/projects/other","model":"gpt-6.1-luna","effort":7}`),
		/* n+6 */ c.result,
		/* n+7 */ c.turn)
	path := write(t, strings.Replace(c.path, ".jsonl", "-records.jsonl", 1), lines...)
	evs, st := readState(t, path, digest.Codex, State{}, 0)
	if st.Unparsed() != 4 {
		t.Fatalf("%d lines that do not parse, want 4:\n%s", st.Unparsed(), dump(evs))
	}
	got := byLine(t, lines, evs)
	check(t, n+3, got[n+3], want{Kind: KindCompaction, Who: digest.Harness, Why: "compacted"})
	check(t, n+6, got[n+6], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_g1", ToolName: "exec_command", IsError: true})
	if len(got[n+7]) == 0 {
		t.Fatalf("no events for the turn:\n%s", dump(evs))
	}
	if e := got[n+7][0]; e.Model != "gpt-6.1-luna" || e.Cwd != "/home/u/projects/other" {
		t.Errorf("the turn's model and cwd: %q %q", e.Model, e.Cwd)
	}
}

// An assistant row whose tool calls do not parse is counted: its text is
// read, its calls are not, and the result after it names no tool. A row with
// no calls, with calls that parse, a compaction copy of a counted row and a
// summary with calls are not counted. Each State counts the rows up to its
// cursor: row by row from saved States, from a bare cursor (its pass counts
// them), stopped by emit and read on, after the store failed, from a State
// whose row at the cursor changed. At the last row a State a read returned
// is returned as it is, and a bare cursor, a State saved before the count or
// a damaged one is read with the pass.
func TestHermesRowsWhoseCallsDoNotParseAreCounted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "state.db")
	store := hermestest.Create(t, path, hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100})
	// Calls of another shape: the arguments an object, not a string of JSON.
	pruning := hermestest.Message{Session: "phone", Role: "assistant", Content: "Pruning.", Finish: "tool_calls", Timestamp: 102,
		ToolCalls: `[{"id":"call_h1","type":"function","function":{"name":"terminal","arguments":{"command":"docker image prune -f"}}}]`}
	rows := []hermestest.Message{
		/* 1 */ {Session: "phone", Role: "user", Content: "prune the docker images", Timestamp: 101},
		/* 2 */ pruning,
		/* 3 */ {Session: "phone", Role: "tool", ToolCallID: "call_h1", Content: `{"output": "permission denied", "exit_code": 1}`, Timestamp: 103},
		// Calls cut short.
		/* 4 */ {Session: "phone", Role: "assistant", Content: "Trying again.", Finish: "tool_calls", Timestamp: 104, ToolCalls: `[{"id":"call_h2","type":"func`},
		/* 5 */ {Session: "phone", Role: "assistant", Content: "Both failed.", Finish: "stop", Timestamp: 105},
		/* 6 */ {Session: "phone", Role: "assistant", Content: "With sudo.", Finish: "tool_calls", Timestamp: 106,
			ToolCalls: `[{"id":"call_h3","type":"function","function":{"name":"terminal","arguments":"{}"}}]`},
		/* 7 */ {Session: "phone", Role: "tool", ToolCallID: "call_h3", Content: `{"output": "ok", "exit_code": 0}`, Timestamp: 107},
		/* 8, row 2 written again by a compaction */ pruning,
		/* 9 */ {Session: "phone", Role: "assistant", Summary: true, Content: "[CONTEXT COMPACTION] the pruning so far", Timestamp: 108, ToolCalls: `[{"id":`},
	}
	bad := map[int64]bool{2: true, 4: true}
	upTo := func(id int64) int {
		n := 0
		for b := range bad {
			if b <= id {
				n++
			}
		}
		return n
	}
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
	states := map[int64]State{}
	for i, m := range rows {
		store.Add(m)
		// The copy, row 8, gives no event to stop at.
		if i > 0 && i+1 != 8 && !resumes(saved(t, at)) {
			t.Fatalf("row %d: the State at %d is read as a bare cursor", i+1, at.Offset)
		}
		evs, next := read(saved(t, at))
		if next.Offset != int64(i+1) || !next.Kept() || next.Unparsed() != upTo(int64(i+1)) {
			t.Fatalf("row %d: the State at %d counts %d (kept %v), want %d", i+1, next.Offset, next.Unparsed(), next.Kept(), upTo(int64(i+1)))
		}
		followed, at = append(followed, evs...), next
		states[next.Offset] = next
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
	check(t, 8, got[8])
	check(t, 9, got[9], want{Kind: KindCompaction, Who: digest.Harness, Why: "compaction summary", Text: "[CONTEXT COMPACTION] the pruning so far"})
	if name := got[3][0].ToolName; name != "" {
		t.Errorf("the result of a call that did not parse names %q", name)
	}

	// From a bare cursor after any row, the pass counts the rows before it,
	// so the read ends in the State a read from the start ends in.
	for after := int64(0); after <= int64(len(rows)); after++ {
		if _, next := read(State{Offset: after}); !bytes.Equal(marshal(t, next), marshal(t, st)) {
			t.Fatalf("after row %d the read ends in %s, want %s", after, marshal(t, next), marshal(t, st))
		}
	}

	// Stopped by emit at row 3, after the row whose calls did not parse: a
	// cursor at row 2 that counts it. At that row's own event: a cursor at row
	// 1 that does not. Read on from either, each is counted once.
	for _, c := range []struct {
		stopAt, cursor int64
		n              int
	}{{3, 2, 1}, {2, 1, 0}} {
		stopped, err := ReadHermesState(ctx, db, session(), State{}, func(e Event) error {
			if e.Offset >= c.stopAt {
				return errStop
			}
			return nil
		})
		if !errors.Is(err, errStop) || stopped.Kept() || stopped.Offset != c.cursor || stopped.Unparsed() != c.n {
			t.Fatalf("stopped at row %d: %v, cursor %d, kept %v, counts %d (want %d)", c.stopAt, err, stopped.Offset, stopped.Kept(), stopped.Unparsed(), c.n)
		}
		if _, next := read(saved(t, stopped)); !bytes.Equal(marshal(t, next), marshal(t, st)) {
			t.Fatalf("read on from the stop at row %d, ending in %s (want %s)", c.stopAt, marshal(t, next), marshal(t, st))
		}
	}

	// The store fails once the rows are read (the context ends in emit): the
	// cursor at the last row counts them.
	cctx, cancel := context.WithCancel(ctx)
	failed, err := ReadHermesState(cctx, db, session(), State{}, func(e Event) error {
		if e.Offset == int64(len(rows)) {
			cancel()
		}
		return nil
	})
	cancel()
	if !errors.Is(err, context.Canceled) || failed.Kept() || failed.Offset != int64(len(rows)) || failed.Unparsed() != 2 {
		t.Fatalf("the store failed after the last row: %v, %+v", err, failed)
	}

	// A State whose row at the cursor changed is read as a cursor, and its
	// pass counts the rows once.
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
	at5 := saved(t, states[5])
	exec(`update messages set content = 'Both failed!' where id = 5`)
	_, fresh := read(State{})
	if _, next := read(at5); resumes(at5) || next.Unparsed() != 2 || !bytes.Equal(marshal(t, next), marshal(t, fresh)) {
		t.Fatalf("after the row at the cursor changed, the read ends in %s; from the start %s", marshal(t, next), marshal(t, fresh))
	}
	exec(`update messages set content = 'Both failed.' where id = 5`)

	// At the last row a State a read returned is returned as it is. A bare
	// cursor, one saved before the count (version 1) and a damaged one are
	// read with the pass, so the State returned holds the count. ReadHermes,
	// which gives no count, reads nothing there.
	if evs, again := read(saved(t, st)); len(evs) != 0 || !bytes.Equal(marshal(t, again), marshal(t, st)) {
		t.Fatalf("the saved State at the last row: %d events, %s", len(evs), marshal(t, again))
	}
	for name, s := range map[string]State{"a bare cursor": {Offset: st.Offset}, "a version 1 State": asVersion1(t, st), "a damaged State": rewired(t, st, `"unparsed":2`, `"unparsed":-5`)} {
		if s.Kept() {
			t.Fatalf("%s at the last row loads as kept", name)
		}
		if evs, again := read(s); len(evs) != 0 || !bytes.Equal(marshal(t, again), marshal(t, st)) {
			t.Fatalf("%s at the last row: %d events, %s (want %s)", name, len(evs), marshal(t, again), marshal(t, st))
		}
	}
	if evs, last := readHermes(t, db, "phone", st.Offset); len(evs) != 0 || last != st.Offset {
		t.Fatalf("ReadHermes at the last row: %d events, cursor %d", len(evs), last)
	}
}

// stray is a line with a field of another type, and the same line without it.
type stray struct{ dirty, clean string }

// Only what a line's kind reads is checked. A field of another type in a kind
// the reader passes over (a new kind, a reasoning item, a token count, a
// label), or one the line's kind does not read (an assistant reply's isMeta,
// a system line's message, a function call's id, a message's name, a turn's
// end with a call id, a model change's summary, a user message's exitCode),
// counts nothing, and the line is read as it is without that field. A field
// the line's kind reads, of another type, counts, and the line gives nothing.
func TestOnlyWhatAKindReadsIsChecked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	g := growingSessions(home)
	reply := `"message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Warming now."}]}`
	for _, c := range []struct {
		g       growing
		lines   []stray
		counted []string
	}{
		{g[0], []stray{
			{cl(1, `"type":"some-new-kind","uuid":"n1","message":"a string"`), cl(1, `"type":"some-new-kind","uuid":"n1"`)},
			{cl(1, `"type":"some-new-kind","uuid":"n2","attachment":["a list"]`), cl(1, `"type":"some-new-kind","uuid":"n2"`)},
			{cl(1, `"type":"some-new-kind","uuid":"n3","isMeta":"yes"`), cl(1, `"type":"some-new-kind","uuid":"n3"`)},
			{cl(1, `"type":"assistant","uuid":"a4","isMeta":"yes",`+reply), cl(1, `"type":"assistant","uuid":"a4",`+reply)},
			{cl(1, `"type":"system","subtype":"turn_duration","uuid":"s5","message":"a string"`), cl(1, `"type":"system","subtype":"turn_duration","uuid":"s5"`)},
		}, []string{
			cl(1, `"type":"assistant","uuid":"a6","message":{"model":5,"role":"assistant","content":[{"type":"text","text":"Warming now."}]}`),
			cl(1, `"type":"assistant","uuid":"a7","isApiErrorMessage":"yes",`+reply),
			cl(1, `"type":"system","subtype":5,"uuid":"s8"`),
			cl(1, `"type":"attachment","uuid":"h9","attachment":{"type":"hook_success","hookEvent":7,"content":"ok"}`),
			cl(1, `"type":"user","uuid":"u10","promptSource":7,"message":{"role":"user","content":"and the docs"}`),
			cl(1, `"type":"some-new-kind","uuid":"n11","cwd":7`),
		}},
		{g[1], []stray{
			{cx(2, "response_item", `{"type":"reasoning","id":7,"summary":[]}`), cx(2, "response_item", `{"type":"reasoning","id":"rs_1","summary":[]}`)},
			{cx(2, "event_msg", `{"type":"token_count","call_id":7}`), cx(2, "event_msg", `{"type":"token_count"}`)},
			{cx(2, "response_item", `{"type":"function_call","id":7,"name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_s1"}`),
				cx(2, "response_item", `{"type":"function_call","id":"fc_s1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"call_s1"}`)},
			{cx(2, "response_item", `{"type":"message","role":"assistant","name":5,"content":[{"type":"output_text","text":"Listing."}]}`),
				cx(2, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Listing."}]}`)},
			{cx(2, "event_msg", `{"type":"task_complete","turn_id":"t1","call_id":7}`), cx(2, "event_msg", `{"type":"task_complete","turn_id":"t1"}`)},
		}, []string{
			cx(2, "response_item", `{"type":"function_call","name":"exec_command","arguments":"{}","call_id":5}`),
			cx(2, "response_item", `{"type":"function_call","name":["exec_command"],"arguments":"{}","call_id":"call_s2"}`),
			cx(2, "response_item", `{"type":"function_call_output","call_id":5,"output":"ok"}`),
			cx(2, "response_item", `{"type":"web_search_call","id":5,"status":"completed","action":{"type":"search","query":"zstd"}}`),
			cx(2, "event_msg", `{"type":"patch_apply_end","call_id":5,"success":false}`),
			cx(2, "turn_context", `{"turn_id":"t2","cwd":5,"model":"gpt-6.1-luna"}`),
		}},
		{g[2], []stray{
			{piEntry(8, `"type":"label","id":"l1","label":"x","summary":{"n":1}`), piEntry(8, `"type":"label","id":"l1","label":"x"`)},
			{piEntry(8, `"type":"model_change","id":"m2","provider":"p","modelId":"qwen3-coder","summary":5`), piEntry(8, `"type":"model_change","id":"m2","provider":"p","modelId":"qwen3-coder"`)},
			{piMsg(9, "a3", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "stop", "toolCallId": 5, "content": piText("Done.")}),
				piMsg(9, "a3", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "stop", "content": piText("Done.")})},
			{piMsg(10, "u4", map[string]any{"role": "user", "exitCode": "x", "content": piText("and the docs")}),
				piMsg(10, "u4", map[string]any{"role": "user", "content": piText("and the docs")})},
		}, []string{
			piEntry(10, `"type":"model_change","id":"m5","provider":"p","modelId":5`),
			piEntry(10, `"type":"compaction","id":"c6","summary":{"text":"the work"}`),
			piEntry(10, `"type":"message","id":7,"message":{"role":"user","content":"and the docs"}`),
			piMsg(10, "a8", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": 5, "content": piText("Done.")}),
			piMsg(10, "r9", map[string]any{"role": "toolResult", "toolCallId": 5, "content": piText("ok")}),
			piMsg(10, "b10", map[string]any{"role": "bashExecution", "command": "ls", "output": "x", "exitCode": "zero"}),
		}},
	} {
		t.Run(c.g.name, func(t *testing.T) {
			dirty := append(append([]string{}, c.g.first...), c.g.call)
			clean := append([]string{}, dirty...)
			for _, s := range c.lines {
				dirty, clean = append(dirty, s.dirty), append(clean, s.clean)
			}
			dirty = append(dirty, c.counted...)
			dirty, clean = append(dirty, c.g.result, c.g.turn), append(clean, c.g.result, c.g.turn)
			dpath := write(t, strings.Replace(c.g.path, ".jsonl", "-stray.jsonl", 1), dirty...)
			cpath := write(t, strings.Replace(c.g.path, ".jsonl", "-nostray.jsonl", 1), clean...)
			got, st := readState(t, dpath, c.g.f, State{}, 0)
			want, wst := readState(t, cpath, c.g.f, State{}, 0)
			if st.Unparsed() != len(c.counted) || wst.Unparsed() != 0 || !reflect.DeepEqual(unplaced(got), unplaced(want)) {
				t.Fatalf("%d lines counted, want %d:\n%s\nwant\n%s", st.Unparsed(), len(c.counted), dump(got), dump(want))
			}
		})
	}
}

// A message whose content is neither a string nor a list (an object, a
// number), or a pi reply's that is not a list, holds no text the reader can
// take: the whole message would be gone, so its line is counted and gives no
// events. A Codex compacted entry whose summary is of another shape still
// marks its compaction. Content that is empty, null, a string or a list is
// read as before.
func TestMessageContentOfAnotherShapeIsCounted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	obj := `{"type":"text","text":"a block that is not in a list"}`
	g := growingSessions(home)
	for _, c := range []struct {
		g       growing
		counted []string
		read    []string // read as before, and not counted
		marks   int      // events the counted lines still give: a compaction's
	}{
		{g[0], []string{
			cl(1, `"type":"assistant","uuid":"a7","message":{"model":"claude-opus-5-5","role":"assistant","content":`+obj+`}`),
			cl(1, `"type":"user","uuid":"u8","promptId":"p8",`+typedMarks+`,"message":{"role":"user","content":`+obj+`}`),
			cl(1, `"type":"attachment","uuid":"h9","attachment":{"type":"hook_additional_context","content":`+obj+`,"hookName":"PreToolUse:Read","toolUseID":"toolu_77","hookEvent":"PreToolUse"}`),
		}, []string{
			cl(1, `"type":"assistant","uuid":"a10","message":{"model":"claude-opus-5-5","role":"assistant","content":[]}`),
			cl(1, `"type":"attachment","uuid":"h11","attachment":{"type":"hook_success","hookName":"PostToolUse:Bash","toolUseID":"toolu_77","hookEvent":"PostToolUse","content":""}`),
			cl(1, `"type":"assistant","uuid":"a12","message":{"model":"claude-opus-5-5","role":"assistant","content":"A reply as a string."}`),
		}, 0},
		{g[1], []string{
			cx(2, "response_item", `{"type":"message","role":"assistant","content":`+obj+`}`),
			cx(2, "response_item", `{"type":"agent_message","author":"/root/ada","content":7}`),
			cx(2, "compacted", `{"message":`+obj+`}`),
		}, []string{
			cx(2, "response_item", `{"type":"message","role":"assistant","content":"Listing as a string."}`),
			cx(2, "compacted", `{"message":"The work so far."}`),
		}, 1},
		{g[2], []string{
			piMsg(9, "u7", map[string]any{"role": "user", "content": map[string]string{"type": "text", "text": "an object"}}),
			piMsg(9, "a8", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "stop", "content": "a string, where pi writes a list"}),
			piMsg(9, "c9", map[string]any{"role": "custom", "content": map[string]int{"n": 1}}),
			piEntry(9, `"type":"custom_message","id":"x10","customType":"plan","content":{"n":1},"display":true`),
		}, []string{
			piMsg(9, "a11", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "error", "errorMessage": "overloaded", "content": []any{}}),
			piEntry(9, `"type":"custom_message","id":"x12","customType":"plan","content":"Plan: 2. test it","display":true`),
		}, 0},
	} {
		t.Run(c.g.name, func(t *testing.T) {
			lines := append(append([]string{}, c.g.first...), c.g.call)
			counted := map[int]bool{}
			for _, l := range c.counted {
				counted[len(lines)] = true
				lines = append(lines, l)
			}
			read := len(lines)
			lines = append(append(lines, c.read...), c.g.result)
			path := write(t, strings.Replace(c.g.path, ".jsonl", "-content.jsonl", 1), lines...)
			evs, st := readState(t, path, c.g.f, State{}, 0)
			if st.Unparsed() != len(c.counted) {
				t.Fatalf("%d lines counted, want %d:\n%s", st.Unparsed(), len(c.counted), dump(evs))
			}
			got := byLine(t, lines, evs)
			marks := 0
			for i := range counted {
				marks += len(got[i])
			}
			// The lines read as before give what they hold: text from the
			// string contents, an error, a compaction, an extension's message.
			readEvents := 0
			for i := read; i < len(lines)-1; i++ {
				readEvents += len(got[i])
			}
			if marks != c.marks || readEvents == 0 {
				t.Fatalf("the counted lines give %d events (want %d), the others %d:\n%s", marks, c.marks, readEvents, dump(evs))
			}
		})
	}
}
