package derive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// collect resolves and reads a checkout the way the curator does when no
// branch is configured.
func collect(dir, repo string) (*Snapshot, error) {
	ctx := context.Background()
	r, err := Resolve(ctx, dir, "")
	if err != nil {
		return nil, err
	}
	return Collect(ctx, dir, repo, r)
}

// origin builds a bare origin with main (the fixture history), a checkout
// "demo" cloned from it, and a second clone "pusher" that stands for whoever
// moves the server.
func origin(t *testing.T) (projects string, c, pusher *repo, bare string) {
	t.Helper()
	root := t.TempDir()
	projects = filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	work := fixture(t, root, "work")
	bare = bareOrigin(t, root, work)
	c = cloneOf(t, bare, projects, "demo")
	pusher = cloneOf(t, bare, filepath.Join(root, "pusher"), "p")
	return projects, c, pusher, bare
}

func (r *repo) pushNew(branch, file string) string {
	r.t.Helper()
	r.write(file, fmt.Sprintf("package n // %d\n", r.n))
	r.commit("new " + file)
	r.git("push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}

// The fetch moves the remote-tracking ref and nothing else in the checkout:
// not the working tree, not a local branch, not HEAD, not FETCH_HEAD, no tags.
func TestFetchReadsTheServersBranchAndTouchesNothingElse(t *testing.T) {
	projects, c, pusher, _ := origin(t)
	pusher.git("tag", "v1")
	pusher.git("push", "-q", "origin", "v1")
	newSha := pusher.pushNew("main", "src/new.go")
	pusher.git("push", "-q", "origin", "HEAD:main")
	newDate := strings.TrimSpace(pusher.git("log", "-1", "--format=%cs"))

	headBefore := strings.TrimSpace(c.git("rev-parse", "HEAD"))
	mainBefore := strings.TrimSpace(c.git("rev-parse", "refs/heads/main"))
	originBefore := strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main"))
	statusBefore := c.git("status", "--porcelain", "--untracked-files=all")
	fileBefore, _ := os.ReadFile(filepath.Join(c.dir, "src", "a.go"))

	store := eligibleStore(t, "demo")
	rep := derived(t, Options{Store: store, Projects: projects, Host: "host-a", Fetch: true})
	if rep.Count(Written) != 1 || rep.Repos[0].Sha != Sha12(newSha) || rep.Repos[0].Detail != "" {
		t.Fatalf("not read at the server's commit %s: %+v", Sha12(newSha), rep.Repos)
	}
	if originBefore == strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main")) {
		t.Fatal("the remote-tracking ref did not move")
	}
	if strings.TrimSpace(c.git("rev-parse", "HEAD")) != headBefore || strings.TrimSpace(c.git("rev-parse", "refs/heads/main")) != mainBefore {
		t.Fatal("a local branch or HEAD moved")
	}
	if c.git("status", "--porcelain", "--untracked-files=all") != statusBefore {
		t.Fatal("the working tree changed")
	}
	if now, _ := os.ReadFile(filepath.Join(c.dir, "src", "a.go")); string(now) != string(fileBefore) {
		t.Fatal("a file in the working tree changed")
	}
	if _, err := os.Stat(filepath.Join(c.dir, ".git", "FETCH_HEAD")); err == nil {
		t.Fatal("FETCH_HEAD was written")
	}
	if tags := strings.TrimSpace(c.git("tag")); tags != "" {
		t.Fatalf("tags were fetched: %s", tags)
	}
	// the date of the commit read is in the body, so a stale read shows
	if m := readTree(t, store)["derived/demo/demo-module-map.md"]; !strings.Contains(m, "read at commit "+Sha12(newSha)+" of "+newDate) {
		t.Fatalf("the commit and its date are not in the body:\n%s", m)
	}
}

// A fetch that fails leaves the read at the ref the checkout had, and says so.
func TestAFailedFetchFallsBackToTheLocalRefAndSaysSo(t *testing.T) {
	projects, c, pusher, _ := origin(t)
	pusher.pushNew("main", "src/new.go")
	pusher.git("push", "-q", "origin", "HEAD:main")
	before := Sha12(strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main")))
	c.git("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	rep := derived(t, Options{Store: eligibleStore(t, "demo"), Projects: projects, Host: "host-a", Fetch: true})
	r := rep.Repos[0]
	if r.Status != Written || r.Sha != before {
		t.Fatalf("expected the old origin/main %s: %+v", before, r)
	}
	if !strings.Contains(r.Detail, "fetching origin/main failed") || !strings.Contains(r.Detail, "read the ref the checkout already had") {
		t.Fatalf("the failure is not reported: %q", r.Detail)
	}
}

// A fetch that stalls ends at the timeout and the read goes on.
func TestAStalledFetchEndsAtTheTimeout(t *testing.T) {
	projects, c, _, _ := origin(t)
	c.git("remote", "set-url", "origin", "ssh://localhost/nowhere.git")
	t.Setenv("GIT_SSH_COMMAND", "sleep 30")
	start := time.Now()
	rep := derived(t, Options{Store: eligibleStore(t, "demo"), Projects: projects, Host: "host-a", Fetch: true, FetchTimeout: 700 * time.Millisecond})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the run took %v past a 0.7 s fetch timeout", d)
	}
	if rep.Repos[0].Status != Written || !strings.Contains(rep.Repos[0].Detail, "fetching origin/main failed") {
		t.Fatalf("%+v", rep.Repos[0])
	}
}

// After three failed fetches in a row the rest are not tried (a dead network
// would otherwise cost every repo the timeout), and the run says so.
func TestFetchingStopsAfterThreeFailuresInARow(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	work := fixture(t, root, "work")
	bare := bareOrigin(t, root, work)
	var names []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		names = append(names, n)
		c := cloneOf(t, bare, projects, n)
		c.git("remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	}
	rep := derived(t, Options{Store: eligibleStore(t, names...), Projects: projects, Host: "host-a", Fetch: true})
	failed := 0
	for _, r := range rep.Repos {
		if strings.Contains(r.Detail, "fetching origin/main failed") {
			failed++
		}
	}
	if failed != maxFetchFailures {
		t.Fatalf("%d fetches were tried, want %d", failed, maxFetchFailures)
	}
	if len(rep.Warnings) != 1 || rep.Warnings[0] != fetchStopped {
		t.Fatalf("warnings %v", rep.Warnings)
	}
	if rep.Count(Written) != 5 {
		t.Fatalf("every repo is still read: %+v", rep.Repos)
	}
}

// A checkout without an origin is read as it is, with nothing to report.
func TestNoOriginMeansNoFetchAndNoComplaint(t *testing.T) {
	projects := t.TempDir()
	fixture(t, projects, "demo")
	rep := derived(t, Options{Store: eligibleStore(t, "demo"), Projects: projects, Host: "host-a", Fetch: true})
	if rep.Repos[0].Status != Written || rep.Repos[0].Detail != "" {
		t.Fatalf("%+v", rep.Repos[0])
	}
}

// ---- the configured branch

func TestAConfiguredBranchIsReadInsteadOfTheDefaultAndFetched(t *testing.T) {
	projects, c, pusher, _ := origin(t)
	devSha := pusher.pushNew("prod", "src/prod_only.go") // a branch the checkout has never seen
	mainBefore := Sha12(strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main")))
	store := eligibleStore(t, "demo")
	o := Options{Store: store, Projects: projects, Host: "host-a", Fetch: true}

	rep := derived(t, o) // unconfigured: the remote default branch
	if rep.Repos[0].Sha != mainBefore {
		t.Fatalf("default read at %s, want origin/main %s", rep.Repos[0].Sha, mainBefore)
	}

	o.Branches = map[string]string{"demo": "prod"}
	rep = derived(t, o) // the config changes the commit, so both files are rewritten
	if rep.Count(Written) != 1 || rep.Repos[0].Sha != Sha12(devSha) {
		t.Fatalf("configured read at %s, want origin/prod %s: %+v", rep.Repos[0].Sha, Sha12(devSha), rep.Repos[0])
	}
	if _, err := os.Stat(filepath.Join(c.dir, "src", "prod_only.go")); err == nil {
		t.Fatal("the branch was checked out")
	}
	for p, s := range readTree(t, store) {
		if strings.HasPrefix(p, "derived/") && !strings.Contains(s, "`origin/prod` (the branch configured for this repo)") {
			t.Errorf("%s does not say which branch it was read at:\n%s", p, s)
		}
	}
}

func TestAConfiguredBranchThatDoesNotExistFallsBackAndSaysSo(t *testing.T) {
	projects, c, _, _ := origin(t)
	mainSha := Sha12(strings.TrimSpace(c.git("rev-parse", "refs/remotes/origin/main")))
	rep := derived(t, Options{Store: eligibleStore(t, "demo"), Projects: projects, Host: "host-a", Branches: map[string]string{"demo": "nope"}})
	r := rep.Repos[0]
	if r.Sha != mainSha || !strings.Contains(r.Detail, "the configured branch nope is neither origin/nope nor a local branch, read origin/main instead") {
		t.Fatalf("%+v", r)
	}
}

func TestAConfiguredBranchThatOnlyExistsLocallyIsRead(t *testing.T) {
	projects := t.TempDir()
	r := fixture(t, projects, "demo")
	r.git("checkout", "-q", "-b", "dev")
	r.write("src/dev_only.go", "package d\n")
	r.commit("dev")
	r.git("checkout", "-q", "main")
	devSha := Sha12(strings.TrimSpace(r.git("rev-parse", "dev")))
	store := eligibleStore(t, "demo")
	rep := derived(t, Options{Store: store, Projects: projects, Host: "host-a", Branches: map[string]string{"demo": "dev"}})
	if rep.Repos[0].Sha != devSha {
		t.Fatalf("%+v", rep.Repos[0])
	}
	if m := readTree(t, store)["derived/demo/demo-module-map.md"]; !strings.Contains(m, "local branch `dev` (the branch configured for this repo; the remote has none)") {
		t.Fatalf("not said:\n%s", m)
	}
}

func TestConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tacit", "derive.yaml")
	if c, err := LoadConfig(path); err != nil || len(c.Branches) != 0 {
		t.Fatalf("a missing file is an empty config: %v %v", c, err)
	}
	if err := SetBranch(path, "myrepo", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := SetBranch(path, "engine", "release/1.x"); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.Branches["myrepo"] != "dev" || c.Branches["engine"] != "release/1.x" {
		t.Fatalf("round trip: %v %v", c, err)
	}
	if err := SetBranch(path, "engine", ""); err != nil {
		t.Fatal(err)
	}
	if c, _ = LoadConfig(path); len(c.Branches) != 1 || c.Branches["myrepo"] != "dev" {
		t.Fatalf("after removing: %v", c)
	}
	for _, bad := range []string{"branches:\n  myrepo: --upload-pack=x\n", "branches:\n  myrepo: a..b\n", "branches:\n  x/y: dev\n", "brnaches:\n  myrepo: dev\n", "branches: [dev]\n"} {
		os.WriteFile(path, []byte(bad), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := SetBranch(path, "x", "-bad"); err == nil {
		t.Error("SetBranch accepted a branch starting with a dash")
	}
	// ordinary_names load, and survive SetBranch, also when it removes the
	// last branch.
	os.WriteFile(path, []byte("ordinary_names: [compass, notebook]\n"), 0o644)
	if err := SetBranch(path, "web", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := SetBranch(path, "web", ""); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadConfig(path); err != nil || !c.Ordinary()["compass"] || !c.Ordinary()["notebook"] || len(c.Branches) != 0 {
		t.Fatalf("ordinary_names: %v %v", c, err)
	}
	for _, bad := range []string{"ordinary_names: [\"a/b\"]\n", "ordinary_names: [\"\"]\n", "ordinary_names: compass\n"} {
		os.WriteFile(path, []byte(bad), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidBranch(t *testing.T) {
	for _, ok := range []string{"dev", "main", "release/1.x", "fix_a-b.c", "v2"} {
		if !ValidBranch(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-x", "--upload-pack=x", "a..b", "a/", "x.lock", "a b", "a:b", "a~b", "a;b", "/x"} {
		if ValidBranch(bad) {
			t.Errorf("%q must not be valid", bad)
		}
	}
}
