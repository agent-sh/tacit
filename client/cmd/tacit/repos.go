package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
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
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	_ = repoid.Record(reg, repos...)
	// The checkouts that are no main checkout (a linked worktree, a separate
	// clone), recorded with the repository they belong to while they exist,
	// so a folder of their name keeps its ancestor after they are removed.
	var checkouts []repoid.Repo
	for _, w := range ws {
		if w.Checkout.Root != "" {
			checkouts = append(checkouts, w.Checkout)
		}
	}
	_ = repoid.RecordCheckouts(reg, repoid.LoadMains(home, repoid.ProjectsDir(home)), checkouts...)
}

// acceptPendingRepos counts the evidence of work in each window's pending
// checkout (outside home or in a temp directory, dream.Window.Pending): the
// sessions the registry holds for it and this run's windows, recorded in
// the registry when record is set, and names the windows' repository for
// the checkout once repoid.MinWorkSessions sessions worked there. With
// record set it also keeps every other window's evidence of work, in its
// cwd's checkout or non-git project folder (dream.Window.WorkRoot and
// WorkFolder), which a project's assessment reads.
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
		var items []repoid.WorkItem
		for _, w := range ws {
			switch {
			case w.Work == "":
			case w.Pending.Root != "":
				items = append(items, repoid.WorkItem{Root: w.Pending.Root, Session: w.Session, By: w.Work})
			case w.WorkRoot != "":
				items = append(items, repoid.WorkItem{Root: w.WorkRoot, Session: w.Session, By: w.Work})
			case w.WorkFolder != "":
				items = append(items, repoid.WorkItem{Folder: w.WorkFolder, Session: w.Session, By: w.Work})
			}
		}
		_ = repoid.RecordWorks(reg, items)
	}
	dream.AcceptPending(ws, work)
}

// recordToolUsage keeps the tools and command programs each window's
// session used in this machine's state (dream.ToolsFile), never the store.
// Best effort.
func recordToolUsage(home string, ws []dream.Window) {
	p := filepath.Join(platform.StateDir(home), dream.ToolsFile)
	u := dream.LoadToolUsage(p)
	day := time.Now().Format("2006-01-02")
	for _, w := range ws {
		tools, programs := dream.ToolsOf(w.Lines)
		u.Add(w.Session, day, tools, programs)
	}
	_ = u.Save(p, time.Now())
}

// sharedCatalog is this machine's repository catalog as the store carries
// it (repoid.CatalogFile): the registry's and the projects directory's
// repositories, not the current directory's.
func sharedCatalog(home string) []byte {
	cache := repoid.LoadCommitCache(filepath.Join(platform.StateDir(home), repoid.CommitCacheFile))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := repoid.LoadCatalog(home, filepath.Join(platform.StateDir(home), repoid.RegistryFile), projectsRoot(home)).SharedWith(func(root string) (repoid.Commits, bool) {
		if ctx.Err() != nil {
			return repoid.Commits{}, false
		}
		return cache.Get(ctx, root)
	})
	_ = cache.Save()
	return b
}

// catalogRel is the store path of this machine's catalog: in its dreamer's
// inbox folder, the one place the dreamer writes.
func catalogRel() string {
	return "inbox/dreamer@" + store.ShortHost() + "/" + repoid.CatalogFile
}

// catalogChanged reports whether the catalog at HEAD differs from b (or
// there is none).
func catalogChanged(ctx context.Context, s *store.Store, rel string, b []byte) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", "HEAD:"+rel)
	cmd.Dir = s.Dir
	if cmd.Run() != nil {
		return true, nil // not at HEAD (or no HEAD yet)
	}
	cmd = exec.CommandContext(ctx, "git", "show", "HEAD:"+rel)
	cmd.Dir = s.Dir
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("read the catalog at HEAD: %w", err)
	}
	return !bytes.Equal(out, b), nil
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
