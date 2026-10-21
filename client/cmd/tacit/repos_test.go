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

// The dreamer keeps the evidence of work for a repository in home and for
// a non-git project folder, and each session's tool names, in this
// machine's state only.
func TestDreamKeepsWorkAndToolUsageInState(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(base, "home")
	os.MkdirAll(home, 0o700)
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	ws := []dream.Window{
		{Session: "s1", WorkRoot: "/r/widget", Work: "edit", Lines: []string{"T: mcp__x__Ticketing V1", "T: Bash make test"}},
		{Session: "s2", WorkFolder: "/p/notes", Work: "edit"},
		{Session: "s3", WorkRoot: "/r/widget"},
	}
	acceptPendingRepos(home, ws, true)
	recordToolUsage(home, ws)
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	if n, _, _ := repoid.WorkSummary(repoid.WorkFor(reg, "/r/widget")); n != 1 || len(repoid.FolderWork(reg)["/p/notes"]) != 1 {
		t.Fatalf("work: %v %v", repoid.WorkFor(reg, "/r/widget"), repoid.FolderWork(reg))
	}
	u := dream.LoadToolUsage(filepath.Join(platform.StateDir(home), dream.ToolsFile))
	if !u.Used("s1", "mcp__*__Ticketing*") || len(u.Sessions["s1"].Commands) != 1 {
		t.Fatalf("tools %+v", u.Sessions)
	}
}
