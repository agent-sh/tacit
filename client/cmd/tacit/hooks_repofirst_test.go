package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// repoFirstRig is a store with 400 filler facts and the checkout
// projects/widget, plus the facts each test writes, committed.
func repoFirstRig(t *testing.T, facts ...fact.Meta) (*rig, string) {
	t.Helper()
	r := newRig(t, rigOpts{repos: []string{"widget"}, filler: 400})
	for _, m := range facts {
		writeFact(t, r.clone, m, m.Description+"\n")
	}
	r.commitIndex(r.clone, "facts")
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	return r, filepath.Join(r.projects, "widget")
}

func startPayload(cwd, session string) string {
	return `{"session_id":"` + session + `","cwd":"` + cwd + `","source":"startup"}`
}

func promptPayload(cwd, session, prompt string) string {
	b, _ := json.Marshal(map[string]string{"session_id": session, "cwd": cwd, "prompt": prompt})
	return string(b)
}

func toolPayload(cwd, session, tool string, input map[string]any) string {
	b, _ := json.Marshal(map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input})
	return string(b)
}

var widgetRepo = fact.AppliesTo{Repos: []string{"widget"}}

// The SessionStart block in a repo is the repo's own rules and standards,
// then its decisions and past mistakes, chosen by metadata: a fact that only
// names the repo in its words, and the repo's host notes and history, stay
// out. With the setting off the block is the old one, ranked by how well a
// fact's words match the repo name.
func TestSessionStartRepoBlockIsTheReposRulesAndDecisions(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-signoff", Description: "Every commit carries a Signed-off-by trailer", Type: "user_standard", Scope: "projects/widget"},
		fact.Meta{Name: "widget-queue-choice", Description: "Jobs go through the one queue, not per-tenant queues", Type: "design_decision", Scope: "user", AppliesTo: widgetRepo},
		fact.Meta{Name: "widget-build-host", Description: "The widget build host has 64 GB and sits in rack 4", Type: "machine_fact", Scope: "projects/widget"},
		fact.Meta{Name: "widget-renamed-history", Description: "widget history: widget was split from widget-old in 2025", Type: "history", Scope: "projects/widget"},
		fact.Meta{Name: "widget-words-only", Description: "widget widget widget: notes that say widget a lot", Type: "user_standard", Scope: "user"},
		fact.Meta{Name: "gadget-signoff", Description: "Every gadget commit is squashed", Type: "user_standard", Scope: "projects/gadget"},
	)
	out := hookOut(t, startPayload(cwd, "s1"), "session-start", "--mcp")
	for _, want := range []string{"widget-signoff", "widget-queue-choice"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s:\n%s", want, out)
		}
	}
	for _, not := range []string{"widget-build-host", "widget-renamed-history", "widget-words-only", "gadget-signoff"} {
		if strings.Contains(out, not) {
			t.Fatalf("%s in the repo block:\n%s", not, out)
		}
	}
	if strings.Index(out, "widget-signoff") > strings.Index(out, "widget-queue-choice") {
		t.Fatalf("the standard does not come first:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_START_CEILING", "1")
	if out := hookOut(t, startPayload(cwd, "s2"), "session-start"); strings.Count(out, "\n- ") != 1 || !strings.Contains(out, "widget-signoff") {
		t.Fatalf("ceiling 1:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	if out := hookOut(t, startPayload(cwd, "s3"), "session-start"); !strings.Contains(out, "widget-words-only") {
		t.Fatalf("setting off: the old block ranks by the repo name:\n%s", out)
	}
}

// A checkout answers to the names its own data gives it: every remote's
// repository name and its root manifest's package name when that shares a
// word with a name it already has. A generic package name does not join
// another project's facts. Off, only the checkout and origin names count.
func TestRepoAliasesComeFromRemotesAndManifests(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "gizmo-standard", Description: "Releases are cut from the release branch only", Type: "user_standard", Scope: "projects/gizmo"},
		fact.Meta{Name: "widget-core-standard", Description: "Public functions carry a doc comment", Type: "user_standard", Scope: "projects/widget-core"},
		fact.Meta{Name: "agents-standard", Description: "Agents run with a 30 minute budget", Type: "user_standard", Scope: "projects/agents"},
	)
	gitIn(t, cwd, "remote", "add", "upstream", "https://example.invalid/acme/gizmo.git")
	os.WriteFile(filepath.Join(cwd, "package.json"), []byte(`{"name": "@acme/widget-core", "version": "1.0.0"}`), 0o644)
	os.WriteFile(filepath.Join(cwd, "Cargo.toml"), []byte("[package]\nname = \"agents\"\n"), 0o644)
	out := hookOut(t, startPayload(cwd, "s1"), "session-start")
	if !strings.Contains(out, "gizmo-standard") || !strings.Contains(out, "widget-core-standard") {
		t.Fatalf("aliases missing:\n%s", out)
	}
	if strings.Contains(out, "agents-standard") {
		t.Fatalf("a package name sharing no word joined:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_REPO_ALIASES", "off")
	if out := hookOut(t, startPayload(cwd, "s2"), "session-start"); strings.Contains(out, "gizmo-standard") || strings.Contains(out, "widget-core-standard") {
		t.Fatalf("aliases off:\n%s", out)
	}
}

// The prompt block adds the repo's fact whose applies_to paths cover a path
// the prompt names, or whose command the prompt names by its action word,
// though its words share nothing with the request. A repeated request adds
// nothing (no backfill), and the setting turns it off.
func TestPromptBlockMatchesAppliesToPathsAndCommands(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "pool-lock-order", Description: "Take the shard lock before the slot lock", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/pool.go"}}},
		fact.Meta{Name: "dco-trailer", Description: "A bot rejects pushes without the DCO trailer", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"git commit"}}},
		fact.Meta{Name: "other-pool-note", Description: "Take the pool lock first", Type: "past_mistake", Scope: "projects/gadget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/pool.go"}}},
	)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off") // the start block would carry both facts
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	if out := hookOut(t, promptPayload(cwd, "s1", "Fix the race in src/scheduler/pool.go when two jobs finish"), "prompt"); !strings.Contains(out, "pool-lock-order") || strings.Contains(out, "other-pool-note") {
		t.Fatalf("path match:\n%s", out)
	}
	if out := hookOut(t, promptPayload(cwd, "s1", "Fix the race in src/scheduler/pool.go when two jobs finish"), "prompt"); strings.Contains(out, "- ") {
		t.Fatalf("a repeated request delivered again:\n%s", out)
	}
	if out := hookOut(t, promptPayload(cwd, "s2", "Make the retry loop quieter and commit it on a branch"), "prompt"); !strings.Contains(out, "dco-trailer") {
		t.Fatalf("command match:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_PROMPT_METADATA", "off")
	if out := hookOut(t, promptPayload(cwd, "s3", "Fix the race in src/scheduler/pool.go when two jobs finish"), "prompt"); strings.Contains(out, "pool-lock-order") {
		t.Fatalf("metadata off:\n%s", out)
	}
}

// Before an edit or a shell command, the PreToolUse hook gives the one rule,
// decision or past mistake of this repo whose applies_to names that file or
// command, as JSON additionalContext, once per session; it reads Claude
// Code's Edit and Bash and Codex's apply_patch, never another repo's fact,
// and prints nothing when nothing matches or the setting is off.
func TestPreToolHookPushesTheMatchingFactOnce(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "worker-no-globals", Description: "Worker state lives in the pool, never in package globals", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/**"}}},
		fact.Meta{Name: "lint-before-push", Description: "Run the linter with the strict profile", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"make lint"}}},
		fact.Meta{Name: "gadget-worker-note", Description: "Gadget workers are stateless", Type: "design_decision", Scope: "projects/gadget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/worker.go"}}},
		fact.Meta{Name: "worker-history", Description: "The worker was rewritten in 2025", Type: "history", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/worker.go"}}},
	)
	ctxOf := func(out string) string {
		t.Helper()
		if strings.TrimSpace(out) == "" {
			return ""
		}
		var v struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "PreToolUse" {
			t.Fatalf("not PreToolUse JSON: %v %q", err, out)
		}
		return v.HookSpecificOutput.AdditionalContext
	}
	edit := toolPayload(cwd, "s1", "Edit", map[string]any{"file_path": filepath.Join(cwd, "src", "scheduler", "worker.go"), "old_string": "a", "new_string": "b"})
	got := ctxOf(hookOut(t, edit, "pre-tool"))
	if !strings.Contains(got, "worker-no-globals") || strings.Contains(got, "gadget-worker-note") || strings.Contains(got, "worker-history") || strings.Count(got, "\n- ") > 1 {
		t.Fatalf("edit:\n%s", got)
	}
	if again := hookOut(t, edit, "pre-tool"); again != "" {
		t.Fatalf("pushed twice in one session: %q", again)
	}
	if got := ctxOf(hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "cd src && make lint"}), "pre-tool")); !strings.Contains(got, "lint-before-push") {
		t.Fatalf("bash:\n%s", got)
	}
	if out := hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "ls -la"}), "pre-tool"); out != "" {
		t.Fatalf("an unmatched command: %q", out)
	}
	patch := "*** Begin Patch\n*** Update File: src/scheduler/pool.go\n@@\n-a\n+b\n*** End Patch\n"
	if got := ctxOf(hookOut(t, toolPayload(cwd, "s2", "apply_patch", map[string]any{"command": patch}), "pre-tool")); !strings.Contains(got, "worker-no-globals") {
		t.Fatalf("apply_patch:\n%s", got)
	}
	t.Setenv("TACIT_DELIVERY_TOOL_PUSH", "off")
	if out := hookOut(t, toolPayload(cwd, "s3", "Edit", map[string]any{"file_path": filepath.Join(cwd, "src", "scheduler", "worker.go")}), "pre-tool"); out != "" {
		t.Fatalf("setting off: %q", out)
	}
}

// With no store the PreToolUse hook prints nothing and exits 0.
func TestPreToolHookFailsOpenWithoutAStore(t *testing.T) {
	t.Setenv("TACIT_STORE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", t.TempDir())
	if out := hookOut(t, toolPayload(t.TempDir(), "s1", "Edit", map[string]any{"file_path": "/x/y.go"}), "pre-tool"); out != "" {
		t.Fatalf("printed %q", out)
	}
}

// The MCP server sends the instruction line as its instructions, which the
// harness shows while the tools are deferred; the setting turns it off.
func TestMCPServerSendsTheInstruction(t *testing.T) {
	instructions := func() string {
		t.Helper()
		ctx := context.Background()
		st, ct := mcp.NewInMemoryTransports()
		srv := storeServer(&store.Store{Dir: t.TempDir()}, "codex")
		ss, err := srv.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ss.Close()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		return cs.InitializeResult().Instructions
	}
	got := instructions()
	if got != mcpInstructions || len(got) > 200 || !strings.Contains(got, "tacit_search") || !strings.Contains(got, "tacit_tree") {
		t.Fatalf("instructions %q", got)
	}
	t.Setenv("TACIT_DELIVERY_INSTRUCTION", "off")
	if got := instructions(); got != "" {
		t.Fatalf("instruction off: %q", got)
	}
}

// When the repo block shows fewer of the repository's facts than it has,
// SessionStart's last line says how many it has and the tree call that lists
// them, as the session names the tree; with all shown, or the setting off,
// there is no such line.
func TestSessionStartPointsToTheRepoFolder(t *testing.T) {
	var facts []fact.Meta
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		facts = append(facts, fact.Meta{Name: "widget-standard-" + n, Description: "Widget standard " + n, Type: "user_standard", Scope: "projects/widget"})
	}
	_, cwd := repoFirstRig(t, facts...)
	out := hookOut(t, startPayload(cwd, "s1"), "session-start", "--mcp")
	if !strings.HasSuffix(out, "# this repo's 8 facts by folder: `mcp__tacit__tacit_tree projects/widget`\n") {
		t.Fatalf("pointer:\n%s", out)
	}
	if out := hookOut(t, startPayload(cwd, "s2"), "session-start"); !strings.Contains(out, "`tacit tree projects/widget`") {
		t.Fatalf("pointer without MCP:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_START_CEILING", "8")
	if out := hookOut(t, startPayload(cwd, "s3"), "session-start", "--mcp"); strings.Contains(out, "tacit_tree") {
		t.Fatalf("all shown, still a pointer:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_START_CEILING", "")
	t.Setenv("TACIT_DELIVERY_TREE_POINTER", "off")
	if out := hookOut(t, startPayload(cwd, "s4"), "session-start", "--mcp"); strings.Contains(out, "tacit_tree") {
		t.Fatalf("setting off:\n%s", out)
	}
}

// A shell command runs a fact's command when a part starts with it, or
// goes on with it after its first word when the second is no flag: a
// program that runs the next one (npx, sudo, a loop's do).
func TestCommandMatchThroughARunner(t *testing.T) {
	for _, c := range []struct {
		fact, cmd string
		want      bool
	}{
		{"vitest", "npx vitest run ts/test/a.test.ts", true},
		{"make lint", "sudo make lint", true},
		{"make lint", "for d in a b; do make lint; done", true},
		{"git commit", "FOO=1 git commit -s -m x", true},
		{"vitest", "npx -y vitest run", false},
		{"make lint", "sudo make test", false},
		{"git stash", "git status && npm test", false},
	} {
		if got := commandMatch(c.fact, c.cmd); got != c.want {
			t.Errorf("commandMatch(%q, %q) = %v, want %v", c.fact, c.cmd, got, c.want)
		}
	}
}

// A command of one word, a bare program, matches a shell command only with a
// word of the fact's own in the command line too: not its key's, not the
// repository's name, not a word of the session's folder or home. Words are
// compared by stem, among the command's first terms (a word deep in a
// heredoc does not count). A longer command, or a script's path or file
// name, matches as before. Of the pushes keyed on a bare program in real
// sessions, few were useful (eval/DELIVERY.md, "Pushes keyed on a bare
// program").
func TestPreToolBareProgramNeedsASecondWord(t *testing.T) {
	r, cwd := repoFirstRig(t,
		fact.Meta{Name: "ssh-gpubox-batchmode", Description: "Reach the widget gpubox in projects from home with ssh and BatchMode; a password prompt hangs the agent", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"ssh"}}},
		fact.Meta{Name: "terraform-applies-lock", Description: "terraform applies need the state lock", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"terraform"}}},
		fact.Meta{Name: "widget-deploy-checks", Description: "A deploy runs the smoke checks first", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"make deploy"}}},
		fact.Meta{Name: "widget-release-script", Description: "The release script tags and signs", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"scripts/release.sh"}}},
		fact.Meta{Name: "rsync-long-note", Description: "Copies amber basil cedar dune ember fable garnet harbor ivory jasper kettle lantern meadow nectar orchid pebble quartz raven saffron timber umber velvet willow yarrow zephyr reach the mirrorbox", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"rsync"}}},
	)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	home := filepath.Join(r.root, "home")
	// A worktree whose folder is not named for the repository.
	wt := filepath.Join(r.root, "wt-x")
	gitIn(t, cwd, "worktree", "add", "-q", "-b", "wt-x", wt)
	push := func(session, command string) string {
		t.Helper()
		dir := cwd
		if strings.HasPrefix(session, "wt") {
			dir = wt
		}
		return preToolContext(t, hookOut(t, toolPayload(dir, session, "Bash", map[string]any{"command": command}), "pre-tool"))
	}
	for _, c := range []struct {
		session, command, want string
	}{
		{"s1", "ssh gpubox nvidia-smi", "ssh-gpubox-batchmode"},
		{"s2", "ssh build-host uptime", ""},
		{"s3", "cd " + cwd + " && ssh build-host 'ls projects'", ""},
		{"s4", "ls " + home + " && ssh build-host uptime", ""},
		{"wt1", "ssh build-host 'ls widget'", ""},
		{"s5", "ssh build-host one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twentyone twentytwo twentythree twentyfour twentyfive gpubox", ""},
		{"s6", "terraform apply -auto-approve", "terraform-applies-lock"},
		{"s7", "make deploy", "widget-deploy-checks"},
		{"s8", "scripts/release.sh", "widget-release-script"},
		{"s9", "rsync -a out mirrorbox:/srv", "rsync-long-note"},
	} {
		got := push(c.session, c.command)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s %q: want %q, got:\n%s", c.session, c.command, c.want, got)
		}
	}
}

// Every word of the session's repository names and folders is left out of a
// command's words, however many repositories the workspace has.
func TestCommandWordsLeaveOutEveryFolderWord(t *testing.T) {
	names := []string{"amber-anchor-atlas", "basil-beacon-bridge", "cedar-canyon-comet", "dune-delta-drift", "ember-echo-eagle", "fable-falcon-fjord", "garnet-glacier-grove", "harbor-heron-helix", "ivory-island-iris"}
	got := commandWords("ssh build-host 'ls projects'", names, "/srv/projects/amber-anchor-atlas")
	if got[search.TermKey("projects")] || !got[search.TermKey("build")] {
		t.Fatalf("command words %v", got)
	}
}
