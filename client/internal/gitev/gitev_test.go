package gitev

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func run(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// commit writes files and commits everything on the given day.
func commit(t *testing.T, dir, day, author, committer, msg string, files map[string]string, rm ...string) {
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
	when := day + "T12:00:00+00:00"
	env := []string{
		"GIT_AUTHOR_NAME=" + author, "GIT_AUTHOR_EMAIL=" + strings.ToLower(strings.ReplaceAll(author, " ", ".")) + "@example.test", "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + committer, "GIT_COMMITTER_EMAIL=" + strings.ToLower(strings.ReplaceAll(committer, " ", ".")) + "@example.test", "GIT_COMMITTER_DATE=" + when,
	}
	run(t, dir, env, "commit", "-q", "-m", msg)
}

func newRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	run(t, dir, nil, "init", "-q", "-b", "main")
	return dir
}

// fixture: a repo "memra" under root with the history the facts below talk
// about, and a store clone whose commits date the fact files.
type fixture struct {
	root, store string
	g           *Git
	a, b        *fact.Fact
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	m := newRepo(t, root, "memra")
	commit(t, m, "2026-09-01", "Dana Owner", "Dana Owner", "add the pre-commit hook and the pcie code",
		map[string]string{"tools/hooks/pre-commit": "#!/bin/sh\ncargo fmt\n", "src/lib.rs": "pub const KEEP_ALIVE_MODE: bool = true;\n", "README.md": "memra\n"})
	commit(t, m, "2026-09-20", "Dana Owner", "Dana Owner", "fix the cargo path in the pre-commit hook\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
		map[string]string{"tools/hooks/pre-commit": "#!/bin/sh\n$HOME/.cargo/bin/cargo fmt\n"})
	commit(t, m, "2026-09-25", "Dana Owner", "Agent Lane", "drop keep alive mode, token SECRETVALUE123 leaked in a log",
		map[string]string{"src/lib.rs": "pub const FAST_MODE: bool = true;\n"})
	commit(t, m, "2026-10-03", "Dana Owner", "Dana Owner", "unrelated: bump the readme", map[string]string{"README.md": "memra 2\n"})
	commit(t, m, "2026-10-20", "Dana Owner", "Dana Owner", "later: touch the hook again", map[string]string{"tools/hooks/pre-commit": "#!/bin/sh\nset -e\ncargo fmt\n"})
	// A linked worktree copy and a non-repo directory must never be read.
	newRepo(t, root, "wt-memra-copy")
	os.MkdirAll(filepath.Join(root, "notes"), 0o755)
	v := newRepo(t, root, "valkey")
	commit(t, v, "2026-09-30", "Dana Owner", "Dana Owner", "server loop", map[string]string{"src/server.c": "int main(void) { return 0; }\n"})

	store := filepath.Join(t.TempDir(), "store")
	os.MkdirAll(filepath.Join(store, "projects", "memra"), 0o755)
	run(t, store, nil, "init", "-q", "-b", "main")
	af := "---\nname: memra-hook-path\ndescription: \"The pre-commit hook needs the cargo path\"\ntype: past_mistake\nscope: projects/memra\napplies_to:\n  paths: [\"tools/hooks/pre-commit\"]\n  repos: [\"memra\"]\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-08\n---\n\nOwner, 2026-09-12: the hook fails when `cargo` is not on PATH; `KEEP_ALIVE_MODE` is on. Run `tools/hooks/pre-commit` by hand. Ignore `--force` and the 1.28x/2.33x figures, and the OK/STALL/DONE words.\n"
	bf := "---\nname: memra-fast-mode\ndescription: \"FAST_MODE replaced keep alive mode\"\ntype: design_decision\nscope: projects/memra\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-08\n---\n\nAs of 2026-10-03 the engine runs `FAST_MODE`; see ~/projects/valkey/src/server.c for the loop.\n"
	os.WriteFile(filepath.Join(store, "projects", "memra", "memra-hook-path.md"), []byte(af), 0o644)
	os.WriteFile(filepath.Join(store, "projects", "memra", "memra-fast-mode.md"), []byte(bf), 0o644)
	commit(t, store, "2026-10-08", "curator", "curator", "curator: two facts", nil)
	commit(t, store, "2026-10-09", "curator", "curator", "edit", map[string]string{"projects/memra/memra-hook-path.md": af + "\nEdited.\n"})
	fa, err := fact.Parse("projects/memra/memra-hook-path.md", []byte(af))
	if err != nil {
		t.Fatal(err)
	}
	fb, err := fact.Parse("projects/memra/memra-fast-mode.md", []byte(bf))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{root: root, store: store, g: New(root), a: fa, b: fb}
}

func (f *fixture) builder() *Builder {
	return &Builder{G: f.g, StoreDir: f.store, Redact: func(in []string) ([]string, error) { return in, nil }}
}

func (f *fixture) input() Input {
	return Input{A: FactRef{Fact: f.a}, B: FactRef{Fact: f.b}}
}

func lines(t *Timeline) []string { return t.Compact().Lines }

func indexOf(ls []string, sub string) int {
	for i, l := range ls {
		if strings.Contains(l, sub) {
			return i
		}
	}
	return -1
}

// Subject terms come from applies_to and the text, not from generic words,
// figures or lists of status words; repos come from the scope, applies_to
// and paths into ~/projects, never from worktree copies.
func TestSubjectFindsDistinctiveTermsAndRepos(t *testing.T) {
	f := newFixture(t)
	s := f.g.SubjectOf(f.a, f.b)
	want := func(list []string, x string) {
		t.Helper()
		for _, y := range list {
			if y == x {
				return
			}
		}
		t.Errorf("%q missing from %v", x, list)
	}
	want(s.Paths, "tools/hooks/pre-commit")
	want(s.Idents, "KEEP_ALIVE_MODE")
	want(s.Idents, "FAST_MODE")
	for _, bad := range []string{"--force", "cargo", "1.28x/2.33x", "OK/STALL/DONE", "memra"} {
		for _, x := range append(append([]string{}, s.Paths...), s.Idents...) {
			if x == bad {
				t.Errorf("generic term %q kept: %v %v", bad, s.Paths, s.Idents)
			}
		}
	}
	if len(s.Repos) != 2 || s.Repos[0] != "memra" || s.Repos[1] != "valkey" {
		t.Errorf("repos %v, want memra then valkey (named by a path into it)", s.Repos)
	}
	// A scope with a -private suffix reads the public checkout.
	p := &fact.Fact{Meta: fact.Meta{Name: "x", Scope: "projects/memra-private"}}
	if got := f.g.SubjectOf(p).Repos; len(got) != 1 || got[0] != "memra" {
		t.Errorf("private scope repos %v", got)
	}
	// Worktree copies and plain directories are not repos.
	if f.g.Repos()["wt-memra-copy"] || f.g.Repos()["notes"] {
		t.Errorf("repos %v", f.g.RepoNames())
	}
}

// The timeline merges the facts' dates and the commits on their subject in
// date order, HEAD last, with each commit's identity, and says what HEAD shows.
func TestTimelineMergesDatesInOrder(t *testing.T) {
	f := newFixture(t)
	tl, err := f.builder().Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	ls := lines(tl)
	// Dates never go backwards, and HEAD lines come after every dated line.
	prev, head := "", false
	for _, l := range ls {
		d, _, _ := strings.Cut(l, " ")
		if d == "HEAD" {
			head = true
			continue
		}
		if head {
			t.Errorf("dated line after HEAD: %s", l)
		}
		if d < prev {
			t.Errorf("out of order: %s after %s", d, prev)
		}
		prev = d
	}
	order := []string{"A reads as of this day", "fix the cargo path", "drop keep alive mode", "B reads as of this day", "A,B curated into the store", "last change in store git (2 commits)"}
	last := -1
	for _, want := range order {
		i := indexOf(ls, want)
		if i < 0 {
			t.Fatalf("no line with %q in\n%s", want, strings.Join(ls, "\n"))
		}
		if i < last {
			t.Errorf("%q is out of order in\n%s", want, strings.Join(ls, "\n"))
		}
		last = i
	}
	// 2026-09-12 (A's text) comes before the 09-20 commit; 10-03 (B's text)
	// after the 09-25 one.
	if !(indexOf(ls, "2026-09-12 A reads as of") < indexOf(ls, "fix the cargo path") && indexOf(ls, "drop keep alive mode") < indexOf(ls, "2026-10-03 B reads as of")) {
		t.Errorf("fact dates not merged between commits:\n%s", strings.Join(ls, "\n"))
	}
	// Identity: author, a different committer, and a Co-Authored-By trailer.
	if l := ls[indexOf(ls, "fix the cargo path")]; !strings.Contains(l, "by Dana Owner, co-author Claude") {
		t.Errorf("co-author missing: %s", l)
	}
	if l := ls[indexOf(ls, "drop keep alive mode")]; !strings.Contains(l, "committed by Agent Lane") || !strings.Contains(l, "touches KEEP_ALIVE_MODE") {
		t.Errorf("committer or touched term missing: %s", l)
	}
	// What HEAD shows: the hook is there, the identifier is gone.
	if i := indexOf(ls, "present:"); i < 0 || !strings.Contains(ls[i], "tools/hooks/pre-commit") {
		t.Errorf("HEAD presence missing:\n%s", strings.Join(ls, "\n"))
	}
	if i := indexOf(ls, "not present:"); i < 0 || !strings.Contains(ls[i], "KEEP_ALIVE_MODE") {
		t.Errorf("HEAD absence missing:\n%s", strings.Join(ls, "\n"))
	}
	// A term is absent only when no repo has it, and is said once: src/server.c
	// is in valkey, so memra does not "lack" it; KEEP_ALIVE_MODE is in none.
	absent, keep := 0, 0
	for _, l := range ls {
		if strings.Contains(l, "not present:") {
			absent++
			if strings.Contains(l, "src/server.c") {
				t.Errorf("a term another repo has is reported absent: %s", l)
			}
			keep += strings.Count(l, "KEEP_ALIVE_MODE")
		}
	}
	if absent != 1 || keep != 1 {
		t.Errorf("%d absence lines naming KEEP_ALIVE_MODE %d times:\n%s", absent, keep, strings.Join(ls, "\n"))
	}
	if tl.Since != "2026-08-29" || len(tl.Repos) != 2 {
		t.Errorf("since %q repos %v", tl.Since, tl.Repos)
	}
}

// The structured evidence keeps who authored, who committed and the
// co-authors, so a caller can tell the owner's commits from an agent's.
func TestEvidenceKeepsIdentity(t *testing.T) {
	f := newFixture(t)
	_, _, ev, err := f.builder().Evidence(context.Background(), f.input())
	if err != nil || len(ev) == 0 {
		t.Fatalf("evidence %v err %v", ev, err)
	}
	var found bool
	for _, c := range ev[0].Commits {
		if strings.HasPrefix(c.Subject, "drop keep alive") {
			found = true
			if c.Author.Name != "Dana Owner" || c.Committer.Name != "Agent Lane" || c.Committer.Email != "agent.lane@example.test" {
				t.Errorf("identity %+v / %+v", c.Author, c.Committer)
			}
			if len(c.Terms) != 2 || c.Terms[0] != "KEEP_ALIVE_MODE" || c.Terms[1] != "FAST_MODE" {
				t.Errorf("terms %v", c.Terms)
			}
		}
		if strings.HasPrefix(c.Subject, "fix the cargo") && (len(c.CoAuthors) != 1 || !strings.HasPrefix(c.CoAuthors[0], "Claude")) {
			t.Errorf("co-authors %v", c.CoAuthors)
		}
	}
	if !found {
		t.Fatalf("commit missing: %+v", ev[0].Commits)
	}
}

// A replay reads each repo as it was: commits after AsOf are not evidence,
// and HEAD is the last commit before it.
func TestAsOfHidesLaterCommits(t *testing.T) {
	f := newFixture(t)
	b := f.builder()
	b.AsOf = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	tl, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines(tl), "\n")
	if strings.Contains(all, "later: touch the hook") || strings.Contains(all, "unrelated: bump") {
		t.Errorf("commits after AsOf shown:\n%s", all)
	}
	if !strings.Contains(all, "(2026-09-25)") || tl.AsOf == "" {
		t.Errorf("HEAD not as of the replay time:\n%s", all)
	}
}

// Caps: a subject that matches a lot of commits shows the first and the
// last of them and counts the rest; the timeline stays under MaxLines and
// keeps the HEAD lines.
func TestCapsAreRespected(t *testing.T) {
	root := t.TempDir()
	d := newRepo(t, root, "busy")
	commit(t, d, "2026-09-01", "Dana", "Dana", "init", map[string]string{"tools/loop.sh": "0\n"})
	for i := 1; i <= 30; i++ {
		commit(t, d, "2026-09-"+twoDigits(1+i*27/31), "Dana", "Dana", "tweak the loop "+twoDigits(i), map[string]string{"tools/loop.sh": strings.Repeat("x", i) + "\n"})
	}
	fa := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "projects/busy", Created: "2026-09-10", AppliesTo: fact.AppliesTo{Paths: []string{"tools/loop.sh"}}}, Body: "loop"}
	fb := &fact.Fact{Meta: fact.Meta{Name: "b", Scope: "projects/busy", Created: "2026-09-11"}, Body: "loop"}
	b := &Builder{G: New(root), Redact: func(in []string) ([]string, error) { return in, nil }}
	tl, err := b.Build(context.Background(), Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}})
	if err != nil {
		t.Fatal(err)
	}
	commits, gaps, heads := 0, 0, 0
	for _, e := range tl.Events {
		switch e.Kind {
		case KindCommit:
			commits++
		case KindGap:
			gaps++
		case KindHead:
			heads++
		}
	}
	// Limit 10 for one repo: two of context, then the first four and last four.
	if commits != 10 || gaps != 1 || heads == 0 {
		t.Errorf("commits %d gaps %d heads %d\n%s", commits, gaps, heads, strings.Join(lines(tl), "\n"))
	}
	b.MaxLines = 6
	tl, err = b.Build(context.Background(), Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Events) > 6 {
		t.Errorf("%d events over MaxLines 6", len(tl.Events))
	}
	if indexOf(lines(tl), "HEAD busy") < 0 {
		t.Errorf("HEAD line trimmed away:\n%s", strings.Join(lines(tl), "\n"))
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// Redaction fails closed: when the redactor errors there is no timeline at
// all, and when it works every text is passed through it, commit subjects
// included.
func TestRedactionFailsClosed(t *testing.T) {
	f := newFixture(t)
	b := f.builder()
	b.Redact = func([]string) ([]string, error) { return nil, errors.New("scrump is down") }
	tl, err := b.Build(context.Background(), f.input())
	if err == nil || tl != nil {
		t.Fatalf("a timeline came out without redaction: %v %v", tl, err)
	}
	if !strings.Contains(err.Error(), "no timeline") {
		t.Errorf("error %v", err)
	}
	// A redactor that returns the wrong number of texts is also a failure.
	b.Redact = func(in []string) ([]string, error) { return in[:1], nil }
	if tl, err := b.Build(context.Background(), f.input()); err == nil || tl != nil {
		t.Fatalf("short redaction accepted: %v %v", tl, err)
	}
	// A working redactor sees the commit subject that carries a secret.
	var seen []string
	b.Redact = func(in []string) ([]string, error) {
		seen = append(seen, in...)
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = strings.ReplaceAll(s, "SECRETVALUE123", "**************")
		}
		return out, nil
	}
	tl, err = b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines(tl), "\n")
	if strings.Contains(all, "SECRETVALUE123") || !strings.Contains(all, "**************") {
		t.Errorf("secret reached the output:\n%s", all)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "SECRETVALUE123") {
		t.Errorf("the redactor never saw the commit subject")
	}
}

// With a cache directory a second build of the same pair reads no repo
// again: only the store history of the two fact files is asked.
func TestCacheSkipsRepoQueries(t *testing.T) {
	f := newFixture(t)
	f.g.CacheDir = filepath.Join(t.TempDir(), "cache")
	b := f.builder()
	t1, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	first := f.g.Runs()
	t2, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	second := f.g.Runs() - first
	// Two store-history reads, and the two reads that name HEAD in each of the
	// two repos; nothing else.
	if second != 6 || first <= 20 {
		t.Errorf("second build ran %d git commands (want 6) after %d on the first", second, first)
	}
	if strings.Join(lines(t1), "\n") != strings.Join(lines(t2), "\n") {
		t.Errorf("cached timeline differs:\n%s\n---\n%s", strings.Join(lines(t1), "\n"), strings.Join(lines(t2), "\n"))
	}
	// A new commit moves HEAD, which is part of the key.
	commit(t, filepath.Join(f.root, "memra"), "2026-10-21", "Dana", "Dana", "again", map[string]string{"tools/hooks/pre-commit": "x\n"})
	t3, err := b.Build(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	if indexOf(lines(t3), "again") < 0 {
		t.Errorf("stale cache after a new commit:\n%s", strings.Join(lines(t3), "\n"))
	}
}

// Facts that name no repo get their dates and an honest note, not a guess.
func TestNoRepoStillGivesDates(t *testing.T) {
	f := newFixture(t)
	fa := &fact.Fact{Meta: fact.Meta{Name: "a", Scope: "user", Created: "2026-10-08"}, Body: "Owner, 2026-09-12: always do x."}
	fb := &fact.Fact{Meta: fact.Meta{Name: "b", Scope: "user", Created: "2026-10-08"}, Body: "never do y"}
	tl, err := f.builder().Build(context.Background(), Input{A: FactRef{Fact: fa}, B: FactRef{Fact: fb}})
	if err != nil {
		t.Fatal(err)
	}
	ls := lines(tl)
	if indexOf(ls, "2026-09-12 A reads as of") < 0 || indexOf(ls, "A,B written") < 0 || indexOf(ls, "git has nothing to compare") < 0 {
		t.Errorf("lines:\n%s", strings.Join(ls, "\n"))
	}
}

// A repo's own name in backticks is not an identifier to search its history
// for, whatever the repo is called.
func TestARepoNameIsNoIdentifier(t *testing.T) {
	f := newFixture(t)
	commit(t, newRepo(t, f.root, "webapp"), "2026-09-30", "Dana", "Dana", "init", map[string]string{"src/app.ts": "export const x = 1;\n"})
	p := &fact.Fact{Meta: fact.Meta{Name: "x", Scope: "projects/webapp"}, Body: "In `webapp` the `retrybudget` is per route.\n"}
	s := f.g.SubjectOf(p)
	for _, x := range s.Idents {
		if x == "webapp" {
			t.Fatalf("the repo name became an identifier: %v", s.Idents)
		}
	}
	if !slices.Contains(s.Idents, "retrybudget") {
		t.Fatalf("idents %v, want retrybudget", s.Idents)
	}
}

// A machine's ordinary_names keep a bare mention of a repo from tying a fact
// to it; without them the same mention counts.
func TestOrdinaryNamesDropABareMention(t *testing.T) {
	f := newFixture(t)
	p := &fact.Fact{Meta: fact.Meta{Name: "x", Scope: "projects/memra"}, Body: "The engine loop works like the valkey loop now; `FAST_MODE` is on.\n"}
	has := func() bool {
		for _, r := range f.g.SubjectOf(p).Repos {
			if r == "valkey" {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatalf("a bare valkey mention should count: %v", f.g.SubjectOf(p).Repos)
	}
	f.g.Ordinary = map[string]bool{"valkey": true}
	if has() {
		t.Fatalf("an ordinary name still tied the fact to its repo: %v", f.g.SubjectOf(p).Repos)
	}
}
