package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The deny matcher fires on the command a key names and on nothing that
// only looks like it: a key in a string, a comment, a here-document, an
// argument, another program or subcommand, a missing option.
func TestDenyMatcherIsPrecise(t *testing.T) {
	cases := []struct {
		key, cmd string
		want     bool
	}{
		{"gh pr merge", "gh pr merge 12 --squash", true},
		{"gh pr merge", "cd repo && gh pr merge 12", true},
		{"gh pr merge", "git fetch; gh pr merge 12", true},
		{"gh pr merge", "cat x | gh pr merge 12", true},
		{"gh pr merge", "GH_REPO=a/b gh pr merge 12", true},
		{"gh pr merge", "/usr/bin/gh pr merge 12", true},
		{"gh pr merge", "sudo -n gh pr merge 1", true},
		{"gh pr merge", "timeout 30 gh pr merge 1", true},
		{"gh pr merge", "echo $(gh pr merge 1)", true},
		{"git push --force", "git push origin main --force", true},
		{"git push --force", "git -C /src/app push --force origin", true},
		{"git push --force", "git push --force-with-lease origin", false},
		{"git push --force", "git push origin main", false},
		{"gh pr merge <n>", "gh pr merge 7", true},
		{"rm -rf", "rm -rf build", true},
		{"rm -rf", "rm -r build", false},
		// near-misses that must not fire
		{"gh pr merge", "echo gh pr merge", false},
		{"gh pr merge", `git commit -m "then gh pr merge 12"`, false},
		{"gh pr merge", "git commit -m 'x; gh pr merge 12'", false},
		{"gh pr merge", "# gh pr merge 12\ngh pr view 12", false},
		{"gh pr merge", "cat <<'EOF' > notes.md\ngh pr merge 12\nEOF\nls", false},
		{"gh pr merge", "grep -n 'gh pr merge' SPEC.md", false},
		{"gh pr merge", "gh pr view 12 --json mergeable", false},
		{"gh pr merge", "gh pr mergeable 12", false},
		{"gh pr merge", "ghx pr merge 12", false},
		{"gh pr merge", "gh pr", false},
		{"gh pr merge", "rg 'gh pr merge' docs > out.txt", false},
		{"git push", "git pushx", false},
		{"cargo test", "cargo build --tests", false},
		{"systemctl --user restart", "systemctl restart x", false},
	}
	for _, c := range cases {
		got := false
		for _, w := range denyCommands(c.cmd) {
			if denyMatch(denyKey(c.key), w) {
				got = true
			}
		}
		if got != c.want {
			t.Errorf("key %q on %q: got %v, want %v (commands %q)", c.key, c.cmd, got, c.want, denyCommands(c.cmd))
		}
	}
}

// A key many facts share is no situation: the table keeps only the keys at
// most denyKeyShare facts carry, of rules, standards and past mistakes.
func TestDenyTableKeepsSpecificKeysOfRulesAndMistakes(t *testing.T) {
	mk := func(name, typ, cmd string, ruling bool) search.Hit {
		return search.Hit{Name: name, Type: typ, Scope: "user", Ruling: ruling, Description: name + " text", Applies: fact.AppliesTo{Commands: []string{cmd}}}
	}
	all := []search.Hit{
		mk("a", "past_mistake", "git push", false), mk("b", "past_mistake", "git push", false), mk("c", "past_mistake", "git push", false),
		mk("d", "past_mistake", "make deploy-prod", false),
		mk("e", "reference", "make release", false),
		mk("f", "user_preference", "gh release create", true),
		mk("g", "design_decision", "kubectl apply", false),
	}
	tab := denyTableOf(all, time.Now())
	var names []string
	for _, f := range tab.Facts {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "d,f" {
		t.Fatalf("table: %v", names)
	}
}

// A fact fits the session: its repository's, or the user's, or this
// machine's; not another project's or another machine's.
func TestDenyScopeFits(t *testing.T) {
	sc := denyScope{repos: []string{"widget"}, host: "box1"}
	for _, c := range []struct {
		f    denyFact
		want bool
	}{
		{denyFact{Scope: "user"}, true},
		{denyFact{Scope: "projects/widget"}, true},
		{denyFact{Scope: "projects/gadget"}, false},
		{denyFact{Scope: "user", Repos: []string{"gadget"}}, false},
		{denyFact{Scope: "projects/gadget", Repos: []string{"widget"}}, true},
		{denyFact{Scope: "machines/box1"}, true},
		{denyFact{Scope: "machines/box2"}, false},
		{denyFact{Scope: "user", Hosts: []string{"box2"}}, false},
		{denyFact{Scope: "orgs/acme"}, false},
	} {
		if got := sc.fits(c.f); got != c.want {
			t.Errorf("%+v: got %v", c.f, got)
		}
	}
}

func preToolDeny(t *testing.T, out string) string {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var v struct {
		HookSpecificOutput struct {
			HookEventName      string `json:"hookEventName"`
			PermissionDecision string `json:"permissionDecision"`
			Reason             string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "PreToolUse" || v.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("not a PreToolUse deny: %v %q", err, out)
	}
	return v.HookSpecificOutput.Reason
}

// The PreToolUse hook denies a command a rule is keyed to once a session,
// with the rule's text, and lets the retry run; an edit, another command,
// another session's retry and a throwaway harness's session are not
// stopped, and no hook delivers context before a tool any more.
func TestPreToolDeniesAKeyedCommandOnce(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "deploy-needs-tag", Description: "Tag the release before make deploy-prod: the deploy reads the tag", Type: "past_mistake", Scope: "projects/widget",
			AppliesTo: fact.AppliesTo{Commands: []string{"make deploy-prod"}, Paths: []string{"deploy/**"}}},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	bash := func(session, cmd string) string {
		return hookOut(t, toolPayload(cwd, session, "Bash", map[string]any{"command": cmd}), "pre-tool", "--mcp")
	}
	reason := preToolDeny(t, bash("s1", "make deploy-prod"))
	if !strings.Contains(reason, "deploy-needs-tag") || !strings.Contains(reason, "Tag the release") || !strings.Contains(reason, "run it again unchanged") {
		t.Fatalf("deny: %q", reason)
	}
	if out := bash("s1", "make deploy-prod"); out != "" {
		t.Fatalf("the retry was stopped: %q", out)
	}
	if out := bash("s1", "echo make deploy-prod"); out != "" {
		t.Fatalf("an argument: %q", out)
	}
	if out := hookOut(t, toolPayload(cwd, "s2", "Edit", map[string]any{"file_path": filepath.Join(cwd, "deploy/run.sh")}), "pre-tool"); out != "" {
		t.Fatalf("an edit: %q", out)
	}
	if preToolDeny(t, bash("s3", "make deploy-prod")) == "" {
		t.Fatal("another session is stopped once too")
	}
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	scratch, _ := json.Marshal(map[string]any{"session_id": "s4", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": "make deploy-prod"},
		"transcript_path": filepath.Join(cacheDir, "run", "t.jsonl")})
	if out := hookOut(t, string(scratch), "pre-tool"); out != "" {
		t.Fatalf("a throwaway harness's session: %q", out)
	}
	// The deny is given: logged, and owed a mark at the turn's Stop.
	var denies int
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "deny" && len(e.Cands) == 1 && e.Cands[0].Name == "deploy-needs-tag" && e.Cands[0].Delivered && e.Cands[0].Via == "deny" {
			denies++
		}
	}
	if denies != 2 {
		t.Fatalf("deny entries: %d", denies)
	}
	if b, _ := os.ReadFile(owedFile("s1")); !strings.HasPrefix(string(b), "deploy-needs-tag\twidget") {
		t.Fatalf("owed: %q", b)
	}
}
