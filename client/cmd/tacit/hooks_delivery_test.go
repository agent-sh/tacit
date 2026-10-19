package main

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The index stems: a request in other forms of a fact's words finds it,
// and two forms of one word are one match, not the two the gate asks for.
func TestPromptStems(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "cr-amend-duplicate", Description: "Running cr again after you amend a commit opens a duplicate review"},
		{Name: "deploy-freeze", Description: "the deployment freeze holds until friday"},
	})
	got, err := promptHits(context.Background(), ix, search.Situation{}, "amended two commits and the reviews got duplicated", nil)
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "cr-amend-duplicate" {
		t.Errorf("inflected request: %s", names(got))
	}
	got, err = promptHits(context.Background(), ix, search.Situation{}, "deployment deployments tomorrow", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("one word in two forms passed the two-match gate: %s", names(got))
	}
}

// ruleSpecs: three observed facts that match "widget cache eviction" well
// and an owner ruling that matches it less, so it ranks fourth.
func ruleSpecs() []fact.Meta {
	return []fact.Meta{
		{Name: "widget-cache-eviction-lru", Description: "widget cache eviction is lru with a widget cache size cap"},
		{Name: "widget-cache-eviction-ttl", Description: "widget cache eviction ttl defaults to one hour for the widget cache"},
		{Name: "widget-cache-eviction-metrics", Description: "widget cache eviction metrics are on the widget cache dashboard"},
		{Name: "keep-eviction-off-weekends", Description: "Never change eviction settings on a weekend", Source: fact.Source{Kind: "user"}},
		{Name: "faint-ruling", Description: "Ask before touching a widget", Source: fact.Source{Kind: "user"}},
	}
}

func TestPromptRuleSlot(t *testing.T) {
	ix := relevanceIndex(t, ruleSpecs())
	sit := search.Situation{Repo: "demo"}
	ctx := context.Background()
	p := "widget cache eviction settings"
	got, err := promptHits(ctx, ix, sit, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != promptLimit+1 || got[promptLimit].Name != "keep-eviction-off-weekends" {
		t.Fatalf("the ruling must be one line after the three facts: %s", names(got))
	}
	for _, h := range got[:promptLimit] {
		if h.Ruling {
			t.Fatalf("setup: a ruling is already in the top three: %s", names(got))
		}
	}

	// Once per session, and no other ruling takes the slot it held.
	seen := map[string]bool{"keep-eviction-off-weekends": true}
	got, err = promptHits(ctx, ix, sit, p, seen)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != promptLimit {
		t.Fatalf("a seen ruling came back or was backfilled: %s", names(got))
	}

	// A block that already has a ruling gets no extra line.
	got, err = promptHits(ctx, ix, sit, "eviction settings weekend widget", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > promptLimit || !strings.Contains(names(got), "keep-eviction-off-weekends") {
		t.Fatalf("ruling in the top three: %s", names(got))
	}
}

func TestPromptRuleSlotFloor(t *testing.T) {
	// The only ruling that matches scores under ruleSlotFloor: no slot line.
	hits := []search.Hit{
		{Name: "a", Score: 30}, {Name: "b", Score: 25}, {Name: "c", Score: 20},
		{Name: "weak-rule", Score: ruleSlotFloor - 0.1, Ruling: true},
	}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,b,c" {
		t.Fatalf("weak ruling added: %s", got)
	}
	hits[3].Score = ruleSlotFloor
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,b,c,weak-rule" {
		t.Fatalf("ruling at the slot floor: %s", got)
	}
	// Derived facts are never in the slot; a ruling under the floor that the
	// selection already holds through relationship evidence is not doubled.
	hits = []search.Hit{{Name: "a", Score: 30}, {Name: "r", Score: 11, Ruling: true, Evidence: &search.RetrievalEvidence{Supported: true}}}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,r" {
		t.Fatalf("supported ruling: %s", got)
	}
}

// The slot takes the best ruling only: with two that qualify, a seen best
// one is not replaced by the next, and a ruling in the three (seen or not)
// leaves no slot at all.
func TestPromptRuleSlotTwoRulings(t *testing.T) {
	hits := []search.Hit{
		{Name: "a", Score: 30}, {Name: "b", Score: 25}, {Name: "c", Score: 20},
		{Name: "r1", Score: 14, Ruling: true}, {Name: "r2", Score: 12, Ruling: true},
	}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,b,c,r1" {
		t.Fatalf("best ruling: %s", got)
	}
	if got := names(promptSelection(hits, nil, map[string]bool{"r1": true}, promptFloor)); got != "a,b,c" {
		t.Fatalf("the slot was refilled after its ruling was seen: %s", got)
	}
	inThree := []search.Hit{
		{Name: "a", Score: 30}, {Name: "r0", Score: 25, Ruling: true}, {Name: "c", Score: 20},
		{Name: "r1", Score: 14, Ruling: true},
	}
	if got := names(promptSelection(inThree, nil, nil, promptFloor)); got != "a,r0,c" {
		t.Fatalf("a ruling in the three got a second: %s", got)
	}
	if got := names(promptSelection(inThree, nil, map[string]bool{"r0": true}, promptFloor)); got != "a,c" {
		t.Fatalf("a seen ruling in the three still counts: %s", got)
	}
}
