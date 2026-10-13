package gitev

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func TestSpecific(t *testing.T) {
	for _, id := range []string{"MAX_RETRIES", "--max-batches", "hyper_range_decode_ws", "ParseConfig", "std::fs", "gitev3", "ALLCAPS", "my-flag"} {
		if !Specific(id) {
			t.Errorf("%q should name something", id)
		}
	}
	for _, id := range []string{"suspended", "replay", "Engine", "run", "cluster", ""} {
		if Specific(id) {
			t.Errorf("%q is a plain word", id)
		}
	}
}

func TestTermsRemovedIgnoresLinesThatOnlyMoved(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.md b/a.md", "--- a/a.md", "+++ b/a.md",
		"@@ -1,3 +0,0 @@",
		"-| FOO_MODE | on | moved row |",
		"-const MAX_RETRIES: u32 = 3;",
		"-other line",
		"diff --git a/b.md b/b.md", "--- a/b.md", "+++ b/b.md",
		"@@ -0,0 +1,3 @@",
		"+  | FOO_MODE | on | moved row |",
		"+const MAX_RETRIES: u32 = 5;",
		"+NEW_NAME is added",
	}, "\n")
	terms := []string{"FOO_MODE", "MAX_RETRIES", "NEW_NAME"}
	if got := termsRemoved(diff, terms); strings.Join(got, ",") != "MAX_RETRIES" {
		t.Errorf("removed or rewrote %v, want only MAX_RETRIES (FOO_MODE moved, NEW_NAME was added)", got)
	}
	if got := termsIn(diff, terms); strings.Join(got, ",") != "FOO_MODE,MAX_RETRIES,NEW_NAME" {
		t.Errorf("terms in the diff %v", got)
	}
}

// storeWithFact is a git repo holding one fact file, committed on each given day.
func storeWithFact(t *testing.T, days ...string) (dir string, f *fact.Fact) {
	dir = t.TempDir()
	run(t, dir, nil, "init", "-q", "-b", "main")
	rel := "projects/x/engine.md"
	for i, day := range days {
		os.MkdirAll(filepath.Join(dir, "projects/x"), 0o755)
		os.WriteFile(filepath.Join(dir, rel), []byte("revision "+string(rune('a'+i))+"\n"), 0o644)
		run(t, dir, nil, "add", "-A")
		when := day + "T12:00:00+00:00"
		run(t, dir, []string{"GIT_AUTHOR_NAME=S", "GIT_AUTHOR_EMAIL=s@e.test", "GIT_AUTHOR_DATE=" + when, "GIT_COMMITTER_NAME=S", "GIT_COMMITTER_EMAIL=s@e.test", "GIT_COMMITTER_DATE=" + when}, "commit", "-q", "-m", "fact")
	}
	f = &fact.Fact{Path: rel, Meta: fact.Meta{Name: "engine", Created: "2026-09-01", Scope: "projects/x", Source: fact.Source{Harness: "curator", Kind: "observed"}}, Body: "The engine is fine.\n"}
	return dir, f
}

// A fact is current as of the date its text says it is from, else its source
// note, else the day it was filed; a later edit (an `updated` day, or a second
// commit of the file in the store) moves it forward.
func TestEffectiveDate(t *testing.T) {
	ctx := context.Background()
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }

	dir, f := storeWithFact(t, "2026-09-10")
	b := &Builder{G: New(t.TempDir()), StoreDir: dir, Redact: func(in []string) ([]string, error) { return in, nil }}
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day("2026-09-01")) || e.Why != "created" {
		t.Errorf("filed once, no dates in the text: %+v", e)
	}
	f.Body = "As measured on 2026-08-20, the engine is fine.\n"
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day("2026-08-20")) || e.Why != "text" {
		t.Errorf("a date in the text: %+v", e)
	}
	note := &fact.Fact{Meta: fact.Meta{Name: "note", Created: "2026-05-02", Source: fact.Source{Harness: "import", Kind: "observed"}}}
	f.Body = "The engine is fine.\n"
	if e := b.Effective(ctx, FactRef{Fact: f, Origin: note}); !e.At.Equal(day("2026-05-02")) || e.Why != "source" {
		t.Errorf("filed from an old note: %+v", e)
	}
	f.Meta.Updated = "2026-09-05"
	if e := b.Effective(ctx, FactRef{Fact: f}); !e.At.Equal(day("2026-09-05")) || e.Why != "updated" {
		t.Errorf("updated after creation: %+v", e)
	}
	// The same file committed again in the store: the later commit's day.
	dir2, f2 := storeWithFact(t, "2026-09-10", "2026-09-20")
	b2 := &Builder{G: New(t.TempDir()), StoreDir: dir2, Redact: b.Redact}
	if e := b2.Effective(ctx, FactRef{Fact: f2}); !e.At.Equal(day("2026-09-20")) || e.Why != "store" {
		t.Errorf("committed twice: %+v", e)
	}
	// A replay does not see a store commit made after its time.
	b2.AsOf = day("2026-09-15")
	if e := b2.Effective(ctx, FactRef{Fact: f2}); !e.At.Equal(day("2026-09-01")) {
		t.Errorf("replay before the second commit: %+v", e)
	}
	// No usable date, no effective date.
	f3 := &fact.Fact{Path: "x.md", Meta: fact.Meta{Name: "x"}}
	if e := b.Effective(ctx, FactRef{Fact: f3}); !e.At.IsZero() {
		t.Errorf("undated: %+v", e)
	}
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// commitAt is commit with an author date and a committer date of its own: a
// rebased or cherry-picked commit reaches the repo long after it was written.
func commitAt(t *testing.T, dir, authored, committed, msg string, files map[string]string, rm ...string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range rm {
		os.Remove(filepath.Join(dir, name))
	}
	run(t, dir, nil, "add", "-A")
	run(t, dir, []string{"GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=d@e.test", "GIT_AUTHOR_DATE=" + authored + "T12:00:00+00:00",
		"GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=d@e.test", "GIT_COMMITTER_DATE=" + committed + "T12:00:00+00:00"}, "commit", "-q", "-m", msg)
}

func noRedaction(in []string) ([]string, error) { return in, nil }

func lightFact(body string) *fact.Fact {
	return &fact.Fact{Path: "projects/x/e.md", Meta: fact.Meta{Name: "e", Scope: "projects/x", Created: "2026-10-01"}, Body: body}
}

const engineBody = "const MAX_RETRIES: u32 = 3;\nconst BACKOFF_MS: u32 = 200;\nfn run() { retry(MAX_RETRIES); sleep(BACKOFF_MS); }\nfn suspended() {}\n"

// A light read searches history only for the names HEAD no longer holds, so
// the commit that removed one is not lost among the many that merely touch a
// name that stands: fifteen commits tune BACKOFF_MS after MAX_RETRIES is
// renamed away, and the rename is still found. Plain words are not searched,
// a line that moved does not count, an added caller does not count.
func TestReadSinceFindsTheCommitThatRemovedAName(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/engine.rs": engineBody, "docs/NOTES.md": "# notes\n| MAX_RETRIES | 3 |\n"})
	commit(t, repo, "2026-10-03", "Dev", "Dev", "add a caller", map[string]string{"src/engine.rs": engineBody + "fn again() { retry(MAX_RETRIES); }\n"})
	commit(t, repo, "2026-10-04", "Dev", "Dev", "move the notes row", map[string]string{"docs/NOTES.md": "# notes\n\n| MAX_RETRIES | 3 |\n", "docs/MORE.md": "| MAX_RETRIES | 3 |\n"})
	renamed := strings.ReplaceAll(engineBody, "MAX_RETRIES", "RETRY_LIMIT") + "fn again() { retry(RETRY_LIMIT); }\n"
	commit(t, repo, "2026-10-05", "Dev", "Dev", "name the retry limit", map[string]string{"src/engine.rs": renamed, "docs/NOTES.md": "# notes\n| RETRY_LIMIT | 3 |\n", "docs/MORE.md": "| RETRY_LIMIT | 3 |\n"})
	body := renamed
	for i := 0; i < 15; i++ {
		body = strings.Replace(body, "BACKOFF_MS: u32 = ", "BACKOFF_MS: u32 = 1", 1)
		commit(t, repo, "2026-10-06", "Dev", "Dev", "tune the pause "+strconv.Itoa(i), map[string]string{"src/engine.rs": body})
	}
	f := lightFact("`MAX_RETRIES` is three, `BACKOFF_MS` is a pause, and the `suspended` helper exists.\n")
	b := &Builder{G: New(root), Redact: noRedaction}
	sub, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sub.Idents, ",") != "MAX_RETRIES,BACKOFF_MS" {
		t.Fatalf("subject %v: the plain word must not be searched", sub.Idents)
	}
	if len(evs) != 1 || evs[0].Incomplete {
		t.Fatalf("evidence %+v", evs)
	}
	ev := evs[0]
	var removing []string
	for _, c := range ev.Changed {
		if len(c.Dropped) > 0 {
			removing = append(removing, c.Subject+":"+strings.Join(c.Dropped, ","))
		}
	}
	// Three commits touch a line holding MAX_RETRIES after the anchor (the added
	// caller, the moved row, the rename); only the rename removed it. BACKOFF_MS
	// stands at HEAD and is not searched, so its fifteen commits are not read.
	if ev.ChangedN != 3 || len(removing) != 1 || removing[0] != "name the retry limit:MAX_RETRIES" {
		t.Fatalf("changed %d, removing: %v", ev.ChangedN, removing)
	}
	for _, c := range ev.Changed {
		if c.Touched < 1 || c.Committed.IsZero() {
			t.Errorf("commit %s: touched %d committed %v", c.Short, c.Touched, c.Committed)
		}
	}
	if ev.CommitsSince != 0 {
		t.Errorf("the light read counted commits: %d", ev.CommitsSince)
	}
}

// A file the fact names that a commit deleted or renamed after the anchor is
// reported with that commit, however many commits come before and after it; a
// file still there is not, and one deleted before the anchor is not.
func TestLightReadReportsDeletedPaths(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/old.rs": "a\n", "src/kept.rs": "b\n", "src/renamed.rs": "c\n", "src/early.rs": "e\n", "NOTES": "n\n"})
	commit(t, repo, "2026-09-25", "Dev", "Dev", "drop early", nil, "src/early.rs")
	for i := 0; i < 3; i++ {
		commit(t, repo, "2026-10-03", "Dev", "Dev", "edit kept "+strconv.Itoa(i), map[string]string{"src/kept.rs": "b" + strconv.Itoa(i) + "\n"})
	}
	commit(t, repo, "2026-10-06", "Dev", "Dev", "drop old, rename", map[string]string{"src/new_name.rs": "c\n"}, "src/old.rs", "src/renamed.rs")
	for i := 0; i < 3; i++ {
		commit(t, repo, "2026-10-07", "Dev", "Dev", "edit kept again "+strconv.Itoa(i), map[string]string{"src/kept.rs": "c" + strconv.Itoa(i) + "\n"})
	}
	f := lightFact("The helpers are `src/old.rs`, `src/kept.rs`, `src/renamed.rs` and `src/early.rs`.\n")
	b := &Builder{G: New(root), Redact: noRedaction}
	_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil || len(evs) != 1 || evs[0].Incomplete {
		t.Fatalf("%v %+v", err, evs)
	}
	var got []string
	for _, g := range evs[0].Deleted {
		got = append(got, g.Term+"@"+g.Commit.Subject)
	}
	sort.Strings(got)
	if strings.Join(got, "|") != "src/old.rs@drop old, rename|src/renamed.rs@drop old, rename" {
		t.Fatalf("deleted: %v", got)
	}
}

// What git filters on, and what the read compares, is the committer date: a
// commit written on 09-28 that reached the repo on 10-08 (a rebase merge, a
// cherry-pick) deleted the file after the fact; one written on 10-05 that
// landed on 09-30 did not.
func TestLightReadUsesTheCommitterDate(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/late.rs": "a\n", "src/early.rs": "b\n", "src/engine.rs": engineBody})
	commitAt(t, repo, "2026-10-05", "2026-09-30", "landed before the fact", nil, "src/early.rs")
	commitAt(t, repo, "2026-09-28", "2026-10-08", "landed after the fact", map[string]string{"src/engine.rs": strings.ReplaceAll(engineBody, "MAX_RETRIES", "RETRY_LIMIT")}, "src/late.rs")
	f := lightFact("`src/late.rs`, `src/early.rs` and `MAX_RETRIES`.\n")
	b := &Builder{G: New(root), Redact: noRedaction}
	_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil || len(evs) != 1 || evs[0].Incomplete {
		t.Fatalf("%v %+v", err, evs)
	}
	var del []string
	for _, g := range evs[0].Deleted {
		del = append(del, g.Term)
	}
	if strings.Join(del, ",") != "src/late.rs" {
		t.Errorf("deleted %v, want only src/late.rs", del)
	}
	removed := 0
	for _, c := range evs[0].Changed {
		if len(c.Dropped) > 0 {
			removed++
		}
	}
	if removed != 1 {
		t.Errorf("the rename authored before the fact and landed after it: %d removing commits, want 1", removed)
	}
}

// A mass refactor puts a name's removal in a commit with other big files. The
// diff is read with -G, so a 770 KB file in the same commit does not push the
// removal past the read's bound.
func TestLightReadFindsARemovalBesideAHugeUnrelatedFile(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/engine.rs": engineBody})
	big := strings.Repeat("an unrelated line of a generated file, 0123456789\n", 16000)
	commit(t, repo, "2026-10-06", "Dev", "Dev", "refactor: rename the limit, add generated data",
		map[string]string{"a_generated.txt": big, "src/engine.rs": strings.ReplaceAll(engineBody, "MAX_RETRIES", "RETRY_LIMIT")})
	f := lightFact("`MAX_RETRIES` is three.\n")
	b := &Builder{G: New(root), Redact: noRedaction}
	_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil || len(evs) != 1 || evs[0].Incomplete {
		t.Fatalf("%v %+v", err, evs)
	}
	removed := 0
	for _, c := range evs[0].Changed {
		if len(c.Dropped) > 0 {
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("the removal beside a big file: %d removing commits, notes %v", removed, evs[0].Notes)
	}
}

// A diff of the files that hold the name that is itself past the bound is cut:
// the removal may be past it, so the read is incomplete and says so.
func TestLightReadCutDiffIsIncomplete(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	var lines []string
	for i := 0; i < 20000; i++ {
		lines = append(lines, "const MAX_RETRIES_"+strconv.Itoa(i)+": u32 = 3; // padding to make the diff long")
	}
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/limits.rs": strings.Join(lines, "\n") + "\n", "README.md": "x\n"})
	commit(t, repo, "2026-10-06", "Dev", "Dev", "drop the limits", nil, "src/limits.rs")
	f := lightFact("`MAX_RETRIES` is three.\n")
	b := &Builder{G: New(root), Redact: noRedaction}
	_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %+v", err, evs)
	}
	if !evs[0].Incomplete || !strings.Contains(strings.Join(evs[0].Notes, "|"), "cut at") {
		t.Fatalf("a cut diff read as complete: incomplete %v notes %v", evs[0].Incomplete, evs[0].Notes)
	}
}

// With Low, every git command runs under nice 19 and the idle I/O class (on
// each child: a priority set on the calling process reaches one thread only),
// and still works.
func TestLowRunsEveryGitUnderNiceAndIonice(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"a.txt": "a\n"})
	g := New(root)
	g.Low = true
	if out, _, err := g.run(context.Background(), repo, 1<<10, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("a low-priority git: %q %v", out, err)
	}
	// Wrappers that record how they were called, then run what they wrap.
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	for name, body := range map[string]string{
		"nice":   "#!/bin/sh\necho \"nice $1 $2\" >> " + log + "\nshift 2\nexec \"$@\"\n",
		"ionice": "#!/bin/sh\necho \"ionice $1\" >> " + log + "\nshift 1\nexec \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, _, err := g.run(context.Background(), repo, 1<<10, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		t.Fatalf("under the wrappers: %q %v", out, err)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "ionice -c3\nnice -n 19\n" {
		t.Fatalf("the wrappers were called as %q, want ionice -c3 then nice -n 19", b)
	}
	// Without Low nothing is wrapped.
	os.Remove(log)
	g.Low = false
	if _, _, err := g.run(context.Background(), repo, 1<<10, "rev-parse", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a git without Low went through nice")
	}
}

// failingGit puts a git on PATH that fails the commands whose first word is in
// fail and runs the real one otherwise.
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

// A file list that failed says nothing about a path: it is neither present nor
// absent, the read is incomplete, and no path is reported deleted.
func TestFailedFileListMakesNoPathLookDeleted(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/engine.rs": engineBody})
	commit(t, repo, "2026-10-05", "Dev", "Dev", "edit the engine", map[string]string{"src/engine.rs": engineBody + "fn extra() {}\n"})
	f := lightFact("The engine is `src/engine.rs`.\n")
	failingGit(t, "ls-tree")
	b := &Builder{G: New(root), Redact: noRedaction}
	_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
	if err != nil || len(evs) != 1 {
		t.Fatalf("%v %+v", err, evs)
	}
	ev := evs[0]
	if !ev.Incomplete || len(ev.Deleted) != 0 {
		t.Fatalf("incomplete %v deleted %v notes %v", ev.Incomplete, ev.Deleted, ev.Notes)
	}
	for _, st := range ev.State {
		if st.Kind == "path" && (st.Present || !st.Unknown) {
			t.Errorf("a path of a failed list: %+v", st)
		}
	}
}

// A read at the repo's default branch is not changed by what the working tree
// has checked out: on branch wip a file is gone that main still has.
func TestResolveReadsTheNamedCommitNotTheCheckout(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-09-20", "Dev", "Dev", "first", map[string]string{"src/old.rs": "a\n", "README.md": "x\n"})
	run(t, repo, nil, "branch", "-m", "main")
	mainSha := strings.TrimSpace(run(t, repo, nil, "rev-parse", "HEAD"))
	run(t, repo, nil, "checkout", "-q", "-b", "wip")
	commit(t, repo, "2026-10-06", "Dev", "Dev", "drop old on wip", nil, "src/old.rs")
	f := lightFact("The helper lives in `src/old.rs`.\n")
	read := func(resolve func(ctx context.Context, repo, dir string) (string, string, error)) RepoEvidence {
		g := New(root)
		g.Resolve = resolve
		b := &Builder{G: g, Redact: noRedaction}
		_, evs, err := b.ReadSince(context.Background(), FactRef{Fact: f}, day(t, "2026-10-02"))
		if err != nil || len(evs) != 1 {
			t.Fatalf("%v %+v", err, evs)
		}
		return evs[0]
	}
	if ev := read(nil); ev.Incomplete || len(ev.Deleted) != 1 {
		t.Fatalf("HEAD is wip: the file is gone there, got %+v", ev.Deleted)
	}
	ev := read(func(context.Context, string, string) (string, string, error) { return mainSha, "origin/main", nil })
	if ev.Incomplete || len(ev.Deleted) != 0 || ev.Branch != "origin/main" || ev.Head != shortSHA(mainSha) {
		t.Fatalf("read at main: deleted %v branch %q head %s", ev.Deleted, ev.Branch, ev.Head)
	}
	if ev := read(func(context.Context, string, string) (string, string, error) { return "", "", errors.New("boom") }); !ev.Incomplete {
		t.Fatal("a branch that could not be resolved read as complete")
	}
}

// A one-fact timeline carries the fact's dates with no side.
func TestBuildOne(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-10-06", "Dev", "Dev", "retry five times", map[string]string{"src/engine.rs": "const MAX_RETRIES: u32 = 5;\n"})
	f := &fact.Fact{Path: "projects/x/e.md", Meta: fact.Meta{Name: "e", Scope: "projects/x", Created: "2026-10-01", Source: fact.Source{Harness: "curator"}},
		Body: "`MAX_RETRIES` is three.\n"}
	b := &Builder{G: New(root), Redact: func(in []string) ([]string, error) { return in, nil }}
	tl, err := b.BuildOne(context.Background(), FactRef{Fact: f})
	if err != nil {
		t.Fatal(err)
	}
	var fact, commit bool
	for _, e := range tl.Events {
		if e.Kind == KindFact {
			fact = true
			if e.Side != "" {
				t.Errorf("fact event with side %q", e.Side)
			}
		}
		commit = commit || (e.Kind == KindCommit && e.Text == "retry five times")
	}
	if !fact || !commit {
		t.Errorf("events %+v", tl.Events)
	}
	if _, err := b.BuildOne(context.Background(), FactRef{}); err == nil {
		t.Error("a timeline without a fact")
	}
}
