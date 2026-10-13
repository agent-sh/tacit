// Package derive builds the derived/<repo>/ facts: hot spots with their
// co-change partners, and a module map, from plain git over the local
// checkouts. No model is involved. A fact is a function of one commit (the
// branch the config names for the repo, else the remote default branch of
// the checkout, else its HEAD, after a fetch of that one branch that moves
// only a remote-tracking ref), so the same commit
// always gives the same bytes and a repo whose commit did not move is not
// touched.
//
// Which repos: those with a curated, live `projects/<repo>` scope in the
// store that also have a checkout at <projects>/<repo>. The facts are
// written to derived/<repo>/ as the curator, lint-clean (SPEC.md L9).
//
// Run only computes. It reads the store for the eligible repos and the
// existing derived files from a Source (a git ref of the clone, or a
// directory), and returns what should exist. Applying that to a working
// tree, committing and pushing is the caller's job, because it needs the
// store lock and a way to put the clone back exactly as it was.
package derive

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// fetchStopped is the report line when the network looks down.
const fetchStopped = "fetching failed several times in a row, so the remaining repos are read at the refs their checkouts already have (network down?)"

// RepoTimeout bounds one repo's git work. A repo that cannot be read in
// this time is reported and skipped; the others still run.
const RepoTimeout = 3 * time.Minute

// Options for Run.
type Options struct {
	Store    string   // store clone, read for the eligible repos
	Existing Source   // where current derived files are read from; nil means FS{Store}
	Projects string   // directory holding the checkouts (~/projects)
	Host     string   // this machine, written to source.host and the only host whose facts it rewrites or retires
	Only     []string // restrict to these repos (retirement is off)
	Force    bool     // rebuild even when derived_from already equals the commit
	TakeOver bool     // also rewrite and retire facts that another host produced

	// Branches maps a repo to the branch to read it at instead of the remote
	// default branch (see Config).
	Branches map[string]string
	// Fetch brings the branch up to date from origin first (FetchBranch). It
	// moves only a remote-tracking ref in the checkout; on failure the local
	// ref is read and the report says so. FetchTimeout 0 means FetchTimeout.
	Fetch        bool
	FetchTimeout time.Duration
}

// Repo status values.
const (
	Written   = "written"
	Unchanged = "unchanged"
	Retired   = "retired"
	Skipped   = "skipped"
	Failed    = "failed"
)

// RepoResult is what Run found for one repo.
type RepoResult struct {
	Repo   string
	Sha    string
	Status string
	Detail string
	Files  []File   // the facts as built (all of them, changed or not)
	Stale  []string // generated files that exist now and that this commit no longer yields, or all of a retired repo's
	Paths  []string // store-relative paths that differ from the Existing source: to write or to remove (sorted)
	Gone   bool     // there is no checkout (missing, or no .git in it); not a transient git error
}

// Report is the outcome of a run.
type Report struct {
	Repos    []RepoResult
	Warnings []string // things the operator should see: unparsable project facts, etc.
	Host     string
	TakeOver bool
}

// Changed lists the store-relative paths that differ from the Existing source.
func (r *Report) Changed() []string {
	var out []string
	for _, x := range r.Repos {
		out = append(out, x.Paths...)
	}
	slices.Sort(out)
	return out
}

// Count returns how many repos have the status.
func (r *Report) Count(status string) int {
	n := 0
	for _, x := range r.Repos {
		if x.Status == status {
			n++
		}
	}
	return n
}

// Group is the change for one repo against a given source: files to write,
// generated files to remove.
type Group struct {
	Repo    string
	Writes  []File
	Removes []string
}

// Paths lists every store-relative path the group touches.
func (g Group) Paths() []string {
	var out []string
	for _, f := range g.Writes {
		out = append(out, f.Rel)
	}
	return append(out, g.Removes...)
}

// Groups compares what Run built with src, which is how a caller that holds
// the store lock asks "what is left to do" of the clone as it is now, not as
// it was when Run read it. A file is removed only if src holds it as a
// generated fact and this host may remove it.
func (r *Report) Groups(ctx context.Context, src Source) []Group {
	var out []Group
	for _, x := range r.Repos {
		g := Group{Repo: x.Repo}
		for _, f := range x.Files {
			if old, ok := src.Read(ctx, f.Rel); !ok || !bytes.Equal(old, f.Data) {
				g.Writes = append(g.Writes, f)
			}
		}
		for _, rel := range x.Stale {
			if data, ok := src.Read(ctx, rel); ok {
				if gf, ok := generated(rel, data); ok && r.mayTouch(gf) {
					g.Removes = append(g.Removes, rel)
				}
			}
		}
		if len(g.Writes)+len(g.Removes) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// Fail marks a repo whose facts could not be put in the store, with the
// reason an operator needs.
func (r *Report) Fail(repo, detail string) {
	for i := range r.Repos {
		if r.Repos[i].Repo == repo {
			r.Repos[i].Status = Failed
			if !strings.Contains(r.Repos[i].Detail, detail) { // a locked part that starts over reports again
				r.Repos[i].Detail = strings.TrimSpace(r.Repos[i].Detail + " " + detail)
			}
			return
		}
	}
	r.Repos = append(r.Repos, RepoResult{Repo: repo, Status: Failed, Detail: detail})
}

func (r *Report) mayTouch(f *fact.Fact) bool {
	return r.TakeOver || f.Meta.Source.Host == r.Host
}

// Apply writes a group under root and removes empty directories it leaves.
func (g Group) Apply(root string) error {
	for _, f := range g.Writes {
		full := filepath.Join(root, filepath.FromSlash(f.Rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Data, 0o644); err != nil {
			return err
		}
	}
	for _, rel := range g.Removes {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		os.Remove(filepath.Dir(full)) // succeeds only when empty
	}
	return nil
}

// Apply writes everything that differs from the directory root into it. It
// is the whole write path of `tacit derive --out`.
func (r *Report) Apply(ctx context.Context, root string) error {
	for _, g := range r.Groups(ctx, FS{Root: root}) {
		if err := g.Apply(root); err != nil {
			return err
		}
	}
	return nil
}

// Eligible returns the repos that have a live curated projects/<repo>
// scope: at least one non-inbox, non-superseded fact in it. Sorted.
func Eligible(facts []*fact.Fact) []string {
	seen := map[string]bool{}
	for _, f := range facts {
		if f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		if repo, ok := strings.CutPrefix(f.Meta.Scope, "projects/"); ok && repo != "" {
			seen[repo] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// Run works out the derived facts for every eligible repo and the generated
// files that should go because their repo left. It writes nothing.
func Run(ctx context.Context, o Options) (*Report, error) {
	facts, perrs, err := fact.Walk(o.Store)
	if err != nil {
		return nil, err
	}
	src := o.Existing
	if src == nil {
		src = FS{Root: o.Store}
	}
	rep := &Report{Host: o.Host, TakeOver: o.TakeOver}
	// A project fact that does not parse may be the only one of its repo. The
	// scope then looks gone, so nothing is retired on such a run.
	noRetire := false
	for _, e := range perrs {
		if m := e.Error(); strings.HasPrefix(m, "projects/") || strings.HasPrefix(m, "derived/") {
			rep.Warnings = append(rep.Warnings, "unparsable fact, nothing is retired on this run: "+m)
			noRetire = true
		}
	}
	taken := map[string]string{} // fact name -> path, for facts that are not derived
	for _, f := range facts {
		if !strings.HasPrefix(f.Meta.Scope, "derived/") {
			taken[f.Meta.Name] = f.Path
		}
	}
	eligible := Eligible(facts)

	// Which derived names are free, decided before any git work and the same
	// way whether or not a repo's commit moved: a name used by a curated fact,
	// or by an earlier repo whose name slugs the same (foo_bar and foo-bar).
	derivedName := map[string]string{}
	blocked := map[string]map[string]string{} // repo -> fact name -> why
	for _, repo := range eligible {
		blocked[repo] = map[string]string{}
		for _, n := range []string{HotName(repo), MapName(repo)} {
			switch {
			case taken[n] != "":
				blocked[repo][n] = fmt.Sprintf("name %s is taken by %s", n, taken[n])
			case derivedName[n] != "":
				blocked[repo][n] = fmt.Sprintf("name %s is already derived for %s", n, derivedName[n])
			default:
				derivedName[n] = repo
			}
		}
	}

	only := map[string]bool{}
	for _, r := range o.Only {
		only[r] = true
		if !slices.Contains(eligible, r) {
			rep.Repos = append(rep.Repos, RepoResult{Repo: r, Status: Skipped, Detail: "no curated projects/" + r + " scope"})
		}
	}
	live := map[string]bool{} // repos whose checkout exists
	fe := &fetcher{timeout: o.FetchTimeout}
	for _, repo := range eligible {
		if len(only) > 0 && !only[repo] {
			continue
		}
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		r := deriveRepo(ctx, o, src, repo, blocked[repo], fe)
		if fe.stopped && !slices.Contains(rep.Warnings, fetchStopped) {
			rep.Warnings = append(rep.Warnings, fetchStopped)
		}
		if !r.Gone {
			live[repo] = true
		}
		rep.Repos = append(rep.Repos, r)
	}
	// Retire derived/<repo> trees whose repo is no longer eligible or whose
	// checkout is gone, but only this host's own: another machine that lacks
	// the checkout must not delete what this one produced, and a machine with
	// a few checkouts must not wipe the rest. Not on a partial run, not when
	// project facts failed to parse, and not when no checkout could be found
	// at all (an unmounted projects directory).
	if len(only) == 0 && !noRetire && (len(live) > 0 || len(eligible) == 0) {
		for _, d := range src.List(ctx, "derived") {
			if !live[d] && !strings.HasSuffix(d, ".md") {
				if r := retire(ctx, o, src, d); r.Status != Unchanged || r.Detail != "" {
					rep.Repos = append(rep.Repos, r)
				}
			}
		}
	}
	return rep, nil
}

func deriveRepo(ctx context.Context, o Options, src Source, repo string, blocked map[string]string, fe *fetcher) RepoResult {
	res := RepoResult{Repo: repo}
	if repo != filepath.Base(repo) || repo == "." || repo == ".." {
		res.Status, res.Detail = Skipped, "unsafe repo name"
		return res
	}
	dir := filepath.Join(o.Projects, repo)
	// A checkout has its own .git (a directory, or a file in a linked
	// worktree). A plain folder inside some enclosing repository does not,
	// and must not be read as that repository.
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		res.Status, res.Detail, res.Gone = Skipped, "no git checkout at "+dir, true
		return res
	}

	type slot struct{ name, rel string }
	var want []slot
	for _, n := range []string{HotName(repo), MapName(repo)} {
		if why := blocked[n]; why != "" {
			res.Detail += why + "; "
			continue
		}
		rel := "derived/" + repo + "/" + n + ".md"
		// a file already at that path that is not a generated fact is
		// someone's own (or corrupt): it is not overwritten
		if data, there := src.Read(ctx, rel); there {
			if _, gen := generated(rel, data); !gen {
				res.Detail += rel + " exists and is not a generated fact; "
				continue
			}
		}
		want = append(want, slot{n, rel})
	}
	if len(want) == 0 {
		res.Status = Skipped
		return res
	}

	// What exists now: generated facts under derived/<repo>/, and whose.
	existing := map[string]*fact.Fact{}
	for _, n := range src.List(ctx, "derived/"+repo) {
		rel := "derived/" + repo + "/" + n
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		if data, ok := src.Read(ctx, rel); ok {
			if f, ok := generated(rel, data); ok {
				if fact.IsResult(f.Meta.Type) {
					continue // results read from git are the results step's (internal/results), not stale hot spots
				}
				if h := f.Meta.Source.Host; h != o.Host && !o.TakeOver {
					res.Status = Skipped
					res.Detail += fmt.Sprintf("the derived facts here were produced on host %s (use --take-over to claim them); ", h)
					return res
				}
				existing[rel] = f
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, RepoTimeout)
	defer cancel()
	// Bring the branch up to date first, so the read is not as old as the last
	// time someone fetched in this checkout. Only a remote-tracking ref moves.
	if o.Fetch && !fe.stopped {
		if b, _ := TargetBranch(ctx, dir, o.Branches[repo]); b != "" && hasOrigin(ctx, dir) {
			if err := FetchBranch(ctx, dir, b, fe.timeoutOrDefault()); err != nil {
				fe.failures++
				fe.stopped = fe.failures >= maxFetchFailures
				res.Detail += fmt.Sprintf("fetching origin/%s failed (%s), read the ref the checkout already had; ", b, shortErr(err))
			} else {
				fe.failures = 0
			}
		}
	}
	r, err := Resolve(ctx, dir, o.Branches[repo])
	if err != nil {
		res.Status, res.Detail = Skipped, res.Detail+err.Error()
		return res
	}
	res.Sha = Sha12(r.Full)
	if r.Missing != "" {
		res.Detail += fmt.Sprintf("the configured branch %s is neither origin/%s nor a local branch, read %s instead; ", r.Missing, r.Missing, r.Ref)
	}

	// The facts are a function of the commit, so when every generated fact
	// present already carries it there is nothing to compute. (A commit that
	// yields only one of the two facts keeps yielding only that one.)
	if !o.Force && len(existing) > 0 {
		current := true
		for rel, f := range existing {
			if f.Meta.DerivedFrom != res.Sha || !slices.ContainsFunc(want, func(s slot) bool { return s.rel == rel }) {
				current = false
			}
		}
		if current {
			res.Status = Unchanged
			return res
		}
	}

	snap, err := Collect(ctx, dir, repo, r)
	if err != nil {
		res.Status, res.Detail = Failed, res.Detail+err.Error()
		return res
	}
	built := Facts(ctx, snap, o.Host)
	if ctx.Err() != nil {
		res.Status, res.Detail = Failed, res.Detail+"timed out after "+RepoTimeout.String()
		return res
	}
	res.Sha = built.Sha
	made := map[string]bool{}
	for _, f := range []*fact.Fact{built.Hot, built.Map} {
		if f == nil || !slices.ContainsFunc(want, func(s slot) bool { return s.rel == f.Path }) {
			continue // no fact this time, or its name or path is not ours to use
		}
		b, err := f.Encode()
		if err != nil {
			res.Status, res.Detail = Failed, res.Detail+err.Error()
			return res
		}
		res.Files = append(res.Files, File{Rel: f.Path, Data: b})
		made[f.Path] = true
		if old, ok := src.Read(ctx, f.Path); !ok || !bytes.Equal(old, b) {
			res.Paths = append(res.Paths, f.Path)
		}
	}
	// A fact this commit no longer yields (history too short for a hot spot,
	// a map that shrank to one line) goes, but only a generated one.
	for rel := range existing {
		if !made[rel] {
			res.Stale = append(res.Stale, rel)
			res.Paths = append(res.Paths, rel)
		}
	}
	slices.Sort(res.Stale)
	slices.Sort(res.Paths)
	res.Status = Unchanged
	if len(res.Paths) > 0 {
		res.Status = Written
	}
	return res
}

// retire lists the generated files of a repo that left. Derived facts hold no
// judgment, only a function of a checkout that is gone, so this is the one
// place the store removes a file; git history keeps it. Only files this host
// produced, unless TakeOver.
func retire(ctx context.Context, o Options, src Source, repo string) RepoResult {
	res := RepoResult{Repo: repo, Status: Retired}
	if repo != filepath.Base(repo) || repo == "." || repo == ".." {
		res.Status = Unchanged
		return res
	}
	for _, n := range src.List(ctx, "derived/"+repo) {
		rel := "derived/" + repo + "/" + n
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		data, ok := src.Read(ctx, rel)
		if !ok {
			continue
		}
		f, ok := generated(rel, data)
		if !ok {
			continue // not ours: left alone
		}
		if h := f.Meta.Source.Host; h != o.Host && !o.TakeOver {
			res.Detail = fmt.Sprintf("its derived facts were produced on host %s, which retires them (use --take-over)", h)
			continue
		}
		res.Stale = append(res.Stale, rel)
		res.Paths = append(res.Paths, rel)
	}
	if len(res.Paths) == 0 {
		res.Status = Unchanged
	}
	return res
}
