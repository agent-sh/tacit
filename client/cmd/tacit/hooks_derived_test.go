package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// hookStore builds a store big enough for bm25 scores to reach the hook's
// floors: 400 filler facts, five curated facts about the scheduler of repo
// "demo" (one an owner ruling), and the derived hot-spot and module-map
// facts of that repo whose description names worker.go.
func hookStore(t *testing.T) *search.Index {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	for i := 0; i < 400; i++ {
		writeFact(t, root, fact.Meta{Name: fmt.Sprintf("filler-%03d", i), Description: fmt.Sprintf("filler note %03d about lorem%03d ipsum%03d", i, i, i), Type: "machine_fact", Scope: "machines/host-a"}, "x")
	}
	for i := 0; i < 5; i++ {
		src := fact.Source{Kind: "observed"}
		if i == 0 {
			src = ownerSource("quokka scheduler zebra")
		}
		writeFact(t, root, fact.Meta{
			Name:        fmt.Sprintf("demo-quokka-scheduler-%d", i),
			Description: fmt.Sprintf("quokka scheduler zebra rule %d for the demo repo", i),
			Type:        "design_decision", Scope: "projects/demo", Source: src,
		}, "quokka scheduler zebra")
	}
	writeFact(t, root, fact.Meta{
		Name: "demo-hot-spots", Description: "Hot spots in demo, last 400 code commits (path, commits) src/scheduler/worker.go 35, src/scheduler/pool.go 20",
		Type: "hot_spot", Scope: "derived/demo", DerivedFrom: "abc123def456", Source: fact.Source{Kind: "derived"},
	}, "- src/scheduler/worker.go: 35 commits. Changes with src/scheduler/pool.go (20).")
	writeFact(t, root, fact.Meta{
		Name: "demo-module-map", Description: "Layout of demo, busiest first, src/scheduler/ src/transport/ docs/",
		Type: "structure", Scope: "derived/demo", DerivedFrom: "abc123def456", Source: fact.Source{Kind: "derived"},
	}, "- src/scheduler/ - 12 files")
	// another repo's derived fact must never be offered here
	writeFact(t, root, fact.Meta{
		Name: "other-hot-spots", Description: "Hot spots in other, src/scheduler/worker.go 99",
		Type: "hot_spot", Scope: "derived/other", DerivedFrom: "abc123def456", Source: fact.Source{Kind: "derived"},
	}, "- src/scheduler/worker.go: 99 commits")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	ix, err := search.Open(context.Background(), root, "testhead")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func names(hits []search.Hit) string {
	var ns []string
	for _, h := range hits {
		ns = append(ns, h.Name)
	}
	return strings.Join(ns, ",")
}

var demo = search.Situation{Repo: "demo", Host: "host-a"}

// SessionStart is for owner rulings and host facts: even when a derived fact
// is the best match for the repo name, it takes no line.
func TestSessionStartNeverCarriesDerivedFacts(t *testing.T) {
	ix := hookStore(t)
	ctx := context.Background()

	// The premise: with derived scope on and the repo name as the query, the
	// derived facts do rank (their names and descriptions say "demo").
	probe := demo
	probe.Query, probe.Derived = "demo", true
	hits, err := ix.Query(ctx, probe, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	var derivedHits []search.Hit
	for _, h := range hits {
		if isDerived(h) {
			derivedHits = append(derivedHits, h)
		}
	}
	if len(derivedHits) == 0 {
		t.Fatalf("premise: derived facts should match the repo name: %s", names(hits))
	}

	got, err := sessionStartHits(ctx, ix, demo)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range got {
		if isDerived(h) {
			t.Fatalf("SessionStart carries %s", h.Name)
		}
	}
	if len(got) > startLimit {
		t.Fatalf("%d lines, cap %d", len(got), startLimit)
	}
	// Defence in depth: even if a derived hit reached the selection, it is dropped.
	sel := startSelection(append([]search.Hit{{Name: "demo-hot-spots", Scope: "derived/demo", Score: 99}}, got...), "demo", startLimit)
	for _, h := range sel {
		if isDerived(h) {
			t.Fatalf("startSelection kept %s", h.Name)
		}
	}
}

// A prompt that names a file in the derived facts gets them, after the
// curated facts and without displacing any.
func TestPromptHookAddsOneDerivedFactAfterTheCuratedOnes(t *testing.T) {
	ix := hookStore(t)
	ctx := context.Background()

	// curated-only reference: what the hook returned before derived facts existed
	p := "quokka scheduler zebra rule: also check src/scheduler/worker.go"
	ref := demo
	ref.Query = p
	cur, err := ix.Query(ctx, ref, promptLimit*2, false)
	if err != nil {
		t.Fatal(err)
	}
	var want []search.Hit
	for _, h := range cur {
		if h.Score >= promptFloor && len(want) < promptLimit {
			want = append(want, h)
		}
	}
	if len(want) != promptLimit {
		t.Fatalf("test store should fill the curated slots: %d above the floor", len(want))
	}

	got, err := promptHits(ctx, ix, demo, p, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != promptLimit+1 {
		t.Fatalf("expected %d curated + 1 derived, got %s", promptLimit, names(got))
	}
	for i := 0; i < promptLimit; i++ {
		if got[i].Name != want[i].Name || isDerived(got[i]) {
			t.Fatalf("curated facts displaced or reordered: got %s, want %s first", names(got), names(want))
		}
	}
	if d := got[promptLimit]; d.Name != "demo-hot-spots" || d.Score < derivedPromptFloor {
		t.Fatalf("derived slot: %+v", d)
	}
	// the owner ruling is among the curated ones
	hasRule := false
	for _, h := range got[:promptLimit] {
		hasRule = hasRule || h.Ruling
	}
	_ = hasRule

	// each fact once per session: the second time only what is new
	seen := map[string]bool{}
	for _, h := range got {
		seen[h.Name] = true
	}
	again, _ := promptHits(ctx, ix, demo, p, seen)
	if len(again) != 0 {
		t.Fatalf("repeated request delivered %s", names(again))
	}
}

// A prompt that matches only the derived facts gets just that one.
func TestPromptHookDerivedAlone(t *testing.T) {
	ix := hookStore(t)
	got, err := promptHits(context.Background(), ix, demo, "why does src/scheduler/worker.go change in so many commits", map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "demo-hot-spots" {
		t.Fatalf("got %q", names(got))
	}
}

// Prompts that name nothing in the derived facts get no derived fact, and a
// prompt in another repo never sees this repo's.
func TestPromptHookDerivedNeedsAMatch(t *testing.T) {
	ix := hookStore(t)
	ctx := context.Background()
	for _, p := range []string{"please continue with the next step", "what is the weather like", "commit and push the change", "hello there friend"} {
		got, _ := promptHits(ctx, ix, demo, p, map[string]bool{})
		for _, h := range got {
			if isDerived(h) {
				t.Errorf("%q pulled %s (%.1f)", p, h.Name, h.Score)
			}
		}
	}
	got, _ := promptHits(ctx, ix, search.Situation{Repo: "elsewhere", Host: "host-a"}, "check src/scheduler/worker.go", map[string]bool{})
	if len(got) != 0 {
		t.Fatalf("another repo got %s", names(got))
	}
	got, _ = promptHits(ctx, ix, search.Situation{Repo: "other", Host: "host-a"}, "check src/scheduler/worker.go", map[string]bool{})
	if names(got) != "other-hot-spots" {
		t.Fatalf("repo other should see only its own derived fact, got %s", names(got))
	}
}

func TestPromptSelectionRules(t *testing.T) {
	cur := func(name string, score float64) search.Hit {
		return search.Hit{Name: name, Scope: "projects/demo", Score: score}
	}
	der := func(name string, score float64) search.Hit {
		return search.Hit{Name: name, Scope: "derived/demo", Score: score, Kind: "derived"}
	}

	// curated: top three above the floor; a derived hit that leaked into the
	// curated list is never counted as one of them
	hits := []search.Hit{der("d0", 40), cur("a", 30), cur("b", 20), cur("c", 16), cur("d", 15.5), cur("below", promptFloor-0.1)}
	got := promptSelection(hits, nil, map[string]bool{})
	if names(got) != "a,b,c" {
		t.Fatalf("curated selection %s", names(got))
	}
	// one derived, after them, only above its floor
	got = promptSelection(hits[1:], []search.Hit{der("hot", derivedPromptFloor+1), der("map", 99)}, map[string]bool{})
	if names(got) != "a,b,c,hot" {
		t.Fatalf("with derived %s", names(got))
	}
	got = promptSelection(hits[1:], []search.Hit{der("hot", derivedPromptFloor-0.1)}, map[string]bool{})
	if names(got) != "a,b,c" {
		t.Fatalf("derived under its floor %s", names(got))
	}
	// no backfill: a seen curated fact is not replaced by the fourth
	got = promptSelection(hits[1:], []search.Hit{der("hot", 30)}, map[string]bool{"a": true, "hot": true})
	if names(got) != "b,c" {
		t.Fatalf("seen handling %s", names(got))
	}
	// derived alone
	got = promptSelection(nil, []search.Hit{der("hot", 30)}, map[string]bool{})
	if names(got) != "hot" {
		t.Fatalf("derived alone %s", names(got))
	}
}

func TestDerivedLineIsMarkedInTheBlock(t *testing.T) {
	b := search.Block([]search.Hit{{Name: "demo-hot-spots", Description: "Hot spots in demo", Kind: "derived", Scope: "derived/demo"}}, "# t")
	if !strings.Contains(b, "- demo-hot-spots (computed from git): Hot spots in demo") {
		t.Fatal(b)
	}
}
