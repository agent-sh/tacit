package check

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/repoid"
)

// fakeForge answers from a table: owner/name -> full name, "" for a 404;
// an owner/name not in the table is unavailable.
type fakeForge struct {
	repos map[string]string
	user  string
	calls int
}

func (f *fakeForge) Name() string { return "github" }
func (f *fakeForge) Host() string { return "github.com" }
func (f *fakeForge) User(context.Context) string {
	return f.user
}
func (f *fakeForge) Lookup(_ context.Context, owner, name string) (ForgeRepo, error) {
	f.calls++
	full, ok := f.repos[owner+"/"+name]
	switch {
	case !ok:
		return ForgeRepo{}, ErrUnavailable
	case full == "":
		return ForgeRepo{}, ErrNotFound
	}
	return ForgeRepo{FullName: full}, nil
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogit"), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// machine is a home with ~/projects/widget (origin github.com/acme/widget,
// a tracked file, a tag and a package.json version) and its catalog.
func machine(t *testing.T) (home string, c *repoid.Catalog) {
	t.Helper()
	home, _ = filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(home, "projects", "widget")
	os.MkdirAll(filepath.Join(dir, "cmd", "serve"), 0o755)
	gitRun(t, dir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "cmd", "serve", "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"widget","version":"1.4.0"}`), 0o644)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.email=o@example.com", "-c", "user.name=o", "commit", "-q", "-m", "one")
	gitRun(t, dir, "tag", "v1.3.0")
	gitRun(t, dir, "remote", "add", "origin", "git@github.com:Acme/widget.git")
	return home, repoid.LoadCatalog(home, "", filepath.Join(home, "projects"))
}

func TestRepoCheckSourcesAndVerdicts(t *testing.T) {
	_, cat := machine(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ff := &fakeForge{user: "owner", repos: map[string]string{
		"acme/widget": "acme/widget", "owner/ghost": "", "acme/ghost": "",
		"owner/old-name": "owner/new-name", "acme/old-name": "",
	}}
	cache := NewCache(ff, filepath.Join(t.TempDir(), CacheFile))
	cache.Now = func() time.Time { return now }
	c := &Checker{Host: "box", Catalog: cat, Forge: cache, Now: func() time.Time { return now }}

	w := c.Repo(context.Background(), "widget")
	if w.Verdict != Holds || w.Checkout != "widget" || w.Renamed != "" || len(w.Results) != 3 {
		t.Fatalf("widget %+v", w)
	}
	if w.Results[0].Source != "checkouts" || w.Results[1].Source != "remotes" || w.Results[2].Source != "forge:github" || w.Results[2].Verdict != Holds {
		t.Fatalf("widget sources %+v", w.Results)
	}
	// A name nothing local has: every owner asked, all 404: fails.
	g := c.Repo(context.Background(), "ghost")
	if g.Verdict != Fails || !strings.Contains(g.Results[2].Output, "not found: owner/ghost, acme/ghost (asked as owner, who sees their own private repositories)") {
		t.Fatalf("ghost %+v", g)
	}
	// The forge follows a rename: the old name fails, with the new one.
	o := c.Repo(context.Background(), "old-name")
	if o.Verdict != Fails || o.Renamed != "new-name" || o.Forge != "owner/new-name" {
		t.Fatalf("old-name %+v", o)
	}
	// No answer from the forge is no verdict.
	u := c.Repo(context.Background(), "nowhere")
	if u.Verdict != Unknown {
		t.Fatalf("nowhere %+v", u)
	}
	for _, r := range append(append(w.Results, g.Results...), o.Results...) {
		if ps := r.Valid(); len(ps) > 0 {
			t.Errorf("%+v: %v", r, ps)
		}
	}
	// No forge at all: a name with no checkout is unknown, never absent.
	nf := &Checker{Host: "box", Catalog: cat}
	if v := nf.Repo(context.Background(), "ghost").Verdict; v != Unknown {
		t.Fatalf("no forge: %s", v)
	}
	if v := nf.Repo(context.Background(), "widget").Verdict; v != Holds {
		t.Fatalf("no forge, a checkout: %s", v)
	}
}

func TestForgeCacheKeepsAnswersForTheirTimeToLive(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ff := &fakeForge{repos: map[string]string{"a/x": "a/x", "a/y": ""}}
	path := filepath.Join(t.TempDir(), CacheFile)
	c := NewCache(ff, path)
	c.Now = func() time.Time { return now }
	ctx := context.Background()
	c.Lookup(ctx, "a", "x")
	c.Lookup(ctx, "a", "y")
	c.Lookup(ctx, "a", "z") // unavailable: never kept
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2 := NewCache(ff, path)
	c2.Now = func() time.Time { return now.Add(2 * time.Hour) }
	calls := ff.calls
	if _, _, err := c2.Lookup(ctx, "a", "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c2.Lookup(ctx, "a", "y"); err != ErrNotFound {
		t.Fatal(err)
	}
	if ff.calls != calls {
		t.Fatalf("fresh answers asked the forge again (%d calls)", ff.calls-calls)
	}
	c2.Lookup(ctx, "a", "z")
	if ff.calls != calls+1 {
		t.Fatal("an unavailable answer was kept")
	}
	// A miss expires after a day, a hit after a week.
	c3 := NewCache(ff, path)
	c3.Now = func() time.Time { return now.Add(25 * time.Hour) }
	calls = ff.calls
	c3.Lookup(ctx, "a", "x")
	c3.Lookup(ctx, "a", "y")
	if ff.calls != calls+1 {
		t.Fatalf("calls %d, want only the expired miss asked again", ff.calls-calls)
	}
}

func TestPathAndVersionChecksAtHead(t *testing.T) {
	home, cat := machine(t)
	root := filepath.Join(home, "projects", "widget")
	c := &Checker{Host: "box", Catalog: cat}
	ctx := context.Background()
	for _, tc := range []struct {
		path, verdict string
	}{{"cmd/serve/main.go", Holds}, {"serve/main.go", Holds}, {"cmd/serve", Holds}, {"cmd/old/main.go", Fails}} {
		if r := c.Path(ctx, "widget", root, tc.path); r.Verdict != tc.verdict {
			t.Errorf("%s: %+v", tc.path, r)
		}
	}
	if r := c.Path(ctx, "widget", filepath.Join(home, "nothing"), "a/b"); r.Verdict != Unknown {
		t.Errorf("no checkout: %+v", r)
	}
	for _, tc := range []struct {
		v, verdict, source string
	}{{"v1.3.0", Holds, "git:tags"}, {"v1.4.0", Holds, "manifest:package.json"}, {"v9.0.0", Fails, "git:tags+manifests"}} {
		if r := c.Version(ctx, "widget", root, tc.v); r.Verdict != tc.verdict || r.Source != tc.source {
			t.Errorf("%s: %+v", tc.v, r)
		}
	}
}

func TestRefsReadsOnlyDryForms(t *testing.T) {
	text := "Moved to github.com/Acme/Widget.git and https://github.com/acme/other. Edit `cmd/serve/main.go`, not `--flag`, `*.go`, `~/x/y`, `/etc/hosts`, `https://x.io/a/b` or `make`. Released v1.4.0, see v2 and 1.2.3."
	got := Refs(text)
	want := []Ref{{KindRepo, "acme", "widget"}, {KindRepo, "acme", "other"}, {KindPath, "", "cmd/serve/main.go"}, {KindVersion, "", "v1.4.0"}}
	if len(got) != len(want) {
		t.Fatalf("refs %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRefutesNeedsAllOfAKindToFailAndOneToHold(t *testing.T) {
	r := func(kind, verdict string) Result { return Result{Kind: kind, Verdict: verdict} }
	if ok, _ := Refutes([]Result{r(KindPath, Fails)}, []Result{r(KindPath, Holds)}); !ok {
		t.Error("a winner whose only path fails over a loser whose path holds")
	}
	if ok, _ := Refutes([]Result{r(KindPath, Fails), r(KindPath, Holds)}, []Result{r(KindPath, Holds)}); ok {
		t.Error("a winner with one path that holds is not refuted")
	}
	if ok, _ := Refutes([]Result{r(KindVersion, Fails)}, []Result{r(KindVersion, Holds)}); ok {
		t.Error("versions never refute (a planned version is no tag yet)")
	}
	if ok, _ := Refutes([]Result{r(KindRepo, Unknown)}, []Result{r(KindRepo, Holds)}); ok {
		t.Error("unknown refutes nothing")
	}
}

func TestGitHubForgeReadsGh(t *testing.T) {
	g := &GitHub{Bin: "gh", run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "api repos/a/x":
			return []byte(`{"full_name":"a/x2","archived":true}`), nil, nil
		case "api repos/a/gone":
			return []byte(`{"message":"Not Found","status":"404"}`), []byte("gh: Not Found (HTTP 404)"), &exec.ExitError{}
		case "api user --jq .login":
			return []byte("me\n"), nil, nil
		}
		return nil, []byte("error connecting to api.github.com"), &exec.ExitError{}
	}}
	ctx := context.Background()
	if r, err := g.Lookup(ctx, "a", "x"); err != nil || r.FullName != "a/x2" || !r.Archived {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := g.Lookup(ctx, "a", "gone"); err != ErrNotFound {
		t.Fatal(err)
	}
	if _, err := g.Lookup(ctx, "a", "down"); err != ErrUnavailable {
		t.Fatal(err)
	}
	if _, err := g.Lookup(ctx, "a", "../../user"); err != ErrNotFound {
		t.Fatal("a name that is no repository name is never asked")
	}
	if u := g.User(ctx); u != "me" {
		t.Fatal(u)
	}
}

func TestSameNamesOneRepository(t *testing.T) {
	home, _ := machine(t)
	// A clone of widget under another name (same origin), and a symlink.
	clone := filepath.Join(home, "projects", "widget-pr7")
	os.MkdirAll(clone, 0o755)
	gitRun(t, clone, "init", "-q", "-b", "main")
	gitRun(t, clone, "remote", "add", "origin", "https://github.com/acme/widget")
	os.Symlink("widget", filepath.Join(home, "projects", "gizmo"))
	other := filepath.Join(home, "projects", "other")
	os.MkdirAll(other, 0o755)
	gitRun(t, other, "init", "-q", "-b", "main")
	cat := repoid.LoadCatalog(home, "", filepath.Join(home, "projects"))
	c := &Checker{Host: "box", Catalog: cat}
	names := func(rs []Result) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Value)
		}
		return strings.Join(out, " ")
	}
	if got := names(c.Same("widget")); got != "gizmo widget-pr7" {
		t.Fatalf("widget: %q", got)
	}
	if got := names(c.Same("gizmo")); got != "widget widget-pr7" {
		t.Fatalf("gizmo: %q", got)
	}
	if got := names(c.Same("other")); got != "" {
		t.Fatalf("other: %q", got)
	}
}
