package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// namesWorld is a fresh machine and store for the orgs and names tests.
type namesWorld struct {
	t           *testing.T
	home, p     string
	s           *store.Store
	ghLog       string
	user        string
	repos, accs map[string]string
}

// newNamesWorld is a home with a projects directory, gh answering from
// repos and accounts, and a new store (format 2).
func newNamesWorld(t *testing.T, repos, accounts map[string]string) *namesWorld {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shell gh fixture")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "overlay"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": "off", "TACIT_FOLLOW_STATE": filepath.Join(home, "no-follower.json"),
		"GIT_CONFIG_GLOBAL": filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	w := &namesWorld{t: t, home: home, p: filepath.Join(home, "projects"), user: "owner", repos: repos, accs: accounts}
	w.ghLog = fakeGHAccounts(t, w.user, repos, accounts)
	os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\temail = owner@example.com\n\tname = Owner\n"), 0o644)
	os.MkdirAll(w.p, 0o755)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	w.s = &store.Store{Dir: filepath.Join(home, "store")}
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	return w
}

// git makes a checkout of name in the projects directory with one own
// commit, its files, and origin.
func (w *namesWorld) git(name, origin string, files ...string) string {
	t := w.t
	dir := filepath.Join(w.p, name)
	os.MkdirAll(dir, 0o755)
	gitIn(t, dir, "init", "-q", "-b", "main")
	files = append(files, "main.go")
	for _, f := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644)
		gitIn(t, dir, "add", f)
	}
	cmd := exec.Command("git", "commit", "-q", "-m", "start "+name)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	if origin != "" {
		gitIn(t, dir, "remote", "add", "origin", origin)
	}
	return dir
}

func (w *namesWorld) fact(scope, name, typ, session, desc string, paths ...string) {
	writeFactFile(w.t, w.s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: typ, Scope: scope,
		AppliesTo: fact.AppliesTo{Paths: paths}, Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-10"}, Body: desc + ".\n"})
}

func (w *namesWorld) commit(msg string) {
	facts, _, _ := fact.Walk(w.s.Dir)
	writeIndexesForTest(w.t, w.s.Dir, facts)
	gitIn(w.t, w.s.Dir, "add", "-A")
	gitIn(w.t, w.s.Dir, "commit", "-q", "-m", msg)
}

// assess runs the assessment with fa as the model.
func (w *namesWorld) assess(fa *fakeAssessor) *curate.Report {
	w.t.Helper()
	prev := assessRun
	assessRun = fa.run
	defer func() { assessRun = prev }()
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), w.s, assessOpts{model: "opus"}, rep); errs != 0 {
		w.t.Fatalf("assess: %d errors", errs)
	}
	return rep
}

func (w *namesWorld) recs() map[string]*record.Record {
	recs, errs := record.Walk(w.s.Dir)
	if len(errs) != 0 {
		w.t.Fatal(errs)
	}
	return recs
}

func factsList(t *testing.T, dir string) []*fact.Fact {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// treeText is the generated index file of folder.
func treeText(dir, folder string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(folder), "_index.md"))
	return string(b), err
}
