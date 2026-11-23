package curate

import (
	"reflect"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Near-copies: three wordings of one rule in one folder share too few terms
// for the description rule, so Duplicates keeps them all; the near pass
// puts them in one cluster, and the model's set retires two of them into
// the one holding the owner's words. A fact of another folder and a fact on
// another subject are in no cluster with them.
func TestNearCopiesKeepTheOwnersWordsThenTheEarliest(t *testing.T) {
	const ws = "areas/render-farm"
	facts := []*fact.Fact{
		cf(ws, "farm-never-leave-nodes-idle", "2026-10-05", "On render farm work never leave the nodes idle; the farm is the costliest resource"),
		cf(ws, "keep-farm-nodes-busy", "2026-10-01", "Never leave render farm nodes idle: start the next render job at once"),
		ruling(cf(ws, "farm-nodes-idle-rule", "2026-10-09", "Render farm nodes never sit idle between render jobs"), "never let the render farm nodes sit idle between jobs"),
		cf(ws, "farm-billing-portal", "2026-10-02", "The render farm billing portal shows the monthly invoice per node"),
		cf("areas/other-farm", "other-farm-nodes-idle", "2026-10-03", "On render farm work never leave the nodes idle between render jobs"),
	}
	if got := copiesOf(Duplicates(facts, DedupeGuard{})); len(got) != 0 {
		t.Fatalf("the description rule already merges them: %v", got)
	}
	cs := NearCandidates(facts)
	if len(cs) != 1 || len(cs[0].Facts) != 3 || cs[0].Scope != ws {
		var names [][]string
		for _, c := range cs {
			var n []string
			for _, f := range c.Facts {
				n = append(n, f.Meta.Name)
			}
			names = append(names, n)
		}
		t.Fatalf("clusters: %v", names)
	}
	// The model's answer, by fact number in the cluster's order (paths).
	idx := map[string]int{}
	for i, f := range cs[0].Facts {
		idx[f.Meta.Name] = i + 1
	}
	answer := `{"groups": [{"group": 1, "same": [[` + itoa(idx["farm-never-leave-nodes-idle"]) + `, ` + itoa(idx["keep-farm-nodes-busy"]) + `, ` + itoa(idx["farm-nodes-idle-rule"]) + `]]}]}`
	v, err := ParseNear("Here you go:\n"+answer, cs)
	if err != nil {
		t.Fatal(err)
	}
	got := copiesOf(NearCopies(cs, v, DedupeGuard{}))
	want := map[string][]string{"farm-nodes-idle-rule": {"farm-never-leave-nodes-idle", "keep-farm-nodes-busy"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NearCopies = %v, want %v", got, want)
	}
	// With no ruling among them, the earliest is kept; a fact the owner kept
	// in a decision is never a copy.
	facts[2] = cf(ws, "farm-nodes-idle-rule", "2026-10-09", "Render farm nodes never sit idle between render jobs")
	cs = NearCandidates(facts)
	v, _ = ParseNear(answer, cs)
	got = copiesOf(NearCopies(cs, v, DedupeGuard{Kept: map[string]bool{"farm-never-leave-nodes-idle": true}}))
	want = map[string][]string{"keep-farm-nodes-busy": {"farm-nodes-idle-rule"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NearCopies without a ruling = %v, want %v", got, want)
	}
	// No verdict, no copy; an answer naming a fact the group lacks is refused.
	if got := NearCopies(cs, nil, DedupeGuard{}); len(got) != 0 {
		t.Fatalf("copies with no verdict: %v", got)
	}
	if _, err := ParseNear(`{"groups": [{"group": 1, "same": [[1, 9]]}]}`, cs); err == nil {
		t.Fatal("a fact the group lacks was read")
	}
	if _, err := ParseNear(`{"groups": [{"group": 1, "same": [[1, 2], [2, 3]]}]}`, cs); err == nil {
		t.Fatal("a fact in two sets was read")
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
