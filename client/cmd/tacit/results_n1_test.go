package main

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/results"
)

// Accepting a flag retires the saved fact that quoted a table or a board
// group. The result fact that replaced it must stay printable by the prompt
// hook: it is now the only fact that carries the number, and a group that was
// type result only because a saved fact quoted it must not flip to result_ref
// when that fact is retired.
func TestAcceptedFlagKeepsTheReplacementPrintable(t *testing.T) {
	r := resultsRig(t, nil)
	ctx := ctxMinute(t)
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	table := "derived/bench/" + results.FactName("bench", "RESULTS.md", "Results, localhost / Scenario") + ".md"
	if got := r.serverFile(table); !strings.Contains(got, "\ntype: result\n") {
		t.Fatalf("the table fact is printable while a saved fact quotes it:\n%s", got[:300])
	}
	if err := r.s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	var item reviewItem
	for _, it := range rs.items() {
		if it.A.Name == "bench-set-conc10" {
			item = it
		}
	}
	if item.ID == "" || item.Kind != kindResultSup {
		t.Fatalf("no flag on bench-set-conc10: %+v", item)
	}
	if _, err := rs.decide(ctx, item.ID, "right", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.serverFile("projects/bench/bench-set-conc10.md"), "superseded_by: "+item.B.Name) {
		t.Fatal("accepting did not retire the saved fact")
	}
	if _, err := resultsStep(ctx, r.s, r.resultsOpts(), false); err != nil {
		t.Fatal(err)
	}
	got := r.serverFile(table)
	if !strings.Contains(got, "\ntype: result\n") {
		t.Fatalf("the replacing fact left the hook when the saved fact was retired:\n%s", got[:300])
	}
	r.assertServerLintsClean()
}
