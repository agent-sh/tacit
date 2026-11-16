package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// repairEnv is a machine with one checkout, widget (origin acme/widget-svc),
// and a store that knows it.
func repairEnv(t *testing.T) (validate.Env, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projects := filepath.Join(home, "projects")
	widget := filepath.Join(projects, "widget")
	put(filepath.Join(widget, ".git", "HEAD"), "ref: refs/heads/main\n")
	put(filepath.Join(widget, ".git", "config"), "[remote \"origin\"]\n\turl = git@example.com:acme/widget-svc.git\n")
	put(filepath.Join(widget, "src", "main.go"), "package main\n")
	store := filepath.Join(home, "store")
	put(filepath.Join(store, "projects", "widget", "a.md"), "x")
	return validate.NewEnv(store, "", repoid.LoadCatalog(home, "", projects), "box"), widget
}

func entryFor(desc string, scope string, applies map[string][]any) curate.Entry {
	return curate.Entry{Origin: "n1", Name: strings.ReplaceAll(strings.ToLower(desc[:10]), " ", "-"), Description: desc, Type: "design_decision", Scope: scope, Kind: "observed", AppliesTo: applies, Body: desc}
}

// The mechanical refusals that sent whole batches back to the model are
// repaired by code before the check: an absolute path in the repository's
// checkout made relative and one outside it dropped, owner/repo read as the
// repository's name and a name that is no repository dropped, a scope naming
// no known repository taken from the note, and an "Owner ruling:" label cut
// from an agent's note. What code cannot fix without a guess is still sent
// back, numbered as the model numbered it.
func TestEntryRepairFixesMechanicalRefusals(t *testing.T) {
	env, widget := repairEnv(t)
	note := &fact.Fact{Meta: fact.Meta{Name: "n1", Scope: "projects/widget", Type: "unclassified", Source: fact.Source{Harness: "dreamer", Kind: "observed"}}, Body: "notes about widget\n"}
	batch := []*fact.Fact{note}
	byName := map[string]*fact.Fact{"n1": note}
	answer := func() []curate.Entry {
		return []curate.Entry{
			entryFor("Owner ruling: never push to main", "projects/widget", map[string][]any{
				"repos": {"acme/widget", "not-a-repo-here"},
				"paths": {filepath.Join(widget, "src", "main.go"), "/elsewhere/tool/x.go", "src/ok.go"},
			}),
			entryFor("widget listens on port 7781", "projects/exp-013-g1", map[string][]any{"repos": {"Acme/Widget-Svc.git"}}),
			entryFor("tags are signed everywhere", "user", map[string][]any{"repos": {"not-a-repo-here"}}),
			entryFor("The owner wants agent sessions on the build box", "projects/widget", nil),
		}
	}
	check := entryCheck(env, "box", batch, byName, nil, false)
	// Without the repair, the first two entries are refused too.
	before := map[int]bool{}
	for _, p := range check(answer()) {
		before[p.Entry] = true
	}
	if !before[1] || !before[2] || !before[3] || !before[4] {
		t.Fatalf("the fixture's entries are not all refused without the repair: %v", before)
	}
	repair := entryRepair(env, "box", batch, byName, nil, false, false)
	entries := answer()
	kept, repaired, err := curate.RepairEntries(entries, repair, check)
	if kept != nil {
		t.Fatalf("entries 3 and 4 cannot be repaired, yet the answer passed")
	}
	got := map[int][]string{}
	for _, p := range curate.Problems(err) {
		got[p.Entry] = append(got[p.Entry], p.Field)
	}
	if want := map[int][]string{3: {"applies_to.repos"}, 4: {"description"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("left for the model: %v, want %v", got, want)
	}
	e1, e2 := entries[0], entries[1]
	if e1.Description != "Never push to main" || e1.Body != "Never push to main" {
		t.Errorf("label not cut: %q / %q", e1.Description, e1.Body)
	}
	if r := strs(e1.AppliesTo["repos"]); !reflect.DeepEqual(r, []string{"widget"}) {
		t.Errorf("entry 1 repos: %v", r)
	}
	if p := strs(e1.AppliesTo["paths"]); !reflect.DeepEqual(p, []string{"src/main.go", "src/ok.go"}) {
		t.Errorf("entry 1 paths: %v", p)
	}
	if e2.Scope != "projects/widget" || !reflect.DeepEqual(strs(e2.AppliesTo["repos"]), []string{"widget-svc"}) {
		t.Errorf("entry 2: scope %s repos %v", e2.Scope, e2.AppliesTo["repos"])
	}
	if entries[2].AppliesTo["repos"][0] != "not-a-repo-here" {
		t.Errorf("a user fact's only repository was dropped, widening it: %v", entries[2].AppliesTo["repos"])
	}
	if len(repaired) != 5 {
		t.Errorf("repaired: %v", repaired)
	}
	for _, r := range repaired {
		if s := r.String(); strings.Contains(s, "acme") || strings.Contains(s, "elsewhere") || strings.Contains(s, "push") {
			t.Errorf("a logged repair holds a value: %s", s)
		}
	}
	// Fixed by the model where code could not: the whole answer passes.
	fixed := answer()[:2]
	if kept, _, err := curate.RepairEntries(fixed, repair, check); err != nil || len(kept) != 2 {
		t.Fatalf("the repaired entries are refused: %v", err)
	}
}

// The rulings pass lists only rulings code can verify: an entry claiming the
// owner's authority with no words of theirs behind it is dropped, not sent
// back, unless cutting its label leaves an agent's note.
func TestRulingsRepairDropsUnverifiedClaims(t *testing.T) {
	env, _ := repairEnv(t)
	note := &fact.Fact{Meta: fact.Meta{Name: "n1", Scope: "projects/widget", Type: "unclassified", Source: fact.Source{Harness: "dreamer", Kind: "observed"}}, Body: "an agent's reading of the session\n"}
	batch := []*fact.Fact{note}
	byName := map[string]*fact.Fact{"n1": note}
	entries := []curate.Entry{
		entryFor("The owner said to rebase every branch before review", "projects/widget", nil),
		entryFor("Owner rule (2026-10-12): release notes go in the tag", "projects/widget", nil),
	}
	for i := range entries {
		entries[i].Kind = "user"
	}
	check := entryCheck(env, "box", batch, byName, nil, false)
	if len(check(entries)) != 2 {
		t.Fatalf("the fixture is not refused without the repair: %v", check(entries))
	}
	kept, repaired, err := curate.RepairEntries(entries, entryRepair(env, "box", batch, byName, nil, false, true), check)
	if err != nil || len(kept) != 1 || kept[0].Description != "Release notes go in the tag" {
		t.Fatalf("kept %v err %v repaired %v", kept, err, repaired)
	}
	// The split pass never drops an entry: what it cannot fix goes back.
	split := []curate.Entry{entryFor("The owner said to rebase every branch before review", "projects/widget", nil)}
	if _, _, err := curate.RepairEntries(split, entryRepair(env, "box", batch, byName, nil, false, false), check); len(curate.Problems(err)) != 1 {
		t.Fatalf("the split pass dropped or passed an unverified claim: %v", err)
	}
}

func strs(xs []any) []string {
	var out []string
	for _, x := range xs {
		out = append(out, x.(string))
	}
	return out
}
