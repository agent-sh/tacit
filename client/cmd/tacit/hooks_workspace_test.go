package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
)

func repoNamesOf(rs []repoid.Repo) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

func gitInit(t *testing.T, dir string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	gitIn(t, dir, "init", "-q", "-b", "main")
	return dir
}

// A folder in no repository that holds repositories is a workspace: a
// Brazil workspace's src/<pkg> (depth 2) and a folder of checkouts (depth
// 1). A repository's linked worktree is that repository, once. Dot
// folders, node_modules, deeper roots and folders behind a symlink are not
// looked into; a symlink that is itself a checkout counts.
func TestWorkspaceReposAreTheGitRootsOfTheFolder(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	ws := filepath.Join(root, "ws")
	gitInit(t, filepath.Join(ws, "src", "Alpha"))
	beta := gitInit(t, filepath.Join(ws, "src", "Beta"))
	os.WriteFile(filepath.Join(beta, "f"), []byte("x"), 0o644)
	gitIn(t, beta, "add", "-A")
	gitIn(t, beta, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c")
	gitIn(t, beta, "worktree", "add", "-q", "--detach", filepath.Join(ws, "src", "Beta-wt"))
	os.WriteFile(filepath.Join(ws, "packageInfo"), []byte("x"), 0o644)
	gitInit(t, filepath.Join(ws, "node_modules", "dep"))
	gitInit(t, filepath.Join(ws, ".cache", "hidden"))
	gitInit(t, filepath.Join(ws, "src", "deep", "er", "Gamma"))
	outside := filepath.Join(root, "outside")
	gitInit(t, filepath.Join(outside, "Delta"))
	epsilon := gitInit(t, filepath.Join(root, "Epsilon"))
	if runtime.GOOS != "windows" {
		os.Symlink(outside, filepath.Join(ws, "src", "link-to-folder"))
		os.Symlink(epsilon, filepath.Join(ws, "Epsilon"))
	}

	want := "Alpha Beta"
	if runtime.GOOS != "windows" {
		want = "Alpha Beta Epsilon"
	}
	if got := strings.Join(repoNamesOf(workspaceRepos(ws, home)), " "); got != want {
		t.Fatalf("workspace %s: %q, want %q", ws, got, want)
	}
	if got := strings.Join(repoNamesOf(workspaceRepos(filepath.Join(ws, "src"), home)), " "); got != "Alpha Beta" {
		t.Fatalf("its src: %q", got)
	}
	if got := workspaceRepos(filepath.Join(ws, "src", "Alpha"), home); got != nil {
		t.Fatalf("a folder in a repository is that repository's, not a workspace: %v", repoNamesOf(got))
	}
	t.Setenv("TACIT_DELIVERY_WORKSPACE", "off")
	if got := workspaceRepos(ws, home); got != nil {
		t.Fatalf("setting off: %v", repoNamesOf(got))
	}
}

// Home, the file system root and every folder above home are never
// scanned, however many checkouts they hold.
func TestWorkspaceNeverScansHomeOrRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	home := filepath.Join(root, "users", "me")
	gitInit(t, filepath.Join(home, "a"))
	gitInit(t, filepath.Join(home, "work", "b"))
	gitInit(t, filepath.Join(root, "users", "c"))
	for _, cwd := range []string{home, filepath.Join(root, "users"), root, string(filepath.Separator)} {
		if got := workspaceRepos(cwd, home); got != nil {
			t.Fatalf("scanned %s: %v", cwd, repoNamesOf(got))
		}
	}
	if got := repoNamesOf(workspaceRepos(filepath.Join(home, "work"), home)); strings.Join(got, " ") != "b" {
		t.Fatalf("a folder under home is scanned: %v", got)
	}
	// A symlink to home is home.
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "home-link")
		os.Symlink(home, link)
		if got := workspaceRepos(link, home); got != nil {
			t.Fatalf("scanned home through a symlink: %v", repoNamesOf(got))
		}
	}
}

// A folder with more repositories than workspaceMaxRepos is a general
// folder of projects: no workspace.
func TestWorkspaceOverTheCapIsNone(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	dir := filepath.Join(root, "checkouts")
	for i := 0; i < workspaceMaxRepos; i++ {
		gitInit(t, filepath.Join(dir, fmt.Sprintf("r%d", i)))
	}
	if got := workspaceRepos(dir, filepath.Join(root, "home")); len(got) != workspaceMaxRepos {
		t.Fatalf("at the cap: %d", len(got))
	}
	gitInit(t, filepath.Join(dir, "one-more"))
	if got := workspaceRepos(dir, filepath.Join(root, "home")); got != nil {
		t.Fatalf("over the cap: %v", repoNamesOf(got))
	}
	// Many worktrees of one repository: one repository, until the roots cap.
	wt := filepath.Join(root, "worktrees")
	main := gitInit(t, filepath.Join(wt, "main"))
	os.WriteFile(filepath.Join(main, "f"), []byte("x"), 0o644)
	gitIn(t, main, "add", "-A")
	gitIn(t, main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "c")
	for i := 1; i < workspaceMaxRoots; i++ {
		gitIn(t, main, "worktree", "add", "-q", "--detach", filepath.Join(wt, fmt.Sprintf("task-%02d", i)))
	}
	if got := repoNamesOf(workspaceRepos(wt, filepath.Join(root, "home"))); strings.Join(got, " ") != "main" {
		t.Fatalf("worktrees at the roots cap: %v", got)
	}
	gitIn(t, main, "worktree", "add", "-q", "--detach", filepath.Join(wt, "task-xx"))
	if got := workspaceRepos(wt, filepath.Join(root, "home")); got != nil {
		t.Fatalf("over the roots cap: %v", repoNamesOf(got))
	}
}

// workspaceRig is repoFirstRig's store with the facts given, and a Brazil
// workspace ws/src/{widget,gadget} (two checkouts with src/scheduler and
// lib), the session's cwd.
func workspaceRig(t *testing.T, facts ...fact.Meta) (*rig, string) {
	t.Helper()
	r, _ := repoFirstRig(t, facts...)
	ws := filepath.Join(r.root, "home", "ws") // under HOME, where activity paths count
	for _, n := range []string{"widget", "gadget"} {
		d := gitInit(t, filepath.Join(ws, "src", n))
		os.MkdirAll(filepath.Join(d, "src", "scheduler"), 0o755)
		os.WriteFile(filepath.Join(d, "src", "scheduler", "worker.go"), []byte("package scheduler\n"), 0o644)
	}
	os.WriteFile(filepath.Join(ws, "packageInfo"), []byte("x"), 0o644)
	return r, ws
}

var wsFixture = []fact.Meta{
	{Name: "widget-signoff", Description: "Every commit carries a Signed-off-by trailer", Type: "user_standard", Scope: "projects/widget"},
	{Name: "gadget-generic-code", Description: "Generic transport code belongs in gadget, never in the agents", Type: "design_decision", Scope: "projects/gadget"},
	{Name: "gadget-worker-paths", Description: "Gadget workers keep no state between jobs", Type: "design_decision", Scope: "projects/gadget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/**"}}},
	{Name: "widget-worker-paths", Description: "Widget workers flush the queue before exit", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/**"}}},
	{Name: "widget-lint", Description: "Run the strict linter profile before pushing", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"make lint"}}},
	{Name: "sprocket-standard", Description: "Every sprocket commit is squashed", Type: "user_standard", Scope: "projects/sprocket"},
	{Name: "gadget-transport-retries", Description: "The gadget transport retries a failed private message delivery three times", Type: "past_mistake", Scope: "projects/gadget"},
}

// A session started in a workspace (no repository of its own) gets every
// repository's rules and decisions at SessionStart, under a title that
// names the workspace and its repositories, and a tree line for each of
// their folders; another project's facts stay out. With the setting off it
// gets what a folder in no repository got before: nothing.
func TestSessionStartInAWorkspaceIsAllItsRepos(t *testing.T) {
	_, ws := workspaceRig(t, wsFixture...)
	t.Setenv("TACIT_DELIVERY_START_CEILING", "20")
	out := hookOut(t, startPayload(ws, "s1"), "session-start", "--mcp")
	for _, want := range []string{"widget-signoff", "gadget-generic-code", "the workspace ws: gadget, widget"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sprocket-standard") {
		t.Fatalf("another project's fact:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_START_CEILING", "1")
	if out := hookOut(t, startPayload(ws, "s2"), "session-start", "--mcp"); !strings.Contains(out, "# the workspace's") || !strings.Contains(out, "projects/gadget`") || !strings.Contains(out, "projects/widget`") {
		t.Fatalf("tree line for each folder:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_WORKSPACE", "off")
	if out := hookOut(t, startPayload(ws, "s3"), "session-start", "--mcp"); strings.Contains(out, "gadget-") || strings.Contains(out, "widget-") {
		t.Fatalf("setting off:\n%s", out)
	}
}

// The prompt hook reads every workspace repository's scope; the PreToolUse
// push narrows to the repository of the file an edit writes (that
// repository's path facts, not the other's), and matches a shell command
// run from the workspace root against all of them.
func TestWorkspacePromptAndToolPush(t *testing.T) {
	_, ws := workspaceRig(t, wsFixture...)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	if out := hookOut(t, promptPayload(ws, "s1", "Add retries to the private message delivery transport"), "prompt"); !strings.Contains(out, "gadget-transport-retries") {
		t.Fatalf("prompt in a workspace:\n%s", out)
	}
	// Paths the prompt names in one repository narrow the session to it.
	w := filepath.Join(ws, "src", "widget")
	narrow := "Add retries to the private message delivery transport in " + filepath.Join(w, "src", "scheduler", "worker.go") + " and " + filepath.Join(w, "src", "scheduler", "pool.go")
	if out := hookOut(t, promptPayload(ws, "s4", narrow), "prompt"); strings.Contains(out, "gadget-transport-retries") {
		t.Fatalf("not narrowed to widget:\n%s", out)
	}
	edit := toolPayload(ws, "s2", "Edit", map[string]any{"file_path": filepath.Join(ws, "src", "gadget", "src", "scheduler", "worker.go"), "old_string": "a", "new_string": "b"})
	got := preToolContext(t, hookOut(t, edit, "pre-tool"))
	if !strings.Contains(got, "gadget-worker-paths") || strings.Contains(got, "widget-worker-paths") {
		t.Fatalf("edit narrows to its repository:\n%s", got)
	}
	if got := preToolContext(t, hookOut(t, toolPayload(ws, "s2", "Bash", map[string]any{"command": "cd src/widget && make lint"}), "pre-tool")); !strings.Contains(got, "widget-lint") {
		t.Fatalf("command from the workspace root:\n%s", got)
	}
	// A relative path from the workspace root narrows the same way.
	rel := toolPayload(ws, "s3", "Edit", map[string]any{"file_path": "src/widget/src/scheduler/worker.go"})
	if got := preToolContext(t, hookOut(t, rel, "pre-tool")); !strings.Contains(got, "widget-worker-paths") {
		t.Fatalf("relative path:\n%s", got)
	}
}

// tacit_search and tacit_tree from a workspace read every repository's
// folder, and a path filter is relative to the repository the path is in.
func TestMCPToolsInAWorkspaceReadAllItsRepos(t *testing.T) {
	_, ws := workspaceRig(t, wsFixture...)
	t.Chdir(ws)
	out := callTool(t, "tacit_tree", map[string]any{})
	if !strings.Contains(out, "projects/gadget") || !strings.Contains(out, "projects/widget") || strings.Contains(out, "projects/sprocket (") {
		t.Fatalf("tree:\n%s", out)
	}
	out = callTool(t, "tacit_search", map[string]any{"query": "workers state jobs queue"})
	if !strings.Contains(out, "gadget-worker-paths") || !strings.Contains(out, "widget-worker-paths") || !strings.Contains(out, "# the workspace's") {
		t.Fatalf("search:\n%s", out)
	}
	out = callTool(t, "tacit_search", map[string]any{"path": filepath.Join(ws, "src", "gadget", "src", "scheduler", "worker.go")})
	if !strings.Contains(out, "gadget-worker-paths") {
		t.Fatalf("path filter:\n%s", out)
	}
}

// repoRanked: a deciding fact of the session's repositories outranks
// another scope's fact with a similar word score; another project's fact
// that is not tied to them needs a stronger match; this machine's facts and
// the user's keep their score unless the machine setting is on.
func TestRepoRankedPutsTheSessionsDecidingFactsFirst(t *testing.T) {
	names := []string{"widget", "gadget"}
	hits := []search.Hit{
		{Name: "other-project", Scope: "projects/sprocket", Type: "user_standard", Score: 22},
		{Name: "machine-note", Scope: "machines/host-a", Type: "machine_fact", Score: 21},
		{Name: "user-history", Scope: "user", Type: "history", Score: 20.5},
		{Name: "gadget-decision", Scope: "projects/gadget", Type: "design_decision", Score: 20},
		{Name: "widget-history", Scope: "projects/widget", Type: "history", Score: 19},
		{Name: "tied-from-user", Scope: "user", Type: "past_mistake", Score: 15, Applies: fact.AppliesTo{Repos: []string{"widget"}}},
	}
	order := func(hs []search.Hit) string {
		var out []string
		for _, h := range hs {
			out = append(out, h.Name)
		}
		return strings.Join(out, " ")
	}
	if got := order(repoRanked(hits, names)); got != "gadget-decision tied-from-user machine-note user-history widget-history other-project" {
		t.Fatalf("ranked: %s", got)
	}
	if hits[0].Score != 22 {
		t.Fatal("repoRanked changed its input")
	}
	t.Setenv("TACIT_DELIVERY_PROMPT_PENALIZE_MACHINES", "on")
	if got := order(repoRanked(hits, names)); got != "gadget-decision tied-from-user user-history widget-history other-project machine-note" {
		t.Fatalf("machines penalized: %s", got)
	}
}

// The prompt's action line may carry the local curator's facts (this
// machine's curated layer), never an unreviewed local note.
func TestActionLineTakesLocalCuratedFacts(t *testing.T) {
	names := []string{"widget"}
	curated := search.Hit{Name: "c", Scope: "projects/widget", Type: "design_decision", Local: true, Path: overlay.Prefix + overlay.CuratedDir + "/projects/widget/c.md"}
	note := search.Hit{Name: "n", Scope: "projects/widget", Type: "design_decision", Local: true, Path: overlay.Prefix + overlay.InboxDir + "/x/n.md"}
	stored := search.Hit{Name: "s", Scope: "projects/widget", Type: "past_mistake", Path: "projects/widget/s.md"}
	if !actionEligible(curated, names) || actionEligible(note, names) || !actionEligible(stored, names) {
		t.Fatalf("curated %v, note %v, store %v", actionEligible(curated, names), actionEligible(note, names), actionEligible(stored, names))
	}
}
