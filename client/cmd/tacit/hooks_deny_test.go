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
		{"gh pr merge", "x=\"$(gh pr merge 3)\"", true},
		{"gh pr merge", "claude-ctrl send \"$(list --json | python3 -c \"\nimport sys\ngh pr merge 1\")\"", false},
		{"import", "claude-ctrl send \"$(list --json | python3 -c \"\nimport json,sys\nprint(1)\")\"", false},
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

// A command denies only through a fact that forbids it as a whole: a rule,
// standard or past mistake whose name or description puts a negation right
// before the key's words and ends the clause after them. A mistake about
// how or when a command fails, a key of one word, a reference and a
// decision deny nothing.
func TestDenyTableKeepsForbiddenCommands(t *testing.T) {
	mk := func(name, typ, desc, cmd string, ruling bool) search.Hit {
		return search.Hit{Name: name, Type: typ, Scope: "user", Ruling: ruling, Description: desc, Applies: fact.AppliesTo{Commands: []string{cmd}}}
	}
	all := []search.Hit{
		mk("a", "past_mistake", "Never `gh pr merge`: land by a fast-forward push", "gh pr merge", false),
		mk("b", "user_standard", "Do not git push --force; open a PR", "git push --force", false),
		mk("b2", "user_standard", "Do not git push --force to a shared branch", "git push --force", false),
		mk("no-sed-for-jsx-edits", "past_mistake", "sed breaks JSX", "sed", false),
		mk("no-sed-i-for-jsx", "past_mistake", "Use the editor", "sed -i", false),
		mk("c", "past_mistake", "sort needs LC_ALL=C before comm", "sort", false),
		mk("d", "past_mistake", "`git rev-parse --short A B` fails: it takes one revision", "git rev-parse --short", false),
		mk("no-make-release", "past_mistake", "The release target publishes", "make release", false),
		mk("e", "reference", "Never make deploy by hand", "make deploy", false),
		mk("f", "user_preference", "Use the release script instead of gh release create", "gh release create", true),
		mk("g", "design_decision", "Never kubectl apply by hand", "kubectl apply", false),
		mk("h", "past_mistake", "A rule says never to retry; then npm publish ran", "npm publish", false),
		mk("i", "past_mistake", "Never pipe `git remote -v`, `git branch` through head", "git remote -v", false),
		mk("j", "past_mistake", "It needs `go mod download all`, not plain `go mod download`; test it", "go mod download", false),
		mk("sparks-avoid-hf-download", "past_mistake", "Fetch with the resumable script", "hf download", false),
		mk("never-npm-publish", "past_mistake", "CI publishes", "npm publish", false),
		mk("k", "user_standard", "Do not run `cargo publish`.", "cargo publish", false),
	}
	tab := denyTableOf(all, time.Now())
	if o := only("In gadget, never git push --force; ask"); o != "gadget" {
		t.Fatalf("only: %q", o)
	}
	var names []string
	for _, f := range tab.Facts {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a,b,no-make-release,f,never-npm-publish,k" {
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
		{denyFact{Scope: "user", Only: "gadget"}, false},
		{denyFact{Scope: "user", Only: "widget"}, true},
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
		fact.Meta{Name: "deploy-needs-tag", Description: "Never run `make deploy-prod`: CI deploys once the release is tagged", Type: "past_mistake", Scope: "projects/widget",
			AppliesTo: fact.AppliesTo{Commands: []string{"make deploy-prod"}, Paths: []string{"deploy/**"}}},
		fact.Meta{Name: "deploy-prod-from-ci", Description: "Do not make deploy-prod; CI deploys from main", Type: "user_standard", Scope: "user", Created: "2020-01-01",
			AppliesTo: fact.AppliesTo{Commands: []string{"make deploy-prod"}}},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	bash := func(session, cmd string) string {
		return hookOut(t, toolPayload(cwd, session, "Bash", map[string]any{"command": cmd}), "pre-tool", "--mcp")
	}
	reason := preToolDeny(t, bash("s1", "make deploy-prod"))
	if !strings.Contains(reason, "deploy-needs-tag") || !strings.Contains(reason, "CI deploys once the release is tagged") || !strings.Contains(reason, "run it again unchanged") {
		t.Fatalf("deny: %q", reason)
	}
	if strings.Contains(reason, "deploy-prod-from-ci") {
		t.Fatalf("two facts for one key: %q", reason)
	}
	if out := bash("s1", "make deploy-prod"); out != "" {
		t.Fatalf("the retry was stopped (by the key's other fact?): %q", out)
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
