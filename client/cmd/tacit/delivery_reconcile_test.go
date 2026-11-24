package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func readVerdicts(t *testing.T) []outcomeLine {
	t.Helper()
	f, err := os.Open(outcomesPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []outcomeLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue // a reconcile's write starts a line of its own
		}
		var l outcomeLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// A mark an older binary logged without recording its verdicts counts:
// the outcomes take it from the log before they are read, under the asked
// block's scope and hook (a Stop ask's, each line's own), once, and a
// verdict the file already holds is not written again.
func TestLoggedMarksReachTheOutcomes(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }

	prompt := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: ts(time.Hour), Hook: "prompt", Repos: []string{"widget"}, MarkAsked: true,
		Cands: []logCand{{Name: "alpha", Delivered: true}, {Name: "beta", Delivered: true}, {Name: "gamma", Delivered: true}}}
	stop := &logEntry{V: deliveryLogVersion, ID: "bbbbbbbbbbbbbbbb", TS: ts(50 * time.Minute), Hook: "stop", MarkAsked: true,
		Cands: []logCand{{Name: "delta", Delivered: true, In: "gadget", Via: "start"}}}
	// The older schema's mark: helped and not_helped.
	old := &logEntry{V: deliveryLogVersion, ID: "cccccccccccccccc", TS: ts(40 * time.Minute), Hook: "mark",
		Mark: &logMarkStep{Block: prompt.ID, Helped: []string{"alpha"}, NotHelped: []string{"beta"}, Wrong: []string{"gamma"}}}
	stopMark := &logEntry{V: deliveryLogVersion, ID: "dddddddddddddddd", TS: ts(30 * time.Minute), Hook: "mark",
		Mark: &logMarkStep{Block: stop.ID, NotHelped: []string{"delta"}}}
	for _, e := range []*logEntry{prompt, stop, old, stopMark} {
		if err := appendLogEntry(e, now); err != nil {
			t.Fatal(err)
		}
	}
	// One verdict of the mark was recorded already (by hand, or by the
	// process that took it).
	recordOutcomes([]outcomeLine{{TS: old.TS, In: "widget", Fact: "beta", Verdict: verdictNotRelevant, Src: "mark", Block: prompt.ID, Hook: "prompt"}})

	if got := outcomes(loadOutcomes(now)["widget"]).class("alpha"); got != outcomePromoted {
		t.Fatalf("a logged used mark did not count: class %d", got)
	}
	// A mark too young for the grace waits; then it counts too.
	later := &logEntry{V: deliveryLogVersion, ID: "ffffffffffffffff", TS: ts(20 * time.Second), Hook: "prompt", MarkAsked: true,
		Cands: []logCand{{Name: "epsilon", Delivered: true}}}
	young := &logEntry{V: deliveryLogVersion, ID: "eeeeeeeeeeeeeeee", TS: ts(10 * time.Second), Hook: "mark",
		Mark: &logMarkStep{Block: later.ID, Helped: []string{"epsilon"}}}
	for _, e := range []*logEntry{later, young} {
		if err := appendLogEntry(e, now); err != nil {
			t.Fatal(err)
		}
	}
	if outcomes(loadOutcomes(now)[""]).class("epsilon") != outcomeNeutral {
		t.Fatal("a mark within the grace was taken")
	}
	if outcomes(loadOutcomes(now.Add(2 * time.Minute))[""]).class("epsilon") != outcomePromoted {
		t.Fatal("a mark past the grace was not taken")
	}
	loadOutcomes(now.Add(3 * time.Minute))

	var got []string
	for _, l := range readVerdicts(t) {
		got = append(got, strings.Join([]string{l.Block[:1], l.Fact, l.Verdict, l.In, l.Hook, l.Src}, " "))
	}
	sort.Strings(got)
	want := []string{
		"a alpha used widget prompt mark",
		"a beta not_relevant widget prompt mark",
		"a gamma wrong widget prompt mark",
		"b delta not_relevant gadget start mark",
		"f epsilon used  prompt mark",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("verdicts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A reconcile reads a bounded part of the log, the search for a mark's
// block included, and the next goes on where it stopped: a mark far from
// its block still counts, after some reconciles, once.
func TestReconcileIsBoundedAndResumes(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	budget, chunk := markReadBudget, markChunk
	markReadBudget, markChunk = 4096, 512
	t.Cleanup(func() { markReadBudget, markChunk = budget, chunk })
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour).Format(time.RFC3339Nano)

	asked := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: ts, Hook: "prompt", MarkAsked: true, Cands: []logCand{{Name: "alpha", Delivered: true}}}
	if err := appendLogEntry(asked, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		e := &logEntry{V: deliveryLogVersion, ID: fmt.Sprintf("%016x", i+1), TS: ts, Hook: "prompt", Terms: []string{strings.Repeat("filler", 30)}}
		if err := appendLogEntry(e, now); err != nil {
			t.Fatal(err)
		}
	}
	mark := &logEntry{V: deliveryLogVersion, ID: "cccccccccccccccc", TS: ts, Hook: "mark", Mark: &logMarkStep{Block: asked.ID, Helped: []string{"alpha"}}}
	if err := appendLogEntry(mark, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	for ; calls < 100 && outcomes(loadOutcomes(now)[""]).class("alpha") != outcomePromoted; calls++ {
	}
	if calls == 0 || calls == 100 {
		t.Fatalf("reconciled after %d calls: want more than one, and an end", calls)
	}
	for i := 0; i < 5; i++ {
		loadOutcomes(now)
	}
	if n := len(readVerdicts(t)); n != 1 {
		t.Fatalf("%d verdicts, want 1", n)
	}
}

// A verdict that could not be appended is not skipped: the cursor stays,
// and the next reconcile appends it.
func TestReconcileKeepsTheCursorOnAFailedAppend(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file this user cannot write")
	}
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Hour).Format(time.RFC3339Nano)
	asked := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: ts, Hook: "prompt", MarkAsked: true, Cands: []logCand{{Name: "alpha", Delivered: true}}}
	mark := &logEntry{V: deliveryLogVersion, ID: "cccccccccccccccc", TS: ts, Hook: "mark", Mark: &logMarkStep{Block: asked.ID, Helped: []string{"alpha"}}}
	for _, e := range []*logEntry{asked, mark} {
		if err := appendLogEntry(e, now); err != nil {
			t.Fatal(err)
		}
	}
	recordOutcomes([]outcomeLine{{TS: ts, Fact: "other", Verdict: verdictUsed, Src: "judge"}})
	if err := os.Chmod(outcomesPath(), 0o400); err != nil {
		t.Fatal(err)
	}
	loadOutcomes(now)
	if err := os.Chmod(outcomesPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	if outcomes(loadOutcomes(now)[""]).class("alpha") != outcomePromoted {
		t.Fatal("a verdict whose append failed was skipped")
	}
}

// The cursor goes on to the next day's file, past an older day's last line
// a crash cut short, and a mark of the day before finds its block there.
func TestReconcileCrossesDays(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	day1 := time.Date(2026, 1, 9, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	now := day2.Add(time.Hour)
	asked := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: day1.Format(time.RFC3339Nano), Hook: "prompt", MarkAsked: true, Cands: []logCand{{Name: "alpha", Delivered: true}}}
	if err := appendLogEntry(asked, day1); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(deliveryLogDir()+"/2026-01-09.jsonl", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"v":1,"id":"0000000000000001","hook":"mark","mark":{"blo`) // cut short
	f.Close()
	mark := &logEntry{V: deliveryLogVersion, ID: "cccccccccccccccc", TS: day2.Format(time.RFC3339Nano), Hook: "mark", Mark: &logMarkStep{Block: asked.ID, Helped: []string{"alpha"}}}
	if err := appendLogEntry(mark, day2); err != nil {
		t.Fatal(err)
	}
	if outcomes(loadOutcomes(now)[""]).class("alpha") != outcomePromoted {
		t.Fatal("a mark of the next day did not count")
	}
}

// The search for a block steps through a line longer than its chunk, even
// one whose chunk holds nothing but the line's newline.
func TestReconcileSearchPassesLongLines(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	chunk := markChunk
	markChunk = 1
	t.Cleanup(func() { markChunk = chunk })
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	asked := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: now.Format(time.RFC3339Nano), Hook: "prompt", MarkAsked: true}
	long := &logEntry{V: deliveryLogVersion, ID: "0000000000000001", TS: asked.TS, Hook: "prompt", Terms: []string{strings.Repeat("x", 2000)}}
	for _, e := range []*logEntry{asked, long} {
		if err := appendLogEntry(e, now); err != nil {
			t.Fatal(err)
		}
	}
	r := &reconciler{dir: deliveryLogDir(), now: now, budget: 1 << 20}
	got, _, _, ok := r.searchFile(deliveryLogDir()+"/2026-01-10.jsonl", -1, []byte(`"id":"aaaaaaaaaaaaaaaa"`), asked.ID)
	if !ok || got == nil || got.ID != asked.ID {
		t.Fatalf("the block was not found past a long line: %v %v", got, ok)
	}
}

// A verdict line a failed write cut short does not swallow the next one.
func TestOutcomesAppendAfterATornLine(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(outcomesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outcomesPath(), []byte(`{"ts":"2026-01-10T11:00:00Z","in":"","fact":"to`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, own := range []bool{false, true} {
		if err := appendOutcomes([]outcomeLine{{TS: now.Format(time.RFC3339Nano), Fact: "alpha", Verdict: verdictUsed, Src: "judge"}}, own); err != nil {
			t.Fatal(err)
		}
		f, _ := os.OpenFile(outcomesPath(), os.O_APPEND|os.O_WRONLY, 0o600)
		f.WriteString(`{"ts":"2026-01-10T11:00:00Z","in":"","fact":"to`) // cut short again
		f.Close()
	}
	if got := tallyOutcomes(now)[""]["alpha"].Used; got != 2 {
		t.Fatalf("%d of 2 verdicts after torn lines", got)
	}
	if outcomes(tallyOutcomes(now)[""]).class("alpha") != outcomePromoted {
		t.Fatal("the verdict after a torn line was lost")
	}
}

// A mark taken just before midnight may reach its day's file after the
// next day's has begun: the cursor stays on a day until it is a minute
// over, so the mark is read.
func TestReconcileWaitsForTheDayToEnd(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	day1 := time.Date(2026, 1, 9, 23, 59, 0, 0, time.UTC)
	midnight := day1.Add(time.Minute)
	asked := &logEntry{V: deliveryLogVersion, ID: "aaaaaaaaaaaaaaaa", TS: day1.Format(time.RFC3339Nano), Hook: "prompt", MarkAsked: true, Cands: []logCand{{Name: "alpha", Delivered: true}}}
	if err := appendLogEntry(asked, day1); err != nil {
		t.Fatal(err)
	}
	next := &logEntry{V: deliveryLogVersion, ID: "0000000000000001", TS: midnight.Format(time.RFC3339Nano), Hook: "prompt"}
	if err := appendLogEntry(next, midnight); err != nil {
		t.Fatal(err)
	}
	loadOutcomes(midnight.Add(20 * time.Second))
	late := day1.Add(59*time.Second + 900*time.Millisecond)
	mark := &logEntry{V: deliveryLogVersion, ID: "cccccccccccccccc", TS: late.Format(time.RFC3339Nano), Hook: "mark", Mark: &logMarkStep{Block: asked.ID, Helped: []string{"alpha"}}}
	if err := appendLogEntry(mark, late); err != nil {
		t.Fatal(err)
	}
	if outcomes(loadOutcomes(midnight.Add(5 * time.Minute))[""]).class("alpha") != outcomePromoted {
		t.Fatal("a mark that reached the day's file late was lost")
	}
}
