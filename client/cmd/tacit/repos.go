package main

import (
	"os"
	"path/filepath"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// recordWindowRepos adds the repositories the dreamer resolved windows to
// (repoid, a real git root each) to this machine's registry, which the
// curator's checks read (machineCatalog). Best effort: a registry that cannot
// be written only means the curator knows fewer repositories.
func recordWindowRepos(home string, ws []dream.Window) {
	seen := map[string]bool{}
	var repos []repoid.Repo
	for _, w := range ws {
		if w.RepoRoot == "" || seen[w.RepoRoot] {
			continue
		}
		seen[w.RepoRoot] = true
		if r, ok := repoid.Of(w.RepoRoot, home); ok {
			repos = append(repos, r)
		}
	}
	_ = repoid.Record(filepath.Join(platform.StateDir(home), repoid.RegistryFile), repos...)
}

// acceptPendingRepos counts the evidence of work in each window's pending
// checkout (outside home or in a temp directory, dream.Window.Pending): the
// sessions the registry holds for it and this run's windows, recorded in
// the registry when record is set, and names the windows' repository for
// the checkout once repoid.MinWorkSessions sessions worked there.
func acceptPendingRepos(home string, ws []dream.Window, record bool) {
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	work := map[string]map[string]bool{}
	for _, w := range ws {
		root := w.Pending.Root
		if root == "" || work[root] != nil {
			continue
		}
		work[root] = map[string]bool{}
		for _, e := range repoid.WorkFor(reg, root) {
			work[root][e.Session] = true
		}
	}
	if record {
		for _, w := range ws {
			if w.Pending.Root != "" && w.Work != "" {
				_ = repoid.RecordWork(reg, w.Pending.Root, w.Session, w.Work)
			}
		}
	}
	dream.AcceptPending(ws, work)
}

// machineCatalog is the repositories this machine has a checkout of: the
// dreamer's registry and the projects directory, plus the repository of the
// current directory.
func machineCatalog(home string) *repoid.Catalog {
	c := repoid.LoadCatalog(home, filepath.Join(platform.StateDir(home), repoid.RegistryFile), projectsRoot(home))
	if cwd, err := os.Getwd(); err == nil {
		if r, ok := c.Of(cwd); ok && c.Accepted(r) {
			c.Add(r)
		}
	}
	return c
}
