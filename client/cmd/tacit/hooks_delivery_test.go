package main

import (
	"context"
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
