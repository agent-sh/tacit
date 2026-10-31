package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

type repoList []string

func (r *repoList) String() string     { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error { *r = append(*r, v); return nil }

// defaultProjects is where the checkouts live: $TACIT_PROJECTS, else ~/projects.
func defaultProjects() string {
	home, _ := os.UserHomeDir()
	return projectsRoot(home)
}

// projectsRoot is $TACIT_PROJECTS, else home/projects.
func projectsRoot(home string) string {
	if d := os.Getenv("TACIT_PROJECTS"); d != "" {
		return d
	}
	return filepath.Join(home, "projects")
}

// runDerive implements `tacit derive`: the curator's derived-facts step on
// its own. `tacit curate` runs the same step after the inbox batches.
//
//	tacit derive                 sync, write derived/<repo>/ into the store clone, commit and push as curator
//	tacit derive --dry-run       read the store, change nothing, report (and with -v print the facts)
//	tacit derive --out DIR       read the store, write derived/<repo>/ under DIR, lint it; nothing touches the store
func runDerive(args []string) int {
	fs := flag.NewFlagSet("derive", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "compute and report, write nothing")
	out := fs.String("out", "", "write derived/ under this directory instead of the store (no sync, no commit)")
	force := fs.Bool("force", false, "rebuild even when derived_from already equals the commit")
	takeOver := fs.Bool("take-over", false, "also rewrite and retire derived facts that another host produced")
	noFetch := fs.Bool("no-fetch", false, "do not fetch origin's branch first (the facts are read at the ref the checkout already has)")
	setBranch := fs.String("set-branch", "", "record <repo>=<branch> in "+derive.ConfigPath()+" (empty branch removes it) and exit")
	projects := fs.String("projects", defaultProjects(), "directory with the checkouts")
	verbose := fs.Bool("v", false, "print each fact built")
	var only repoList
	fs.Var(&only, "repo", "restrict to this repo (repeatable)")
	fs.Parse(args)

	if *setBranch != "" {
		repo, branch, ok := strings.Cut(*setBranch, "=")
		if !ok || repo == "" {
			fmt.Fprintln(os.Stderr, "derive: --set-branch wants <repo>=<branch>")
			return 2
		}
		if err := derive.SetBranch(derive.ConfigPath(), repo, branch); err != nil {
			fmt.Fprintln(os.Stderr, "derive:", err)
			return 2
		}
		if branch == "" {
			fmt.Printf("derive: %s reads the remote default branch again (%s)\n", repo, derive.ConfigPath())
		} else {
			fmt.Printf("derive: %s is read at branch %s (%s)\n", repo, branch, derive.ConfigPath())
		}
		return 0
	}

	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "derive: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && *out == "" && refuseWriter(context.Background(), s, "curator", "derive") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	o, err := deriveOptions(s.Dir, store.ShortHost())
	if err != nil {
		fmt.Fprintln(os.Stderr, "derive:", err)
		return 2
	}
	// Only a real run fetches. A dry run and --out read the refs the checkouts
	// have: they must not move anything in any checkout or use the network.
	o.Projects, o.Only, o.Force, o.TakeOver, o.Fetch = *projects, only, *force, *takeOver, !*noFetch && !*dry && *out == ""

	if *out != "" {
		o.Existing = derive.FS{Root: *out}
		rep, err := derive.Run(ctx, o)
		if err != nil {
			fmt.Fprintln(os.Stderr, "derive:", err)
			return 2
		}
		if !*dry {
			if err := rep.Apply(ctx, *out); err != nil {
				fmt.Fprintln(os.Stderr, "derive:", err)
				return 2
			}
		}
		printDerive(rep, *verbose)
		if !*dry {
			if fs, err := lint.Run(*out); err == nil && len(fs) > 0 {
				for _, f := range fs {
					fmt.Fprintln(os.Stderr, "derive: lint:", f)
				}
				return 1
			}
		}
		return exitFor(rep)
	}

	if *dry {
		o.Existing = committedSource(ctx, s)
		rep, err := derive.Run(ctx, o)
		if err != nil {
			fmt.Fprintln(os.Stderr, "derive:", err)
			return 2
		}
		printDerive(rep, *verbose)
		return exitFor(rep)
	}

	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "derive: sync:", err)
		return 1
	}
	rep, err := deriveStep(ctx, s, o, false)
	printDerive(rep, *verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, "derive:", err)
		return 1
	}
	return exitFor(rep)
}

// deriveOptions are the options every caller shares: the store, this host, and
// the per-repo branches from the config file. Callers set the rest. A config
// file that does not parse stops the step: a typo that silently derives the
// wrong branch is worse than a run that says so.
func deriveOptions(storeDir, host string) (derive.Options, error) {
	cfg, err := derive.LoadConfig(derive.ConfigPath())
	if err != nil {
		return derive.Options{}, err
	}
	return derive.Options{Store: storeDir, Host: host, Branches: cfg.Branches, Fetch: true}, nil
}

func exitFor(rep *derive.Report) int {
	if rep != nil && rep.Count(derive.Failed) > 0 {
		return 1
	}
	return 0
}

func printDerive(rep *derive.Report, verbose bool) {
	if rep == nil {
		return
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "derive: warning:", w)
	}
	for _, r := range rep.Repos {
		line := fmt.Sprintf("derive: %-9s %s", r.Status, r.Repo)
		if r.Sha != "" {
			line += " " + r.Sha
		}
		if len(r.Paths) > 0 {
			line += " (" + strings.Join(r.Paths, ", ") + ")"
		}
		if r.Detail != "" {
			line += " - " + strings.TrimSpace(r.Detail)
		}
		// Most project scopes have no checkout, and most checkouts did not
		// move since the last run; the summary line counts both. Anything with
		// something to say (a name clash, another host's facts, a refusal) is
		// always shown.
		quiet := (r.Status == derive.Skipped && r.Gone) || (r.Status == derive.Unchanged && r.Detail == "")
		if quiet && !verbose {
			continue
		}
		fmt.Println(line)
		if verbose {
			for _, f := range r.Files {
				fmt.Printf("---- %s\n%s\n", f.Rel, f.Data)
			}
		}
	}
	fmt.Printf("derive: %d written, %d unchanged, %d retired, %d skipped, %d failed\n",
		rep.Count(derive.Written), rep.Count(derive.Unchanged), rep.Count(derive.Retired), rep.Count(derive.Skipped), rep.Count(derive.Failed))
}

// committedSource reads the derived files as the server has them (origin/main
// of the clone, else its HEAD), never the working tree: an interrupted run
// can leave that dirty, and "unchanged" must mean unchanged where it counts.
func committedSource(ctx context.Context, s *store.Store) derive.Source {
	ref := "HEAD"
	if _, err := store.Git(ctx, s.Dir, "rev-parse", "--verify", "-q", "origin/main^{commit}"); err == nil {
		ref = "origin/main"
	}
	return derive.GitRef{Dir: s.Dir, Ref: ref}
}

// lintRun is lint.Run; a test replaces it to make a generated file fail.
var lintRun = lint.Run

// afterGenerate runs between computing the facts and taking the store lock;
// a test uses it to be a second writer.
var afterGenerate = func() {}

// ownFindings keeps the lint findings on the files this run wrote. Those
// mean the generator is wrong; findings elsewhere in the store are not its
// business and must not block it.
func ownFindings(findings []lint.Finding, paths []string) []lint.Finding {
	mine := map[string]bool{}
	for _, p := range paths {
		mine[p] = true
	}
	var out []lint.Finding
	for _, f := range findings {
		if mine[f.Path] && !indexFinding(f) {
			out = append(out, f)
		}
	}
	return out
}

// deriveStep computes the derived facts and, unless dry, puts them in the
// store.
//
// Computing writes nothing: the facts live in memory, and "has this changed"
// is asked of origin/main, not of the working tree. Only then does the step
// take the store lock, sync, put the clone back to exactly what is committed
// under derived/ and MEMORY.md (a killed earlier run may have left them
// dirty), write what still differs, regenerate MEMORY.md, lint, and push.
// Whatever goes wrong, the clone is put back exactly as it was.
//
// One repo's refused blob or lint finding does not stop the others: a
// refused push is retried repo by repo, and the repo that fails is reported
// with the server's own lines.
func deriveStep(ctx context.Context, s *store.Store, o derive.Options, dry bool) (*derive.Report, error) {
	if dry {
		o.Fetch = false
	}
	o.Existing = committedSource(ctx, s)
	rep, err := derive.Run(ctx, o)
	if err != nil || dry || len(rep.Changed()) == 0 {
		return rep, err
	}
	afterGenerate()
	for attempt := 0; ; attempt++ {
		err = s.WithLock(ctx, func() error {
			if err := s.SyncLocked(ctx); err != nil {
				var stranded *store.Stranded
				if !errors.As(err, &stranded) { // the clone follows the server now; go on
					return fmt.Errorf("sync: %w", err)
				}
			}
			cleanGenerated(ctx, s)
			groups := rep.Groups(ctx, derive.FS{Root: s.Dir})
			if len(groups) == 0 {
				return nil
			}
			return pushGroups(ctx, s, rep, groups)
		})
		if errors.Is(err, store.ErrBehind) && attempt < 3 {
			continue // the server moved between the sync and the push; start the locked part over
		}
		return rep, err
	}
}

// cleanGenerated puts back what a killed run left under derived/: tracked
// generated files that differ from HEAD are restored, untracked generated
// files are removed, and then MEMORY.md is put back to HEAD. Generated files
// are only ever written under the store lock and removed again on every
// failure, so such dirt is what a killed run left. A file under derived/ that
// is not a generated fact (hand-made, committed or not) is not touched.
func cleanGenerated(ctx context.Context, s *store.Store) {
	cleaned := false
	// tracked files that differ from HEAD (edited, staged or deleted)
	if out, err := store.Git(ctx, s.Dir, "diff", "--name-only", "-z", "HEAD", "--", "derived"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if rel == "" {
				continue
			}
			if old, err := store.Git(ctx, s.Dir, "cat-file", "blob", "HEAD:"+rel); err == nil && derive.IsGenerated(rel, []byte(old)) {
				if _, err := store.Git(ctx, s.Dir, "checkout", "--quiet", "HEAD", "--", rel); err == nil {
					cleaned = true
				}
			}
		}
	}
	// untracked files
	if out, err := store.Git(ctx, s.Dir, "ls-files", "--others", "--exclude-standard", "-z", "--", "derived"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if rel == "" {
				continue
			}
			full := filepath.Join(s.Dir, filepath.FromSlash(rel))
			if b, err := os.ReadFile(full); err == nil && derive.IsGenerated(rel, b) {
				if os.Remove(full) == nil {
					cleaned = true
					os.Remove(filepath.Dir(full)) // only when empty
				}
			}
		}
	}
	if !cleaned {
		return
	}
	restoreIndexes(ctx, s.Dir)
	if head, err := s.Head(ctx); err == nil {
		search.Invalidate(head)
	}
}

// attempt is one commit's worth of groups.
type attempt struct {
	pushed    []string       // repos in the commit that reached the server
	remaining []derive.Group // groups not dropped by the linter
	rejected  *store.Rejected
}

// pushGroups pushes the groups in one commit. If the server refuses it, the
// clone has been restored; the repos its lines name (the real pre-receive
// hook says "tacit: derived/<repo>/<file> looks like it contains a secret")
// are dropped and reported, and the rest go in one commit again. When the
// refusal names no repo, the set is halved and each half pushed the same
// way. So one bad repo costs one extra push, not one push per repo, and the
// store lock is not held for minutes. It returns an error only for trouble
// that is not about a repo (the git calls themselves, the linter, the
// network).
func pushGroups(ctx context.Context, s *store.Store, rep *derive.Report, groups []derive.Group) error {
	if len(groups) == 0 {
		return nil
	}
	a, err := tryGroups(ctx, s, rep, groups)
	if err != nil {
		return err
	}
	if a.rejected == nil {
		return nil
	}
	rest := a.remaining
	if bad := namedRepos(a.rejected, rest); len(bad) > 0 {
		var keep []derive.Group
		for _, g := range rest {
			if bad[g.Repo] {
				rep.Fail(g.Repo, refusal(a.rejected))
			} else {
				keep = append(keep, g)
			}
		}
		return pushGroups(ctx, s, rep, keep)
	}
	if len(rest) == 1 {
		rep.Fail(rest[0].Repo, refusal(a.rejected))
		return nil
	}
	mid := len(rest) / 2
	if err := pushGroups(ctx, s, rep, rest[:mid]); err != nil {
		return err
	}
	return pushGroups(ctx, s, rep, rest[mid:])
}

var derivedPathRe = regexp.MustCompile(`derived/([^/\s]+)/`)

// namedRepos are the repos among groups that the server's lines point at.
func namedRepos(r *store.Rejected, groups []derive.Group) map[string]bool {
	in := map[string]bool{}
	for _, g := range groups {
		in[g.Repo] = true
	}
	out := map[string]bool{}
	for _, l := range append(append([]string(nil), r.Remote...), r.Reason) {
		for _, m := range derivedPathRe.FindAllStringSubmatch(l, -1) {
			if in[m[1]] {
				out[m[1]] = true
			}
		}
	}
	return out
}

func refusal(r *store.Rejected) string {
	if len(r.Remote) > 0 {
		return "the server refused the push: " + strings.Join(r.Remote, " | ")
	}
	return "the server refused the push: " + r.Reason
}

func repoOf(path string) string {
	rest, ok := strings.CutPrefix(path, "derived/")
	if !ok {
		return ""
	}
	repo, _, _ := strings.Cut(rest, "/")
	return repo
}

// tryGroups writes the groups, regenerates MEMORY.md, lints with the real
// linter, and commits and pushes once. A repo whose files the linter rejects
// is dropped and reported; the others go on. Every error return leaves the
// working tree as it found it.
func tryGroups(ctx context.Context, s *store.Store, rep *derive.Report, groups []derive.Group) (attempt, error) {
	a := attempt{remaining: groups}
	var paths []string // the last write's: the groups' files, MEMORY.md, the folder indexes
	for {
		var err error
		paths, err = writeGroups(s, a.remaining)
		if err != nil {
			restoreTree(s, paths)
			return a, err
		}
		findings, err := lintRun(s.Dir) // a linter that cannot run is a failed gate
		if err != nil {
			restoreTree(s, paths)
			return a, fmt.Errorf("lint could not run, nothing pushed: %w", err)
		}
		own := ownFindings(findings, paths)
		if len(own) == 0 {
			if len(findings) > 0 {
				fmt.Fprintf(os.Stderr, "derive: lint: %d finding(s) elsewhere in the store (first: %s)\n", len(findings), findings[0])
			}
			break
		}
		restoreTree(s, paths)
		bad := map[string]bool{}
		for _, f := range own {
			repo := repoOf(f.Path)
			if repo == "" { // MEMORY.md: the index is ours to generate, so this is a bug here
				return a, fmt.Errorf("lint rejects the regenerated index, nothing pushed: %s", f)
			}
			if !bad[repo] {
				rep.Fail(repo, "lint rejects what was generated: "+f.String())
			}
			bad[repo] = true
		}
		var keep []derive.Group
		for _, g := range a.remaining {
			if !bad[g.Repo] {
				keep = append(keep, g)
			}
		}
		a.remaining = keep
		if len(keep) == 0 {
			return a, nil
		}
	}

	var repos []string
	for _, g := range a.remaining {
		repos = append(repos, g.Repo)
	}
	shown := repos
	if len(shown) > 8 {
		shown = append(append([]string(nil), repos[:8]...), fmt.Sprintf("and %d more", len(repos)-8))
	}
	msg := fmt.Sprintf("curator: derived facts for %d repo(s): %s", len(repos), strings.Join(shown, ", "))

	before, _ := s.Head(ctx)
	err := s.CommitPushOnceLocked(ctx, paths, msg, "curator")
	var rej *store.Rejected
	switch {
	case err == nil:
		a.pushed = repos
		return a, nil
	case errors.As(err, &rej):
		a.rejected = rej // the store put the files back
		search.Invalidate(before)
		return a, nil
	case errors.Is(err, store.ErrBehind):
		return a, err // the store undid the commit; the caller syncs and starts over
	default:
		// The commit may have been made and kept for a later push (network,
		// credentials). Derived facts are regenerable, so drop it and put the
		// tree back instead of leaving a half-pushed clone.
		undoKept(s, before, paths)
		return a, err
	}
}

func groupPaths(groups []derive.Group) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.Paths()...)
	}
	return out
}

// writeGroups applies the groups to the clone and regenerates MEMORY.md and
// the folder indexes. It
// returns every path it may have touched, also on error, so the caller can
// restore them.
func writeGroups(s *store.Store, groups []derive.Group) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	paths := append(groupPaths(groups), memoryPaths(ctx, s.Dir)...)
	for _, g := range groups {
		if err := g.Apply(s.Dir); err != nil {
			return paths, err
		}
	}
	all, _, err := fact.Walk(s.Dir)
	if err != nil {
		return paths, err
	}
	rels, err := regenIndexes(ctx, s.Dir, all, nil)
	return dedupe(append(paths, rels...)), err
}

// restoreTree puts the paths back to HEAD, removes the empty directories the
// writes created, and drops the index a hook may have cached from the dirty
// tree.
func restoreTree(s *store.Store, paths []string) {
	s.Restore(paths)
	for _, p := range paths {
		if strings.HasPrefix(p, "derived/") {
			os.Remove(filepath.Dir(filepath.Join(s.Dir, p))) // only when empty
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if head, err := s.Head(ctx); err == nil {
		search.Invalidate(head)
	}
}

// undoKept drops a local commit that was made but not pushed and restores
// the tree.
func undoKept(s *store.Store, before string, paths []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if before != "" {
		_, _ = store.Git(ctx, s.Dir, "reset", "--quiet", "--mixed", before)
	}
	restoreTree(s, paths)
}
