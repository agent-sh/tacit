package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/results"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runResults implements `tacit results`: the curator's results-from-git step
// on its own. `tacit curate` runs the same step after the derived facts.
//
//	tacit results                 sync, write the result facts and findings into the store clone, commit and push as curator
//	tacit results --dry-run       read the store, change nothing, report (and with -v print the facts and findings)
//	tacit results --out DIR       read the store, write derived/<repo>/ and review/ under DIR, lint it; nothing touches the store
//
// It reads the refs the checkouts already have (tacit derive and tacit curate
// fetch them first) and never moves anything in ~/projects.
func runResults(args []string) int {
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "compute and report, write nothing")
	out := fs.String("out", "", "write derived/ and review/ under this directory instead of the store (no sync, no commit)")
	takeOver := fs.Bool("take-over", false, "also rewrite result facts that another host produced")
	projects := fs.String("projects", defaultProjects(), "directory with the checkouts")
	date := fs.String("date", "", "date of the review file (default today)")
	verbose := fs.Bool("v", false, "print each fact and finding built")
	var only repoList
	fs.Var(&only, "repo", "restrict to this repo (repeatable)")
	fs.Parse(args)

	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "results: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && *out == "" && refuseWriter(context.Background(), s, "curator", "results") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	o, err := resultsOptions(s.Dir, store.ShortHost())
	if err != nil {
		fmt.Fprintln(os.Stderr, "results:", err)
		return 2
	}
	o.Projects, o.Only, o.TakeOver, o.Date = *projects, only, *takeOver, *date

	if *out != "" {
		o.Existing = derive.FS{Root: *out}
		res, err := results.Run(ctx, o)
		if err != nil {
			fmt.Fprintln(os.Stderr, "results:", err)
			return 2
		}
		if !*dry {
			if err := res.Report.Apply(ctx, *out); err != nil {
				fmt.Fprintln(os.Stderr, "results:", err)
				return 2
			}
		}
		printResults(res, *verbose)
		if !*dry {
			if fs, err := lint.Run(*out); err == nil && len(fs) > 0 {
				for _, f := range fs {
					fmt.Fprintln(os.Stderr, "results: lint:", f)
				}
				return 1
			}
		}
		return resultsExit(res)
	}
	if *dry {
		o.Existing = committedSource(ctx, s)
		res, err := results.Run(ctx, o)
		if err != nil {
			fmt.Fprintln(os.Stderr, "results:", err)
			return 2
		}
		printResults(res, *verbose)
		return resultsExit(res)
	}
	if err := s.Sync(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "results: sync:", err)
		return 1
	}
	res, err := resultsStep(ctx, s, o, false)
	printResults(res, *verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, "results:", err)
		return 1
	}
	return resultsExit(res)
}

// resultsOptions are the options every caller shares: the store, this host
// and the per-repo branches the derive config names (the same branch a repo's
// hot spots are read at).
func resultsOptions(storeDir, host string) (results.Options, error) {
	cfg, err := derive.LoadConfig(derive.ConfigPath())
	if err != nil {
		return results.Options{}, err
	}
	return results.Options{Store: storeDir, Host: host, Branches: cfg.Branches, CacheDir: resultsCacheDir()}, nil
}

// resultsCacheDir is where the series read from a repo's history are kept per
// commit (~/.cache/tacit/results). It holds nothing the store needs.
func resultsCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "tacit", "results")
}

func resultsExit(res *results.Result) int {
	if res != nil && res.Report != nil && res.Report.Count(derive.Failed) > 0 {
		return 1
	}
	return 0
}

// curateResults is what `tacit curate` runs after its derived-facts step: the
// result facts and the findings, as curator, in the same locked write path.
// The derive options carry the store, host, projects directory and branches.
func curateResults(ctx context.Context, s *store.Store, d derive.Options, dry bool) error {
	if d.Store == "" {
		return nil
	}
	o := results.Options{Store: d.Store, Host: d.Host, Projects: d.Projects, Branches: d.Branches, TakeOver: d.TakeOver, CacheDir: resultsCacheDir()}
	res, err := resultsStep(ctx, s, o, dry)
	printResults(res, false)
	if err == nil && res != nil && res.Report.Count(derive.Failed) > 0 {
		err = errors.New("results: a repo failed")
	}
	if err != nil {
		return fmt.Errorf("results: %w", err)
	}
	return nil
}

// resultsStep computes the result facts and findings and, unless dry, puts
// them in the store with derive's write path: computed in memory, then under
// the store lock the clone is synced and put back to what is committed, the
// files that still differ are written, MEMORY.md is regenerated, the linter
// runs and one commit is pushed; a refusal drops the repo it names and the
// rest go again.
func resultsStep(ctx context.Context, s *store.Store, o results.Options, dry bool) (*results.Result, error) {
	o.Existing = committedSource(ctx, s)
	res, err := results.Run(ctx, o)
	if err != nil || dry || len(res.Report.Changed()) == 0 {
		return res, err
	}
	afterGenerate()
	for attempt := 0; ; attempt++ {
		err = s.WithLock(ctx, func() error {
			if err := s.SyncLocked(ctx); err != nil {
				var stranded *store.Stranded
				if !errors.As(err, &stranded) {
					return fmt.Errorf("sync: %w", err)
				}
			}
			cleanGenerated(ctx, s)
			groups := res.Report.Groups(ctx, derive.FS{Root: s.Dir})
			if len(groups) == 0 {
				return nil
			}
			return pushGroups(ctx, s, res.Report, groups)
		})
		if errors.Is(err, store.ErrBehind) && attempt < 3 {
			continue
		}
		return res, err
	}
}

func printResults(res *results.Result, verbose bool) {
	if res == nil || res.Report == nil {
		return
	}
	rep := res.Report
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "results: warning:", w)
	}
	for _, r := range rep.Repos {
		quiet := r.Status == derive.Unchanged && r.Detail == "" || r.Status == derive.Skipped && r.Gone
		if quiet && !verbose {
			continue
		}
		if r.Status == derive.Unchanged && len(r.Files) == 0 && !verbose {
			continue
		}
		line := fmt.Sprintf("results: %-9s %s", r.Status, r.Repo)
		if r.Sha != "" {
			line += " " + r.Sha
		}
		if len(r.Paths) > 0 {
			line += fmt.Sprintf(" (%d file(s))", len(r.Paths))
		}
		if r.Detail != "" {
			line += " - " + strings.TrimSpace(r.Detail)
		}
		fmt.Println(line)
		if verbose {
			for _, f := range r.Files {
				fmt.Printf("---- %s\n%s\n", f.Rel, f.Data)
			}
		}
	}
	if verbose {
		for _, h := range res.Quotes {
			state := "quotes the current value"
			if !h.Named {
				state = "quotes a replaced value"
			}
			fmt.Printf("results: %s %s %s for %s (%s) now %s\n", h.Fact.Path, state, h.Literal, strings.TrimSpace(h.S.Label), h.S.File, h.S.Cur().Raw)
		}
		for _, f := range res.Findings {
			fmt.Println(f.Line())
		}
	}
	fmt.Printf("results: %d written, %d unchanged, %d removed, %d skipped, %d failed, %d finding(s)\n",
		rep.Count(derive.Written), rep.Count(derive.Unchanged), len(removed(rep)), rep.Count(derive.Skipped), rep.Count(derive.Failed), len(res.Findings))
}

func removed(rep *derive.Report) []string {
	var out []string
	for _, r := range rep.Repos {
		out = append(out, r.Stale...)
	}
	return out
}
