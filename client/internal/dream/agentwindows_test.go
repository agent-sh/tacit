package dream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// claudeEntry is one Claude Code transcript line as Claude Code 2.1.30x
// writes it in a session the owner started in a terminal.
func claudeEntry(t *testing.T, kind string, at time.Time, content any, extra map[string]any) string {
	t.Helper()
	o := map[string]any{"type": kind, "entrypoint": "cli", "cwd": "/home/u/projects/shop", "sessionId": "own", "version": "2.1.302",
		"timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"), "message": map[string]any{"role": kind, "content": content}}
	for k, v := range extra {
		o[k] = v
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ownerTurn is a turn the owner typed (origin human, typed).
func ownerTurn(t *testing.T, at time.Time, text string) string {
	return claudeEntry(t, "user", at, text, map[string]any{"origin": map[string]any{"kind": "human"}, "promptSource": "typed", "turnOrigin": "human"})
}

func agentText(t *testing.T, at time.Time, text string) string {
	return claudeEntry(t, "assistant", at, []any{map[string]any{"type": "text", "text": text}}, nil)
}

// agentStep is one step of the agent working on its own: a remark, a
// command, and its result.
func agentStep(t *testing.T, at time.Time, i int) []string {
	id := fmt.Sprintf("toolu_%04d", i)
	result := []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}
	if i%40 == 7 {
		result = []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": true, "content": fmt.Sprintf("--- FAIL: TestShard%d (0.31s)\nexit status 1", i)}}
	}
	return []string{
		claudeEntry(t, "assistant", at, []any{
			map[string]any{"type": "text", "text": fmt.Sprintf("Step %d: rerunning the checkout suite after the cache change; %s", i, strings.Repeat("the shard timings look stable ", 8))},
			map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": fmt.Sprintf("go test ./checkout/... -run Shard%d", i)}},
		}, nil),
		claudeEntry(t, "user", at, result, nil),
	}
}

// autonomous is a transcript of the owner's session that ran on its own for
// days: the owner starts it, the agent works through several windows alone,
// and the owner steps in once near the end.
func autonomous(t *testing.T, start time.Time) (string, []string) {
	lines := []string{
		ownerTurn(t, start, "keep the checkout suite green while you migrate the cache; report when done"),
		agentText(t, start.Add(time.Minute), "Starting the cache migration."),
	}
	at := start.Add(time.Hour)
	for i := 0; i < 600; i++ {
		lines = append(lines, agentStep(t, at.Add(time.Duration(i)*10*time.Minute), i)...)
	}
	end := at.Add(600 * 10 * time.Minute)
	lines = append(lines,
		agentText(t, end, "Pushed the migration straight to main."),
		ownerTurn(t, end.Add(time.Minute), "never push to main in this repo, open a PR instead"),
		agentText(t, end.Add(2*time.Minute), "Understood: reverted, and opened PR 41 instead."),
	)
	for i := 600; i < 640; i++ {
		lines = append(lines, agentStep(t, end.Add(time.Duration(i)*time.Minute), i)...)
	}
	return strings.Join(lines, "\n") + "\n", lines
}

// isolate keeps a test off this machine's home: no projects scan, no state.
func isolate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	t.Setenv("TACIT_STATE_DIR", filepath.Join(home, "state"))
}

func windowsWith(t *testing.T, f string, format digest.Format, p AgentWindows, now time.Time) (Scan, *State) {
	t.Helper()
	st := &State{Offsets: map[string]int64{f: 0}}
	sc, err := ScanWindows([]Source{{Glob: f, Format: format}}, st, false, Select{Agent: p, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return sc, st
}

// An owner's session that ran on its own: the windows of the autonomous
// stretch hold no turn of the owner's and are agent content; with
// agent_windows none they are passed over and the owner's two windows are
// cut to their turns with the agent's lines around them. With the default,
// every window is dreamed whole, as before.
func TestAgentWindowsInAnOwnersAutonomousSession(t *testing.T) {
	isolate(t)
	f := filepath.Join(t.TempDir(), "own.jsonl")
	body, _ := autonomous(t, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	os.WriteFile(f, []byte(body), 0o644)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	all, _ := windowsWith(t, f, digest.Claude, AgentWindows{}, now)
	if len(all.Windows) < 4 || all.Passed != (Passed{}) {
		t.Fatalf("default: %d windows, passed %+v", len(all.Windows), all.Passed)
	}
	owner := 0
	for _, w := range all.Windows {
		if w.Send != nil {
			t.Fatal("the default cut a window")
		}
		if HasOwnerTurn(w.Lines) {
			owner++
		}
	}
	if owner != 2 {
		t.Fatalf("%d of %d windows hold an owner turn, want the first and the last", owner, len(all.Windows))
	}

	none, st := windowsWith(t, f, digest.Claude, AgentWindows{Mode: "none"}, now)
	if len(none.Windows) != 2 || none.Passed.Windows != len(all.Windows)-2 || none.Passed.Cut != 2 || none.Passed.Chars == 0 || none.Passed.CutChars == 0 {
		t.Fatalf("none: %d windows, passed %+v (of %d)", len(none.Windows), none.Passed, len(all.Windows))
	}
	// The cursor waits for the windows to dream; the caller moves it to their
	// end, which is past the passed-over windows too.
	if st.Offsets[f] != 0 || none.Windows[1].End != int64(len(body)) {
		t.Fatalf("cursor %d, end %d of %d", st.Offsets[f], none.Windows[1].End, len(body))
	}
	last := none.Windows[1]
	sent := last.Sent()
	i := slices.Index(sent, "U: never push to main in this repo, open a PR instead")
	if i < 1 || sent[i-1] != "A: Pushed the migration straight to main." || sent[i+1] != "A: Understood: reverted, and opened PR 41 instead." {
		t.Fatalf("the intervention without its context: %v", sent[max(0, i-2):min(len(sent), i+3)])
	}
	if len(sent) > 2*OwnerContext+3 || !strings.HasPrefix(sent[0], "H: [tacit: ") || windowChars(sent) >= windowChars(last.Lines) {
		t.Fatalf("the window was not cut to the owner's turn: %d lines, first %q", len(sent), sent[0])
	}
	// The window's identity is its whole content, whatever the policy sends.
	for _, w := range all.Windows {
		if w.Start == last.Start && w.Part == last.Part && !slices.Equal(w.Lines, last.Lines) {
			t.Fatal("the policy changed the window's lines")
		}
	}
	if got := CheckOwnerWords(Extracted{Kind: "user", Quote: "never push to main in this repo"}, sent); got.Kind != "user" {
		t.Fatalf("the owner's words in a cut window: %+v", got)
	}
}

// A delta of an owner's session with no turn of theirs (the middle of an
// autonomous stretch) yields no window with agent_windows none: its cursor
// moves to the delta's end, as for a delta with nothing to extract.
func TestAgentWindowsPassOverAnAutonomousDelta(t *testing.T) {
	isolate(t)
	f := filepath.Join(t.TempDir(), "own.jsonl")
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 300; i++ {
		lines = append(lines, agentStep(t, at.Add(time.Duration(i)*time.Minute), i)...)
	}
	head := ownerTurn(t, at.Add(-time.Hour), "migrate the cache") + "\n"
	body := head + strings.Join(lines, "\n") + "\n"
	os.WriteFile(f, []byte(body), 0o644)
	st := &State{Offsets: map[string]int64{f: int64(len(head))}}
	sc, err := ScanWindows([]Source{{Glob: f, Format: digest.Claude}}, st, false, Select{Agent: AgentWindows{Mode: "none"}, Now: at.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Windows) != 0 || sc.Passed.Windows < 2 || st.Offsets[f] != int64(len(body)) {
		t.Fatalf("windows %d, passed %+v, cursor %d of %d", len(sc.Windows), sc.Passed, st.Offsets[f], len(body))
	}
	// The default dreams the same delta.
	st = &State{Offsets: map[string]int64{f: int64(len(head))}}
	if ws, _ := WindowsFor([]Source{{Glob: f, Format: digest.Claude}}, st, false, Select{}); len(ws) != sc.Passed.Windows {
		t.Fatalf("default: %d windows, want %d", len(ws), sc.Passed.Windows)
	}
}

// recent:<days> dreams an agent window whose newest line is within the
// days, passes over an older one, and cuts an older window of the owner's to
// their turns; a recent window of the owner's is dreamed whole.
func TestAgentWindowsRecent(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	old, _ := autonomous(t, now.AddDate(0, 0, -60))
	oldF := filepath.Join(dir, "old.jsonl")
	os.WriteFile(oldF, []byte(old), 0o644)
	recentBody, _ := autonomous(t, now.AddDate(0, 0, -6))
	newF := filepath.Join(dir, "new.jsonl")
	os.WriteFile(newF, []byte(recentBody), 0o644)
	p, err := ParseAgentWindows("recent:14")
	if err != nil {
		t.Fatal(err)
	}
	o, _ := windowsWith(t, oldF, digest.Claude, p, now)
	if len(o.Windows) != 2 || o.Passed.Cut != 2 || o.Passed.Windows == 0 {
		t.Fatalf("old: %d windows, passed %+v", len(o.Windows), o.Passed)
	}
	n, _ := windowsWith(t, newF, digest.Claude, p, now)
	all, _ := windowsWith(t, newF, digest.Claude, AgentWindows{}, now)
	if len(n.Windows) != len(all.Windows) || n.Passed != (Passed{}) {
		t.Fatalf("recent: %d of %d windows, passed %+v", len(n.Windows), len(all.Windows), n.Passed)
	}
	for _, w := range n.Windows {
		if w.Send != nil || w.At.IsZero() {
			t.Fatalf("a recent window was cut or has no time: %v", w.At)
		}
	}
}

// Hermes: the owner writes almost every other message, so every window holds
// a turn of theirs, none is passed over, and every line is within the
// context of a turn: nothing is cut.
func TestAgentWindowsHermesAlternating(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "state.db")
	db := hermestest.Create(t, path, hermestest.Session{ID: "tg1", Source: "telegram", StartedAt: 1})
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	at := float64(now.AddDate(0, 0, -30).Unix())
	var ms []hermestest.Message
	for i := 0; i < 300; i++ {
		ms = append(ms,
			hermestest.Message{Session: "tg1", Role: "user", Content: fmt.Sprintf("and the %dth shard? keep the old retry budget", i), Timestamp: at + float64(2*i)},
			hermestest.Message{Session: "tg1", Role: "assistant", Content: fmt.Sprintf("Shard %d passes with the old retry budget. %s", i, strings.Repeat("timings stable ", 18)), Timestamp: at + float64(2*i+1)},
		)
	}
	db.Add(ms...)
	src := []Source{{DB: path, Format: digest.Hermes}}
	st := &State{Offsets: map[string]int64{}, Sources: []string{src[0].root()}}
	all, err := ScanWindows(src, st, false, Select{Now: now})
	if err != nil || len(all.Windows) < 2 {
		t.Fatalf("default: %d windows %v", len(all.Windows), err)
	}
	st = &State{Offsets: map[string]int64{}, Sources: []string{src[0].root()}}
	none, _ := ScanWindows(src, st, false, Select{Agent: AgentWindows{Mode: "none"}, Now: now})
	if len(none.Windows) != len(all.Windows) || none.Passed != (Passed{}) {
		t.Fatalf("none: %d of %d windows, passed %+v", len(none.Windows), len(all.Windows), none.Passed)
	}
	for _, w := range none.Windows {
		if !slices.Equal(w.Sent(), w.Lines) || w.At.IsZero() {
			t.Fatalf("an alternating Hermes window lost lines (%d of %d) or its time", len(w.Sent()), len(w.Lines))
		}
	}
	if _, moved := st.Offsets[hermes.Key(path, "tg1")]; moved {
		t.Fatal("the cursor moved before the windows were dreamed")
	}
}

func TestParseAgentWindows(t *testing.T) {
	for in, want := range map[string]string{"": "all", "all": "all", "ALL": "all", " none ": "none", "recent:30": "recent:30", "recent:3650": "recent:3650"} {
		p, err := ParseAgentWindows(in)
		if err != nil || p.String() != want {
			t.Errorf("%q: %v %v", in, p, err)
		}
	}
	for _, in := range []string{"some", "recent", "recent:0", "recent:-3", "recent:3651", "recent:7d", "none:1"} {
		if _, err := ParseAgentWindows(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestOwnerTurnsKeepsContextAndMarksGaps(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("T: Bash step %d", i))
	}
	lines[10] = "U: stop and rebase first"
	got := OwnerTurns(lines, 2)
	want := []string{"H: [tacit: 8 line(s) of the agent's own work left out]", "T: Bash step 8", "T: Bash step 9", "U: stop and rebase first", "T: Bash step 11", "T: Bash step 12", "H: [tacit: 17 line(s) of the agent's own work left out]"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	if HasOwnerTurn(lines[:10]) || !HasOwnerTurn(lines) {
		t.Fatal("HasOwnerTurn")
	}
}
