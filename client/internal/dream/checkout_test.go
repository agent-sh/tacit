package dream

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// A session's cwd in a linked worktree: the window names the main
// repository and carries the worktree for the registry; once the worktree is
// removed, the relation the registry kept still names the repository.
func TestWorktreeCwdKeepsItsRepository(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("HOME", home)
	p := filepath.Join(home, "projects")
	t.Setenv("TACIT_PROJECTS", p)
	main := filepath.Join(p, "widget")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(main, ".git"), 0o755))
	must(os.WriteFile(filepath.Join(main, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	wt := filepath.Join(home, "work", "fix-7")
	admin := filepath.Join(main, ".git", "worktrees", "fix-7")
	must(os.MkdirAll(admin, 0o755))
	must(os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644))
	must(os.WriteFile(filepath.Join(admin, "HEAD"), []byte("ref: refs/heads/fix-7\n"), 0o644))
	must(os.MkdirAll(wt, 0o755))
	must(os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644))

	mains := repoid.LoadMains(home, p)
	raw := []byte(`{"cwd":"` + filepath.Join(wt, "src") + `"}`)
	r, cwd := repoOf(raw, home, mains)
	if r.Name != "widget" || r.Worktree != wt {
		t.Fatalf("worktree cwd: %+v", r)
	}
	ws := windowsOf([]string{"user: fix it"}, nil, r, cwd, home, mains, nil, func(part int, c []string) Window { return Window{Session: "s1"} })
	if len(ws) != 1 || ws[0].Checkout.Worktree != wt || ws[0].Repo != "widget" {
		t.Fatalf("window: %+v", ws)
	}
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	must(repoid.RecordCheckouts(reg, mains, ws[0].Checkout))
	must(os.RemoveAll(wt))
	if r, _ := repoOf(raw, home, mains); r.Name != "widget" || r.Root != main {
		t.Fatalf("removed worktree: %+v", r)
	}
	// A cwd in no recorded checkout stays in none.
	if r, _ := repoOf([]byte(`{"cwd":"`+filepath.Join(home, "elsewhere")+`"}`), home, mains); r.Name != "" {
		t.Fatalf("elsewhere: %+v", r)
	}
}
