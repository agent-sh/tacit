package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// The real pre-receive hook stops at the first offending file, so a second
// bad repo shows up only after the first is dropped. Two bad repos among four
// cost three pushes (all, then without the first, then without the second),
// not one per repo, and the good ones land in a single commit.
func TestTwoRefusedReposCostThreePushesAndOneCommit(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"a", "b", "c", "d"}})
	r.refuse(true, "a", "c")
	commits := r.commitCount()

	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 2 || rep.Count(derive.Written) != 2 {
		t.Fatalf("report %+v", rep.Repos)
	}
	for _, x := range rep.Repos {
		if x.Status == derive.Failed && !strings.Contains(x.Detail, "derived/"+x.Repo+"/") {
			t.Errorf("%s: the server's line is not in the report: %q", x.Repo, x.Detail)
		}
	}
	if got := r.serverDerived(); !strings.Contains(got, "derived/b/") || !strings.Contains(got, "derived/d/") || strings.Contains(got, "derived/a/") || strings.Contains(got, "derived/c/") {
		t.Fatalf("server holds %s", got)
	}
	if r.pushAttempts() != 3 || r.commitCount() != commits+1 {
		t.Fatalf("%d push attempts and %d new commits, want 3 and 1", r.pushAttempts(), r.commitCount()-commits)
	}
	r.assertClean("after two refusals")
	r.assertServerLintsClean()
}

// A refusal that names no repo is bisected: the halves are pushed on their
// own until the bad repo stands alone. Ten repos, one bad, still end with the
// nine pushed and the tenth reported, in a bounded number of pushes.
func TestARefusalThatNamesNoRepoIsBisected(t *testing.T) {
	var names []string
	for _, n := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r0"} {
		names = append(names, n)
	}
	r := newRig(t, rigOpts{repos: names})
	r.refuse(false, "r6")

	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 1 || rep.Count(derive.Written) != 9 {
		t.Fatalf("report %+v", rep.Repos)
	}
	for _, x := range rep.Repos {
		if x.Status == derive.Failed && (x.Repo != "r6" || !strings.Contains(x.Detail, "tacit: refused")) {
			t.Fatalf("%+v", x)
		}
	}
	if got := r.serverDerived(); strings.Contains(got, "derived/r6/") || strings.Count(got, "-hot-spots.md") != 9 {
		t.Fatalf("server holds %s", got)
	}
	// 1 for everything, then the halves: at most 2 per level of a 10-leaf tree
	if n := r.pushAttempts(); n > 1+2*4 {
		t.Fatalf("%d push attempts for one bad repo in ten", n)
	}
	r.assertClean("after bisecting")
	r.assertServerLintsClean()
}

// ---- a dry run, --out and curate --dry-run do not fetch

// withOrigin gives the workstation's checkout an origin that is one commit ahead of
// the remote-tracking ref the checkout has, and returns that ref's sha.
func (r *rig) withOrigin(name string) (stale string) {
	r.t.Helper()
	dir := filepath.Join(r.projects, name)
	origin := filepath.Join(r.root, name+"-origin.git")
	gitIn(r.t, r.root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(r.t, dir, "remote", "add", "origin", origin)
	gitIn(r.t, dir, "push", "-q", "origin", "main")
	gitIn(r.t, dir, "fetch", "-q", "origin")
	stale = gitIn(r.t, dir, "rev-parse", "refs/remotes/origin/main")
	other := filepath.Join(r.root, name+"-other")
	gitIn(r.t, r.root, "clone", "-q", origin, other)
	gitIn(r.t, other, "config", "user.email", "o@o")
	gitIn(r.t, other, "config", "user.name", "o")
	os.WriteFile(filepath.Join(other, "src", "scheduler", "worker.go"), []byte("package scheduler // newer\n"), 0o644)
	gitIn(r.t, other, "commit", "-q", "-am", "newer")
	gitIn(r.t, other, "push", "-q", "origin", "main")
	return stale
}

func TestDryRunOutAndCurateDryRunDoNotFetch(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	stale := r.withOrigin("demo")
	demo := filepath.Join(r.projects, "demo")
	tracking := func() string { return gitIn(t, demo, "rev-parse", "refs/remotes/origin/main") }

	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("TACIT_PROJECTS", r.projects)
	t.Setenv("TACIT_DERIVE_CONFIG", filepath.Join(r.root, "none.yaml"))
	t.Setenv("HOME", filepath.Join(r.root, "home"))

	if code := runDerive([]string{"--dry-run", "--projects", r.projects}); code != 0 {
		t.Fatalf("--dry-run exit %d", code)
	}
	if tracking() != stale {
		t.Fatal("--dry-run fetched")
	}
	out := filepath.Join(r.root, "out")
	if code := runDerive([]string{"--out", out, "--projects", r.projects}); code != 0 {
		t.Fatalf("--out exit %d", code)
	}
	if tracking() != stale {
		t.Fatal("--out fetched")
	}
	if _, err := os.Stat(filepath.Join(out, "derived", "demo", "demo-hot-spots.md")); err != nil {
		t.Fatal("--out wrote nothing")
	}
	if code := runCurate([]string{"--dry-run"}); code != 0 {
		t.Fatalf("curate --dry-run exit %d", code)
	}
	if tracking() != stale {
		t.Fatal("curate --dry-run fetched")
	}
	// control: the real run fetches, reads the newer commit and pushes it
	if code := runDerive([]string{"--projects", r.projects}); code != 0 {
		t.Fatalf("live exit %d", code)
	}
	if tracking() == stale {
		t.Fatal("the live run did not fetch")
	}
	if !strings.Contains(gitIn(t, r.bare, "show", "main:derived/demo/demo-module-map.md"), derive.Sha12(tracking())) {
		t.Fatal("the pushed fact was not read at the fetched commit")
	}
}

// The usage text lists every flag of `tacit derive`.
func TestDeriveUsageListsEveryFlag(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--dry-run", "--out", "--repo", "--force", "--take-over", "--no-fetch", "--set-branch", "--projects"} {
		if !strings.Contains(string(b), flag) {
			t.Errorf("usage does not mention %s", flag)
		}
	}
}

// ---- cleaning what a killed run left touches only generated files

// A killed run left generated facts on disk. Next to them the owner has an
// untracked hand-made file, an untracked hand-made fact, and a committed
// hand-made file with an edit. The next run removes the generated dirt, ships
// the facts, and leaves every hand-made file exactly as it was.
func TestCleaningAKilledRunsDirtLeavesHandMadeFilesAlone(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	// a committed hand-made file under derived/, then edited
	handCommitted := filepath.Join(r.clone, "derived", "demo", "committed-notes.md")
	os.MkdirAll(filepath.Dir(handCommitted), 0o755)
	os.WriteFile(handCommitted, []byte("---\nname: committed-notes\n---\nwritten by hand\n"), 0o644)
	gitIn(t, r.clone, "add", "-A")
	gitIn(t, r.clone, "commit", "-q", "-m", "a hand-made file")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	edited := "---\nname: committed-notes\n---\nwritten by hand, then edited\n"
	os.WriteFile(handCommitted, []byte(edited), 0o644)

	untrackedNote := filepath.Join(r.clone, "derived", "demo", "scratch.txt")
	os.WriteFile(untrackedNote, []byte("my notes\n"), 0o644)
	untrackedFact := filepath.Join(r.clone, "derived", "demo", "hand-fact.md")
	handFact := "---\nname: hand-fact\ndescription: mine\ntype: structure\nscope: derived/demo\nsource:\n  harness: human\n  host: host-a\n  kind: user\ncreated: \"2026-10-01\"\n---\n\nby hand\n"
	os.WriteFile(untrackedFact, []byte(handFact), 0o644)

	// the killed run's dirt: generated facts and a regenerated index
	rep, err := derive.Run(ctxMinute(t), r.opts())
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Apply(ctxMinute(t), r.clone); err != nil {
		t.Fatal(err)
	}
	all, _, _ := fact.Walk(r.clone)
	writeIndexesForTest(t, r.clone, all)

	rep, err = r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 || !strings.Contains(r.serverDerived(), "derived/demo/demo-hot-spots.md") {
		t.Fatalf("the facts did not ship: %+v / %s", rep.Repos, r.serverDerived())
	}
	for path, want := range map[string]string{handCommitted: edited, untrackedNote: "my notes\n", untrackedFact: handFact} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s was changed or removed: %v %q", filepath.Base(path), err, got)
		}
	}
	if strings.Contains(r.serverDerived(), "hand-fact") || strings.Contains(r.serverDerived(), "scratch") {
		t.Fatalf("a hand-made file was pushed: %s", r.serverDerived())
	}
	// no generated dirt is left: only the hand-made files show in status
	st := gitIn(t, r.clone, "status", "--porcelain", "--untracked-files=all")
	for _, line := range strings.Split(st, "\n") {
		if line != "" && !strings.Contains(line, "committed-notes.md") && !strings.Contains(line, "scratch.txt") && !strings.Contains(line, "hand-fact.md") {
			t.Fatalf("dirt left behind: %q\n%s", line, st)
		}
	}
}
