// Package gitev reads git as evidence about facts. Given facts it finds what
// they are about (repos under one root, paths, identifiers), reads the
// history of those subjects with bounded git queries, and merges it with the
// facts' own dates into one dated timeline. No model is involved: the same
// facts and the same repos give the same lines.
//
// Git here is strong evidence for what happened and when, and for what the
// numbers were. It never replaces an owner ruling: the committer is usually
// the main agent or a subagent, so every commit carries its author and
// committer identity and its Co-Authored-By trailers, and callers decide what
// an identity means.
//
// The package serves the review page and the timeline command, and is meant
// for the curator's staleness check and git-sourced facts next. It reads
// repositories with read-only git commands: no locks, no writes, and no
// network (a partial clone's missing files are not fetched).
package gitev

import (
	"bytes"
	"context"
	"errors"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Git runs the queries. The zero value is not usable; call New.
type Git struct {
	// Root holds the checkouts: Root/<name> is a git working tree.
	Root string
	// CacheDir, when set, keeps per-repo evidence keyed by the commit it was
	// read at and the subject searched, so a pair of facts about an unchanged
	// repo costs nothing the second time.
	CacheDir string
	// Timeout bounds one git command. Every query is a command, so a huge
	// repository or a loaded machine cannot stall a caller for long; what
	// finished before the deadline is used and the repo evidence says so.
	Timeout time.Duration
	// RepoBudget bounds all queries for one repo.
	RepoBudget time.Duration
	// MaxRepos is how many checkouts one subject may read (default 3).
	MaxRepos int
	// Parallel bounds the git processes running at once per repo (default 3).
	Parallel int

	// CacheMaxFiles bounds the disk cache (default 4000 files).
	CacheMaxFiles int

	// Ordinary names this machine's repos that are also plain words (beyond
	// the built-in few): a bare mention of one is not evidence a fact is
	// about that repo. Set from ordinary_names in derive.yaml.
	Ordinary map[string]bool

	// Resolve, when set, names the commit a light read (Window.Light) is made at
	// for a repo: its default or configured branch, so that the branch a
	// checkout happens to have checked out never decides what is "gone". It
	// returns the full sha and a ref name for the notes. Nil reads HEAD.
	Resolve func(ctx context.Context, repo, dir string) (sha, ref string, err error)

	// Low runs every git command under `nice -n 19` and, where there is an
	// `ionice`, in the idle I/O class. A scan that starts many git processes
	// for many minutes uses it; a priority set on the calling process would only
	// reach one thread of it on Linux, so the children are wrapped instead.
	Low bool

	verOnce     sync.Once
	noLazy      bool   // git honours GIT_NO_LAZY_FETCH (2.44 and later)
	testVersion string // tests: pretend `git --version` said this

	once  sync.Once
	repos map[string]bool
	runs  int64 // git commands started, for tests
	puts  int   // cache writes, to prune every so often
	mu    sync.Mutex
}

// New returns a Git reading checkouts under root.
func New(root string) *Git {
	return &Git{Root: root, Timeout: 15 * time.Second, RepoBudget: 40 * time.Second, MaxRepos: 3, Parallel: 3}
}

// Runs reports how many git commands were started.
func (g *Git) Runs() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs
}

// Repos lists the checkouts under Root: directories holding a .git entry,
// except linked worktree copies named wt-*.
func (g *Git) Repos() map[string]bool {
	g.once.Do(func() {
		g.repos = map[string]bool{}
		ents, _ := os.ReadDir(g.Root)
		for _, e := range ents {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, "wt-") || strings.HasPrefix(n, ".") {
				continue
			}
			if _, err := os.Stat(filepath.Join(g.Root, n, ".git")); err == nil {
				g.repos[n] = true
			}
		}
	})
	return g.repos
}

// RepoNames is Repos as a sorted list.
func (g *Git) RepoNames() []string {
	var out []string
	for n := range g.Repos() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (g *Git) maxRepos() int {
	if g.MaxRepos > 0 {
		return g.MaxRepos
	}
	return 3
}

// noise are pathspecs excluded from content searches: lock files, bundles
// and data dumps change constantly and match everything.
var noise = []string{
	":(exclude)*.lock", ":(exclude)*-lock.json", ":(exclude)*.min.js", ":(exclude)*.map", ":(exclude)*.svg",
	":(exclude)*.ipynb", ":(exclude)*.snap", ":(exclude)*.jsonl", ":(exclude)*.csv",
	":(exclude)vendor/*", ":(exclude)node_modules/*", ":(exclude)third_party/*",
}

// run starts git in dir with a hard deadline: its own process group, killed
// as a group when the context ends. Output past max bytes cancels the
// command and is dropped (truncated reports it). Read-only: optional locks
// are off, so a query never takes an index lock a working tree needs.
func (g *Git) run(ctx context.Context, dir string, max int, args ...string) (out string, truncated bool, err error) {
	return g.runIn(ctx, dir, max, "", args...)
}

// runIn is run with text on the command's standard input (the object names
// that cat-file --batch reads).
func (g *Git) runIn(ctx context.Context, dir string, max int, stdin string, args ...string) (out string, truncated bool, err error) {
	to := g.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	g.mu.Lock()
	g.runs++
	g.mu.Unlock()
	full := append([]string{"--no-pager", "-c", "core.quotepath=off", "-c", "log.showSignature=false", "-c", "core.fsmonitor=false", "-c", "diff.external="}, args...)
	name, argv := "git", full
	if g.Low {
		name, argv = lowPriority(name, argv)
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = dir
	// GIT_NO_LAZY_FETCH: a blob:none clone must not fetch the blobs a
	// query needs; the query fails and says so instead.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "LC_ALL=C")
	platform.Prepare(cmd)
	cmd.WaitDelay = 2 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	w := &capWriter{max: max, cancel: cancel}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &errb
	err = cmd.Run()
	if w.over {
		// We stopped it ourselves; what was read is what the caller gets.
		return w.buf.String(), true, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return w.buf.String(), true, ctx.Err()
		}
		msg := strings.TrimSpace(errb.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return w.buf.String(), false, errors.New("git " + args[0] + ": " + msg)
	}
	return w.buf.String(), false, nil
}

type capWriter struct {
	buf    bytes.Buffer
	max    int
	over   bool
	cancel context.CancelFunc
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if c.max > 0 && c.buf.Len()+len(p) > c.max {
		c.buf.Write(p[:c.max-c.buf.Len()])
		c.over = true
		c.cancel()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// noLazyFetch reports whether this git honours GIT_NO_LAZY_FETCH, which came
// in 2.44. An older git, or one whose version cannot be read, would fetch a
// partial clone's missing files into the owner's repo, so such repos are not
// read at all.
func (g *Git) noLazyFetch(ctx context.Context) bool {
	g.verOnce.Do(func() {
		v := g.testVersion
		if v == "" {
			o, _, err := g.run(ctx, "", 256, "--version")
			if err != nil {
				return
			}
			v = o
		}
		f := strings.Fields(strings.TrimSpace(v))
		if len(f) < 3 {
			return
		}
		p := strings.Split(f[2], ".")
		if len(p) < 2 {
			return
		}
		maj, e1 := strconv.Atoi(p[0])
		min, e2 := strconv.Atoi(p[1])
		g.noLazy = e1 == nil && e2 == nil && (maj > 2 || (maj == 2 && min >= 44))
	})
	return g.noLazy
}

// isPartial reports whether dir is a partial clone (a promisor remote).
func (g *Git) isPartial(ctx context.Context, dir string) bool {
	o, _, err := g.run(ctx, dir, 256, "config", "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor)$`)
	return err == nil && strings.TrimSpace(o) != ""
}

// lowPriority wraps a command in nice 19 and, when ionice exists, the idle I/O
// class. Each wrapper execs the next, so the process (and its process group)
// that git runs in is the one the caller kills.
func lowPriority(name string, args []string) (string, []string) {
	if p, err := exec.LookPath("nice"); err == nil {
		name, args = p, append([]string{"-n", "19", name}, args...)
	}
	if p, err := exec.LookPath("ionice"); err == nil {
		name, args = p, append([]string{"-c3", name}, args...)
	}
	return name, args
}
