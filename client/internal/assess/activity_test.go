package assess

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/record"
)

func activityOf(today string, days ...string) *Activity {
	a := &Activity{}
	for _, d := range days {
		a.AddDay(d, "forge", "commit:"+strings.ReplaceAll(d, "-", ""))
	}
	a.Measure(today)
	return a
}

// Code's status is the dates against the folder's own rhythm, within the
// rhythm's bounds.
func TestActivityRhythm(t *testing.T) {
	for _, c := range []struct {
		name, today string
		days        []string
		status      string
		gap, window int
		measured    bool
	}{
		// Gaps 10, 10, 10 (days in a row are one stretch): typical 10, window 20.
		{"own rhythm, inside", "2026-10-30", []string{"2026-10-10", "2026-09-30", "2026-09-20", "2026-09-10"}, record.StatusActive, 10, 20, true},
		{"own rhythm, past it", "2026-11-01", []string{"2026-10-10", "2026-09-30", "2026-09-20", "2026-09-10"}, record.StatusDormant, 10, 20, true},
		{"a stretch of days in a row is one", "2026-10-30", []string{"2026-10-11", "2026-10-10", "2026-09-30", "2026-09-29", "2026-09-20", "2026-09-10"}, record.StatusActive, 10, 20, true},
		// Too few gaps: the owner's usual 8 days, window 16.
		{"too little history", "2026-10-26", []string{"2026-10-10", "2026-09-01"}, record.StatusActive, defaultGapDays, 16, false},
		{"too little history, past it", "2026-10-27", []string{"2026-10-10"}, record.StatusDormant, defaultGapDays, 16, false},
		// Every other day: 2 x 2 = 4, the floor is 7.
		{"floor", "2026-10-17", []string{"2026-10-10", "2026-10-08", "2026-10-06", "2026-10-04"}, record.StatusActive, 2, minWindowDays, true},
		// Every 60 days: 120, the ceiling is 32.
		{"ceiling", "2026-11-12", []string{"2026-10-10", "2026-08-11", "2026-06-12", "2026-04-13"}, record.StatusDormant, 60, maxWindowDays, true},
	} {
		a := activityOf(c.today, c.days...)
		if a.Status != c.status || a.TypicalGap != c.gap || a.Window != c.window || a.Measured != c.measured {
			t.Errorf("%s: status %s gap %d window %d measured %v (idle %d, %d gaps)", c.name, a.Status, a.TypicalGap, a.Window, a.Measured, a.Idle, a.Gaps)
		}
	}
	a := &Activity{Forge: &check.Activity{FullName: "o/x", Archived: true}}
	a.AddDay("2026-10-20", "forge", "commit:abc")
	if a.Measure("2026-10-21"); a.Status != record.StatusDormant || !strings.Contains(a.StatusWhy(), "archived") {
		t.Errorf("archived: %s %s", a.Status, a.StatusWhy())
	}
	// One day, two sources: the citable one is kept, a commit here over the forge's.
	b := &Activity{}
	b.AddDay("2026-10-20", "catalog", "")
	b.AddDay("2026-10-20", "session", "session:s1")
	b.AddDay("2026-10-20", "commit", "commit:abcdef1")
	if len(b.Days) != 1 || b.Days[0].Cite != "commit:abcdef1" {
		t.Errorf("days %+v", b.Days)
	}
	// The order days come in changes nothing.
	x := activityOf("2026-10-30", "2026-09-10", "2026-10-10", "2026-09-20", "2026-09-30")
	y := activityOf("2026-10-30", "2026-10-10", "2026-09-30", "2026-09-20", "2026-09-10")
	if x.Key() != y.Key() || x.StatusWhy() != y.StatusWhy() {
		t.Errorf("order moved it: %s | %s", x.Key(), y.Key())
	}
}

// The model's status on a dated folder: active or dormant is code's; dead
// and shipped stand only on explicit evidence, which code checks.
func TestApplyStatus(t *testing.T) {
	dated := func() Candidate {
		return Candidate{Folder: "projects/x", Activity: activityOf("2026-10-23", "2026-10-20", "2026-10-10", "2026-09-30", "2026-09-20")}
	}
	shown := "### folder=projects/x\n  - [[x-removed]] (projects/y, history, 2026-10-01): x was removed\nowner's statement, said once (quote ledger, claude:s9, 2026-10-02): \"x is done\"\n"
	for _, c := range []struct {
		name   string
		cand   func() Candidate
		d      Decision
		status string
		code   bool
	}{
		{"dormant answer, active dates", dated, Decision{Kind: "repo", Status: "dormant", Why: "a tool"}, record.StatusActive, true},
		{"code's own answer still gets code's why", dated, Decision{Kind: "repo", Status: "active", Why: "a tool"}, record.StatusActive, true},
		{"needs-review on own commits", dated, Decision{Kind: "repo", Status: "needs-review", Why: "kind unclear"}, record.StatusActive, true},
		{"needs-review on sessions alone stands", func() Candidate {
			a := &Activity{}
			a.AddDay("2026-10-20", "session", "session:s1")
			a.Measure("2026-10-23")
			return Candidate{Folder: "projects/x", Activity: a}
		}, Decision{Kind: "repo", Status: "needs-review", Why: "kind unclear"}, record.StatusNeedsReview, false},
		{"a fold stands", dated, Decision{Kind: "repo", Status: "dead", SuppressedBy: "projects/y", Why: "part of y"}, record.StatusDead, false},
		{"none stands", dated, Decision{Kind: KindNone, Status: "dead", Why: "upstream"}, record.StatusDead, false},
		{"dead on no evidence", dated, Decision{Kind: "repo", Status: "dead", Why: "old commits"}, record.StatusActive, true},
		{"dead on a note not shown", dated, Decision{Kind: "repo", Status: "dead", Why: "note:elsewhere"}, record.StatusActive, true},
		{"dead on a shown note", dated, Decision{Kind: "repo", Status: "dead", Why: "removed, note:x-removed"}, record.StatusDead, false},
		{"dead, archived", func() Candidate {
			c := dated()
			c.Activity.Forge = &check.Activity{Archived: true}
			c.Activity.Measure("2026-10-23")
			return c
		}, Decision{Kind: "repo", Status: "dead", Why: "gone"}, record.StatusDead, false},
		{"dead, archived with a live successor", func() Candidate {
			c := dated()
			c.Activity.Forge = &check.Activity{Archived: true}
			c.Activity.Measure("2026-10-23")
			c.Ancestors = []Ancestor{{Name: "y", Folder: "projects/y"}}
			return c
		}, Decision{Kind: "repo", Status: "dead", Why: "gone"}, record.StatusDormant, true},
		{"dead, deleted", func() Candidate {
			c := dated()
			c.Activity.Deleted = true
			return c
		}, Decision{Kind: "repo", Status: "dead", Why: "gone"}, record.StatusDead, false},
		{"shipped on a release alone", func() Candidate {
			c := dated()
			c.Activity.Forge = &check.Activity{Release: "v1.0.0"}
			return c
		}, Decision{Kind: "repo", Status: "shipped", Why: "released"}, record.StatusActive, true},
		{"shipped on a release and the owner's words", func() Candidate {
			c := dated()
			c.Activity.Forge = &check.Activity{Release: "v1.0.0"}
			return c
		}, Decision{Kind: "repo", Status: "shipped", Why: "released and the owner said it is done (claude:s9)"}, record.StatusShipped, false},
		{"the owner's status", func() Candidate {
			c := dated()
			c.Old = &record.Record{Status: "dormant", Was: []record.Entry{{Status: "dormant", By: record.ByOwner}}}
			return c
		}, Decision{Kind: "repo", Status: "dormant", Why: "x"}, record.StatusDormant, false},
		{"no dates: the model's", func() Candidate { return Candidate{Folder: "projects/x"} }, Decision{Kind: "repo", Status: "dormant", Why: "x"}, record.StatusDormant, false},
	} {
		got, code := ApplyStatus(c.cand(), c.d, shown)
		if got.Status != c.status || code != c.code {
			t.Errorf("%s: %s %v (%s)", c.name, got.Status, code, got.Why)
		}
		if code && (!strings.HasPrefix(got.Why, "code: "+c.status+" from its dates") || !strings.Contains(got.Why, "What it is: "+c.d.Why)) {
			t.Errorf("%s: why %q", c.name, got.Why)
		}
	}
}
