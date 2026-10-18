package repoinfer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func mkRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNameFindsTheRepoAPathIsIn(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := filepath.Join(home, "workplace")
	mkRepo(t, filepath.Join(w, "widget"))
	mkdir(t, filepath.Join(w, "widget", "pkg", "api"))
	mkRepo(t, filepath.Join(w, "ws", "src", "OrderService"))
	write(t, filepath.Join(w, "ws", "packageInfo"), "")   // a Brazil workspace that still exists
	write(t, filepath.Join(w, "gone", "src", "x.go"), "") // no workspace marker
	mkdir(t, filepath.Join(w, "notes"))
	// A worktree whose .git is a file pointing at its repository.
	mkRepo(t, filepath.Join(w, "api"))
	write(t, filepath.Join(w, "api-fix", ".git"), "gitdir: "+filepath.Join(w, "api", ".git", "worktrees", "api-fix")+"\n")
	// ~/projects/<repo>-worktrees/<task>
	mkRepo(t, filepath.Join(home, "projects", "atlas-worktrees", "KV-1"))
	for path, want := range map[string]string{
		filepath.Join(w, "widget", "pkg", "api", "server.go"):             "widget",
		filepath.Join(w, "widget"):                                        "widget",
		"~/workplace/widget/README.md":                                    "widget",
		filepath.Join(w, "widget", ".worktrees", "fix-1", "a"):            "widget",
		filepath.Join(w, "ws", "src", "OrderService", "x"):                "OrderService",
		filepath.Join(w, "ws", "src", "Billing", "src", "A.java"):         "Billing", // removed from a live workspace
		filepath.Join(w, "gone", "src", "Billing", "a.java"):              "",        // no workspace: not a Brazil shape
		filepath.Join(w, "api-fix", "main.go"):                            "api",
		filepath.Join(home, "projects", "atlas-worktrees", "KV-1", "a"):   "atlas",
		filepath.Join(w, "notes", "todo.md"):                              "", // a folder, not a repo
		w:                                                                 "",
		home:                                                              "",
		filepath.Join(home, ".claude", "projects", "x.jsonl"):             "", // agent state
		"/usr/local/go/src/runtime/panic.go":                              "", // outside home
		"/rustc/0123abcd/library/core/src/panicking.rs":                   "",
		filepath.Join(home, "go", "pkg", "mod", "x@v1", "src", "util.go"): "",
	} {
		if got := Name(path, home); got != want {
			t.Errorf("Name(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestDominantNeedsAClearMajority(t *testing.T) {
	for _, c := range []struct {
		counts map[string]int
		want   string
	}{
		{map[string]int{"widget": 5, "api": 1}, "widget"},
		{map[string]int{"widget": 2, "api": 2}, ""}, // a tie names neither
		{map[string]int{"widget": 2, "api": 2, "cli": 1}, ""},
		{map[string]int{"widget": 1}, ""}, // below minCount
		{map[string]int{}, ""},
	} {
		if got := Dominant(c.counts, 2); got != c.want {
			t.Errorf("Dominant(%v) = %q, want %q", c.counts, got, c.want)
		}
	}
}

func TestCountsReadsPathsInCommandsAndJSON(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepo(t, filepath.Join(home, "workplace", "widget"))
	wd := home + "/workplace/widget"
	texts := []string{
		"T: Bash cd " + wd + " && go test ./...",
		`{"command":"git -C ` + wd + ` status"}`,
		"T: Read ~/workplace/widget/main.go",
		"T: Bash ls " + home + "/.claude/projects",
		"see https://example.com/workplace/widget/x and ./workplace/widget/y",
		// grep output inside a JSON tool result: each path starts an escaped line
		`{"type":"tool_result","content":"` + wd + `/a.go:3: x\n` + wd + `/b.go:4: y"}`,
		"panic: boom\n\t/usr/local/go/src/runtime/panic.go:770 +0x1",
	}
	got := Counts(texts, home, 1, nil)
	// cd and git -C count Strong each, the three other mentions once.
	if got["widget"] != 2*Strong+3 || len(got) != 1 {
		t.Fatalf("counts %v, want widget %d and nothing else", got, 2*Strong+3)
	}
}

func TestWorkPathsOutweighToolPaths(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepo(t, filepath.Join(home, "w", "widget"))
	mkRepo(t, filepath.Join(home, "w", "bridge")) // a tool the session only runs
	texts := []string{
		"T: Bash " + home + "/w/bridge/bin/run --tab 1",
		"T: Bash " + home + "/w/bridge/bin/run --tab 2",
		"T: Bash cd " + home + "/w/widget && make",
		`{"name":"Edit","input":{"file_path":"` + home + `/w/widget/main.go"}}`,
		"T: Edit " + home + "/w/widget/util.go",
	}
	c := Counts(texts, home, 1, nil)
	if got := Dominant(c, 3); got != "widget" {
		t.Fatalf("counts %v: dominant %q, want widget", c, got)
	}
	// Mentions alone, spread over two repos, name neither.
	if got := Dominant(Counts(texts[:2], home, 1, nil), 3); got != "" {
		t.Fatalf("two runs of a tool named %q", got)
	}
}

func TestResolverStopsWhenAskedOrFull(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	mkRepo(t, filepath.Join(home, "a", "repo"))
	r := NewResolver(home)
	r.Stop = func() bool { return true }
	if got := r.Name(filepath.Join(home, "a", "repo", "x")); got != "" {
		t.Fatalf("a stopped resolver named %q", got)
	}
	r = NewResolver(home)
	r.MaxDirs = 1
	r.Name(filepath.Join(home, "b", "c", "d"))
	if got := r.Name(filepath.Join(home, "a", "repo", "x")); got != "" {
		t.Fatalf("a full resolver named %q", got)
	}
	// Every stat counts against the cap, the per-mention ones too.
	r = NewResolver(home)
	r.MaxDirs = 10
	for i := range 100 {
		r.Name(filepath.Join(home, "a", "repo", fmt.Sprintf("f%d.go", i)))
	}
	if n := len(r.roots) + len(r.stats); n > r.MaxDirs {
		t.Fatalf("%d stats past a cap of %d", n, r.MaxDirs)
	}
}
