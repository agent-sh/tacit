package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/stale"
)

// staleIDOf is the id and newest commit of a fact's stale finding.
func staleIDOf(t *testing.T, rs *reviewServer, name string) (id, sha string) {
	t.Helper()
	rs.loadFacts()
	for _, it := range rs.staleItems() {
		if it.A.Name == name {
			return it.ID, it.Flag.Sha
		}
	}
	t.Fatalf("no stale finding for %s", name)
	return
}

func flagFixture(t *testing.T) (*staleFx, context.Context, string, string) {
	t.Helper()
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	id, sha := staleIDOf(t, fx.rs, "engine-retries")
	return fx, ctx, id, sha
}

func commitStore(t *testing.T, dir, msg string, rels ...string) {
	t.Helper()
	git(t, dir, append([]string{"add", "--"}, rels...)...)
	git(t, dir, "commit", "-q", "-m", msg)
	git(t, dir, "push", "-q", "origin", "HEAD:main")
}

// Keeping one fact over another in a pair is a word on both facts. An agent
// may not retire a fact the owner kept, the way it may not in the pair flow.
func TestAnAgentMayNotRetireAFactTheOwnerKeptInAPair(t *testing.T) {
	fx, ctx, id, sha := flagFixture(t)
	dir := fx.rs.s.Dir
	os.MkdirAll(filepath.Join(dir, "review"), 0o755)
	line := `{"id":"abc123abc123","action":"left","a":"engine-retries","b":"steady-keepalive","at":"2026-10-08T07:00:00Z"}` + "\n"
	fh, err := os.OpenFile(filepath.Join(dir, "review", "decisions.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(line)
	fh.Close()
	commitStore(t, dir, "owner kept engine-retries", "review/decisions.jsonl")
	path := filepath.Join(dir, "projects", "x", "engine-retries.md")
	orig := mustRead(t, path)
	_, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, A: "engine-retries", B: sha, Mark: "retire", By: "agent", Evidence: "git deleted the name"}})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "owner") {
		t.Fatalf("an agent retired a fact the owner kept: %v", errs[0])
	}
	if string(mustRead(t, path)) != string(orig) {
		t.Fatal("the fact was written")
	}
	// The owner's own mark still works.
	if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, Mark: "current"}}); err != nil || errs[0] != nil {
		t.Fatalf("owner mark: %v %v", err, errs)
	}
}

// The guard on an agent's marks lives in the op, not in whoever builds it: only
// current, retire, fix and ask get past it, and a reopen with nothing to reopen
// is an error, not a panic.
func TestStaleAgentMarksAreWhitelistedAndAReopenOfNothingIsRefused(t *testing.T) {
	fx, ctx, id, sha := flagFixture(t)
	path := filepath.Join(fx.rs.s.Dir, "projects", "x", "engine-retries.md")
	orig := mustRead(t, path)
	for _, m := range []string{"reopen", "both", "left", "right", "query", "skip", "wrong", "bogus", ""} {
		_, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, A: "engine-retries", B: sha, Mark: m, By: "agent", Evidence: "x", Question: "q"}})
		if err != nil || errs[0] == nil {
			t.Errorf("agent mark %q: err %v errs %v, want a refusal", m, err, errs)
		}
	}
	if string(mustRead(t, path)) != string(orig) {
		t.Fatal("a refused mark wrote the fact")
	}
	if got := staleItemByName(t, fx.rs, "engine-retries"); got.Decision != nil || got.Ask != nil {
		t.Fatalf("a refused mark left a record: %+v %+v", got.Decision, got.Ask)
	}
	// The owner reopening a finding with no mark is refused too.
	if _, errs, _ := fx.rs.applyOps(ctx, []op{{ID: id, Mark: "reopen"}}); errs[0] == nil {
		t.Fatal("reopen with no mark was accepted")
	}
}

func staleItemByName(t *testing.T, rs *reviewServer, name string) staleItem {
	t.Helper()
	rs.loadFacts()
	for _, it := range rs.staleItems() {
		if it.A.Name == name {
			return it
		}
	}
	t.Fatalf("no stale item for %s", name)
	return staleItem{}
}

// An agent's fix is closed when the fact is edited after the verdict, and an
// open fix does not stop the check: a later deletion makes a new finding that
// carries the earlier fix.
func TestAFixHasACompletionPathAndDoesNotBlockTheCheck(t *testing.T) {
	fx, ctx, id, sha := flagFixture(t)
	if _, errs, err := fx.rs.applyOps(ctx, []op{{ID: id, A: "engine-retries", B: sha, Mark: "fix", By: "agent", Evidence: "MAX_RETRIES is RETRY_LIMIT now. Fix needed: say RETRY_LIMIT"}}); err != nil || errs[0] != nil {
		t.Fatalf("fix: %v %v", err, errs)
	}
	it := staleItemByName(t, fx.rs, "engine-retries")
	if !it.NeedsOwner || it.Applied {
		t.Fatalf("a fix nobody applied: needs owner %v applied %v", it.NeedsOwner, it.Applied)
	}
	// The fact is still a candidate: the fix does not hold the check back.
	ins, open := staleCandidates(fx.rs, []string{"engine-retries"})
	if len(ins) != 1 || open != 0 || ins[0].LastSha != sha {
		t.Fatalf("candidates %d open %d last %q", len(ins), open, ins[0].LastSha)
	}
	// A second deletion flags a new finding, and the page still shows the fix.
	fx.changeBackoff(t, time.Now().AddDate(0, 0, 1).Format("2006-01-02"))
	rep, err := staleStep(ctx, fx.rs.s, fx.opts())
	if err != nil || flaggedNames(rep.Findings) != "engine-retries" {
		t.Fatalf("a deletion after an open fix: %q %v", flaggedNames(rep.Findings), err)
	}
	again := staleItemByName(t, fx.rs, "engine-retries")
	if again.ID == id || again.Prior == nil || !strings.Contains(again.Prior.Evidence, "Fix needed: say RETRY_LIMIT") {
		t.Fatalf("new finding %s (old %s), prior %+v", again.ID, id, again.Prior)
	}
	// Applying the fix, an edit of the file after the verdict, closes the old
	// finding's claim on the owner: here the old finding is replaced, so check
	// the completion on a fixture with one finding.
	fx2, ctx2, id2, sha2 := flagFixture(t)
	if _, errs, err := fx2.rs.applyOps(ctx2, []op{{ID: id2, A: "engine-retries", B: sha2, Mark: "fix", By: "agent", Evidence: "Fix needed: say RETRY_LIMIT"}}); err != nil || errs[0] != nil {
		t.Fatal(err, errs)
	}
	time.Sleep(1100 * time.Millisecond) // the clone's commit time must be after the verdict's second
	p := filepath.Join(fx2.rs.s.Dir, "projects", "x", "engine-retries.md")
	b := mustRead(t, p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "`MAX_RETRIES`", "`RETRY_LIMIT`", 1)), 0o644)
	commitStore(t, fx2.rs.s.Dir, "apply the fix", "projects/x/engine-retries.md")
	done := staleItemByName(t, fx2.rs, "engine-retries")
	if !done.Applied || done.NeedsOwner || done.AppliedAt == "" {
		t.Fatalf("an applied fix: applied %v needs owner %v at %q", done.Applied, done.NeedsOwner, done.AppliedAt)
	}
	// The applied fix moves the fact's date: the deletion it answered is behind it.
	ins2, _ := staleCandidates(fx2.rs, []string{"engine-retries"})
	if len(ins2) != 1 || ins2[0].Verified.IsZero() {
		t.Fatalf("an applied fix did not date the fact: %+v", ins2)
	}
}

// failingGit puts a git on PATH that fails every command naming one of fail.
func failingGit(t *testing.T, fail ...string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	var cases []string
	for _, w := range fail {
		cases = append(cases, `"`+w+`"`)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in " + strings.Join(cases, "|") + ") echo \"fatal: injected failure\" >&2; exit 128;; esac; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRetryAfterBacksOffAndStopsAtAWeek(t *testing.T) {
	want := []time.Duration{20 * time.Hour, 40 * time.Hour, 80 * time.Hour, 160 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour}
	for i, w := range want {
		if got := retryAfter(i + 1); got != w {
			t.Errorf("retryAfter(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// A fact whose read does not finish is not recorded as checked, is retried the
// next day, then waits longer each time, and is checked and forgotten once it
// finishes.
func TestAFactThatNeverFinishesBacksOffInsteadOfSittingAtTheFront(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	statePath := filepath.Join(t.TempDir(), "stale.json")
	now := time.Date(2026, 10, 11, 5, 30, 0, 0, time.UTC)
	o := fx.opts()
	o.StatePath = statePath
	o.Now = func() time.Time { return now }
	ctx := context.Background()

	realPath := os.Getenv("PATH")
	failingGit(t, "ls-tree", "grep") // every tree read fails
	rep, err := staleStep(ctx, fx.rs.s, o)
	if err != nil || len(rep.Findings) != 0 || rep.Skips[stale.SkipIncomplete] != 3 {
		t.Fatalf("first run: %v findings %d skips %v", err, len(rep.Findings), rep.Skips)
	}
	st := loadStaleState(statePath)
	if len(st.Checked) != 0 || len(st.Incomplete) != 3 || st.Incomplete["engine-retries"].N != 1 {
		t.Fatalf("state after a failed read: checked %v incomplete %v", st.Checked, st.Incomplete)
	}
	// The same day: nothing is read for them.
	t.Setenv("PATH", realPath)
	rep, err = staleStep(ctx, fx.rs.s, o)
	if err != nil || rep.BackedOff != 3 || len(rep.Results) != 0 {
		t.Fatalf("second run: %v backed off %d results %d", err, rep.BackedOff, len(rep.Results))
	}
	// A day later they are tried again, finish, flag and are forgotten.
	now = now.Add(24 * time.Hour)
	rep, err = staleStep(ctx, fx.rs.s, o)
	if err != nil || rep.BackedOff != 0 || flaggedNames(rep.Findings) != "engine-retries,owner-rule" {
		t.Fatalf("third run: %v backed off %d findings %q skips %v", err, rep.BackedOff, flaggedNames(rep.Findings), rep.Skips)
	}
	if st := loadStaleState(statePath); len(st.Incomplete) != 0 || len(st.Checked) != 3 {
		t.Fatalf("state after it finished: checked %v incomplete %v", st.Checked, st.Incomplete)
	}
}

// A repo is read at its default branch, or the one derive.yaml names, not at
// whatever the checkout has out: on branch wip a file is gone that origin/main
// still has.
func TestTheStepReadsTheDefaultBranchNotTheCheckout(t *testing.T) {
	requireScrump(t)
	fx := newStaleFixture(t)
	dir := fx.rs.s.Dir
	if err := os.WriteFile(filepath.Join(dir, "projects", "x", "old-module.md"), []byte(staleFact("old-module", "observed", "machine_fact", "The helper lives in `src/old.rs`.")), 0o644); err != nil {
		t.Fatal(err)
	}
	commitStore(t, dir, "old-module", "projects/x/old-module.md")
	// Repo x: old.rs on main, pushed to an origin; the checkout moves to wip,
	// where it is deleted.
	commitDay(t, fx.repo, "2026-09-25", "x: add old.rs", map[string]string{"src/old.rs": "a\n"})
	origin := filepath.Join(fx.root, "xorigin.git")
	git(t, fx.root, "clone", "-q", "--bare", fx.repo, origin)
	git(t, fx.repo, "remote", "add", "origin", origin)
	git(t, fx.repo, "fetch", "-q", "origin")
	git(t, fx.repo, "remote", "set-head", "origin", "main")
	git(t, fx.repo, "checkout", "-q", "-b", "wip")
	gitDay(t, fx.repo, "2026-10-06", "rm", "-q", "src/old.rs")
	gitDay(t, fx.repo, "2026-10-06", "commit", "-q", "-m", "wip: drop old.rs")

	cfg := filepath.Join(t.TempDir(), "derive.yaml")
	t.Setenv("TACIT_DERIVE_CONFIG", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx := context.Background()
	o := staleOptions{Projects: fx.projects, Dry: true, Workers: 2, Only: []string{"old-module"}}
	rep := computeStale(ctx, fx.rs.s, o)
	if rep.fatal != nil || len(rep.Findings) != 0 || rep.Skips[stale.SkipQuiet] != 1 {
		t.Fatalf("read at the checkout's branch: fatal %v findings %q skips %v", rep.fatal, flaggedNames(rep.Findings), rep.Skips)
	}
	// derive.yaml names wip for x: the same read now sees the deletion.
	if err := derive.SetBranch(cfg, "x", "wip"); err != nil {
		t.Fatal(err)
	}
	rep = computeStale(ctx, fx.rs.s, o)
	if rep.fatal != nil || flaggedNames(rep.Findings) != "old-module" {
		t.Fatalf("read at the configured branch: fatal %v findings %q skips %v", rep.fatal, flaggedNames(rep.Findings), rep.Skips)
	}
	// A config that does not parse stops the step.
	os.WriteFile(cfg, []byte("branches: [not a map\n"), 0o644)
	if rep := computeStale(ctx, fx.rs.s, o); rep.fatal == nil {
		t.Fatal("an unreadable derive.yaml did not stop the step")
	}
}
