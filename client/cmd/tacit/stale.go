package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/stale"
	"github.com/agent-sh/tacit/client/internal/store"
)

// defaultStaleMax is how many new findings one run writes. It equals the
// verifier's default batch of findings per run (tacit verify --max), so a day's
// flags are a day's verifier work; the strongest go first and the rest are
// found again the next day.
const defaultStaleMax = 60

type staleOptions struct {
	Projects string
	AsOf     time.Time
	Max      int // 0 writes every finding
	Dry      bool
	Workers  int
	Only     []string         // fact names to check; empty is every curated fact
	Builder  *gitev.Builder   // tests; nil builds the usual one
	Now      func() time.Time // tests
	// GitTimeout bounds one git command (gitev's default when zero). A command
	// that runs out leaves its fact "incomplete": nothing is recorded and the
	// next run tries again.
	GitTimeout time.Duration
	// Budget is how long a run reads git; zero is until the context ends. The
	// facts it did not reach are checked first in the next run.
	Budget time.Duration
	// StatePath is where this machine remembers when each fact was last
	// checked, so a run that ran out of time continues where it stopped. Empty
	// keeps no state and checks in name order.
	StatePath string
}

func (o staleOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

type staleReport struct {
	Facts     int // curated facts looked at
	Open      int // skipped: an open finding exists
	Skips     map[stale.Skip]int
	Findings  []*stale.Finding // flagged, strongest first, all of them
	Held      int              // flagged beyond Max, left for the next run
	Results   []stale.Result
	Errors    []string
	Took      time.Duration // reading git
	GitRuns   int64         // git commands started
	BackedOff int           // skipped: their read did not finish on recent runs
	checked   []string      // facts whose check finished and whose result is kept
	failed    []string      // facts whose read did not finish
	fatal     error         // the step could not run (an unreadable branch config)
}

// staleState is what one machine remembers between runs: when each fact was
// last checked. Nothing in it can make a fact look checked that was not.
type staleState struct {
	Checked map[string]string `json:"checked"` // fact name -> RFC 3339
	// Incomplete: facts whose read did not finish, how many runs in a row and
	// when, so a fact that never finishes does not sit at the front of every run.
	Incomplete map[string]incompleteEntry `json:"incomplete,omitempty"`
}

type incompleteEntry struct {
	N  int    `json:"n"`
	At string `json:"at"` // RFC 3339
}

// retryAfter is how long a fact whose read has not finished n runs in a row
// waits: the daily timer retries it the next day (20 h, so the next run's
// clock is past it), then every 2nd and 4th day, then weekly. A read that is
// slow because the machine is loaded finishes on a quieter day; one that never does
// costs a seventh of what it would cost every run.
func retryAfter(n int) time.Duration {
	d := 20 * time.Hour
	for i := 1; i < n && d < 7*24*time.Hour; i++ {
		d *= 2
	}
	return min(d, 7*24*time.Hour)
}

func loadStaleState(path string) *staleState {
	st := &staleState{Checked: map[string]string{}, Incomplete: map[string]incompleteEntry{}}
	if path == "" {
		return st
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, st)
		if st.Checked == nil {
			st.Checked = map[string]string{}
		}
		if st.Incomplete == nil {
			st.Incomplete = map[string]incompleteEntry{}
		}
	}
	return st
}

func (st *staleState) save(path string) {
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	b, _ := json.MarshalIndent(st, "", " ")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// byCursor puts the facts never checked first, then the ones checked longest
// ago, so a run with too little time still reaches every fact in turn.
func byCursor(ins []stale.Input, st *staleState) {
	sort.SliceStable(ins, func(i, j int) bool {
		a, b := st.Checked[ins[i].Fact.Meta.Name], st.Checked[ins[j].Fact.Meta.Name]
		if a != b {
			return a < b // RFC 3339 in one zone sorts as time; "" is never
		}
		return ins[i].Fact.Meta.Name < ins[j].Fact.Meta.Name
	})
}

// staleCandidates are the curated facts the check looks at, with what the
// store already says about each.
func staleCandidates(rs *reviewServer, only []string) (ins []stale.Input, open int) {
	rs.loadFacts()
	items := map[string]staleItem{}
	for _, it := range rs.staleItems() {
		items[it.A.Name] = it
	}
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	names := make([]string, 0, len(rs.facts))
	for n := range rs.facts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := rs.facts[n]
		if f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Meta.Scope, "derived/") || (len(want) > 0 && !want[n]) {
			continue
		}
		in := stale.Input{Fact: f, Origin: rs.inbox[f.Meta.Origin], Ruling: isRuling(f)}
		if it, ok := items[n]; ok {
			// A finding nobody has answered keeps the fact out: the page and the
			// verifier read git fresh. Once answered (still true, an agent's fix
			// that is waiting for the owner, a fix applied) a later deletion is a
			// new event; the same newest commit is not flagged again.
			if it.Decision == nil {
				open++
				continue
			}
			in.LastSha = it.Flag.Sha
			switch d := it.Decision; {
			case d.Action == "current":
				if t, err := time.Parse(time.RFC3339, d.At); err == nil {
					in.Verified = t
				}
			case it.Applied:
				if t, err := time.Parse(time.RFC3339, it.AppliedAt); err == nil {
					in.Verified = t
				}
			}
		}
		ins = append(ins, in)
	}
	return ins, open
}

// computeStale checks the store's curated facts against git. It reads the
// clone as it is and writes nothing.
func computeStale(ctx context.Context, s *store.Store, o staleOptions) *staleReport {
	rs := &reviewServer{s: s}
	ins, open := staleCandidates(rs, o.Only)
	rep := &staleReport{Facts: len(ins) + open, Open: open, Skips: map[stale.Skip]int{}}
	b := o.Builder
	if b == nil {
		b = newTimelineBuilder(s, o.AsOf)
		if o.Projects != "" {
			b.G.Root = o.Projects
		}
		// Every git of the scan at nice 19 and idle I/O: a few at a time for many
		// minutes on a machine with a desktop and GPU jobs.
		b.G.Low = true
		// Read each repo at its default branch, or the one derive.yaml names, the
		// way the derived facts are read: never whatever the checkout has out.
		cfg, err := derive.LoadConfig(derive.ConfigPath())
		if err != nil {
			rep.fatal = err
			return rep
		}
		b.G.Resolve = staleResolver(cfg.Branches)
	}
	if o.GitTimeout > 0 {
		b.G.Timeout, b.G.RepoBudget = o.GitTimeout, 4*o.GitTimeout
	}
	workers := o.Workers
	if workers < 1 {
		workers = 4
	}
	st := loadStaleState(o.StatePath)
	if len(o.Only) == 0 {
		// A fact whose read keeps not finishing waits before it is tried again.
		var due []stale.Input
		for _, in := range ins {
			e, failing := st.Incomplete[in.Fact.Meta.Name]
			if t, err := time.Parse(time.RFC3339, e.At); failing && err == nil && o.now().Before(t.Add(retryAfter(e.N))) {
				rep.BackedOff++
				continue
			}
			due = append(due, in)
		}
		ins = due
	}
	byCursor(ins, st)
	rctx := ctx
	if o.Budget > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, o.Budget)
		defer cancel()
	}
	start := time.Now()
	rep.Results = stale.Run(rctx, b, ins, workers)
	rep.Took, rep.GitRuns = time.Since(start), b.G.Runs()
	for _, r := range rep.Results {
		if r.Finding != nil {
			rep.Findings = append(rep.Findings, r.Finding)
			continue
		}
		rep.Skips[r.Skip]++
		if r.Err != nil && len(rep.Errors) < 5 {
			rep.Errors = append(rep.Errors, r.Name+": "+shortErr(r.Err))
		}
		switch r.Skip {
		case stale.SkipUnreached:
		case stale.SkipError, stale.SkipIncomplete:
			rep.failed = append(rep.failed, r.Name)
		default:
			rep.checked = append(rep.checked, r.Name)
		}
	}
	stale.Order(rep.Findings)
	if o.Max > 0 && len(rep.Findings) > o.Max {
		rep.Held = len(rep.Findings) - o.Max
		rep.Findings = rep.Findings[:o.Max]
	}
	for _, f := range rep.Findings {
		rep.checked = append(rep.checked, f.Name) // held ones are not: the next run starts with them
	}
	return rep
}

// remember records the facts this run finished, once their findings are in
// the store (a push that failed leaves them unchecked).
func (r *staleReport) remember(o staleOptions, written bool) {
	if o.StatePath == "" || o.Dry {
		return
	}
	st := loadStaleState(o.StatePath)
	now := o.now().Format(time.RFC3339)
	flagged := map[string]bool{}
	for _, f := range r.Findings {
		flagged[f.Name] = true
	}
	for _, n := range r.checked {
		if flagged[n] && !written {
			continue
		}
		st.Checked[n] = now
		delete(st.Incomplete, n)
	}
	for _, n := range r.failed {
		e := st.Incomplete[n]
		st.Incomplete[n] = incompleteEntry{N: e.N + 1, At: now}
	}
	st.save(o.StatePath)
}

// writeStale appends the findings to today's review file and pushes. It takes
// the store lock, syncs, drops any finding whose fact got an open one in the
// meantime, and puts the clone back exactly if the push fails.
func writeStale(ctx context.Context, s *store.Store, rep *staleReport, o staleOptions) (int, error) {
	if len(rep.Findings) == 0 {
		return 0, nil
	}
	rel := "review/" + o.now().Format("2006-01-02") + ".md"
	written := 0
	err := s.WithLock(ctx, func() error {
		if err := s.SyncLocked(ctx); err != nil {
			var st *store.Stranded
			if !errors.As(err, &st) {
				return fmt.Errorf("sync: %w", err)
			}
		}
		rs := &reviewServer{s: s}
		rs.loadFacts()
		current := map[string]bool{}
		for _, it := range rs.staleItems() {
			if it.Decision == nil {
				current[it.A.Name] = true
			}
		}
		var lines []string
		n := 0
		for _, f := range rep.Findings {
			if current[f.Name] {
				continue // someone flagged it while this run was reading git
			}
			lines = append(lines, f.Block())
			n++
		}
		if n == 0 {
			return nil
		}
		full := filepath.Join(s.Dir, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, werr := fmt.Fprintf(fh, "## stale check %s, %d fact(s) git has overtaken\n\n%s\n", o.now().Format(time.RFC3339), n, strings.Join(lines, "\n"))
		if cerr := fh.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			s.Restore([]string{rel})
			return werr
		}
		if err := s.CommitPushLocked(ctx, []string{rel}, fmt.Sprintf("curator: %d stale? finding(s)", n), "curator"); err != nil {
			s.Restore([]string{rel})
			return err
		}
		written = n
		return nil
	})
	return written, err
}

// staleStep is the curator's staleness step: sync, check every curated fact
// against git, and write what git has overtaken as findings. A dry run only
// reports. It never changes a fact.
func staleStep(ctx context.Context, s *store.Store, o staleOptions) (*staleReport, error) {
	if !o.Dry {
		if err := s.Sync(ctx); err != nil {
			return nil, fmt.Errorf("sync: %w", err)
		}
	}
	rep := computeStale(ctx, s, o)
	if rep.fatal != nil {
		return rep, rep.fatal
	}
	if o.Dry {
		return rep, nil
	}
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	_, err := writeStale(ctx, s, rep, o)
	rep.remember(o, err == nil)
	return rep, err
}

func (r *staleReport) summary(dry bool) string {
	var parts []string
	var keys []string
	for k := range r.Skips {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", r.Skips[stale.Skip(k)], k))
	}
	verb := "flagged"
	if dry {
		verb = "would flag"
	}
	held := ""
	if r.Held > 0 {
		held = fmt.Sprintf(" (%d more held for the next run)", r.Held)
	}
	if r.BackedOff > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting after reads that did not finish", r.BackedOff))
	}
	return fmt.Sprintf("stale: %d curated fact(s) looked at: %s %d%s, %d with an open finding; %s; %d git commands in %.0fs", r.Facts, verb, len(r.Findings), held, r.Open, strings.Join(parts, ", "), r.GitRuns, r.Took.Seconds())
}

func printStale(rep *staleReport, dry, verbose bool) {
	if rep == nil {
		return
	}
	fmt.Println(rep.summary(dry))
	for _, e := range rep.Errors {
		fmt.Fprintln(os.Stderr, "stale: read failed:", e)
	}
	if verbose || dry {
		for _, f := range rep.Findings {
			fmt.Println(f.Block())
		}
	}
}

// staleResolver reads each repo at the commit derive reads it at: the branch
// derive.yaml names for it, else the remote default branch, else (no remote
// at all) HEAD. A repo is resolved once per run.
func staleResolver(branches map[string]string) func(ctx context.Context, repo, dir string) (string, string, error) {
	type res struct {
		sha, ref string
		err      error
	}
	var mu sync.Mutex
	memo := map[string]res{}
	return func(ctx context.Context, repo, dir string) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := memo[repo]; ok {
			return r.sha, r.ref, r.err
		}
		r, err := derive.Resolve(ctx, dir, branches[repo])
		out := res{sha: r.Full, ref: r.Ref, err: err}
		memo[repo] = out
		return out.sha, out.ref, out.err
	}
}

// staleStatePath is where this machine keeps the staleness cursor.
func staleStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(platform.StateDir(home), "stale.json")
}

// curateStale is the call `tacit curate` makes after the derived-facts step.
// It returns the number of errors to add to the run's.
func curateStale(ctx context.Context, s *store.Store, dry bool) int {
	if dry {
		// The scan reads git for up to its whole budget; a dry run of curate is
		// a quick look at the inbox, so it points at the step's own dry run.
		fmt.Println("stale: not run in a dry run; `tacit stale --dry-run` reads git and prints what it would flag")
		return 0
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "curate: stale: redaction needs scrump with the text profile:", err)
		return 1
	}
	sctx, cancel := context.WithTimeout(ctx, 55*time.Minute)
	defer cancel()
	rep, err := staleStep(sctx, s, staleOptions{Projects: defaultProjects(), Max: defaultStaleMax, Dry: dry, Workers: 4, Budget: 45 * time.Minute, StatePath: staleStatePath()})
	printStale(rep, dry, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate: stale:", err)
		return 1
	}
	return 0
}

// runStale implements `tacit stale`: the curator's staleness step on its own.
//
//	tacit stale             sync, flag the curated facts git has overtaken in review/<date>.md, push as curator
//	tacit stale --dry-run   read the store and git, print what would be flagged, change nothing
func runStale(args []string) int {
	fs := flag.NewFlagSet("stale", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "compute and print, write nothing (no sync)")
	asJSON := fs.Bool("json", false, "print the findings and every skip as JSON")
	max := fs.Int("max", defaultStaleMax, "write at most this many findings (0 is no cap); the strongest go first")
	projects := fs.String("projects", defaultProjects(), "directory with the checkouts")
	asOf := fs.String("as-of", "", "read every repo and fact date as it was at this time (RFC 3339, YYYY-MM-DD HH:MM or YYYY-MM-DD)")
	workers := fs.Int("workers", 4, "facts checked at once")
	budget := fs.Duration("budget", 45*time.Minute, "read git for at most this long; the facts not reached are checked first next run")
	gitTimeout := fs.Duration("git-timeout", 0, "bound one git command (default 15s); a loaded machine may need more")
	verbose := fs.Bool("v", false, "print each finding")
	var only repoList
	fs.Var(&only, "fact", "check only this fact (repeatable)")
	fs.Parse(args)
	if *max < 0 || *workers < 1 {
		fmt.Fprintln(os.Stderr, "stale: --max must be 0 or more and --workers at least 1")
		return 2
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "stale: redaction needs scrump with the text profile (install it from https://github.com/avifenesh/scrump):", err)
		return 2
	}
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "stale: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && refuseWriter(context.Background(), s, "curator", "stale") {
		return 2
	}
	o := staleOptions{Projects: *projects, Max: *max, Dry: *dry, Workers: *workers, Only: only, GitTimeout: *gitTimeout, StatePath: staleStatePath()}
	if *asOf != "" {
		if o.AsOf, err = parseAsOf(*asOf); err != nil {
			fmt.Fprintln(os.Stderr, "stale:", err)
			return 2
		}
	}
	o.Budget = *budget
	ctx, cancel := context.WithTimeout(context.Background(), *budget+10*time.Minute)
	defer cancel()
	rep, err := staleStep(ctx, s, o)
	if *asJSON && rep != nil {
		type skip struct {
			Name string `json:"name"`
			Skip string `json:"skip"`
		}
		var skips []skip
		for _, r := range rep.Results {
			if r.Finding == nil {
				skips = append(skips, skip{r.Name, string(r.Skip)})
			}
		}
		// The id is the finding's id on the page and for `tacit verify --ids`.
		type withID struct {
			ID string `json:"id"`
			*stale.Finding
		}
		var fs []withID
		for _, f := range rep.Findings {
			fs = append(fs, withID{pairID(kindStale, f.Name, f.Sha), f})
		}
		b, _ := json.MarshalIndent(map[string]any{"summary": rep.summary(*dry), "findings": fs, "skips": skips}, "", " ")
		fmt.Println(string(b))
	} else {
		printStale(rep, *dry, *verbose)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stale:", err)
		return 1
	}
	return 0
}
