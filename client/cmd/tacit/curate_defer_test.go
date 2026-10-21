package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// A note whose scope names a repository no catalog knows waits, from the
// first run that held it back, up to maxRepoDefer; a known repository, an
// alias, another scope or a malformed one goes on at once, and a note that
// waited its time goes on to the normal path.
func TestDeferUnknownReposIsBounded(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	cat := repoid.LoadCatalog(home, "", "")
	cat.AddShared([]byte(`{"repos":[{"name":"gadget","aliases":["gadget-svc"]}]}`))
	env := validate.NewEnv("", "", cat, "box")
	note := func(name, scope string) *fact.Fact {
		return &fact.Fact{Meta: fact.Meta{Name: name, Scope: scope}}
	}
	pending := []*fact.Fact{note("a", "projects/nowhere"), note("b", "projects/gadget"), note("c", "projects/gadget-svc"), note("d", "user"), note("e", "projects/Not_A_Slug"), note("f", "projects/Nowhere")}
	cst := &curatorState{}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	kept, n, changed := deferUnknownRepos(pending, env, cst, now)
	if n != 2 || !changed || len(kept) != 4 || cst.DeferredRepo["a"] != "2026-10-01" || cst.DeferredRepo["f"] != "2026-10-01" {
		t.Fatalf("first run: %d deferred, kept %d, state %v", n, len(kept), cst.DeferredRepo)
	}
	// A later run inside the bound: still waiting, the first date kept.
	kept, n, changed = deferUnknownRepos(pending, env, cst, now.Add(13*24*time.Hour))
	if n != 2 || changed || cst.DeferredRepo["a"] != "2026-10-01" {
		t.Fatalf("day 13: %d deferred, changed %v, %v", n, changed, cst.DeferredRepo)
	}
	// Past it: the normal path (the entries face the checks and the strikes).
	kept, n, _ = deferUnknownRepos(pending, env, cst, now.Add(15*24*time.Hour))
	if n != 0 || len(kept) != 6 {
		t.Fatalf("day 15: %d deferred, kept %d", n, len(kept))
	}
	// Its repository known: the note goes on and leaves the state.
	cat.AddShared([]byte(`{"repos":[{"name":"nowhere"}]}`))
	_, n, changed = deferUnknownRepos(pending, validate.NewEnv("", "", cat, "box"), cst, now.Add(24*time.Hour))
	if n != 0 || !changed || len(cst.DeferredRepo) != 0 {
		t.Fatalf("known now: %d deferred, %v", n, cst.DeferredRepo)
	}
}

// End to end on a fresh store: a note filed under a repository no catalog
// knows is not sent to the model and stays in the inbox; once a machine's
// catalog naming it reaches the store, the next curate takes it.
func TestCurateWaitsForTheCatalogOfANotesRepo(t *testing.T) {
	noAssess(t)
	_, s, prompts := treeHome(t)
	ctx := context.Background()
	// A note from another machine's dreamer, whose catalog is not in the store yet.
	n := &fact.Fact{Path: "inbox/dreamer@farbox/gizmo-queue.md", Meta: fact.Meta{Name: "gizmo-queue", Description: "the gizmo queue drains every minute", Type: "unclassified", Scope: "projects/gizmo", Source: fact.Source{Harness: "dreamer", Host: "farbox", Kind: "observed"}, Created: "2026-10-20"}, Body: "The gizmo queue drains every minute.\n"}
	writeFactFile(t, s.Dir, n)
	if _, err := store.Git(ctx, s.Dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Git(ctx, s.Dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "farbox note"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_USER", answer(t, entry("gizmo-queue", "gizmo-queue-drains", "The gizmo queue drains every minute", "design_decision", "projects/gizmo")))
	countSteps(t)
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	for _, p := range prompts() {
		if strings.Contains(p, "gizmo") {
			t.Fatal("a note about an unknown repository was sent to the model")
		}
	}
	cst := loadCuratorState(filepath.Join(platform.StateDir(os.Getenv("HOME")), "curator.json"))
	if cst.DeferredRepo["gizmo-queue"] == "" {
		t.Fatalf("no deferral recorded: %+v", cst)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "gizmo")); err == nil {
		t.Fatal("the deferred note was curated")
	}
	// farbox's catalog arrives.
	cat := filepath.Join(s.Dir, "inbox", "dreamer@farbox", repoid.CatalogFile)
	os.WriteFile(cat, []byte(`{"generated":"x","repos":[{"name":"gizmo","urls":["github.com/acme/gizmo"]}]}`+"\n"), 0o644)
	store.Git(ctx, s.Dir, "add", "-A")
	store.Git(ctx, s.Dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "farbox catalog")
	if code := runCurate(nil); code != 0 {
		t.Fatalf("second curate exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "gizmo", "gizmo-queue-drains.md")); err != nil {
		t.Fatalf("the note was not curated once its repository was known: %v", err)
	}
	if cst := loadCuratorState(filepath.Join(platform.StateDir(os.Getenv("HOME")), "curator.json")); cst.DeferredRepo["gizmo-queue"] != "" {
		t.Errorf("the deferral outlived the catalog: %v", cst.DeferredRepo)
	}
}
