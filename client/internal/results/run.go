// Package results turns measured results committed to a repo into facts.
//
// Where results live was surveyed on the repos a real store has scopes for
// (eval/VALUE.md, "Results from git"): row logs (a jsonl or csv that appends
// one row per run: a CI perf log), boards (a json or table that is
// rewritten as results change: a published-numbers file, a BENCHMARKS.md) and
// per-experiment receipt folders written once. The step reads the first two
// kinds deterministically, with no model: a number, the setup it was measured
// under, the commit that wrote it, and what it replaced. A newer commit's
// number replaces the older one in the fact, and a saved fact that still quotes
// the older number is flagged for the verifier and the owner.
//
// The facts are generated like derive's: type result, scope derived/<repo>,
// source.kind derived, derived_from the newest commit among the rows shown, a
// pure function of the history behind them. Nothing here retires a fact: a
// flag is a line in review/, and an owner ruling is only ever offered to the
// owner. A repo whose history could not be read whole keeps what the store
// already holds.
package results

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/derive"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
)

// Options for Run.
type Options struct {
	Store    string
	Existing derive.Source // where the current generated files are read from; nil means the store directory
	Projects string
	Host     string
	Only     []string
	TakeOver bool
	Branches map[string]string
	Date     string   // date of the review files; today when empty
	Red      Redactor // digest.RedactAll when nil
	Git      *gitev.Git
	// CacheDir keeps what reading a repo's history yielded, per (repo, commit),
	// so an unchanged repo is not read again. Empty means no cache.
	CacheDir string
	// ReadBudget is the bytes of newest file versions read per repo; 0 is the
	// default (headBudget). A read that hits it is cut: what was read is
	// written and no fact is removed.
	ReadBudget int
}

// Result is what a run found.
type Result struct {
	Report   *derive.Report
	Findings []Finding
	Stats    map[string]*Stats
	Quotes   []Hit    // every saved fact that quotes a row of a series, current or replaced
	Review   []string // review files written (one per repo with findings)
}

var reviewPair = regexp.MustCompile(`^- (?:result-superseded\?|RULING CHANGE\? new) \[\[([^\]]+)\]\] (?:may replace|may supersede owner ruling) \[\[([^\]]+)\]\]`)

// Run reads the repos and works out the result facts and the findings. It
// writes nothing.
func Run(ctx context.Context, o Options) (*Result, error) {
	facts, perrs, err := fact.Walk(o.Store)
	if err != nil {
		return nil, err
	}
	src := o.Existing
	if src == nil {
		src = derive.FS{Root: o.Store}
	}
	red := o.Red
	if red == nil {
		red = digest.RedactAll
	}
	g := o.Git
	if g == nil {
		g = gitev.New(o.Projects)
	}
	g.Timeout = 2 * time.Minute
	date := o.Date
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	rep := &derive.Report{Host: o.Host, TakeOver: o.TakeOver}
	res := &Result{Report: rep, Stats: map[string]*Stats{}}
	noRetire := false
	for _, e := range perrs {
		if m := e.Error(); strings.HasPrefix(m, "projects/") || strings.HasPrefix(m, "derived/") {
			rep.Warnings = append(rep.Warnings, "unparsable fact, no result fact is removed on this run: "+m)
			noRetire = true
		}
	}
	taken := map[string]bool{}
	referenced := map[string]bool{} // names some fact points at (superseded_by, conflicts_with)
	pins := map[string]bool{}       // names a saved fact was retired in favour of (superseded_by)
	var curated []*fact.Fact
	for _, f := range facts {
		if f.Meta.SupersededBy != "" {
			referenced[f.Meta.SupersededBy] = true
			if !strings.HasPrefix(f.Meta.Scope, "derived/") {
				pins[f.Meta.SupersededBy] = true
			}
		}
		for _, c := range f.Meta.ConflictsWith {
			referenced[c] = true
		}
		if !strings.HasPrefix(f.Meta.Scope, "derived/") {
			taken[f.Meta.Name] = true
		}
		if !f.InInbox() && f.Meta.SupersededBy == "" && f.Meta.Retired == "" && f.Meta.Source.Kind != "derived" && !strings.HasPrefix(f.Meta.Scope, "derived/") {
			curated = append(curated, f)
		}
	}
	eligible := derive.Eligible(facts)
	only := map[string]bool{}
	for _, r := range o.Only {
		only[r] = true
	}

	// 1. series of every repo. A repo whose read failed is reported and left
	// exactly as the store has it: a partial list of series would retire
	// facts for files that were merely not read.
	type repoRead struct {
		reading Reading
		series  []*Series
		notes   []string
		partial bool // a bound the step sets itself cut the read: write what was read, retire nothing
	}
	reads := map[string]*repoRead{}
	var repos []string
	for _, repo := range eligible {
		if len(only) > 0 && !only[repo] {
			continue
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		dir := filepath.Join(o.Projects, repo)
		if repo != filepath.Base(repo) || repo == "." || repo == ".." {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
			continue // most project scopes have no checkout
		}
		st := &Stats{}
		res.Stats[repo] = st
		rctx, cancel := context.WithTimeout(ctx, derive.RepoTimeout)
		r, err := derive.Resolve(rctx, dir, o.Branches[repo])
		if err != nil {
			cancel()
			rep.Repos = append(rep.Repos, derive.RepoResult{Repo: repo, Status: derive.Skipped, Detail: err.Error()})
			continue
		}
		if ok, why := g.Readable(rctx, dir); !ok {
			cancel()
			rep.Repos = append(rep.Repos, derive.RepoResult{Repo: repo, Status: derive.Skipped, Detail: why})
			continue
		}
		rr := &repoRead{reading: Reading{Sha: derive.Sha12(r.Full), Date: r.Date, Ref: r.Ref}}
		if c, ok := cacheLoad(o.CacheDir, repo, r.Full); ok {
			rr.series = c.Series
			*st = c.Stats
		} else {
			rd := &Reader{G: g, Dir: dir, Ref: r.Full, HeadBudget: o.ReadBudget}
			series := rd.Series(rctx, repo, st)
			timedOut := rctx.Err() != nil
			cancel()
			switch {
			case timedOut:
				rep.Repos = append(rep.Repos, derive.RepoResult{Repo: repo, Sha: rr.reading.Sha, Status: derive.Failed, Detail: "timed out after " + derive.RepoTimeout.String() + "; the store's result facts for this repo are left as they are"})
				continue
			case len(rd.Failed) > 0:
				rep.Repos = append(rep.Repos, derive.RepoResult{Repo: repo, Sha: rr.reading.Sha, Status: derive.Failed, Detail: "git read failed (" + strings.Join(rd.Failed, "; ") + "); nothing written and nothing removed for this repo"})
				continue
			}
			rr.series, rr.notes, rr.partial = series, rd.Notes, len(rd.Partial) > 0
			if !rr.partial {
				cacheSave(o.CacheDir, repo, r.Full, series, *st)
			}
		}
		cancel()
		reads[repo] = rr
		repos = append(repos, repo)
	}

	// 2. which rows a saved fact quotes
	var all []*Series
	for _, repo := range repos {
		all = append(all, reads[repo].series...)
	}
	ix := NewIndex(all)
	hits := ix.Match(curated)
	res.Quotes = hits
	named := map[*Series]bool{}
	quoting := map[string]map[string]bool{} // repo -> saved facts that quote one of its series
	for _, h := range hits {
		named[h.S] = true
		if st := res.Stats[h.S.Repo]; st != nil {
			if quoting[h.S.Repo] == nil {
				quoting[h.S.Repo] = map[string]bool{}
			}
			quoting[h.S.Repo][h.Fact.Path] = true
		}
	}
	for s := range named {
		if st := res.Stats[s.Repo]; st != nil {
			st.Quoted++
		}
	}
	for repo, fs := range quoting {
		res.Stats[repo].Quoting = len(fs)
	}

	// 3. groups and facts, per repo. names says which fact shows a series; it
	// only holds repos whose facts were all built.
	names := map[*Series]string{}
	written := map[string]int{} // repo -> index in rep.Repos of a repo that writes
	for _, repo := range repos {
		rr := reads[repo]
		st := res.Stats[repo]
		groups := buildGroups(repo, rr.series, named, pins)
		st.Groups = len(groups)
		if len(groups) > MaxFactsPerRepo {
			groups = groups[:MaxFactsPerRepo]
		}
		result := derive.RepoResult{Repo: repo, Sha: rr.reading.Sha, Status: derive.Unchanged}
		existing := existingResults(ctx, src, repo)
		foreign := ""
		for _, ef := range existing {
			if h := ef.Meta.Source.Host; h != o.Host && !o.TakeOver {
				foreign = h
			}
		}
		if foreign != "" {
			result.Status = derive.Skipped
			result.Detail = fmt.Sprintf("the result facts here were produced on host %s (use --take-over to claim them)", foreign)
			rep.Repos = append(rep.Repos, result)
			continue
		}
		var failed error
		madeHere := map[string]bool{}
		namesHere := map[*Series]string{}
		memo := newMemoRed(red)
		if err := memo.prefetch(groups); err != nil {
			failed = fmt.Errorf("redaction failed, no fact written: %w", err)
			groups = nil
		}
		for _, gr := range groups {
			f, err := BuildFact(gr, rr.reading, o.Host, memo.Redact)
			if err != nil {
				failed = err
				break
			}
			if taken[f.Meta.Name] {
				result.Detail += "name " + f.Meta.Name + " is taken by a curated fact; "
				continue
			}
			b, err := f.Encode()
			if err != nil {
				failed = err
				break
			}
			madeHere[f.Meta.Name] = true
			shown := gr.Series
			if len(shown) > MaxRowsPerFact {
				shown = shown[:MaxRowsPerFact]
			}
			for _, s := range shown {
				namesHere[s] = f.Meta.Name
			}
			result.Files = append(result.Files, derive.File{Rel: f.Path, Data: b})
			if old, ok := src.Read(ctx, f.Path); !ok || string(old) != string(b) {
				result.Paths = append(result.Paths, f.Path)
			}
			st.Facts++
		}
		if failed != nil {
			result.Status, result.Detail = derive.Failed, result.Detail+failed.Error()
			result.Files, result.Paths = nil, nil
			rep.Repos = append(rep.Repos, result)
			continue
		}
		for s, n := range namesHere {
			names[s] = n
		}
		if rr.partial {
			result.Detail += "the read was cut by a bound (" + strings.Join(rr.notes, "; ") + "), so no result fact is removed; "
		}
		for rel, ef := range existing {
			if !madeHere[ef.Meta.Name] && !noRetire && !rr.partial {
				if referenced[ef.Meta.Name] {
					// a fact names it (lint L7 would fail without it): it stays as written
					result.Detail += "keeps " + ef.Meta.Name + " (a fact points at it); "
					continue
				}
				result.Stale = append(result.Stale, rel)
				result.Paths = append(result.Paths, rel)
			}
		}
		sort.Strings(result.Stale)
		sort.Strings(result.Paths)
		if len(result.Paths) > 0 {
			result.Status = derive.Written
		}
		result.Detail += st.String()
		rep.Repos = append(rep.Repos, result)
		written[repo] = len(rep.Repos) - 1
	}

	// 4. findings: saved facts that quote a replaced value of a fact this run
	// writes. A repo that did not write has no fact to point at.
	already := flaggedPairs(ctx, src)
	type pk struct{ old, new string }
	grouped := map[pk]*Finding{}
	var order []pk
	for _, h := range hits {
		if h.Named {
			continue
		}
		nf := names[h.S]
		if nf == "" {
			continue
		}
		k := pk{h.Fact.Meta.Name, nf}
		if already[k.old+"\x00"+k.new] {
			continue
		}
		f := grouped[k]
		if f == nil {
			f = &Finding{Old: h.Fact, New: nf, Repo: h.S.Repo, Ruling: fact.IsRuling(h.Fact)}
			grouped[k] = f
			order = append(order, k)
		}
		f.Hits = append(f.Hits, h)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].old != order[j].old {
			return order[i].old < order[j].old
		}
		return order[i].new < order[j].new
	})
	for _, k := range order {
		res.Findings = append(res.Findings, *grouped[k])
		if st := res.Stats[grouped[k].Repo]; st != nil {
			st.Flags++
		}
	}
	for i := range rep.Repos {
		if st := res.Stats[rep.Repos[i].Repo]; st != nil && st.Flags > 0 {
			rep.Repos[i].Detail = strings.Replace(rep.Repos[i].Detail, ", flags 0", fmt.Sprintf(", flags %d", st.Flags), 1)
		}
	}
	if len(res.Findings) > 0 {
		if err := res.addReview(ctx, src, date, red, written); err != nil {
			return res, err
		}
	}
	return res, nil
}

// buildGroups splits a repo's series into the groups that become facts and
// keeps the rows worth a fact: those a saved fact quotes and those that
// changed. Groups that a saved fact quotes (or was retired in favour of) come
// first, then the ones with the most changed rows, then the newest.
func buildGroups(repo string, series []*Series, named map[*Series]bool, pins map[string]bool) []*Group {
	type gk struct{ file, group string }
	byG := map[gk]*Group{}
	var keys []gk
	for _, s := range series {
		k := gk{s.File, s.Group}
		g := byG[k]
		if g == nil {
			g = &Group{Repo: repo, File: s.File, Name: s.Group, Pins: pins}
			byG[k] = g
			keys = append(keys, k)
		}
		g.Total++
		if named[s] || s.Recurs() {
			g.Series = append(g.Series, s)
			if named[s] {
				g.Named = true
			}
		}
	}
	var out []*Group
	for _, k := range keys {
		g := byG[k]
		if len(g.Series) == 0 || !derive.Usable(g.File) {
			continue
		}
		sortRows(g.Series, named)
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ha, hb := a.Named || a.pinned(), b.Named || b.pinned(); ha != hb {
			return ha // a group a saved fact quotes, or was retired in favour of, survives the cap
		}
		if len(a.Series) != len(b.Series) {
			return len(a.Series) > len(b.Series) // a board with many changed rows over a lone number
		}
		if da, db := a.Series[0].Cur().Date, b.Series[0].Cur().Date; da != db {
			return da > db
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Name < b.Name
	})
	return out
}

// existingResults lists the generated result facts under derived/<repo>/.
func existingResults(ctx context.Context, src derive.Source, repo string) map[string]*fact.Fact {
	out := map[string]*fact.Fact{}
	for _, n := range src.List(ctx, "derived/"+repo) {
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		rel := "derived/" + repo + "/" + n
		data, ok := src.Read(ctx, rel)
		if !ok {
			continue
		}
		f, err := fact.Parse(rel, data)
		if err != nil || !fact.IsResult(f.Meta.Type) || f.Meta.Source.Kind != "derived" || f.Meta.DerivedFrom == "" {
			continue
		}
		out[rel] = f
	}
	return out
}

// flaggedPairs are the (saved fact, result fact) pairs some review file
// already carries, so a pair is raised once and an answer (or a dismissal)
// is not asked for again.
func flaggedPairs(ctx context.Context, src derive.Source) map[string]bool {
	out := map[string]bool{}
	for _, n := range src.List(ctx, "review") {
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		data, ok := src.Read(ctx, "review/"+n)
		if !ok {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if m := reviewPair.FindStringSubmatch(l); m != nil {
				out[m[2]+"\x00"+m[1]] = true // old, new
			}
		}
	}
	return out
}

// ReviewPath is the review file of one repo's results findings on one day.
// The step owns it whole, so it can be written like any generated file and the
// curator's own review file is never edited. It is one file per repo and
// travels with that repo's result facts: when the server or the linter
// refuses the repo, its findings are not pushed pointing at facts that do not
// exist.
func ReviewPath(date, repo string) string {
	return "review/" + date + "-results-" + derive.Slug(repo) + ".md"
}

// addReview writes the findings as one review file per repo: the lines the
// day already holds for that repo plus the new ones, redacted in one call.
// written maps a repo to its place in the report; a finding of a repo that
// does not write is dropped.
func (r *Result) addReview(ctx context.Context, src derive.Source, date string, red Redactor, written map[string]int) error {
	byRepo := map[string][]Finding{}
	for _, f := range r.Findings {
		if _, ok := written[f.Repo]; ok {
			byRepo[f.Repo] = append(byRepo[f.Repo], f)
		}
	}
	var repos []string
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var fresh []string
	for _, repo := range repos {
		for _, f := range byRepo[repo] {
			fresh = append(fresh, f.Line())
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	clean, err := red(fresh)
	if err != nil {
		return fmt.Errorf("redaction failed, no finding written: %w", err)
	}
	if len(clean) != len(fresh) {
		return errors.New("redaction changed the number of findings")
	}
	n := 0
	for _, repo := range repos {
		rel := ReviewPath(date, repo)
		var lines []string
		seen := map[string]bool{}
		if old, ok := src.Read(ctx, rel); ok {
			for _, l := range strings.Split(string(old), "\n") {
				if strings.HasPrefix(l, "- ") && !seen[l] {
					seen[l] = true
					lines = append(lines, l)
				}
			}
		}
		for range byRepo[repo] {
			l := strings.Join(strings.Fields(clean[n]), " ")
			n++
			if !seen[l] {
				seen[l] = true
				lines = append(lines, l)
			}
		}
		sort.Strings(lines)
		var b strings.Builder
		b.WriteString("## results from git, " + date + ", " + repo + "\n\n")
		b.WriteString("Saved facts that quote a measured value a newer commit replaced. A is the saved fact, B the result fact from git; the question is whether B is now the current one. Written by `tacit results`; a ruling is only ever put to the owner.\n\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		data := []byte(b.String())
		r.Review = append(r.Review, rel)
		rr := &r.Report.Repos[written[repo]]
		rr.Files = append(rr.Files, derive.File{Rel: rel, Data: data})
		if old, ok := src.Read(ctx, rel); !ok || string(old) != string(data) {
			rr.Paths = append(rr.Paths, rel)
			sort.Strings(rr.Paths)
			if rr.Status == derive.Unchanged {
				rr.Status = derive.Written
			}
		}
	}
	return nil
}
