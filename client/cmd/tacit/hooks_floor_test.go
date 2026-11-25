package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// selFloor is a floor under the scores of the tests of the rule slot, the
// share of the best, the ceiling and the derived line: they test those
// gates, not the floor.
const selFloor = 15.0

func TestPromptFloorBoundaryAndNoBackfill(t *testing.T) {
	hits := []search.Hit{
		{Name: "best", Scope: "system", Score: 35},
		{Name: "boundary", Scope: "system", Score: repoLessPromptFloor},
		{Name: "below", Scope: "system", Score: repoLessPromptFloor - 0.01},
	}
	if got := names(promptSelection(hits, nil, nil, repoLessPromptFloor)); got != "best,boundary" {
		t.Fatalf("repo-less: %s", got)
	}
	if got := names(promptSelection(hits, nil, map[string]bool{"best": true}, repoLessPromptFloor)); got != "boundary" {
		t.Fatalf("seen: %s", got)
	}
}

// The floors are where graded prompt lines stopped being mostly noise
// (eval/DELIVERY.md, "Floors from marks and labels"): a ranked line needs a
// score of 30 in a known repository, whose own rules and decisions reach the
// session by the start block and the repository boost, and 25 without one.
func TestPromptFloorsInAndOutOfARepository(t *testing.T) {
	hits := []search.Hit{
		{Name: "strong", Scope: "user", Score: 31},
		{Name: "middling", Scope: "user", Score: 28},
		{Name: "weak", Scope: "user", Score: 24},
	}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "strong" {
		t.Fatalf("in a repository: %s", got)
	}
	if got := names(promptSelection(hits, nil, nil, repoLessPromptFloor)); got != "strong,middling" {
		t.Fatalf("without one: %s", got)
	}
}

// The hook picks the floor by the session's repositories: the same fact,
// tied to no repository and scoring between the two floors, is printed
// without a repository and left out in one.
func TestPromptHookFloorFollowsTheRepository(t *testing.T) {
	var specs []fact.Meta
	for i := 0; i < 40; i++ {
		specs = append(specs, fact.Meta{Name: fmt.Sprintf("route-%d", i), Description: "endpoint api routes"})
		specs = append(specs, fact.Meta{Name: fmt.Sprintf("gw-%d", i), Description: "gateway timeouts"})
	}
	specs = append(specs, fact.Meta{Name: "ingress-note", Description: "endpoint api gateway"})
	ix := relevanceIndex(t, specs)
	ctx := context.Background()
	probe, err := ix.Query(ctx, search.Situation{Query: "endpoint api gateway", PromptRelevance: true}, 1, false)
	if err != nil || len(probe) == 0 || probe[0].Name != "ingress-note" || probe[0].Score < 25 || probe[0].Score >= 30 {
		t.Fatalf("setup: the fact must score between the floors: %+v %v", probe, err)
	}
	got, err := promptHits(ctx, ix, search.Situation{}, "endpoint api gateway", nil)
	if err != nil || names(got) != "ingress-note" {
		t.Fatalf("without a repository: %s %v", names(got), err)
	}
	got, err = promptHits(ctx, ix, search.Situation{Repo: "demo"}, "endpoint api gateway", nil)
	if err != nil || names(got) != "" {
		t.Fatalf("in a repository: %s %v", names(got), err)
	}
}
