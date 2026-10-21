package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// The dreamer's evidence of work in a checkout elsewhere is kept in the
// registry across runs (a dry run records none), and the second session's
// run names the windows' repository and puts it in the machine's catalog.
func TestAcceptPendingReposKeepsTheEvidenceAcrossRuns(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	os.MkdirAll(home, 0o700)
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	scratch := filepath.Join(base, "srv", "memra")
	os.MkdirAll(filepath.Join(scratch, ".git"), 0o755)
	os.WriteFile(filepath.Join(scratch, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	r, _ := repoid.Of(scratch, home)
	win := func(session string) []dream.Window {
		return []dream.Window{{File: session, Session: session, Pending: r, Work: "commit"}}
	}
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	ws := win("s1")
	acceptPendingRepos(home, ws, false)
	if len(repoid.WorkFor(reg, scratch)) != 0 || ws[0].Repo != "" {
		t.Fatal("a dry run recorded evidence or named the repo")
	}
	acceptPendingRepos(home, ws, true)
	recordWindowRepos(home, ws)
	if ws[0].Repo != "" || machineCatalog(home).Has("memra") {
		t.Fatal("one session named the repo")
	}
	ws = win("s2")
	acceptPendingRepos(home, ws, true)
	recordWindowRepos(home, ws)
	if ws[0].Repo != "memra" || ws[0].RepoRoot != scratch || !machineCatalog(home).Has("memra") {
		t.Fatalf("two sessions: %+v", ws[0])
	}
}
