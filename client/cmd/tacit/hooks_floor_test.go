package main

import (
	"github.com/agent-sh/tacit/client/internal/search"
	"testing"
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
