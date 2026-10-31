package main

// A workspace is a folder that is in no repository but holds several: one
// whose packages are each a checkout under src/ (a multi-package workspace),
// a folder of checkouts. A session started there works in all of them, so
// its repositories are all of them (SPEC.md, Delivery, "Workspaces"): the
// SessionStart repo block, the prompt hook's scopes, the PreToolUse push and
// tacit_tree/tacit_search read every one, and a file path under one of them
// narrows to that one.
import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

const (
	// workspaceDepth is how deep under cwd a repository's root may be: 1 for
	// a folder of checkouts (dir/<repo>), 2 for a multi-package workspace
	// (ws/src/<pkg>).
	workspaceDepth = 2
	// workspaceMaxRepos is the most distinct repositories a workspace holds;
	// a folder with more is a general folder of projects, not one piece of
	// work, and gets no repositories (as before workspaces). Measured on a
	// real machine (eval/VALUE.md, "Workspaces"): multi-package workspaces
	// and worktree folders hold a handful of distinct repositories, the
	// general folders above them more than the cap.
	workspaceMaxRepos = 8
	// workspaceMaxRoots bounds the git roots the scan resolves (each one a
	// few file reads), and workspaceMaxEntries the directory entries it looks
	// at, so a huge folder costs a bounded share of a hook's budget; past
	// either the folder is not a workspace. A multi-package workspace holds a
	// few git roots; in a folder of a few hundred task worktrees the PreToolUse
	// hook took 350 ms of its 400 ms without this cap, in a small one about
	// what a repository costs.
	workspaceMaxRoots   = 64
	workspaceMaxEntries = 2000
)

// workspaceRepos are the repositories of the workspace cwd is: the git
// roots (repoid: a real .git directory or gitfile, a worktree's main
// repository) at most workspaceDepth below cwd, not inside dot folders or
// node_modules, one per repository (a repository's worktrees are one), in
// name order. None when cwd is in a repository (its repository is the
// session's), when cwd is the home directory, the file system root or a
// folder above home (never scanned), or when the folder holds more than
// workspaceMaxRepos repositories, workspaceMaxRoots git roots or
// workspaceMaxEntries entries.
func workspaceRepos(cwd, home string) []repoid.Repo {
	if cwd == "" || !filepath.IsAbs(cwd) || !deliveryOn("WORKSPACE", true) {
		return nil
	}
	dir := repoid.RealPath(filepath.Clean(cwd))
	if refusedWorkspace(dir, home) {
		return nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	c := repoid.NewCache(home)
	if _, ok := c.Of(dir); ok {
		return nil
	}
	byName := map[string]repoid.Repo{}
	entries, roots := 0, 0
	var walk func(d string, depth int) bool
	walk = func(d string, depth int) bool {
		es, err := os.ReadDir(d)
		if err != nil {
			return true
		}
		for _, e := range es {
			if entries++; entries > workspaceMaxEntries {
				return false
			}
			n := e.Name()
			if strings.HasPrefix(n, ".") || n == "node_modules" {
				continue
			}
			p := filepath.Join(d, n)
			isDir := e.IsDir()
			if e.Type()&os.ModeSymlink != 0 {
				fi, err := os.Stat(p)
				isDir = err == nil && fi.IsDir()
			}
			if !isDir {
				continue
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				if roots++; roots > workspaceMaxRoots {
					return false
				}
				// A git root, as repoid finds it from inside: the checkout
				// it names must be p itself.
				if r, ok := c.Of(p); ok && r.Worktree == repoid.RealPath(p) {
					if _, have := byName[strings.ToLower(r.Name)]; !have {
						byName[strings.ToLower(r.Name)] = r
						if len(byName) > workspaceMaxRepos {
							return false
						}
					}
				}
				continue
			}
			// A symlinked folder is looked at, never walked: it may lead
			// anywhere, home included.
			if depth < workspaceDepth && e.Type()&os.ModeSymlink == 0 {
				if !walk(p, depth+1) {
					return false
				}
			}
		}
		return true
	}
	if !walk(dir, 1) {
		return nil
	}
	out := make([]repoid.Repo, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// refusedWorkspace: the home directory, the file system root and every
// folder above home are never scanned for repositories (a session started
// there is in no project, and a scan of home would read every checkout the
// user has).
func refusedWorkspace(dir, home string) bool {
	if dir == string(filepath.Separator) || filepath.Dir(dir) == dir {
		return true
	}
	if home == "" {
		return false
	}
	h := repoid.RealPath(filepath.Clean(home))
	return dir == h || strings.HasPrefix(h, dir+string(filepath.Separator))
}

// workspaceNames are the names delivery knows the workspace's repositories
// by: each one's repoAliases, lower case, distinct.
func workspaceNames(rs []repoid.Repo) []string {
	var out []string
	for _, r := range rs {
		out = append(out, repoAliases(r)...)
	}
	return lowerDistinct(out)
}

func lowerDistinct(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		x = strings.ToLower(strings.TrimSpace(x))
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// workspaceDisplay names a workspace in a hook's title: the folder and its
// repositories ("the workspace ws: WidgetCore, GadgetAgents").
func workspaceDisplay(cwd string, rs []repoid.Repo) string {
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.Name
	}
	return "the workspace " + filepath.Base(filepath.Clean(cwd)) + ": " + strings.Join(names, ", ")
}

// withWorkspace is sit with the workspace cwd is when the session has no
// repository of its own: its repositories (Situation.Workspace) and its
// display name. It returns the repositories too, for narrowing.
func withWorkspace(sit search.Situation, cwd string) (search.Situation, []repoid.Repo) {
	if sit.Repo != "" {
		return sit, nil
	}
	home, _ := os.UserHomeDir()
	ws := workspaceRepos(cwd, home)
	if len(ws) == 0 {
		return sit, nil
	}
	sit.Workspace = workspaceNames(ws)
	sit.RepoDisplay = workspaceDisplay(cwd, ws)
	return sit, ws
}

// narrowTo is sit for the one workspace repository r: the session works in
// it (a file path under it), so it is the situation's repository and the
// others leave.
func narrowTo(sit search.Situation, r repoid.Repo) search.Situation {
	sit.Repo, sit.RepoAliases, sit.Workspace, sit.RepoDisplay = r.Name, repoAliases(r), nil, ""
	return sit
}

// workspaceRepoOf is the workspace repository p (absolute, or relative to
// cwd) is in, and false when it is in none of them.
func workspaceRepoOf(ws []repoid.Repo, cwd, p string) (repoid.Repo, bool) {
	for _, r := range ws {
		if _, ok := repoRel(r, cwd, p); ok {
			return r, true
		}
	}
	return repoid.Repo{}, false
}

// inWorkspace reports whether a read of the store for cwd (readSession,
// with no repo named) answers for a workspace: cwd in no repository and no
// project folder a record names, and a workspace there.
func inWorkspace(recs map[string]*record.Record, cwd, repo string) bool {
	if repo != "" || cwd == "" {
		return false
	}
	if _, ok := sessionRepo(cwd); ok {
		return false
	}
	home, _ := os.UserHomeDir()
	if rootFolder(recs, store.ShortHost(), home, cwd) != "" {
		return false
	}
	return len(workspaceRepos(cwd, home)) > 0
}

// workspaceRepoNamed is the workspace repository known by name n.
func workspaceRepoNamed(ws []repoid.Repo, n string) (repoid.Repo, bool) {
	for _, r := range ws {
		for _, a := range repoAliases(r) {
			if strings.EqualFold(a, n) {
				return r, true
			}
		}
	}
	return repoid.Repo{}, false
}
