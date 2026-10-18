package repoinfer

import (
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

func TestNameFindsTheRepoAPathIsIn(t *testing.T) {
	home := t.TempDir()
	mkRepo(t, filepath.Join(home, "workplace", "widget"))
	mkRepo(t, filepath.Join(home, "workplace", "ws", "src", "OrderService"))
	os.MkdirAll(filepath.Join(home, "workplace", "widget", "pkg", "api"), 0o755)
	os.MkdirAll(filepath.Join(home, "workplace", "notes"), 0o755)
	for path, want := range map[string]string{
		filepath.Join(home, "workplace", "widget", "pkg", "api", "server.go"):  "widget",
		filepath.Join(home, "workplace", "widget"):                             "widget",
		filepath.Join(home, "workplace", "ws", "src", "OrderService", "x"):     "OrderService",
		"~/workplace/widget/README.md":                                         "widget",
		filepath.Join(home, "workplace", "notes", "todo.md"):                   "", // a folder, not a repo
		filepath.Join(home, "workplace"):                                       "",
		home:                                                                   "",
		filepath.Join(home, ".claude", "projects", "x.jsonl"):                  "",        // agent state
		filepath.Join(home, "workplace", "gone", "src", "Billing", "a.java"):   "Billing", // deleted workspace, Brazil shape
		filepath.Join(home, "workplace", "widget", ".worktrees", "fix-1", "a"): "widget",
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
		{map[string]int{"widget": 2, "api": 2, "cli": 1}, ""}, // spread
		{map[string]int{"widget": 1}, ""},                     // one mention
		{map[string]int{}, ""},
	} {
		if got := Dominant(c.counts, 2); got != c.want {
			t.Errorf("Dominant(%v) = %q, want %q", c.counts, got, c.want)
		}
	}
}

func TestCountsReadsPathsInCommandsAndJSON(t *testing.T) {
	home := t.TempDir()
	mkRepo(t, filepath.Join(home, "workplace", "widget"))
	texts := []string{
		"T: Bash cd " + home + "/workplace/widget && go test ./...",
		`{"command":"git -C ` + home + `/workplace/widget status"}`,
		"T: Read ~/workplace/widget/main.go",
		"T: Bash ls " + home + "/.claude/projects",
		"see https://example.com/workplace/widget/x and ./workplace/widget/y",
	}
	got := Counts(texts, home, 1, nil)
	if got["widget"] != 3 || len(got) != 1 {
		t.Fatalf("counts %v", got)
	}
}
