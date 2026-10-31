package gitev

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// token builds a GitHub-shaped token at run time so no secret-shaped literal
// sits in the source.
func token() string { return "gh" + "p_" + "aB3dE5gH7jK9mN2pQ4rS6tU8vW0xY1zA3bC5" }

// Redaction runs before any text is clipped. A secret cut in half by the
// 90-rune subject clip, the 24-rune name clip or the 51-byte file clip is no
// longer recognised, so the redactor must see it whole. This goes through
// the real redactor.
func TestRedactionRunsBeforeClipping(t *testing.T) {
	if err := digest.RedactionReady(); err != nil {
		t.Skip("scrump with the text profile is not available:", err)
	}
	sec := token()
	// Premise: a clipped secret slips through the redactor.
	clipped := clip(strings.Repeat("s", 67)+" fix "+sec, 90)
	if out, err := digest.RedactAll([]string{clipped}); err != nil || !strings.Contains(out[0], sec[:12]) {
		t.Skip("the redactor masks a clipped token too; the premise of this test is gone")
	}
	root := t.TempDir()
	d := newRepo(t, root, "leaky")
	commit(t, d, "2026-09-01", "Dana", "Dana", "init", map[string]string{"secrets-dir/seed.md": "x\n"})
	dirty := "secrets-dir/" + sec + "/" + strings.Repeat("d", 30) + ".md"
	commit(t, d, "2026-09-20", "Tok "+sec, "Dana", strings.Repeat("s", 67)+" fix "+sec, map[string]string{dirty: "y\n"})
	fa := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "projects/leaky", Created: "2026-09-10", AppliesTo: fact.AppliesTo{Paths: []string{"secrets-dir/**"}}}, Body: "x"}
	fb := &fact.Fact{Meta: fact.Meta{Name: "b", Scope: "projects/leaky", Created: "2026-09-11"}, Body: "y"}
	in := Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}}
	b := &Builder{G: New(root)} // the default redactor
	tl, err := b.Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines(tl), "\n")
	if indexOf(lines(tl), "fix *") < 0 || indexOf(lines(tl), "Tok ") < 0 || indexOf(lines(tl), "dddddddd") < 0 {
		t.Fatalf("the commit is not in the timeline, so nothing was tested:\n%s", all)
	}
	for _, leak := range []string{"ghp_", sec[:12], sec[len(sec)-12:]} {
		if strings.Contains(all, leak) {
			t.Errorf("%q leaked into the timeline:\n%s", leak, all)
		}
	}
	// The structured evidence is redacted the same way.
	_, _, ev, err := b.Evidence(context.Background(), in)
	if err != nil || len(ev) == 0 || len(ev[0].Commits) == 0 {
		t.Fatalf("evidence %+v err %v", ev, err)
	}
	for _, c := range ev[0].Commits {
		for _, s := range append([]string{c.Subject, c.Author.Name, c.Author.Email}, c.Files...) {
			if strings.Contains(s, "ghp_") || strings.Contains(s, sec[:12]) {
				t.Errorf("evidence leaks %q", s)
			}
		}
	}
}

// Evidence is redacted inside and fails closed.
func TestEvidenceIsRedactedAndFailsClosed(t *testing.T) {
	f := newFixture(t)
	b := f.builder()
	b.Redact = func(in []string) ([]string, error) {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ReplaceAll(s, "SECRETVALUE123", "**************")
		}
		return out, nil
	}
	_, _, ev, err := b.Evidence(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, c := range ev[0].Commits {
		if strings.Contains(c.Subject, "SECRETVALUE123") {
			t.Errorf("unredacted subject in evidence: %q", c.Subject)
		}
		seen = seen || strings.Contains(c.Subject, "**************")
	}
	if !seen {
		t.Errorf("the commit with the secret is not in the evidence: %+v", ev[0].Commits)
	}
	b.Redact = func([]string) ([]string, error) { return nil, errors.New("scrump is down") }
	if _, _, ev, err := b.Evidence(context.Background(), f.input()); err == nil || ev != nil {
		t.Errorf("evidence came out without redaction: %v %v", ev, err)
	}
}

// "." and ".." are not paths, and a pathspec git refuses must not hide what
// the others show.
func TestBadPathsDoNotHideTheRealOnes(t *testing.T) {
	f := newFixture(t)
	body := "Owner, 2026-09-12: the hook `tools/hooks/pre-commit` and also `../shared/notes.md` and `./../x/y.md` and `a/../b.md`."
	a := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "projects/gearbox", Created: "2026-10-08", AppliesTo: fact.AppliesTo{Paths: []string{"tools/hooks/pre-commit", "../escape.md"}}}, Body: body}
	s := f.g.SubjectOf(a)
	for _, p := range s.Paths {
		if strings.Contains(p, "..") {
			t.Errorf("a path with .. was kept: %q in %v", p, s.Paths)
		}
	}
	tl, err := f.builder().Build(context.Background(), Input{A: FactRef{Fact: a}, B: FactRef{Fact: f.b}})
	if err != nil {
		t.Fatal(err)
	}
	ls := lines(tl)
	if indexOf(ls, "later: touch the hook again") < 0 || indexOf(ls, "present: tools/hooks/pre-commit") < 0 || tl.Incomplete {
		t.Errorf("the hook commit is missing or the read is incomplete:\n%s", strings.Join(ls, "\n"))
	}
	// Even when a refused pathspec reaches the query (it bypasses the parser),
	// the others are still asked one by one and the refusal is a note.
	ev := f.g.Collect(context.Background(), Subject{Repos: []string{"gearbox"}, Paths: []string{"../escape.md", "tools/hooks/pre-commit"}}, Window{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Anchor: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)})[0]
	if !ev.Incomplete || indexOf(ev.Notes, "refused by git") < 0 {
		t.Errorf("a refused path is not reported: %+v", ev.Notes)
	}
	var hook bool
	for _, c := range ev.Commits {
		hook = hook || strings.Contains(c.Subject, "pre-commit hook") || strings.Contains(c.Subject, "touch the hook")
	}
	if !hook {
		t.Errorf("commits on the good path are gone: %+v", ev.Commits)
	}
}

// realGit is the git on PATH before any test puts a shim in front of it.
var realGit, _ = exec.LookPath("git")

// shim puts a git in front of PATH that runs script first and then the real git.
func shim(t *testing.T, script string) {
	t.Helper()
	if realGit == "" {
		t.Skip("no git")
	}
	real := realGit
	dir := t.TempDir()
	body := "#!/bin/sh\n" + script + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A git that hangs is killed with its children at the timeout, and a command
// that writes too much is stopped at the cap with what it printed so far.
func TestRunStopsAHungGitAndCapsOutput(t *testing.T) {
	f := newFixture(t)
	shim(t, `case "$*" in *hang-forever*) sleep 30 & wait ;; esac`)
	g := New(f.root)
	g.Timeout = 400 * time.Millisecond
	start := time.Now()
	_, trunc, err := g.run(context.Background(), filepath.Join(f.root, "gearbox"), 1<<10, "hang-forever")
	if !errors.Is(err, context.DeadlineExceeded) || !trunc {
		t.Errorf("a hung git returned %v (truncated %v)", err, trunc)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the hung git held the caller for %v", d)
	}
	g.Timeout = 15 * time.Second
	out, trunc, err := g.run(context.Background(), filepath.Join(f.root, "gearbox"), 50, "log", "--format=%H%n%s")
	if err != nil || !trunc || len(out) > 50 || len(out) == 0 {
		t.Errorf("capped output: %d bytes, truncated %v, err %v", len(out), trunc, err)
	}
}

// A read that timed out is marked incomplete, says so, and is not written to
// the disk cache: the next build, with a healthy git, reads again and keeps it.
func TestTimedOutReadIsNotCached(t *testing.T) {
	f := newFixture(t)
	f.g.CacheDir = filepath.Join(t.TempDir(), "cache")
	f.g.Timeout = time.Second
	shim(t, `case "$*" in *"rev-list --count"*) sleep 5 ;; esac`)
	b := f.builder()
	tl, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	if !tl.Incomplete || indexOf(lines(tl), "commit count failed: timed out") < 0 {
		t.Fatalf("a timed-out count is not reported:\n%s", strings.Join(lines(tl), "\n"))
	}
	if files, _ := filepath.Glob(filepath.Join(f.g.CacheDir, "*.json")); len(files) != 0 {
		t.Fatalf("an incomplete read was cached: %v", files)
	}
	// Healthy git again (the shim's PATH entry is gone with the next shim).
	shim(t, `:`)
	f.g.Timeout = 15 * time.Second
	tl, err = b.Build(context.Background(), f.input())
	if err != nil || tl.Incomplete {
		t.Fatalf("second build: incomplete %v err %v", tl != nil && tl.Incomplete, err)
	}
	if i := indexOf(lines(tl), "HEAD gearbox"); i < 0 || strings.Contains(lines(tl)[i], ": 0 commits since") {
		t.Errorf("the count is still wrong:\n%s", strings.Join(lines(tl), "\n"))
	}
	if files, _ := filepath.Glob(filepath.Join(f.g.CacheDir, "*.json")); len(files) == 0 {
		t.Errorf("a complete read was not cached")
	}
}

// partialClone makes a blob:none clone named pc under a fresh projects
// directory and returns the directory and a counter of the files in the
// clone's objects/pack (a fetch adds some).
func partialClone(t *testing.T) (projects string, packs func() int) {
	t.Helper()
	root := t.TempDir()
	origin := newRepo(t, root, "origin-src")
	run(t, origin, nil, "config", "uploadpack.allowFilter", "true")
	run(t, origin, nil, "config", "uploadpack.allowAnySHA1InWant", "true")
	commit(t, origin, "2026-09-01", "Dana", "Dana", "add the loop", map[string]string{"src/loop.rs": "const KEEP_ALIVE_MODE: u8 = 1;\n"})
	commit(t, origin, "2026-09-20", "Dana", "Dana", "change the loop", map[string]string{"src/loop.rs": "const KEEP_ALIVE_MODE: u8 = 2;\n"})
	projects = filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	cmd := exec.Command("git", "clone", "-q", "--no-checkout", "--filter=blob:none", "file://"+origin, filepath.Join(projects, "pc"))
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skip("cannot make a blob:none clone with this git:", err, string(out))
	}
	return projects, func() int {
		p, _ := filepath.Glob(filepath.Join(projects, "pc", ".git", "objects", "pack", "*"))
		return len(p)
	}
}

func pcInput() Input {
	fa := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "projects/pc", Created: "2026-09-10", AppliesTo: fact.AppliesTo{Symbols: []string{"KEEP_ALIVE_MODE"}}}, Body: "x"}
	fb := &fact.Fact{Meta: fact.Meta{Name: "b", Scope: "projects/pc", Created: "2026-09-11"}, Body: "y"}
	return Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}}
}

// A partial clone's missing files are not fetched, the queries that need them
// are reported, and a term whose search failed is not called "not present".
func TestPartialCloneIsNotFetched(t *testing.T) {
	projects, packs := partialClone(t)
	before := packs()
	b := &Builder{G: New(projects), Redact: func(in []string) ([]string, error) { return in, nil }}
	tl, err := b.Build(context.Background(), pcInput())
	if err != nil {
		t.Fatal(err)
	}
	if after := packs(); after != before {
		t.Errorf("the queries fetched from the origin: %d files in objects/pack before, %d after", before, after)
	}
	ls := lines(tl)
	if !tl.Incomplete || indexOf(ls, "partial clone") < 0 {
		t.Errorf("a partial clone is not reported:\n%s", strings.Join(ls, "\n"))
	}
	if i := indexOf(ls, "not present:"); i >= 0 && strings.Contains(ls[i], "KEEP_ALIVE_MODE") {
		t.Errorf("a failed search was reported as not present:\n%s", strings.Join(ls, "\n"))
	}
}

// GIT_NO_LAZY_FETCH came in git 2.44. With an older git (or one whose version
// cannot be read) a partial clone is not read at all, because the queries
// would fetch into it; a normal repo is read as usual.
func TestOldGitDoesNotReadAPartialClone(t *testing.T) {
	projects, packs := partialClone(t)
	before := packs()
	g := New(projects)
	g.testVersion = "git version 2.43.1"
	b := &Builder{G: g, Redact: func(in []string) ([]string, error) { return in, nil }}
	runs := g.Runs()
	tl, err := b.Build(context.Background(), pcInput())
	if err != nil {
		t.Fatal(err)
	}
	ls := lines(tl)
	if !tl.Incomplete || indexOf(ls, "older than 2.44") < 0 || indexOf(ls, "commits since") >= 0 {
		t.Errorf("the partial clone was read or the skip is not reported:\n%s", strings.Join(ls, "\n"))
	}
	if after := packs(); after != before {
		t.Errorf("the old-git path fetched: %d files before, %d after", before, after)
	}
	// Only the partial check ran against the clone, no history query.
	if n := g.Runs() - runs; n > 4 {
		t.Errorf("%d git commands ran for a repo that is skipped", n)
	}
	// An unreadable version counts as old.
	g2 := New(projects)
	g2.testVersion = "something odd"
	if g2.noLazyFetch(context.Background()) {
		t.Error("an unreadable version was trusted")
	}
	g3 := New(projects)
	g3.testVersion = "git version 2.44.0"
	g4 := New(projects)
	g4.testVersion = "git version 3.0.1"
	if !g3.noLazyFetch(context.Background()) || !g4.noLazyFetch(context.Background()) {
		t.Error("git 2.44 and 3.0 honour GIT_NO_LAZY_FETCH")
	}
	// A repo that is not a partial clone is read with an old git as well.
	f := newFixture(t)
	f.g.testVersion = "git version 2.43.1"
	tl, err = f.builder().Build(context.Background(), f.input())
	if err != nil || tl.Incomplete || indexOf(lines(tl), "fix the cargo path") < 0 {
		t.Errorf("a normal repo was not read with an old git: %v\n%s", err, strings.Join(lines(tl), "\n"))
	}
}

// A build that is cancelled does not wait for the redactor: it stops with
// its context, so an abandoned request frees its slot at once.
func TestBuildStopsWithItsContextInsideTheRedactor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scrump"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t) // fixture before the PATH change: git must stay the real one
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Builder{G: f.g, StoreDir: f.store} // the default redactor
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	tl, err := b.Build(ctx, f.input())
	if err == nil || tl != nil || !strings.Contains(err.Error(), "redaction failed") {
		t.Fatalf("a build with a hung redactor returned %v, %v", tl, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the build waited %v for a redactor after its context ended", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error does not say the context ended: %v", err)
	}
}

// --as-of is a replay of the whole picture: the store history, the facts'
// dates and the repos are all cut at that time, and a repo is read along its
// first-parent line (a side branch merged later is not part of what it was).
func TestAsOfCutsStoreHistoryAndFactDates(t *testing.T) {
	f := newFixture(t)
	b := f.builder()
	b.AsOf = time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	tl, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines(tl) {
		if d, _, _ := strings.Cut(l, " "); d != "HEAD" && d > "2026-10-08" {
			t.Errorf("a date after the replay time: %s", l)
		}
	}
	if indexOf(lines(tl), "last change in store git") >= 0 {
		t.Errorf("a store commit made after the replay time is shown:\n%s", strings.Join(lines(tl), "\n"))
	}
	// Without --as-of the same store history is there.
	live, err := f.builder().Build(context.Background(), f.input())
	if err != nil || indexOf(lines(live), "last change in store git (2 commits)") < 0 {
		t.Fatalf("control: %v\n%s", err, strings.Join(lines(live), "\n"))
	}
}

func TestAsOfReadsTheFirstParentLine(t *testing.T) {
	root := t.TempDir()
	d := newRepo(t, root, "branchy")
	commit(t, d, "2026-09-01", "Dana", "Dana", "m1 mainline", map[string]string{"tools/a.sh": "1\n"})
	commit(t, d, "2026-09-02", "Dana", "Dana", "m2 mainline", map[string]string{"tools/a.sh": "2\n"})
	run(t, d, nil, "checkout", "-q", "-b", "side", "HEAD~1")
	commit(t, d, "2026-09-03", "Dana", "Dana", "s1 side", map[string]string{"tools/b.sh": "1\n"})
	run(t, d, nil, "checkout", "-q", "main")
	env := []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a", "GIT_AUTHOR_DATE=2026-09-06T12:00:00+00:00", "GIT_COMMITTER_DATE=2026-09-06T12:00:00+00:00"}
	run(t, d, env, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	fa := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "projects/branchy", Created: "2026-09-02", AppliesTo: fact.AppliesTo{Paths: []string{"tools/**"}}}, Body: "x"}
	fb := &fact.Fact{Meta: fact.Meta{Name: "b", Scope: "projects/branchy", Created: "2026-09-02"}, Body: "y"}
	b := &Builder{G: New(root), AsOf: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), Redact: func(in []string) ([]string, error) { return in, nil }}
	_, _, ev, err := b.Evidence(context.Background(), Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}})
	if err != nil || len(ev) != 1 {
		t.Fatalf("evidence %v err %v", ev, err)
	}
	var subjects []string
	for _, c := range ev[0].Commits {
		subjects = append(subjects, c.Subject)
	}
	if indexOf(subjects, "m2 mainline") < 0 {
		t.Errorf("the mainline commit before the replay time is hidden by a later merge: %v", subjects)
	}
}

// The disk cache holds at most CacheMaxFiles files; the oldest go first.
func TestCacheIsPruned(t *testing.T) {
	g := New(t.TempDir())
	g.CacheDir = filepath.Join(t.TempDir(), "cache")
	g.CacheMaxFiles = 8
	os.MkdirAll(g.CacheDir, 0o700)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		p := filepath.Join(g.CacheDir, "k"+strconv.Itoa(100+i)+".json")
		os.WriteFile(p, []byte("{}"), 0o600)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	g.pruneCache()
	left, _ := filepath.Glob(filepath.Join(g.CacheDir, "*.json"))
	if len(left) != 6 {
		t.Fatalf("%d files left, want 6 (three quarters of 8)", len(left))
	}
	if _, err := os.Stat(filepath.Join(g.CacheDir, "k119.json")); err != nil {
		t.Errorf("the newest file was removed")
	}
	if _, err := os.Stat(filepath.Join(g.CacheDir, "k100.json")); err == nil {
		t.Errorf("the oldest file stayed")
	}
	// A write past the limit prunes on the first put.
	g.CacheMaxFiles = 4
	g.cachePut("fresh", RepoEvidence{Name: "x"})
	left, _ = filepath.Glob(filepath.Join(g.CacheDir, "*.json"))
	if len(left) > 4 {
		t.Errorf("%d files after a put with a limit of 4", len(left))
	}
}
