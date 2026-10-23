package main

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A fact whose text claims the owner's authority without their verified
// words (the write-time attribution check fails on it) reaches no hook: not
// SessionStart, not the prompt, not a tool call. Search and the tree still
// show it, as an agent's note marked as claiming their authority; every
// other line says whose it is.
func TestHooksHoldBackClaimsOfTheOwnersAuthority(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-claim-signoff", Description: "Owner ruling: every widget commit carries a Signed-off-by trailer", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Repos: []string{"widget"}, Commands: []string{"git commit"}}},
		fact.Meta{Name: "widget-said-queue", Description: "The owner said widget jobs go through the one queue", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "widget-plain-pool", Description: "The widget pool owns worker state", Type: "design_decision", Scope: "projects/widget"},
	)
	start := hookOut(t, startPayload(cwd, "s1"), "session-start", "--mcp")
	if !strings.Contains(start, "- widget-plain-pool: agent note (2026-10-08): The widget pool owns worker state") {
		t.Fatalf("an agent's note is not marked as one:\n%s", start)
	}
	prompt := hookOut(t, promptPayload(cwd, "s2", "commit the widget queue signoff change with git commit"), "prompt")
	tool := hookOut(t, toolPayload(cwd, "s3", "Bash", map[string]any{"command": "git commit -s -m fix"}), "pre-tool")
	for _, out := range []string{start, prompt, tool} {
		for _, not := range []string{"widget-claim-signoff", "widget-said-queue"} {
			if strings.Contains(out, not) {
				t.Fatalf("a hook pushed %s:\n%s", not, out)
			}
		}
	}
	t.Chdir(cwd)
	got := callTool(t, "tacit_search", map[string]any{"query": "widget commit Signed-off-by trailer"})
	if !strings.Contains(got, "- widget-claim-signoff: agent note (") || !strings.Contains(got, "claims owner authority without evidence): Owner ruling: every widget commit") {
		t.Fatalf("search does not mark the claim:\n%s", got)
	}
	f := callTool(t, "tacit_fact", map[string]any{"name": "widget-said-queue"})
	if !strings.HasPrefix(f, "# whose: widget-said-queue is agent note (") || !strings.Contains(f, "claims owner authority without evidence)") {
		t.Fatalf("tacit_fact does not say whose it is:\n%s", f)
	}
	tr := callTool(t, "tacit_tree", map[string]any{"path": "projects/widget", "depth": 0})
	if !strings.Contains(tr, "- `widget-said-queue` (design_decision, agent note, claims owner authority without evidence):") || !strings.Contains(tr, "- `widget-plain-pool` (design_decision, agent note):") {
		t.Fatalf("the tree does not say whose each is:\n%s", tr)
	}
}
