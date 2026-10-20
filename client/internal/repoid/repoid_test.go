package repoid

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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

// mkRepo makes dir a checkout with a .git directory and, when origin is
// set, an origin remote.
func mkRepo(t *testing.T, dir, origin string) {
	t.Helper()
	mkdir(t, filepath.Join(dir, ".git"))
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	if origin != "" {
		write(t, filepath.Join(dir, ".git", "config"), "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = "+origin+"\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n")
	}
}

// mkWorktree makes wt a linked worktree of main, the way git does: a gitfile
// and an admin directory with a commondir file.
func mkWorktree(t *testing.T, main, wt string) {
	t.Helper()
	admin := filepath.Join(main, ".git", "worktrees", filepath.Base(wt))
	write(t, filepath.Join(admin, "commondir"), "../..\n")
	write(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/x\n")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
}

func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOfNamesARepoOnlyThroughARealGitRoot(t *testing.T) {
	home := realTemp(t)
	p := filepath.Join(home, "projects")
	mkRepo(t, filepath.Join(p, "widget"), "git@github.com:acme/widget-svc.git")
	mkdir(t, filepath.Join(p, "widget", "pkg", "api"))
	mkRepo(t, filepath.Join(p, "org", "tool"), "")
	mkdir(t, filepath.Join(p, "notes", "draft")) // a folder, no git
	mkWorktree(t, filepath.Join(p, "widget"), filepath.Join(p, "widget-worktrees", "fix-1"))
	mkWorktree(t, filepath.Join(p, "widget"), filepath.Join(p, "wt-textflip")) // a worktree named like a task
	mkWorktree(t, filepath.Join(p, "widget"), filepath.Join(p, "widget", ".claude", "worktrees", "agent-1"))
	mkWorktree(t, filepath.Join(p, "widget"), filepath.Join(home, ".codex", "worktrees", "ab12", "widget"))
	mkRepo(t, filepath.Join(home, ".cache", "scratch"), "") // tool state
	mkRepo(t, filepath.Join(home, ".config"), "")           // a dotfiles checkout
	mkdir(t, filepath.Join(home, "tmpwork", "tune"))        // a scratch folder outside any repo
	outside := realTemp(t)
	mkRepo(t, filepath.Join(outside, "srv", "memra"), "")
	mkdir(t, filepath.Join(outside, "tmp", "negeig", "tune"))

	for path, want := range map[string]string{
		filepath.Join(p, "widget"):                                         "widget",
		filepath.Join(p, "widget", "pkg", "api", "server.go"):              "widget",
		filepath.Join(p, "widget", "gone", "deleted.go"):                   "widget", // a path that no longer exists
		"~/projects/widget/README.md":                                      "widget",
		filepath.Join(p, "widget-worktrees", "fix-1", "a.go"):              "widget",
		filepath.Join(p, "wt-textflip"):                                    "widget",
		filepath.Join(p, "widget", ".claude", "worktrees", "agent-1", "x"): "widget",
		filepath.Join(home, ".codex", "worktrees", "ab12", "widget"):       "widget", // a worktree in a dot dir is its repo's
		filepath.Join(p, "org", "tool", "src"):                             "tool",
		filepath.Join(p, "org"):                                            "", // a folder of checkouts
		filepath.Join(p, "notes", "draft"):                                 "", // a folder named like a project
		p:                                                                  "",
		home:                                                               "",
		filepath.Join(home, ".cache", "scratch", "run"):                    "", // agent and tool state
		filepath.Join(home, ".config", "nvim"):                             "",
		filepath.Join(home, "tmpwork", "tune"):                             "",
		filepath.Join(outside, "srv", "memra", "lib"):                      "memra", // a real git root outside home
		filepath.Join(outside, "tmp", "negeig", "tune"):                    "",      // its basename is no repo
		"relative/path":                                                    "",
	} {
		if got := Name(path, home); got != want {
			t.Errorf("Name(%s) = %q, want %q", path, got, want)
		}
	}
	r, ok := Of(filepath.Join(p, "wt-textflip"), home)
	if !ok || r.Root != filepath.Join(p, "widget") || r.Worktree != filepath.Join(p, "wt-textflip") {
		t.Fatalf("worktree: %+v", r)
	}
	if len(r.Aliases) != 1 || r.Aliases[0] != "widget-svc" {
		t.Fatalf("origin alias: %v", r.Aliases)
	}
}

func TestOfResolvesSymlinksToTheRealCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	home := realTemp(t)
	p := filepath.Join(home, "projects")
	mkRepo(t, filepath.Join(p, "pikadon"), "https://github.com/acme/ginza")
	if err := os.Symlink("pikadon", filepath.Join(p, "ginza")); err != nil {
		t.Fatal(err)
	}
	r, ok := Of(filepath.Join(p, "ginza", "src"), home)
	if !ok || r.Name != "pikadon" {
		t.Fatalf("symlinked checkout: %+v", r)
	}
	if len(r.Aliases) != 1 || r.Aliases[0] != "ginza" {
		t.Fatalf("aliases %v, want the lexical and remote name ginza once", r.Aliases)
	}
	// A symlinked home: paths under the home as given are the real home's.
	link := filepath.Join(realTemp(t), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if got := Name(filepath.Join(link, "projects", "pikadon"), link); got != "pikadon" {
		t.Fatalf("symlinked home: %q", got)
	}
}

// Every remote's repository name is a Remote, read from the config (no git
// process); only origin's is an Alias, and the repository's own name is
// neither.
func TestOfReadsEveryRemoteFromTheConfig(t *testing.T) {
	home := realTemp(t)
	dir := filepath.Join(home, "projects", "widget")
	mkRepo(t, dir, "")
	write(t, filepath.Join(dir, ".git", "config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://example.com/acme/Gizmo.git/\n[remote \"origin\"]\n\turl = git@example.com:me/widget-svc.git\n[remote \"self\"]\n\turl = ../widget\n[branch \"main\"]\n\tremote = origin\n")
	r, ok := Of(dir, home)
	if !ok || r.Name != "widget" {
		t.Fatalf("%+v", r)
	}
	if len(r.Aliases) != 1 || r.Aliases[0] != "widget-svc" {
		t.Fatalf("aliases %v: origin's name only", r.Aliases)
	}
	if len(r.Remotes) != 2 || r.Remotes[0] != "gizmo" || r.Remotes[1] != "widget-svc" {
		t.Fatalf("remotes %v", r.Remotes)
	}
	if got := URLName("acme/widget"); got != "widget" {
		t.Fatalf("URLName owner/repo: %q", got)
	}
}

func TestOfStopsAtABrokenGitfile(t *testing.T) {
	home := realTemp(t)
	mkRepo(t, filepath.Join(home, "outer"), "")
	write(t, filepath.Join(home, "outer", "inner", ".git"), "gitdir: /nowhere/at/all\n")
	write(t, filepath.Join(home, "outer", "sub", ".git"), "gitdir: ../.git/modules/sub\n")
	write(t, filepath.Join(home, "outer", ".git", "modules", "sub", "HEAD"), "ref: refs/heads/main\n")
	// A gitfile naming a checkout instead of a git directory.
	mkRepo(t, filepath.Join(home, "outer", "admin", "task-1"), "")
	write(t, filepath.Join(home, "outer", "task-1", ".git"), "gitdir: ../admin/task-1\n")
	if got := Name(filepath.Join(home, "outer", "task-1", "x"), home); got != "" {
		t.Errorf("a gitfile naming a work tree named %q", got)
	}
	if got := Name(filepath.Join(home, "outer", "inner", "x"), home); got != "" {
		t.Errorf("a gitfile that points nowhere named %q (git stops there too)", got)
	}
	if got := Name(filepath.Join(home, "outer", "sub", "x"), home); got != "sub" {
		t.Errorf("a submodule is its own repository: %q", got)
	}
}

func TestCacheStopsWhenAskedOrFull(t *testing.T) {
	home := realTemp(t)
	mkRepo(t, filepath.Join(home, "a", "repo"), "")
	c := NewCache(home)
	c.Stop = func() bool { return true }
	if _, ok := c.Of(filepath.Join(home, "a", "repo", "x")); ok {
		t.Fatal("a stopped cache found a repo")
	}
	c = NewCache(home)
	c.MaxDirs = 1
	c.Of(filepath.Join(home, "a", "repo", "x", "y", "z"))
	if _, ok := c.Of(filepath.Join(home, "b", "c")); ok {
		t.Fatal("a full cache kept looking")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Widget": "widget", "my-repo": "my-repo", "my_repo": "", "foo.js": "", "": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCatalogKnowsOnlyRealCheckouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	home := realTemp(t)
	p := filepath.Join(home, "projects")
	mkRepo(t, filepath.Join(p, "pikadon"), "git@github.com:acme/ginza.git")
	mkRepo(t, filepath.Join(p, "org", "tool"), "")
	mkdir(t, filepath.Join(p, "park-predictor")) // no git
	elsewhere := filepath.Join(home, "work", "deep", "lib")
	mkRepo(t, elsewhere, "")
	gone := filepath.Join(home, "work", "gone")
	mkRepo(t, gone, "")
	reg := filepath.Join(home, "state", RegistryFile)
	for _, d := range []string{elsewhere, gone} {
		r, _ := Of(d, home)
		if err := Record(reg, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	c := LoadCatalog(home, reg, p)
	for name, want := range map[string]bool{"pikadon": true, "tool": true, "lib": true, "gone": false, "park-predictor": false, "org": false, "ginza": false} {
		if got := c.Has(name); got != want {
			t.Errorf("Has(%s) = %v, want %v", name, got, want)
		}
	}
	if got := c.AliasOf("ginza"); got != "pikadon" {
		t.Errorf("AliasOf(ginza) = %q", got)
	}
	if got := c.AliasOf("pikadon"); got != "" {
		t.Errorf("a repo's own name is no alias: %q", got)
	}
}
