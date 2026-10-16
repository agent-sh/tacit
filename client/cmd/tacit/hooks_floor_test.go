package main

import (
	"github.com/agent-sh/tacit/client/internal/search"
	"testing"
)

func TestPromptFloorBoundaryAndNoBackfill(t *testing.T) {
	hits := []search.Hit{
		{Name: "best", Scope: "system", Score: 25},
		{Name: "boundary", Scope: "system", Score: repoLessPromptFloor},
		{Name: "below", Scope: "system", Score: repoLessPromptFloor - 0.01},
	}
	if got := names(promptSelection(hits, nil, nil, repoLessPromptFloor)); got != "best,boundary" {
		t.Fatalf("repo-less: %s", got)
	}
	if got := names(promptSelection(hits, nil, nil, promptFloor)); got != "best,boundary,below" {
		t.Fatalf("project: %s", got)
	}
	if got := names(promptSelection(hits, nil, map[string]bool{"best": true}, repoLessPromptFloor)); got != "boundary" {
		t.Fatalf("seen: %s", got)
	}
}
