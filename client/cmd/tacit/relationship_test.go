package main

import (
	"github.com/agent-sh/tacit/client/internal/search"
	"testing"
)

func TestRelationshipSelectionKeepsNoBackfillAndDerivedIsolation(t *testing.T) {
	var hits []search.Hit
	for _, name := range []string{"first", "second", "third", "weaker"} {
		hits = append(hits, search.Hit{Name: name, Scope: "system", Score: 1, Evidence: &search.RetrievalEvidence{Supported: true}})
	}
	// Selection occurs before seen filtering, even for the evidence path.
	got := promptSelection(hits, nil, map[string]bool{"first": true, "second": true, "third": true}, 20)
	if len(got) != 0 {
		t.Fatalf("relationship path backfilled: %v", got)
	}
	got = promptSelection(hits, nil, nil, 20)
	if len(got) != 3 || got[0].Name != "first" {
		t.Fatalf("supported low-BM25 facts lost: %v", got)
	}
	derived := search.Hit{Name: "derived", Scope: "derived/repo", Score: 1, Evidence: &search.RetrievalEvidence{Supported: true}}
	got = promptSelection([]search.Hit{derived}, []search.Hit{derived}, nil, 20)
	if len(got) != 0 {
		t.Fatalf("relation evidence bypassed derived floor: %v", got)
	}
}
