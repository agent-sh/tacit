package main

// Checked claims (SPEC.md, "Checked claims"): the curator's steps run code's
// checks of dry claims (a repository's existence and current name, a path at
// HEAD, a version against tags and manifests), show the results to the
// model, and refuse a decision a result refutes. This file builds the
// machine's checker and checks a fact's references.

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// newForge is the forge the checks ask: GitHub through gh when it is
// installed and logged in, none with TACIT_FORGE=off. Tests substitute it
// (TestMain: none), so no test asks a real forge.
var newForge = func(ctx context.Context) check.Forge {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TACIT_FORGE"))) {
	case "off", "none", "0", "false", "no":
		return nil
	}
	return check.NewGitHub(ctx)
}

// newChecker is this machine's checker for the store at dir: catalog (this
// machine's, with other machines' names), every machine's shared catalog,
// and the forge with its answers cached in the state directory.
func newChecker(ctx context.Context, home, dir string, catalog *repoid.Catalog) *check.Checker {
	return &check.Checker{
		Host:    store.ShortHost(),
		Catalog: catalog,
		Shared:  repoid.StoreEvidence(dir),
		Forge:   check.NewCache(newForge(ctx), filepath.Join(platform.StateDir(home), check.CacheFile)),
	}
}

// factRepo is the repository a fact is about, with its checkout on this
// machine: its project folder's, else its one applies_to repository; ""
// when it names none or its repository has no checkout here.
func factRepo(catalog *repoid.Catalog, f *fact.Fact) (name, root string) {
	n, ok := strings.CutPrefix(f.Meta.Scope, "projects/")
	if !ok && len(f.Meta.AppliesTo.Repos) == 1 {
		n, ok = f.Meta.AppliesTo.Repos[0], true
	}
	if !ok {
		return "", ""
	}
	if own := catalog.AliasOf(n); own != "" {
		n = own
	}
	if r, ok := catalog.Repo(n); ok && r.Root != "" {
		return r.Slug(), r.Root
	}
	return n, ""
}

// factChecks are the checks of f's dry references (check.Refs in its
// description and body): forge repositories always, paths and versions in
// its repository's checkout here. nil with no checker.
func factChecks(ctx context.Context, ch *check.Checker, f *fact.Fact) []check.Result {
	if ch == nil || f == nil {
		return nil
	}
	repo, root := factRepo(ch.Catalog, f)
	return ch.Facts(ctx, f.Meta.Description+"\n"+f.Body, repo, root)
}

// storeChecker is newChecker for the store at dir (its shared catalogs;
// with the local layer, this machine's overlay catalog too).
func storeChecker(ctx context.Context, dir string, layer bool) *check.Checker {
	home, _ := os.UserHomeDir()
	return newChecker(ctx, home, dir, knownCatalog(home, dir, layer))
}

// refuter is the checks' guard on keeping one fact over another: true when
// code's checks refute winner's dry references where loser's hold
// (check.Refutes). Each fact is checked once.
func refuter(ctx context.Context, ch *check.Checker) func(winner, loser *fact.Fact) bool {
	memo := map[*fact.Fact][]check.Result{}
	get := func(f *fact.Fact) []check.Result {
		if r, ok := memo[f]; ok {
			return r
		}
		r := factChecks(ctx, ch, f)
		memo[f] = r
		return r
	}
	return func(w, l *fact.Fact) bool {
		ok, _ := check.Refutes(get(w), get(l))
		return ok
	}
}
