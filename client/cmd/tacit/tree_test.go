package main

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// callTool runs one MCP tool call against tacit's server for the store at
// TACIT_STORE and returns its text.
func callTool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	ctx := context.Background()
	s, err := store.Default()
	if err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ss, err := storeServer(s, "codex").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func treeRig(t *testing.T) {
	t.Helper()
	host := store.ShortHost()
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-signoff", Description: "Every commit carries a Signed-off-by trailer", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"git commit"}}},
		fact.Meta{Name: "widget-pool-decision", Description: "The pool owns worker state", Type: "design_decision", Scope: "user", AppliesTo: fact.AppliesTo{Repos: []string{"widget"}, Paths: []string{"src/scheduler/**"}}},
		fact.Meta{Name: "widget-old-layout", Description: "The scheduler moved to src/scheduler in 2025", Type: "history", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/**"}}},
		fact.Meta{Name: "gadget-decision", Description: "Gadget keeps one queue", Type: "design_decision", Scope: "projects/gadget"},
		fact.Meta{Name: "this-host-fact", Description: "This machine has 64 GB", Type: "machine_fact", Scope: "machines/" + host},
		fact.Meta{Name: "other-host-fact", Description: "The other machine has 8 GB", Type: "machine_fact", Scope: "machines/host-zz"},
	)
	t.Chdir(cwd)
}

// tacit_search filters by scope, type and an applies_to path or command,
// with no query; it returns one line per fact and no bodies, reads another
// project's or machine's scope only when asked for it, and needs a query or
// a filter.
func TestTacitSearchFiltersTheTree(t *testing.T) {
	treeRig(t)
	got := callTool(t, "tacit_search", map[string]any{"scope": "projects/widget", "path": "src/scheduler/pool.go"})
	if !strings.Contains(got, "- widget-pool-decision: agent note (2026-10-08): The pool owns worker state") || !strings.Contains(got, "widget-old-layout") || strings.Contains(got, "widget-signoff") {
		t.Fatalf("path filter:\n%s", got)
	}
	if strings.Index(got, "widget-pool-decision") > strings.Index(got, "widget-old-layout") {
		t.Fatalf("a decision does not come before history:\n%s", got)
	}
	got = callTool(t, "tacit_search", map[string]any{"command": "git commit -s -m fix"})
	if !strings.Contains(got, "widget-signoff") || strings.Count(got, "\n- ") != 1 {
		t.Fatalf("command filter:\n%s", got)
	}
	got = callTool(t, "tacit_search", map[string]any{"type": "design_decision"})
	if !strings.Contains(got, "widget-pool-decision") || strings.Contains(got, "gadget-decision") {
		t.Fatalf("type filter, eligible scopes:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"scope": "projects/gadget", "type": "design_decision"}); !strings.Contains(got, "gadget-decision") {
		t.Fatalf("an explicit project scope:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"query": "pool worker state", "scope": "projects/widget"}); !strings.Contains(got, "widget-pool-decision") {
		t.Fatalf("query within a project scope (tied from user):\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{}); !strings.Contains(got, "give a query or a filter") {
		t.Fatalf("no query, no filter: %q", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"scope": "../etc"}); !strings.HasPrefix(got, "error: scope must be") {
		t.Fatalf("a bad scope: %q", got)
	}
}

// tacit_tree starts with the scopes this session reads and their facts by
// type (a project counts the facts tied to it from elsewhere; the fixture files
// one note in projects/widget), never another project's or machine's unless
// named as the path; tacit_search with a scope and no query lists that
// scope's facts, paged, another machine's when named.
func TestTreeCountsAndSearchListAScope(t *testing.T) {
	treeRig(t)
	got := callTool(t, "tacit_tree", map[string]any{"depth": 0})
	if !strings.Contains(got, "- projects/widget (4): design_decision 2, history 1, user_standard 1") {
		t.Fatalf("scopes:\n%s", got)
	}
	if strings.Contains(got, "projects/gadget") || strings.Contains(got, "host-zz") {
		t.Fatalf("a scope this session does not read:\n%s", got)
	}
	if got := callTool(t, "tacit_tree", map[string]any{"path": "projects/gadget", "depth": 0}); !strings.Contains(got, "- projects/gadget (1): design_decision 1") || !strings.Contains(got, "gadget-decision") {
		t.Fatalf("a named project:\n%s", got)
	}
	got = callTool(t, "tacit_search", map[string]any{"scope": "projects/widget", "limit": 2})
	if !strings.Contains(got, "1-2 of 4") || !strings.Contains(got, "more: offset 2") || !strings.Contains(got, "widget-signoff") {
		t.Fatalf("page 1:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"scope": "projects/widget", "limit": 2, "offset": 2}); !strings.Contains(got, "3-4 of 4") || !strings.Contains(got, "widget-old-layout") {
		t.Fatalf("page 2:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"scope": "projects/widget"}); !strings.Contains(got, "1-4 of 4") {
		t.Fatalf("no query, default page:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"scope": "machines/host-zz"}); !strings.Contains(got, "other-host-fact") {
		t.Fatalf("an explicit machine scope:\n%s", got)
	}
}

// A word search over the session's scopes keeps an agent's two-letter words
// (a tool named pi), weighs the session's repository's facts over the same
// match elsewhere, and ends with the tree call that lists the repository's
// facts by folder; a filtered search has no such line.
func TestTacitSearchShortWordsRepoWeightAndTreeLine(t *testing.T) {
	host := store.ShortHost()
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-pi-moved", Description: "The pi agent repo moved to example/pi; link the new one", Type: "history", Scope: "projects/widget"},
		fact.Meta{Name: "widget-retry-budget", Description: "Retries back off for at most ninety seconds", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "host-retry-budget", Description: "Retries back off for at most ninety seconds", Type: "machine_fact", Scope: "machines/" + host},
	)
	t.Chdir(cwd)
	got := callTool(t, "tacit_search", map[string]any{"query": "pi"})
	if !strings.Contains(got, "widget-pi-moved") {
		t.Fatalf("a two-letter word:\n%s", got)
	}
	got = callTool(t, "tacit_search", map[string]any{"query": "retries back off ninety seconds"})
	if strings.Index(got, "widget-retry-budget") < 0 || strings.Index(got, "widget-retry-budget") > strings.Index(got, "host-retry-budget") {
		t.Fatalf("the repo's fact does not come first:\n%s", got)
	}
	if !strings.HasSuffix(got, "# this repo's 3 facts by folder: `tacit_tree projects/widget`\n") {
		t.Fatalf("tree line:\n%s", got)
	}
	if got := callTool(t, "tacit_search", map[string]any{"query": "retries", "scope": "projects/widget"}); strings.Contains(got, "tacit_tree") {
		t.Fatalf("a filtered search points to the tree:\n%s", got)
	}
}
