package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	releaseCaps(t)
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
	releaseCaps(t)
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

// A ruling whose quote was an instruction inside its task ("lets clear the
// widget cache eviction settings now") is their ruling, guarded as one, but
// delivered as an ordinary fact: in the three it leaves the rule slot open
// for a standing rule, it is no RULE line, and SessionStart does not put it
// first.
func TestInTaskRulingIsAnOrdinaryFact(t *testing.T) {
	quotes := map[string]string{
		"clear-widget-cache-now":     "lets clear the widget cache eviction settings now",
		"keep-eviction-off-weekends": "never change eviction settings on a weekend",
	}
	specs := []fact.Meta{
		{Name: "widget-cache-eviction-lru", Description: "widget cache eviction is lru with a widget cache size cap"},
		{Name: "widget-cache-eviction-ttl", Description: "widget cache eviction ttl defaults to one hour for the widget cache"},
		{Name: "clear-widget-cache-now", Description: "Owner 2026-10-01: lets clear the widget cache eviction settings now"}, // privacy-lint: allow (synthetic attribution)
		{Name: "keep-eviction-off-weekends", Description: "Never change eviction settings on a weekend"},
	}
	var ledger strings.Builder
	for i, m := range specs {
		if q := quotes[m.Name]; q != "" {
			specs[i].Source = fact.Source{Kind: "user", OwnerQuote: q, OwnerQuoteFrom: "claude:s1"}
			fmt.Fprintf(&ledger, `{"name":%q,"quote":%q,"from":"claude:s1","turn_hash":"h","at":"2026-10-01"}`+"\n", m.Name, q)
		}
	}
	root := relevanceStore(t, specs)
	os.MkdirAll(filepath.Join(root, "review"), 0o755)
	os.WriteFile(filepath.Join(root, "review", "rulings-audit.json"), []byte(`{"version":1,"audited_head":"x","at":"t"}`), 0o644)
	os.WriteFile(filepath.Join(root, "review", "owner-quotes.jsonl"), []byte(ledger.String()), 0o644)
	ix := openRelevance(t, root)
	ctx := context.Background()

	got, err := promptHits(ctx, ix, search.Situation{}, "widget cache eviction settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names(got[:min(len(got), promptLimit)]), "clear-widget-cache-now") {
		t.Fatalf("setup: the in-task ruling is not in the three: %s", names(got))
	}
	if len(got) != promptLimit+1 || got[promptLimit].Name != "keep-eviction-off-weekends" {
		t.Fatalf("the standing rule must take the slot: %s", names(got))
	}
	for _, h := range got {
		if h.Name == "clear-widget-cache-now" && (!h.Ruling || !h.InTask || h.Rule()) {
			t.Fatalf("in-task ruling indexed as %+v", h)
		}
	}
	block := search.Block(got, "facts:")
	if strings.Contains(block, "RULE clear-widget-cache-now") || !strings.Contains(block, "- RULE keep-eviction-off-weekends") {
		t.Fatalf("block:\n%s", block)
	}

	start := startSelection([]search.Hit{
		{Name: "host-fact", Scope: "machines/host-a", Score: 9},
		{Name: "repo-fact", Scope: "projects/demo", Score: 9},
		{Name: "clear-widget-cache-now", Scope: "system", Ruling: true, InTask: true, Score: 8},
		{Name: "keep-eviction-off-weekends", Scope: "system", Ruling: true, Score: 2},
	}, "", startLimit)
	if names(start) != "keep-eviction-off-weekends,clear-widget-cache-now,repo-fact,host-fact" {
		t.Fatalf("start order: %s", names(start))
	}
}

// releaseCaps pins the prompt block to the release's fixed cap of three with
// no relative threshold, for tests of what the cap leaves (the rule slot,
// the derived line, no backfill), not of the cap.
func releaseCaps(t *testing.T) {
	t.Helper()
	t.Setenv("TACIT_DELIVERY_PROMPT_CEILING", "3")
	t.Setenv("TACIT_DELIVERY_PROMPT_RELATIVE", "0")
}

// The prompt block takes the curated facts above the floor that score at
// least prompt_relative of the best one, up to prompt_ceiling: a weak tail
// does not ride on a strong head, and an even field fills to the ceiling.
func TestPromptSelectionThresholdAndCeiling(t *testing.T) {
	hits := []search.Hit{{Name: "a", Score: 40}, {Name: "b", Score: 30}, {Name: "c", Score: 27.9}, {Name: "d", Score: 26}}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,b" {
		t.Fatalf("0.7 of 40 is 28: %s", got)
	}
	even := []search.Hit{{Name: "a", Score: 30}, {Name: "b", Score: 29}, {Name: "c", Score: 28}, {Name: "d", Score: 27}, {Name: "e", Score: 26}, {Name: "f", Score: 25}}
	if got := names(promptSelection(even, nil, nil, promptFloor)); got != "a,b,c,d,e" {
		t.Fatalf("ceiling 5: %s", got)
	}
	t.Setenv("TACIT_DELIVERY_PROMPT_RELATIVE", "0")
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "a,b,c,d" {
		t.Fatalf("no threshold: %s", got)
	}
	t.Setenv("TACIT_DELIVERY_PROMPT_CEILING", "2")
	if got := names(promptSelection(even, nil, nil, promptFloor)); got != "a,b" {
		t.Fatalf("ceiling 2: %s", got)
	}
}

// A request of fewer than 3 distinct words gets no rule slot; the ranked
// lines stay. One of 3 words or more keeps the slot.
func TestPromptShortRequestHasNoRuleSlot(t *testing.T) {
	releaseCaps(t)
	hits := []search.Hit{
		{Name: "a", Score: 30}, {Name: "b", Score: 25}, {Name: "c", Score: 20},
		{Name: "r1", Score: 14, Ruling: true},
	}
	for p, want := range map[string]string{
		"deploy now?":              "a,b,c",
		"deploy the widget now":    "a,b,c,r1",
		"the deploy, the deploys?": "a,b,c",
	} {
		if got := names(promptSelectionFor(p, hits, nil, nil, promptFloor)); got != want {
			t.Errorf("%q: %s, want %s", p, got, want)
		}
	}
	for p, want := range map[string]bool{
		"deploy now":            true,
		"deploy the widget now": false,
		"déploie ça maintenant": true,  // a word of 2 letters counts for no script
		"fix src/main.go":       false, // a path's parts are words
	} {
		if shortRequest(p) != want {
			t.Errorf("shortRequest(%q) = %v", p, !want)
		}
	}
}
