package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/store"
)

// rig is a store with a real remote (a bare repo, so there can be a server
// side hook and a second writer), a clone the curator works in, and checkouts
// to derive from.
type rig struct {
	t        *testing.T
	root     string
	bare     string
	clone    string
	projects string
	s        *store.Store
	preAudit bool // a store the rulings audit has not run on: commitIndex writes no marker
}

type rigOpts struct {
	repos  []string // each gets a curated projects/<repo> note and a checkout
	filler int      // extra curated facts, so bm25 scores reach the hook floors
	rules  int      // curated facts about "quokka scheduler zebra" in projects/<first repo>, the first an owner ruling
	// preAudit: a store the rulings audit has not run on (no marker).
	preAudit bool
}

func writeFact(t *testing.T, root string, m fact.Meta, body string) {
	t.Helper()
	m.Source = fact.Source{Harness: "curator", Host: "host-a", Kind: orDefault(m.Source.Kind, "observed"),
		OwnerQuote: m.Source.OwnerQuote, OwnerQuoteFrom: m.Source.OwnerQuoteFrom, Endorsed: m.Source.Endorsed,
		OwnerWords: m.Source.OwnerWords, OwnerWordsFrom: m.Source.OwnerWordsFrom}
	m.Created = "2026-10-08"
	f := &fact.Fact{Path: m.Scope + "/" + m.Name + ".md", Meta: m, Body: body}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, f.Path)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	r := &rig{t: t, root: root, bare: filepath.Join(root, "store.git"), clone: filepath.Join(root, "clone"), projects: filepath.Join(root, "projects"), preAudit: o.preAudit}
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", r.bare)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "-q", r.bare, seed)
	for _, repo := range o.repos {
		writeFact(t, seed, fact.Meta{Name: repo + "-note", Description: "a curated note about " + repo, Type: "design_decision", Scope: "projects/" + repo}, "body")
	}
	for i := 0; i < o.filler; i++ {
		writeFact(t, seed, fact.Meta{Name: fmt.Sprintf("filler-%03d", i), Description: fmt.Sprintf("filler note %03d about lorem%03d ipsum%03d", i, i, i), Type: "machine_fact", Scope: "machines/host-a"}, "x")
	}
	for i := 0; i < o.rules; i++ {
		src := fact.Source{Kind: "observed"}
		if i == 0 {
			src = ownerSource("quokka scheduler zebra") // the owner's ruling, their words on record
		}
		writeFact(t, seed, fact.Meta{Name: fmt.Sprintf("%s-quokka-scheduler-%d", o.repos[0], i), Description: fmt.Sprintf("quokka scheduler zebra rule %d for the %s repo", i, o.repos[0]),
			Type: "design_decision", Scope: "projects/" + o.repos[0], Source: src}, "quokka scheduler zebra")
	}
	r.commitIndex(seed, "seed")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "clone", "-q", r.bare, r.clone)
	gitIn(t, r.clone, "config", "user.email", "curator@tacit")
	gitIn(t, r.clone, "config", "user.name", "curator")
	r.s = &store.Store{Dir: r.clone}
	for _, repo := range o.repos {
		r.checkout(repo)
	}
	return r
}

// commitIndex writes what the live store already carries (.gitattributes, a
// current MEMORY.md) and commits everything in dir.
func (r *rig) commitIndex(dir, msg string) {
	r.t.Helper()
	if !r.preAudit {
		markAudited(r.t, dir)
	}
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(strings.Join(store.UnionFiles, "\n")+"\n"), 0o644)
	facts, _, _ := fact.Walk(dir)
	writeIndexesForTest(r.t, dir, facts)
	gitIn(r.t, dir, "add", "-A")
	gitIn(r.t, dir, "commit", "-q", "-m", msg)
}

// checkout makes projects/<name> a git repo where src/scheduler/worker.go and
// pool.go change together in four commits and lib/b.go alone in four.
func (r *rig) checkout(name string) string {
	r.t.Helper()
	dir := filepath.Join(r.projects, name)
	os.MkdirAll(filepath.Join(dir, "src", "scheduler"), 0o755)
	os.MkdirAll(filepath.Join(dir, "lib"), 0o755)
	gitIn(r.t, dir, "init", "-q", "-b", "main")
	for i := 0; i < 4; i++ {
		os.WriteFile(filepath.Join(dir, "src", "scheduler", "worker.go"), []byte(fmt.Sprintf("package scheduler // %d\n", i)), 0o644)
		os.WriteFile(filepath.Join(dir, "src", "scheduler", "pool.go"), []byte(fmt.Sprintf("package scheduler // pool %d\n", i)), 0o644)
		os.WriteFile(filepath.Join(dir, "lib", "b.go"), []byte(fmt.Sprintf("package b // %d\n", i)), 0o644)
		gitIn(r.t, dir, "add", "-A")
		gitIn(r.t, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

func (r *rig) opts() derive.Options {
	return derive.Options{Store: r.clone, Projects: r.projects, Host: "host-a"}
}

func (r *rig) step(ctx context.Context) (*derive.Report, error) {
	return deriveStep(ctx, r.s, r.opts(), false)
}

func (r *rig) serverHead() string { return gitIn(r.t, r.bare, "rev-parse", "main") }

func (r *rig) serverFiles() []string {
	return strings.Fields(gitIn(r.t, r.bare, "ls-tree", "-r", "--name-only", "main"))
}

func (r *rig) serverDerived() string {
	var out []string
	for _, f := range r.serverFiles() {
		if strings.HasPrefix(f, "derived/") && !fact.IsIndexFile(filepath.Base(f)) {
			out = append(out, f)
		}
	}
	return strings.Join(out, ",")
}

// refuse installs a server-side pre-receive hook that refuses any push whose
// diff touches derived/<name>/ for one of names. naming says whether its line
// names the file (the real hook's does) or only says "refused". Each push
// attempt is counted in a file.
func (r *rig) refuse(naming bool, names ...string) {
	r.t.Helper()
	line := `echo "tacit: refused" >&2`
	if naming {
		line = `echo "tacit: derived/$bad/$bad-hot-spots.md looks like it contains a secret" >&2`
	}
	script := fmt.Sprintf(`#!/bin/sh
echo x >> %q
while read old new ref; do
  files=$(git diff --name-only "$old" "$new")
  for bad in %s; do
    if echo "$files" | grep -q "^derived/$bad/"; then
      %s
      exit 1
    fi
  done
done
exit 0
`, filepath.Join(r.root, "attempts"), strings.Join(names, " "), line)
	if err := os.WriteFile(filepath.Join(r.bare, "hooks", "pre-receive"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) pushAttempts() int {
	b, _ := os.ReadFile(filepath.Join(r.root, "attempts"))
	return strings.Count(string(b), "x")
}

func (r *rig) commitCount() int {
	var n int
	fmt.Sscanf(gitIn(r.t, r.bare, "rev-list", "--count", "main"), "%d", &n)
	return n
}

// assertClean fails unless the clone has nothing modified, staged or untracked.
func (r *rig) assertClean(when string) {
	r.t.Helper()
	if st := gitIn(r.t, r.clone, "status", "--porcelain", "--untracked-files=all"); st != "" {
		r.t.Fatalf("%s: the clone is dirty:\n%s", when, st)
	}
}

// assertServerLintsClean clones what the server holds and lints it.
func (r *rig) assertServerLintsClean() {
	r.t.Helper()
	fresh := filepath.Join(r.t.TempDir(), "fresh")
	gitIn(r.t, r.root, "clone", "-q", r.bare, fresh)
	if fs, err := lint.Run(fresh); err != nil || len(fs) != 0 {
		r.t.Fatalf("lint of what the server holds: %v %v", err, fs)
	}
}

func ctxMinute(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// The curator's derive step against a store with a real remote: one curator
// commit with the derived facts and a regenerated MEMORY.md, the pushed tree
// lints clean, a second run pushes nothing, and a repo that leaves is retired
// in one more commit.
func TestDeriveStepPushesLintCleanFactsOnce(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo", "keepalive"}})
	ctx := ctxMinute(t)
	seedHead := r.serverHead()

	rep, err := r.step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 2 {
		t.Fatalf("report %+v", rep.Repos)
	}
	if a := gitIn(t, r.bare, "log", "-1", "--format=%an|%s", "main"); !strings.HasPrefix(a, "curator|curator: derived facts for 2 repo(s): demo, keepalive") {
		t.Fatalf("commit %q", a)
	}
	changed := strings.Fields(gitIn(t, r.bare, "diff", "--name-only", seedHead, "main"))
	want := "MEMORY.md,derived/demo/demo-hot-spots.md,derived/demo/demo-module-map.md,derived/keepalive/keepalive-hot-spots.md,derived/keepalive/keepalive-module-map.md"
	if strings.Join(withoutIndexes(changed), ",") != want {
		t.Fatalf("pushed files %v", changed)
	}
	// The folder indexes of the new folders ride in the same commit.
	for _, ix := range []string{"derived/_index.md", "derived/demo/_index.md", "derived/keepalive/_index.md"} {
		if !strings.Contains(strings.Join(changed, ","), ix) {
			t.Fatalf("pushed files %v lack %s", changed, ix)
		}
	}
	r.assertClean("after a push")
	r.assertServerLintsClean()
	if mem := gitIn(t, r.bare, "show", "main:MEMORY.md"); !strings.Contains(mem, "demo-hot-spots") {
		t.Fatalf("MEMORY.md lacks the derived fact:\n%s", mem)
	}

	// same commit: no new server commit
	head1 := r.serverHead()
	rep, err = r.step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Changed()) != 0 || r.serverHead() != head1 {
		t.Fatalf("second run changed the store: %+v", rep.Repos)
	}

	// the repo goes away (its checkout, not the scope): retired in one more commit
	os.RemoveAll(filepath.Join(r.projects, "demo"))
	rep, err = r.step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Retired) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	if got := r.serverDerived(); got != "derived/keepalive/keepalive-hot-spots.md,derived/keepalive/keepalive-module-map.md" {
		t.Fatalf("after retirement the server holds %s", got)
	}
	r.assertClean("after retirement")
	r.assertServerLintsClean()
}

// A run that dies between writing and pushing leaves the clone exactly as it
// was, and the next run still pushes the facts.
func TestInterruptedRunLeavesTheCloneCleanAndTheNextRunPushes(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	before := gitIn(t, r.clone, "rev-parse", "HEAD")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	real := lintRun
	defer func() { lintRun = real }()
	lintRun = func(dir string) ([]lint.Finding, error) { // the context ends between lint and push
		fs, err := real(dir)
		cancel()
		return fs, err
	}
	_, err := r.step(ctx)
	if err == nil {
		t.Fatal("an interrupted run should say so")
	}
	r.assertClean("after the interrupted run")
	if gitIn(t, r.clone, "rev-parse", "HEAD") != before || r.serverDerived() != "" {
		t.Fatal("the interrupted run left a commit behind")
	}

	lintRun = real
	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 || r.serverDerived() != "derived/demo/demo-hot-spots.md,derived/demo/demo-module-map.md" {
		t.Fatalf("the next run did not push: %+v / %s", rep.Repos, r.serverDerived())
	}
	r.assertClean("after the next run")
	r.assertServerLintsClean()
}

// What a killed run leaves in the working tree does not hide the facts from
// the next run (which asks what is committed, not what is on disk), and it is
// cleaned away before anything else is written.
func TestDirtDroppedByAKilledRunIsNotMistakenForProgress(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	// the killed run: facts and a regenerated index on disk, nothing committed
	rep, err := derive.Run(ctxMinute(t), r.opts())
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Apply(ctxMinute(t), r.clone); err != nil {
		t.Fatal(err)
	}
	all, _, _ := fact.Walk(r.clone)
	writeIndexesForTest(t, r.clone, all)
	if gitIn(t, r.clone, "status", "--porcelain") == "" {
		t.Fatal("setup: the clone should be dirty")
	}

	rep, err = r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 || r.serverDerived() != "derived/demo/demo-hot-spots.md,derived/demo/demo-module-map.md" {
		t.Fatalf("the facts never shipped: %+v / %s", rep.Repos, r.serverDerived())
	}
	r.assertClean("after recovering from the killed run")
	r.assertServerLintsClean()
}

// A push failure that is not a server refusal (here a client-side hook that
// fails) leaves no commit and no dirt, and the next run pushes.
func TestAPushErrorThatIsNotARefusalRestoresTheClone(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	before := gitIn(t, r.clone, "rev-parse", "HEAD")
	hook := filepath.Join(r.clone, ".git", "hooks", "pre-push")
	os.WriteFile(hook, []byte("#!/bin/sh\necho 'network is down' >&2\nexit 1\n"), 0o755)

	_, err := r.step(ctxMinute(t))
	if err == nil {
		t.Fatal("expected the push error")
	}
	if _, refused := err.(*store.Rejected); refused {
		t.Fatalf("this is not a refusal: %v", err)
	}
	r.assertClean("after the failed push")
	if gitIn(t, r.clone, "rev-parse", "HEAD") != before {
		t.Fatal("a local commit was kept")
	}
	if r.serverDerived() != "" {
		t.Fatal("something reached the server")
	}

	os.Remove(hook)
	rep, err := r.step(ctxMinute(t))
	if err != nil || rep.Count(derive.Written) != 1 {
		t.Fatalf("next run: %v %+v", err, rep.Repos)
	}
	r.assertServerLintsClean()
}

// Another writer pushes an unrelated change that touches MEMORY.md while the
// facts are being computed, and again right before the push. Neither breaks
// the sync, neither loses the other writer's change, and the index on the
// server lists both.
func TestAnotherWriterPushingMeanwhileDoesNotBreakTheRun(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	other := filepath.Join(r.root, "other")
	gitIn(t, r.root, "clone", "-q", r.bare, other)
	gitIn(t, other, "config", "user.email", "o@o")
	gitIn(t, other, "config", "user.name", "other")
	note := func(name string) {
		writeFact(t, other, fact.Meta{Name: name, Description: "another writer's note " + name, Type: "machine_fact", Scope: "machines/host-a"}, "x")
		facts, _, _ := fact.Walk(other)
		writeIndexesForTest(t, other, facts)
		gitIn(t, other, "add", "-A")
		gitIn(t, other, "commit", "-q", "-m", name)
	}

	// 1: between computing and locking
	afterGenerate = func() {
		note("first-other-note")
		gitIn(t, other, "push", "-q", "origin", "HEAD:main")
	}
	defer func() { afterGenerate = func() {} }()

	// 2: after our commit exists, right before our push (a pre-push hook in our
	// clone): the other writer lands a fact and touches MEMORY.md, so our push is
	// not a fast-forward and the locked part has to start over.
	stage := filepath.Join(r.root, "stage")
	writeFact(t, stage, fact.Meta{Name: "second-other-note", Description: "another writer's note second-other-note", Type: "machine_fact", Scope: "machines/host-a"}, "x")
	marker := filepath.Join(r.root, "pushed-once")
	hook := filepath.Join(r.clone, ".git", "hooks", "pre-push")
	script := fmt.Sprintf(`#!/bin/sh
[ -e %[1]q ] && exit 0
touch %[1]q
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
cp -r %[2]s/machines %[3]s/ && printf '\n' >> %[3]s/MEMORY.md
git -C %[3]s add -A && git -C %[3]s commit -q -m second && git -C %[3]s pull -q --rebase origin main && git -C %[3]s push -q origin HEAD:main
exit 0
`, marker, stage, other)
	os.WriteFile(hook, []byte(script), 0o755)

	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	files := strings.Join(r.serverFiles(), ",")
	for _, want := range []string{"derived/demo/demo-hot-spots.md", "machines/host-a/first-other-note.md", "machines/host-a/second-other-note.md"} {
		if !strings.Contains(files, want) {
			t.Fatalf("the server lacks %s: %s", want, files)
		}
	}
	r.assertClean("after two other writers")
	r.assertServerLintsClean()
	mem := gitIn(t, r.bare, "show", "main:MEMORY.md")
	for _, want := range []string{"demo-hot-spots", "first-other-note", "second-other-note"} {
		if !strings.Contains(mem, want) {
			t.Fatalf("MEMORY.md on the server lacks %s:\n%s", want, mem)
		}
	}
}

// The server refuses one repo's files. The other repos are pushed in one
// commit, the refused one is reported with the server's own line, the clone
// is clean, and the next run neither repeats the good ones nor stops trying
// the bad one.
func TestOneRefusedRepoDoesNotStopTheOthers(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"good", "bad", "fine"}})
	r.refuse(true, "bad")
	commits := r.commitCount()

	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 1 || rep.Count(derive.Written) != 2 {
		t.Fatalf("report %+v", rep.Repos)
	}
	var bad derive.RepoResult
	for _, x := range rep.Repos {
		if x.Status == derive.Failed {
			bad = x
		}
	}
	if bad.Repo != "bad" || !strings.Contains(bad.Detail, "looks like it contains a secret") {
		t.Fatalf("the refusal is not reported with the server's line: %+v", bad)
	}
	if got := r.serverDerived(); !strings.Contains(got, "derived/good/") || !strings.Contains(got, "derived/fine/") || strings.Contains(got, "derived/bad/") {
		t.Fatalf("server holds %s", got)
	}
	r.assertClean("after a refusal")
	r.assertServerLintsClean() // the index on the server lists exactly what it has
	// the refused repo is dropped and the rest go in ONE commit: two pushes
	// reached the server (the refused one and the retry), not one per repo
	if r.commitCount() != commits+1 || r.pushAttempts() != 2 {
		t.Fatalf("expected one new commit after 2 push attempts, got %d commits after %d attempts", r.commitCount()-commits, r.pushAttempts())
	}

	// next day: the good ones are unchanged, the bad one is tried again and fails the same way
	head := r.serverHead()
	rep, err = r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 1 || rep.Count(derive.Unchanged) != 2 || r.serverHead() != head {
		t.Fatalf("second run %+v", rep.Repos)
	}
	r.assertClean("after the second refusal")
}

// The real linter gates what is pushed. An empty host makes every generated
// fact fail L1; nothing is pushed and every repo says why.
func TestTheRealLinterRefusesWhatItShould(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"a", "b"}})
	o := r.opts()
	o.Host = ""
	rep, err := deriveStep(ctxMinute(t), r.s, o, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 2 || r.serverDerived() != "" {
		t.Fatalf("report %+v / %s", rep.Repos, r.serverDerived())
	}
	for _, x := range rep.Repos {
		if !strings.Contains(x.Detail, "lint rejects what was generated") || !strings.Contains(x.Detail, "L1") {
			t.Fatalf("no reason: %+v", x)
		}
	}
	r.assertClean("after the linter refused")
}

// One repo's lint finding drops that repo; the rest go.
func TestOneRepoFailingLintDoesNotBlockTheOthers(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"a", "b"}})
	real := lintRun
	defer func() { lintRun = real }()
	lintRun = func(dir string) ([]lint.Finding, error) {
		fs, err := real(dir)
		if _, serr := os.Stat(filepath.Join(dir, "derived", "a")); serr == nil {
			fs = append(fs, lint.Finding{Rule: "L9", Path: "derived/a/a-hot-spots.md", Msg: "test"})
		}
		return fs, err
	}
	rep, err := r.step(ctxMinute(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Failed) != 1 || r.serverDerived() != "derived/b/b-hot-spots.md,derived/b/b-module-map.md" {
		t.Fatalf("report %+v / %s", rep.Repos, r.serverDerived())
	}
	r.assertClean("after dropping one repo")
	r.assertServerLintsClean()
}

// A linter that cannot run is a failed gate, not a pass.
func TestALinterThatCannotRunFailsClosed(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	real := lintRun
	defer func() { lintRun = real }()
	lintRun = func(string) ([]lint.Finding, error) { return nil, fmt.Errorf("walk failed") }
	_, err := r.step(ctxMinute(t))
	if err == nil || !strings.Contains(err.Error(), "lint could not run") {
		t.Fatalf("got %v", err)
	}
	if r.serverDerived() != "" {
		t.Fatal("pushed without a lint")
	}
	r.assertClean("after the lint error")
}

// A finding elsewhere in the store does not block the step.
func TestAFindingElsewhereInTheStoreDoesNotBlock(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	real := lintRun
	defer func() { lintRun = real }()
	lintRun = func(dir string) ([]lint.Finding, error) {
		fs, err := real(dir)
		return append(fs, lint.Finding{Rule: "L2", Path: "user/other.md", Msg: "elsewhere"}), err
	}
	if _, err := r.step(ctxMinute(t)); err != nil {
		t.Fatal(err)
	}
	if r.serverDerived() == "" {
		t.Fatal("nothing pushed")
	}
}

// A dry run reads the store and changes nothing, including its refs.
func TestDryRunTouchesNothing(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	before := gitIn(t, r.clone, "rev-parse", "HEAD")
	rep, err := deriveStep(ctxMinute(t), r.s, r.opts(), true)
	if err != nil || rep.Count(derive.Written) != 1 {
		t.Fatalf("%v %+v", err, rep.Repos)
	}
	r.assertClean("after a dry run")
	if gitIn(t, r.clone, "rev-parse", "HEAD") != before || r.serverDerived() != "" {
		t.Fatal("a dry run changed the store")
	}
}
