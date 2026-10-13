package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/derive"
)

// `tacit derive --set-branch` is how a machine is set up: it writes the
// config the curator reads, and the options every caller (derive, curate)
// builds carry it.
func TestSetBranchWritesTheConfigTheCuratorReads(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "tacit", "derive.yaml")
	t.Setenv("TACIT_DERIVE_CONFIG", cfg)

	if code := runDerive([]string{"--set-branch", "myrepo=dev"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	o, err := deriveOptions("/store", "host-a")
	if err != nil || o.Branches["myrepo"] != "dev" || !o.Fetch || o.Host != "host-a" || o.Store != "/store" {
		t.Fatalf("options %+v %v", o, err)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "myrepo: dev") {
		t.Fatalf("config:\n%s", b)
	}

	// removing it
	if code := runDerive([]string{"--set-branch", "myrepo="}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if o, _ := deriveOptions("/store", "host-a"); len(o.Branches) != 0 {
		t.Fatalf("still configured: %v", o.Branches)
	}

	// a config that does not parse stops the step instead of reading the wrong branch
	os.WriteFile(cfg, []byte("branches:\n  myrepo: --upload-pack=x\n"), 0o644)
	if _, err := deriveOptions("/store", "host-a"); err == nil {
		t.Fatal("a bad branch name was accepted")
	}
}

// The configured branch, fetched, end to end through the curator's step: the
// checkout has never seen the branch, the step fetches it, reads it, and the
// facts that reach the server say so.
func TestTheConfiguredBranchIsFetchedAndPushed(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	cfg := filepath.Join(r.root, "derive.yaml")
	t.Setenv("TACIT_DERIVE_CONFIG", cfg)
	if code := runDerive([]string{"--set-branch", "demo=dev"}); code != 0 {
		t.Fatalf("exit %d", code)
	}

	// an origin for the checkout with main and a dev that moved after the last fetch
	origin := filepath.Join(r.root, "origin.git")
	gitIn(t, r.root, "init", "-q", "--bare", "-b", "main", origin)
	demo := filepath.Join(r.projects, "demo")
	gitIn(t, demo, "remote", "add", "origin", origin)
	gitIn(t, demo, "push", "-q", "origin", "main")
	gitIn(t, demo, "checkout", "-q", "-b", "dev")
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(demo, "src", "dev_only.go"), []byte("package d // "+strings.Repeat("x", i+1)+"\n"), 0o644)
		gitIn(t, demo, "add", "-A")
		gitIn(t, demo, "commit", "-q", "-m", "dev")
	}
	devSha := gitIn(t, demo, "rev-parse", "HEAD")
	gitIn(t, demo, "push", "-q", "origin", "dev")
	gitIn(t, demo, "checkout", "-q", "main")
	gitIn(t, demo, "branch", "-D", "dev")
	gitIn(t, demo, "update-ref", "-d", "refs/remotes/origin/dev") // never fetched here

	o, err := deriveOptions(r.clone, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	o.Projects = r.projects
	rep, err := deriveStep(ctxMinute(t), r.s, o, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count(derive.Written) != 1 || rep.Repos[0].Sha != derive.Sha12(devSha) || rep.Repos[0].Detail != "" {
		t.Fatalf("want a read at origin/dev %s: %+v", derive.Sha12(devSha), rep.Repos)
	}
	body := gitIn(t, r.bare, "show", "main:derived/demo/demo-module-map.md")
	if !strings.Contains(body, "`origin/dev` (the branch configured for this repo)") || !strings.Contains(body, "read at commit "+derive.Sha12(devSha)) {
		t.Fatalf("the pushed fact does not say what it was read at:\n%s", body)
	}
	if gitIn(t, demo, "rev-parse", "--abbrev-ref", "HEAD") != "main" || gitIn(t, demo, "status", "--porcelain") != "" {
		t.Fatal("the checkout was touched")
	}
	r.assertServerLintsClean()
}
