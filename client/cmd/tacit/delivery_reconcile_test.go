package main

import (
	"bufio"
	"encoding/json"
	"os"
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
	young := &logEntry{V: deliveryLogVersion, ID: "eeeeeeeeeeeeeeee", TS: ts(10 * time.Second), Hook: "mark",
		Mark: &logMarkStep{Block: prompt.ID, Helped: []string{"beta"}}}
	if err := appendLogEntry(young, now); err != nil {
		t.Fatal(err)
	}
	loadOutcomes(now)
	loadOutcomes(now.Add(2 * time.Minute))

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
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("verdicts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
