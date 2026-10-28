package main

// The overview step of the full `tacit curate` (SPEC.md, "Overviews"),
// after the assessment and the migration: for the live project, area and
// topic folders, the curator's model writes _overview.md from the folder's
// live facts (and, for a project, the facts tied to it from elsewhere);
// code checks every citation and writes the frontmatter. A folder gets a
// new overview only on a material change (overview.Changed), at most
// overviewMax in a run, the largest change first. An overview whose folder
// is no longer due one (no live facts, fewer than overviewMinFacts, no live
// record) is removed, at most overviewDropMax in a run in one commit, and so
// is one whose rewrite was refused twice while it cites a fact the store no
// longer has (a removed derived fact): an overview is generated, like the
// folder index, and lint L19 holds every citation to a fact the store has.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/overview"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// overviewRun is the overview's model call; tests substitute it.
var overviewRun curate.Runner = curate.RunCLI

const (
	// overviewMax is how many overviews one run writes: each is one model
	// call over up to overviewFacts facts. TACIT_OVERVIEW_MAX.
	overviewMax = 10
	// overviewFacts bounds the facts one call reads (rules first, then
	// standards, decisions and past mistakes, newest first).
	overviewFacts = 400
	// overviewMinFacts: a folder with fewer live facts needs no overview;
	// its index says it all.
	overviewMinFacts = 3
	// overviewShare is the share of a folder's facts that must have been
	// added, replaced or retired since its overview for a new one, absent a
	// forced reason. Not measured on a replay yet. TACIT_OVERVIEW_CHANGE_PCT.
	overviewShare = 0.2
	// overviewDropMax is how many stale overviews one run removes, in one
	// commit and with no model call.
	overviewDropMax = 50
)

// curateOverviewStep is the full curate's overview step; tests substitute it.
var curateOverviewStep = func(ctx context.Context, s *store.Store, model string, dry bool, rep *curate.Report) int {
	if ok, _ := fact.FormatWrites(s.Dir); !ok {
		return 0
	}
	n, dropped, errs := overviewStep(ctx, s, model, envInt("TACIT_OVERVIEW_MAX", overviewMax), dry, rep)
	if n > 0 || dropped > 0 || errs > 0 {
		fmt.Printf("curate: overviews: %d written, %d removed, %d error(s)\n", n, dropped, errs)
	}
	return errs
}

// overviewDrop is an overview to remove and why.
type overviewDrop struct {
	folder string
	why    string
}

// overviewDue is "" for a folder due an overview (a live record and at
// least overviewMinFacts live facts), else why it is not.
func overviewDue(r *record.Record, live int) string {
	switch {
	case live == 0:
		return "no live facts"
	case r == nil:
		return "no record"
	case !r.Live():
		return "its record is redirected"
	case live < overviewMinFacts:
		return fmt.Sprintf("%d live fact(s), fewer than %d", live, overviewMinFacts)
	}
	return ""
}

type overviewCand struct {
	folder string
	old    *overview.Overview
	change overview.Change
	facts  []*fact.Fact // live, in the folder
	tied   []*fact.Fact // live, elsewhere, tied to the project
	all    []*fact.Fact // every fact of the folder, live or not
	status string
	desc   string // the record's description
	// red: the old overview cites a fact the store no longer has (lint L19).
	red bool
}

// weight orders the candidates: an overview lint holds red first, then a
// folder with none, then a forced change, then the largest share.
func (c overviewCand) weight() float64 {
	if c.old == nil {
		return 1 + float64(len(c.facts))/1e6
	}
	if c.red {
		return 3 + c.change.Share
	}
	if len(c.change.Forced) > 0 {
		return 2 + c.change.Share
	}
	return c.change.Share
}

// overviewStep removes the overviews no longer due and writes the overviews
// due, into the store, or into the overlay for this machine's local
// folders. It returns how many it wrote, how many it removed and the errors.
func overviewStep(ctx context.Context, s *store.Store, model string, max int, dry bool, rep *curate.Report) (int, int, int) {
	local := localLayer(ctx, s)
	if local && !overlay.Outside(overlay.Dir(), s.Dir) {
		fmt.Fprintln(os.Stderr, "overview:", overlay.ErrInside)
		return 0, 0, 1
	}
	if !local && !dry {
		if err := writerCheck(ctx, s, "curator"); err != nil {
			fmt.Fprintln(os.Stderr, "overview: nothing done:", err)
			return 0, 0, 1
		}
	}
	cands, drops, byName := overviewCands(s.Dir, local)
	if max <= 0 {
		max = overviewMax
	}
	if len(cands) > max {
		cands = cands[:max]
	}
	written, errs := 0, 0
	dropped, err := dropOverviews(ctx, s, local, dry, drops)
	if err != nil {
		fmt.Fprintf(os.Stderr, "overview: removing stale overviews: %v\n", err)
		errs++
	}
	for _, c := range cands {
		if ctx.Err() != nil {
			errs++
			break
		}
		body, cost, err := writeOverview(ctx, model, c, byName)
		rep.CostUSD += cost
		if err != nil {
			fmt.Fprintf(os.Stderr, "overview: %s: %v\n", c.folder, err)
			errs++
			continue
		}
		if body == "" {
			// An old overview citing a fact the store no longer has is red
			// under lint L19 until it is rewritten: removed instead, and the
			// next run writes the folder a new one.
			if c.old != nil {
				if gone := missingCites(c.old.Body, byName); len(gone) > 0 {
					d := overviewDrop{folder: c.folder, why: "cites " + strings.Join(gone, ", ") + ", which the store no longer has, and the rewrite was refused twice"}
					n, err := dropOverviews(ctx, s, local, dry, []overviewDrop{d})
					if err != nil {
						fmt.Fprintf(os.Stderr, "overview: %s: remove: %v\n", c.folder, err)
						errs++
					}
					dropped += n
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "overview: %s: refused twice, left for the next run\n", c.folder)
			continue
		}
		n, basis, rulings := overview.Basis(c.facts, ruledHere)
		o := &overview.Overview{Meta: overview.Meta{Folder: c.folder, Written: time.Now().Format("2006-01-02"), Facts: n, Basis: basis, Rulings: rulings, Status: c.status, By: record.ByOpus}, Body: body}
		data, err := o.Encode()
		if err != nil {
			errs++
			continue
		}
		fmt.Fprintf(os.Stderr, "overview: %s: %d fact(s), $%.3f\n", c.folder, n, cost)
		if dry {
			fmt.Printf("--- %s\n%s\n", overview.Rel(c.folder), data)
			written++
			continue
		}
		if local {
			if err := overlay.Write(overlay.Dir(), s.Dir, overlay.CuratedDir+"/"+overview.Rel(c.folder), data); err != nil {
				errs++
				continue
			}
			written++
			continue
		}
		why := "new"
		if c.old != nil {
			why = fmt.Sprintf("%.0f%% changed", 100*c.change.Share)
			if len(c.change.Forced) > 0 {
				why = strings.Join(c.change.Forced, "; ")
			}
		}
		_, err = curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
			return map[string][]byte{overview.Rel(c.folder): data}, fmt.Sprintf("curator: overview: %s (%d facts, %s)", c.folder, n, why), nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "overview: %s: write: %v\n", c.folder, err)
			errs++
			continue
		}
		written++
	}
	return written, dropped, errs
}

// missingCites are the [[names]] body cites that name no fact.
func missingCites(body string, byName map[string]*fact.Fact) []string {
	var out []string
	for _, n := range overview.Cites(body) {
		if byName[n] == nil {
			out = append(out, "[["+n+"]]")
		}
	}
	return out
}

// dropOverviews removes the overviews of drops (at most overviewDropMax),
// from the store in one curator commit or from the overlay; the store's are
// checked again on the fresh clone under the lock. It returns how many.
func dropOverviews(ctx context.Context, s *store.Store, local, dry bool, drops []overviewDrop) (int, error) {
	if len(drops) > overviewDropMax {
		drops = drops[:overviewDropMax]
	}
	if len(drops) == 0 {
		return 0, nil
	}
	if dry {
		for _, d := range drops {
			fmt.Printf("--- remove %s (%s)\n", overview.Rel(d.folder), d.why)
		}
		return len(drops), nil
	}
	if local {
		n := 0
		for _, d := range drops {
			if err := overlay.Remove(overlay.Dir(), s.Dir, overlay.CuratedDir+"/"+overview.Rel(d.folder)); err != nil {
				return n, err
			}
			fmt.Fprintf(os.Stderr, "overview: %s: removed (%s)\n", d.folder, d.why)
			n++
		}
		return n, nil
	}
	var done []overviewDrop
	_, err := curatorChange(ctx, s, func() (map[string][]byte, []string, string, error) {
		done = nil
		_, fresh, byName := overviewCands(s.Dir, false)
		still := map[string]bool{}
		for _, d := range fresh {
			still[d.folder] = true
		}
		var rels []string
		for _, d := range drops {
			if still[d.folder] {
				done = append(done, d)
				rels = append(rels, overview.Rel(d.folder))
				continue
			}
			// No longer stale as such; one whose rewrite was refused stays
			// removable while it cites a fact the store does not have.
			if b, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(overview.Rel(d.folder)))); err == nil && strings.HasPrefix(d.why, "cites ") {
				if o, err := overview.Parse(b); err != nil || len(missingCites(o.Body, byName)) > 0 {
					done = append(done, d)
					rels = append(rels, overview.Rel(d.folder))
				}
			}
		}
		if len(done) == 0 {
			return nil, nil, "", nil
		}
		var parts []string
		for i, d := range done {
			if i == 5 {
				parts = append(parts, fmt.Sprintf("and %d more", len(done)-5))
				break
			}
			parts = append(parts, d.folder+" ("+d.why+")")
		}
		msg := fmt.Sprintf("curator: overview: removed %d stale overview(s): %s", len(done), strings.Join(parts, ", "))
		return nil, rels, msg, nil
	})
	if err != nil {
		return 0, err
	}
	for _, d := range done {
		fmt.Fprintf(os.Stderr, "overview: %s: removed (%s)\n", d.folder, d.why)
	}
	return len(done), nil
}

// ruledHere reports the owner's rule, in the store or the local layer.
func ruledHere(f *fact.Fact) bool {
	if overlay.IsCurated(f.Path) {
		return overlay.IsRuling(f)
	}
	return isRuling(f)
}

// overviewCands are the folders due an overview, the largest change first,
// the overviews of folders no longer due one, by folder, and every curated
// fact by name.
func overviewCands(dir string, local bool) ([]overviewCand, []overviewDrop, map[string]*fact.Fact) {
	facts, _, _ := fact.Walk(dir)
	root := dir
	recs, _ := record.Walk(dir)
	var storeRecs map[string]*record.Record
	if local {
		lf, _ := overlay.Load(overlay.Dir())
		facts = append(facts, lf...)
		root = filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir))
		storeRecs = recs
		recs, _ = record.Walk(root)
		for f := range storeRecs {
			delete(recs, f) // the store's folders keep the store's overview
		}
	}
	byName := map[string]*fact.Fact{}
	inFolder := map[string][]*fact.Fact{}
	allIn := map[string][]*fact.Fact{}
	for _, f := range facts {
		if f.InInbox() || overlay.IsInbox(f.Path) {
			continue
		}
		byName[f.Meta.Name] = f
		allIn[f.Meta.Scope] = append(allIn[f.Meta.Scope], f)
		if f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			inFolder[f.Meta.Scope] = append(inFolder[f.Meta.Scope], f)
		}
	}
	share := overviewShare
	if p := envInt("TACIT_OVERVIEW_CHANGE_PCT", -1); p >= 0 && p <= 100 {
		share = float64(p) / 100
	}
	var out []overviewCand
	for folder, r := range recs {
		if overviewDue(r, len(inFolder[folder])) != "" {
			continue
		}
		c := overviewCand{folder: folder, facts: inFolder[folder], all: allIn[folder], status: r.Status, desc: r.Description}
		if r.Kind == record.KindRepo || r.Kind == record.KindFolder {
			names := map[string]bool{}
			for _, n := range r.Names() {
				names[strings.ToLower(n)] = true
			}
			for _, f := range facts {
				if f.Meta.Scope == folder || f.InInbox() || overlay.IsInbox(f.Path) || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
					continue
				}
				for _, rp := range f.Meta.AppliesTo.Repos {
					if names[strings.ToLower(rp)] {
						c.tied = append(c.tied, f)
						break
					}
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(overview.Rel(folder)))); err == nil {
			if o, err := overview.Parse(b); err == nil {
				c.old = o
				c.red = len(missingCites(o.Body, byName)) > 0
				n, basis, rulings := overview.Basis(c.facts, ruledHere)
				live := liveSet(c)
				c.change = overview.Changed(o, c.all, n, basis, rulings, c.status, func(n string) bool { return live[n] })
				if len(c.change.Forced) == 0 && c.change.Share < share {
					continue
				}
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := out[i].weight(), out[j].weight(); a != b {
			return a > b
		}
		return out[i].folder < out[j].folder
	})
	var drops []overviewDrop
	for _, folder := range overviewFolders(root) {
		if storeRecs[folder] != nil {
			continue // the local layer leaves the store's folders alone
		}
		if why := overviewDue(recs[folder], len(inFolder[folder])); why != "" {
			drops = append(drops, overviewDrop{folder: folder, why: why})
		}
	}
	return out, drops, byName
}

// overviewFolders are the folders under root that hold an overview, sorted.
func overviewFolders(root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != overview.File || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err == nil && rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func liveSet(c overviewCand) map[string]bool {
	m := map[string]bool{}
	for _, f := range c.facts {
		m[f.Meta.Name] = true
	}
	for _, f := range c.tied {
		m[f.Meta.Name] = true
	}
	return m
}

// writeOverview asks the model for c's overview and checks it, once more
// with the problems; "" when it was refused twice.
func writeOverview(ctx context.Context, model string, c overviewCand, byName map[string]*fact.Fact) (string, float64, error) {
	pick := append(append([]*fact.Fact(nil), c.facts...), c.tied...)
	sort.SliceStable(pick, func(i, j int) bool {
		a, b := pick[i], pick[j]
		if ra, rb := ruledHere(a), ruledHere(b); ra != rb {
			return ra
		}
		if ta, tb := overviewTier(a), overviewTier(b); ta != tb {
			return ta < tb
		}
		if a.Meta.Created != b.Meta.Created {
			return a.Meta.Created > b.Meta.Created
		}
		return a.Meta.Name < b.Meta.Name
	})
	if len(pick) > overviewFacts {
		pick = pick[:overviewFacts]
	}
	var lines []string
	for _, f := range pick {
		tag := f.Meta.Type
		if ruledHere(f) {
			tag = "RULE"
		}
		where := ""
		if f.Meta.Scope != c.folder {
			where = " (from " + f.Meta.Scope + ")"
		}
		lines = append(lines, fmt.Sprintf("- [[%s]] %s%s: %s", f.Meta.Name, tag, where, strings.Join(strings.Fields(f.Meta.Description), " ")))
	}
	red, err := digest.RedactAllContext(ctx, lines)
	if err != nil {
		return "", 0, fmt.Errorf("redaction: %w", err)
	}
	about := "Its record: " + c.status
	if c.desc != "" {
		about += ", " + c.desc
	}
	prompt := fmt.Sprintf(overview.Prompt, c.folder, about, strings.Join(red, "\n"))
	live := liveSet(c)
	check := func(body string) []string {
		ps := overview.Form(body, func(n string) bool { return byName[n] != nil })
		return append(ps, overview.Live(body, func(n string) bool { return live[n] })...)
	}
	res, cost, err := overviewRun(ctx, model, prompt)
	if err != nil {
		return "", cost, err
	}
	body := overview.Clean(res)
	ps := check(body)
	if len(ps) == 0 {
		return body, cost, nil
	}
	res2, c2, err := overviewRun(ctx, model, overview.RetryPrompt(prompt, res, ps))
	cost += c2
	if err != nil {
		return "", cost, err
	}
	body = overview.Clean(res2)
	if len(check(body)) > 0 {
		return "", cost, nil
	}
	return body, cost, nil
}

func overviewTier(f *fact.Fact) int {
	switch f.Meta.Type {
	case "user_standard":
		return 0
	case "design_decision", "past_mistake":
		return 1
	}
	return 2
}
