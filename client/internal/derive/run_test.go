package derive

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/lint"
)

func curatedFact(repo string) (string, []byte) {
	f := &fact.Fact{
		Path: "projects/" + repo + "/" + repo + "-note.md",
		Meta: fact.Meta{Name: repo + "-note", Description: "a curated note about " + repo, Type: "design_decision", Scope: "projects/" + repo,
			Source: fact.Source{Harness: "curator", Host: "host-a", Kind: "observed"}, Created: "2026-10-01"},
		Body: "body\n",
	}
	b, err := f.Encode()
	if err != nil {
		panic(err)
	}
	return f.Path, b
}

func eligibleStore(t *testing.T, repos ...string) string {
	t.Helper()
	store := t.TempDir()
	for _, repo := range repos {
		rel, b := curatedFact(repo)
		p := filepath.Join(store, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
	return store
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

// derived runs the step the way the curator does, minus git: compute, then
// write what differs into the store directory.
func derived(t *testing.T, o Options) *Report {
	t.Helper()
	ctx := context.Background()
	rep, err := Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	root := o.Store
	if fs, ok := o.Existing.(FS); ok {
		root = fs.Root
	}
	if err := rep.Apply(ctx, root); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestRunWritesLintCleanIdempotentFacts(t *testing.T) {
	projects := t.TempDir()
	r := fixture(t, projects, "demo")
	store := eligibleStore(t, "demo", "nocheckout")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	ctx := context.Background()

	rep := derived(t, o)
	if rep.Count(Written) != 1 || rep.Count(Skipped) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	paths := rep.Changed()
	if strings.Join(paths, ",") != "derived/demo/demo-hot-spots.md,derived/demo/demo-module-map.md" {
		t.Fatalf("paths %v", paths)
	}
	// Lint passes with the index regenerated, derived_from is the commit.
	facts, _, _ := fact.Walk(store)
	if err := os.WriteFile(filepath.Join(store, "MEMORY.md"), []byte(index.Generate(facts)), 0o644); err != nil {
		t.Fatal(err)
	}
	if fs, err := lint.Run(store); err != nil || len(fs) != 0 {
		t.Fatalf("lint: %v %v", err, fs)
	}
	sha := Sha12(strings.TrimSpace(r.git("rev-parse", "HEAD")))
	for _, p := range paths {
		b, _ := os.ReadFile(filepath.Join(store, p))
		f, err := fact.Parse(p, b)
		if err != nil {
			t.Fatal(err)
		}
		m := f.Meta
		if m.DerivedFrom != sha || m.Source.Kind != "derived" || m.Source.Harness != "curator" || !strings.HasPrefix(m.Scope, "derived/demo") || m.Created != m.Updated {
			t.Fatalf("%s meta %+v", p, m)
		}
		s := string(b)
		if strings.Contains(s, "—") {
			t.Fatalf("em dash in %s", p)
		}
		// no branch is hidden: this checkout has no remote, so HEAD is used and says so
		if !strings.Contains(s, "the checkout's HEAD (branch `main`; no remote default branch found)") {
			t.Fatalf("%s does not say which commit it was read at:\n%s", p, s)
		}
		// names outside the safe set are counted, never quoted
		for _, bad := range []string{"IGNORE", "evil", "日", "sk-aaaa", "[["} {
			if strings.Contains(s, bad) {
				t.Fatalf("%s quotes an unsafe name (%q):\n%s", p, bad, s)
			}
		}
	}
	hot := readTree(t, store)["derived/demo/demo-hot-spots.md"]
	if !strings.Contains(hot, "5 files the window touched have names outside the safe character set and are not shown.") {
		t.Fatalf("hot spots do not count the omitted names:\n%s", hot)
	}
	if m := readTree(t, store)["derived/demo/demo-module-map.md"]; !strings.Contains(m, "5 of them with names outside the safe character set, not listed or counted below") {
		t.Fatalf("map does not count the omitted names:\n%s", m)
	}

	// Same commit: nothing recomputed, nothing rewritten, bytes identical.
	before := readTree(t, store)
	st1, _ := os.Stat(filepath.Join(store, paths[0]))
	time.Sleep(20 * time.Millisecond)
	rep = derived(t, o)
	if rep.Count(Unchanged) != 1 || len(rep.Changed()) != 0 {
		t.Fatalf("second run %+v", rep.Repos)
	}
	st2, _ := os.Stat(filepath.Join(store, paths[0]))
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("an unchanged repo was rewritten")
	}
	// Force recomputes but finds the same bytes, so nothing changes on disk.
	o.Force = true
	rep = derived(t, o)
	if len(rep.Changed()) != 0 || len(rep.Repos[0].Files) != 2 {
		t.Fatalf("forced run %+v", rep.Repos)
	}
	o.Force = false
	if fmt.Sprint(before) != fmt.Sprint(readTree(t, store)) {
		t.Fatal("store changed on an unchanged commit")
	}

	// Another machine's run of the same commit and host: byte-identical files.
	out := t.TempDir()
	derived(t, Options{Store: store, Existing: FS{Root: out}, Projects: projects, Host: "host-a"})
	for _, p := range paths {
		a, _ := os.ReadFile(filepath.Join(store, p))
		b, _ := os.ReadFile(filepath.Join(out, p))
		if string(a) != string(b) || len(a) == 0 {
			t.Fatalf("%s differs between runs of the same commit", p)
		}
	}

	// HEAD moves: the facts are rewritten with the new commit.
	r.write("src/a.go", "package a // moved\n")
	r.write("src/a_test.go", "package a // moved\n")
	r.commit("moved")
	rep = derived(t, o)
	if rep.Count(Written) != 1 {
		t.Fatalf("after a new commit %+v", rep.Repos)
	}
	b, _ := os.ReadFile(filepath.Join(store, paths[0]))
	f, _ := fact.Parse(paths[0], b)
	if f.Meta.DerivedFrom != Sha12(strings.TrimSpace(r.git("rev-parse", "HEAD"))) || f.Meta.DerivedFrom == sha {
		t.Fatalf("derived_from %s", f.Meta.DerivedFrom)
	}
	_ = ctx
}

// Run computes and writes nothing; Apply is what writes.
func TestRunWritesNothing(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	store := eligibleStore(t, "demo")
	rep, err := Run(context.Background(), Options{Store: store, Projects: projects, Host: "host-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(Written) != 1 || len(rep.Repos[0].Files) != 2 || len(rep.Changed()) != 2 {
		t.Fatalf("report %+v", rep.Repos)
	}
	if _, err := os.Stat(filepath.Join(store, "derived")); err == nil {
		t.Fatal("Run wrote derived/")
	}
}

// "Unchanged" is a question about the source it is asked of. A working tree
// that an earlier run left dirty says "unchanged"; what is committed says
// "not there yet".
func TestUnchangedIsDecidedAgainstTheGivenSource(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	store := eligibleStore(t, "demo")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	derived(t, o) // the files are now on disk, as a killed run would leave them

	ctx := context.Background()
	rep, _ := Run(ctx, o)
	if rep.Count(Unchanged) != 1 {
		t.Fatalf("against the working tree: %+v", rep.Repos)
	}
	isolate(t)
	g := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = store
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	g("init", "-q", "-b", "main")
	g("add", "projects")
	g("commit", "-q", "-m", "curated only")
	o.Existing = GitRef{Dir: store, Ref: "HEAD"}
	rep, _ = Run(ctx, o)
	if rep.Count(Written) != 1 || len(rep.Changed()) != 2 {
		t.Fatalf("against what is committed: %+v", rep.Repos)
	}
	g("add", "-A")
	g("commit", "-q", "-m", "derived")
	rep, _ = Run(ctx, o)
	if rep.Count(Unchanged) != 1 {
		t.Fatalf("once committed: %+v", rep.Repos)
	}
}

func TestRetireWhenTheRepoDisappears(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	fixture(t, projects, "other")
	fixture(t, projects, "third")
	store := eligibleStore(t, "demo", "other", "third")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	derived(t, o)
	// A hand-made file that is not a generated fact stays.
	os.WriteFile(filepath.Join(store, "derived", "other", "keep.txt"), []byte("x"), 0o644)

	// one checkout gone: its facts are retired, the other repos' stay
	os.RemoveAll(filepath.Join(projects, "other"))
	rep := derived(t, o)
	if rep.Count(Retired) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	tree := readTree(t, store)
	for p := range tree {
		if strings.HasPrefix(p, "derived/other/") && p != "derived/other/keep.txt" {
			t.Errorf("retired repo still has %s", p)
		}
	}
	if _, ok := tree["derived/demo/demo-hot-spots.md"]; !ok {
		t.Fatal("a live repo's fact was removed")
	}

	// the scope disappears from the store: retired as well
	os.RemoveAll(filepath.Join(store, "projects", "demo"))
	rep = derived(t, o)
	if rep.Count(Retired) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	tree = readTree(t, store)
	if _, ok := tree["derived/demo/demo-hot-spots.md"]; ok {
		t.Fatal("demo facts survive the loss of its projects scope")
	}
	if _, ok := tree["derived/third/third-hot-spots.md"]; !ok {
		t.Fatal("a live repo's fact was removed")
	}
}

// An unmounted projects directory must not wipe the derived facts.
func TestNoRetirementWhenNoCheckoutIsFound(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	store := eligibleStore(t, "demo")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	derived(t, o)
	o.Projects = filepath.Join(t.TempDir(), "unmounted")
	rep := derived(t, o)
	if rep.Count(Retired) != 0 {
		t.Fatalf("retired with no projects dir: %+v", rep.Repos)
	}
	if _, ok := readTree(t, store)["derived/demo/demo-hot-spots.md"]; !ok {
		t.Fatal("facts removed")
	}
}

// What a machine produced only that machine retires or rewrites. A second
// machine with a few checkouts must not delete the rest, nor flip the facts of
// a repo it also has.
func TestOnlyTheProducingHostRetiresOrRewrites(t *testing.T) {
	projectsA := t.TempDir()
	for _, n := range []string{"demo", "other", "third"} {
		fixture(t, projectsA, n)
	}
	store := eligibleStore(t, "demo", "other", "third")
	derived(t, Options{Store: store, Projects: projectsA, Host: "host-a"})
	before := readTree(t, store)

	// Machine host-b has one of the three repos, at another commit.
	projectsB := t.TempDir()
	rb := fixture(t, projectsB, "demo")
	rb.write("src/a.go", "package a // host-b only\n")
	rb.write("src/a_test.go", "package a // host-b only\n")
	rb.commit("host-b only")
	oB := Options{Store: store, Projects: projectsB, Host: "host-b"}
	rep := derived(t, oB)
	if rep.Count(Retired) != 0 {
		t.Fatalf("the host-b retired the host-a's facts: %+v", rep.Repos)
	}
	if fmt.Sprint(before) != fmt.Sprint(readTree(t, store)) {
		t.Fatal("the host-b changed the host-a's facts")
	}
	var detail string
	for _, r := range rep.Repos {
		if r.Repo == "demo" {
			detail = r.Detail
			if r.Status != Skipped {
				t.Fatalf("demo: %+v", r)
			}
		}
	}
	if !strings.Contains(detail, "produced on host host-a") {
		t.Fatalf("the owner is not named: %q", detail)
	}
	// retirement by the other host is reported, not done
	var said int
	for _, r := range rep.Repos {
		if (r.Repo == "other" || r.Repo == "third") && strings.Contains(r.Detail, "produced on host host-a") {
			said++
		}
	}
	if said != 2 {
		t.Fatalf("the refusal to retire is not reported: %+v", rep.Repos)
	}

	// Explicitly taking over lets it: other and third have no checkout on the
	// host-b and go; demo is rewritten with host-b's commit and host.
	oB.TakeOver = true
	rep = derived(t, oB)
	if rep.Count(Retired) != 2 {
		t.Fatalf("take-over report %+v", rep.Repos)
	}
	b, _ := os.ReadFile(filepath.Join(store, "derived/demo/demo-hot-spots.md"))
	f, _ := fact.Parse("x", b)
	if f.Meta.Source.Host != "host-b" {
		t.Fatalf("take-over did not rewrite demo: host %s", f.Meta.Source.Host)
	}
}

// A project fact that does not parse may be the only one of its repo, which
// would then look gone: no retirement on such a run, and the operator is told.
func TestUnparsableProjectFactStopsRetirement(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	fixture(t, projects, "other")
	store := eligibleStore(t, "demo", "other")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	derived(t, o)

	// other's only project fact becomes unparsable
	os.WriteFile(filepath.Join(store, "projects", "other", "other-note.md"), []byte("---\nname: [broken\n---\nbody\n"), 0o644)
	rep := derived(t, o)
	if rep.Count(Retired) != 0 {
		t.Fatalf("retired on a store that did not parse: %+v", rep.Repos)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "projects/other/other-note.md") {
		t.Fatalf("warnings %v", rep.Warnings)
	}
	if _, ok := readTree(t, store)["derived/other/other-hot-spots.md"]; !ok {
		t.Fatal("facts removed")
	}
}

// A folder inside some other repository is not a checkout of its own.
func TestPlainFolderInsideAnotherRepoIsSkipped(t *testing.T) {
	projects := t.TempDir()
	outer := newRepo(t, projects, "outer")
	outer.write("inner/a.go", "x\n")
	outer.commit("x")
	store := eligibleStore(t, "inner")
	o := Options{Store: store, Projects: filepath.Join(projects, "outer"), Host: "host-a"}
	rep, _ := Run(context.Background(), o)
	if len(rep.Repos) != 1 || rep.Repos[0].Status != Skipped || !rep.Repos[0].Gone {
		t.Fatalf("report %+v", rep.Repos)
	}
}

// A linked worktree has .git as a file and is a checkout.
func TestLinkedWorktreeIsACheckout(t *testing.T) {
	projects := t.TempDir()
	r := fixture(t, projects, "main-repo")
	wt := filepath.Join(projects, "wt")
	r.git("worktree", "add", "-q", wt, "-b", "wtb")
	store := eligibleStore(t, "wt")
	rep := derived(t, Options{Store: store, Projects: projects, Host: "host-a"})
	if rep.Count(Written) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
}

// A repo with too little history yields no hot-spot fact and removes a stale one.
func TestTooLittleHistoryYieldsNoHotSpotFact(t *testing.T) {
	projects := t.TempDir()
	r := newRepo(t, projects, "tiny")
	r.write("src/a.go", "x\n")
	r.write("lib/b.go", "x\n")
	r.commit("one")
	r.write("src/a.go", "y\n")
	r.commit("two")
	store := eligibleStore(t, "tiny")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	rep := derived(t, o)
	tree := readTree(t, store)
	if _, ok := tree["derived/tiny/tiny-hot-spots.md"]; ok {
		t.Fatal("two touches is not a hot spot")
	}
	if _, ok := tree["derived/tiny/tiny-module-map.md"]; !ok || rep.Count(Written) != 1 {
		t.Fatalf("report %+v", rep.Repos)
	}
	// a third touch makes it one
	r.write("src/a.go", "z\n")
	r.commit("three")
	derived(t, o)
	if _, ok := readTree(t, store)["derived/tiny/tiny-hot-spots.md"]; !ok {
		t.Fatal("expected a hot spot fact after the third touch")
	}
	// and when history changes so that it is gone again, the stale fact goes
	// (only a generated one: the hand-made file next to it stays)
	os.WriteFile(filepath.Join(store, "derived", "tiny", "notes.md"), []byte("---\nname: notes\n---\nhand made\n"), 0o644)
	o.Force = true
	r.git("reset", "-q", "--hard", "HEAD~2") // back to one commit: no file has three touches
	rep = derived(t, o)
	tree = readTree(t, store)
	if _, ok := tree["derived/tiny/tiny-hot-spots.md"]; ok {
		t.Fatalf("stale hot-spot fact kept: %+v", rep.Repos)
	}
	if _, ok := tree["derived/tiny/notes.md"]; !ok {
		t.Fatal("a file that is not a generated fact was removed")
	}
}

// A derived name that a curated fact already uses is skipped, reported, and
// does not make the repo recompute every day.
func TestNameClashWithACuratedFactIsReported(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	store := eligibleStore(t, "demo")
	f := &fact.Fact{Path: "user/demo-hot-spots.md", Meta: fact.Meta{Name: "demo-hot-spots", Description: "d", Type: "user", Scope: "user", Source: fact.Source{Harness: "human", Host: "host-a", Kind: "user"}, Created: "2026-10-01"}, Body: "b\n"}
	b, _ := f.Encode()
	os.MkdirAll(filepath.Join(store, "user"), 0o755)
	os.WriteFile(filepath.Join(store, f.Path), b, 0o644)
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	rep := derived(t, o)
	if _, ok := readTree(t, store)["derived/demo/demo-hot-spots.md"]; ok {
		t.Fatalf("clobbered a curated name: %+v", rep.Repos)
	}
	if !strings.Contains(rep.Repos[0].Detail, "demo-hot-spots") {
		t.Fatalf("clash not reported: %+v", rep.Repos[0])
	}
	// the map is still written, and a second run is Unchanged but still says so
	rep = derived(t, o)
	if rep.Repos[0].Status != Unchanged || !strings.Contains(rep.Repos[0].Detail, "demo-hot-spots") {
		t.Fatalf("second run: %+v", rep.Repos[0])
	}

	// both names taken: no git work at all, reported
	g := &fact.Fact{Path: "user/demo-module-map.md", Meta: fact.Meta{Name: "demo-module-map", Description: "d", Type: "user", Scope: "user", Source: fact.Source{Harness: "human", Host: "host-a", Kind: "user"}, Created: "2026-10-01"}, Body: "b\n"}
	gb, _ := g.Encode()
	os.WriteFile(filepath.Join(store, g.Path), gb, 0o644)
	os.RemoveAll(filepath.Join(store, "derived"))
	rep = derived(t, o)
	if rep.Repos[0].Status != Skipped || rep.Repos[0].Sha != "" || !strings.Contains(rep.Repos[0].Detail, "demo-module-map") {
		t.Fatalf("both clash: %+v", rep.Repos[0])
	}
}

// Two repos whose names slug the same must not both write the same fact
// names, also when the first one's commit did not move.
func TestSlugClashBetweenReposIsCaughtWithoutRecomputing(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "foo_bar")
	fixture(t, projects, "foo-bar")
	store := eligibleStore(t, "foo_bar", "foo-bar")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	for i := 0; i < 2; i++ { // the second pass finds the first repo unchanged
		rep := derived(t, o)
		var skipped RepoResult
		for _, r := range rep.Repos {
			if r.Status == Skipped {
				skipped = r
			}
		}
		if skipped.Repo != "foo_bar" || !strings.Contains(skipped.Detail, "already derived for foo-bar") {
			t.Fatalf("pass %d: %+v", i, rep.Repos)
		}
	}
	tree := readTree(t, store)
	if _, ok := tree["derived/foo-bar/foo-bar-hot-spots.md"]; !ok {
		t.Fatal("the first repo lost its facts")
	}
	if _, ok := tree["derived/foo_bar/foo-bar-hot-spots.md"]; ok {
		t.Fatal("the second repo wrote the same name")
	}
}

// A file at an expected path that is not a generated fact is not ours.
func TestForeignFileAtAnExpectedPathIsLeftAlone(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	store := eligibleStore(t, "demo")
	os.MkdirAll(filepath.Join(store, "derived", "demo"), 0o755)
	foreign := "---\nname: demo-hot-spots\ndescription: mine\ntype: hot_spot\nscope: derived/demo\nsource:\n  harness: human\n  host: host-a\n  kind: user\ncreated: \"2026-10-01\"\n---\n\nwritten by hand\n"
	os.WriteFile(filepath.Join(store, "derived", "demo", "demo-hot-spots.md"), []byte(foreign), 0o644)
	rep := derived(t, Options{Store: store, Projects: projects, Host: "host-a"})
	if got := readTree(t, store)["derived/demo/demo-hot-spots.md"]; got != foreign {
		t.Fatalf("a hand-made file was overwritten:\n%s", got)
	}
	if !strings.Contains(rep.Repos[0].Detail, "not a generated fact") {
		t.Fatalf("not reported: %+v", rep.Repos[0])
	}
	if _, ok := readTree(t, store)["derived/demo/demo-module-map.md"]; !ok {
		t.Fatal("the map should still be written")
	}
}

// ---- which commit: the remote default branch, else HEAD

func bareOrigin(t *testing.T, root string, work *repo) string {
	t.Helper()
	bare := filepath.Join(root, "origin.git")
	cmd := exec.Command("git", "init", "-q", "--bare", "-b", "main", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	work.git("remote", "add", "origin", bare)
	work.git("push", "-q", "origin", "main")
	return bare
}

func cloneOf(t *testing.T, bare, projects, name string) *repo {
	t.Helper()
	isolate(t)
	dir := filepath.Join(projects, name)
	cmd := exec.Command("git", "clone", "-q", bare, dir)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	return &repo{t: t, dir: dir, n: 200}
}

// A feature branch checked out in the working copy must not become a shared
// fact: the facts are read at the remote default branch.
func TestFactsAreReadAtTheRemoteDefaultBranchNotTheCheckedOutFeature(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	work := fixture(t, root, "work")
	bare := bareOrigin(t, root, work)
	c := cloneOf(t, bare, projects, "demo")
	mainSha := Sha12(strings.TrimSpace(c.git("rev-parse", "origin/main")))

	c.git("checkout", "-q", "-b", "feature/wip")
	for i := 0; i < 5; i++ {
		c.write("src/wip_only.go", fmt.Sprintf("package wip // %d\n", i))
		c.commit(fmt.Sprintf("wip %d", i))
	}
	store := eligibleStore(t, "demo")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	rep := derived(t, o)
	if rep.Count(Written) != 1 || rep.Repos[0].Sha != mainSha {
		t.Fatalf("read at %s, want origin/main %s: %+v", rep.Repos[0].Sha, mainSha, rep.Repos)
	}
	for p, s := range readTree(t, store) {
		if !strings.HasPrefix(p, "derived/") {
			continue
		}
		if strings.Contains(s, "wip_only") || strings.Contains(s, "feature/wip") {
			t.Errorf("%s leaked the feature branch", p)
		}
		if !strings.Contains(s, "on `origin/main`") || !strings.Contains(s, mainSha) {
			t.Errorf("%s does not say it was read at origin/main:\n%s", p, s)
		}
	}
	// the fact is a function of that commit: switching branches changes nothing
	c.git("checkout", "-q", "main")
	if rep := derived(t, o); rep.Count(Unchanged) != 1 {
		t.Fatalf("switching the checked-out branch rewrote the facts: %+v", rep.Repos)
	}

	// no origin/HEAD, but origin/main exists: still the remote branch
	c.git("remote", "set-head", "origin", "-d")
	c.git("checkout", "-q", "feature/wip")
	o.Force = true
	if rep := derived(t, o); rep.Repos[0].Sha != mainSha {
		t.Fatalf("without origin/HEAD: %+v", rep.Repos[0])
	}
}

// With no remote default branch the checkout's HEAD is used, and the fact
// says on which branch (or that it was detached).
func TestFallbackToHeadRecordsTheBranch(t *testing.T) {
	projects := t.TempDir()
	r := fixture(t, projects, "demo")
	r.git("checkout", "-q", "-b", "feature/review-x")
	store := eligibleStore(t, "demo")
	o := Options{Store: store, Projects: projects, Host: "host-a"}
	derived(t, o)
	if s := readTree(t, store)["derived/demo/demo-module-map.md"]; !strings.Contains(s, "the checkout's HEAD (branch `feature/review-x`; no remote default branch found)") {
		t.Fatalf("branch not recorded:\n%s", s)
	}
	r.git("checkout", "-q", "--detach")
	o.Force = true
	derived(t, o)
	if s := readTree(t, store)["derived/demo/demo-module-map.md"]; !strings.Contains(s, "the checkout's HEAD (detached; no remote default branch found)") {
		t.Fatalf("detached HEAD not recorded:\n%s", s)
	}
}
