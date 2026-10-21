package main

// The overview step of the full `tacit curate` (SPEC.md, "Overviews"),
// after the assessment and the migration: for the live project, area and
// topic folders, the curator's model writes _overview.md from the folder's
// live facts (and, for a project, the facts tied to it from elsewhere);
// code checks every citation and writes the frontmatter. A folder gets a
// new overview only on a material change (overview.Changed), at most
// overviewMax in a run, the largest change first.

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
)

// curateOverviewStep is the full curate's overview step; tests substitute it.
var curateOverviewStep = func(ctx context.Context, s *store.Store, model string, dry bool, rep *curate.Report) int {
	if ok, _ := fact.FormatWrites(s.Dir); !ok {
		return 0
	}
	n, errs := overviewStep(ctx, s, model, envInt("TACIT_OVERVIEW_MAX", overviewMax), dry, rep)
	if n > 0 || errs > 0 {
		fmt.Printf("curate: overviews: %d written, %d error(s)\n", n, errs)
	}
	return errs
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
}

func (c overviewCand) weight() float64 {
	if c.old == nil {
		return 1 + float64(len(c.facts))/1e6
	}
	if len(c.change.Forced) > 0 {
		return 2 + c.change.Share
	}
	return c.change.Share
}

// overviewStep writes the overviews due, into the store, or into the
// overlay for this machine's local folders. It returns how many it wrote.
func overviewStep(ctx context.Context, s *store.Store, model string, max int, dry bool, rep *curate.Report) (int, int) {
	local := localLayer(ctx, s)
	if local && !overlay.Outside(overlay.Dir(), s.Dir) {
		fmt.Fprintln(os.Stderr, "overview:", overlay.ErrInside)
		return 0, 1
	}
	if !local && !dry {
		if err := writerCheck(ctx, s, "curator"); err != nil {
			fmt.Fprintln(os.Stderr, "overview: nothing done:", err)
			return 0, 1
		}
	}
	cands, byName := overviewCands(s.Dir, local)
	if max <= 0 {
		max = overviewMax
	}
	if len(cands) > max {
		cands = cands[:max]
	}
	written, errs := 0, 0
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
	return written, errs
}

// ruledHere reports the owner's rule, in the store or the local layer.
func ruledHere(f *fact.Fact) bool {
	if overlay.IsCurated(f.Path) {
		return overlay.IsRuling(f)
	}
	return isRuling(f)
}

// overviewCands are the folders due an overview, the largest change first,
// and every curated fact by name.
func overviewCands(dir string, local bool) ([]overviewCand, map[string]*fact.Fact) {
	facts, _, _ := fact.Walk(dir)
	root := dir
	recs, _ := record.Walk(dir)
	if local {
		lf, _ := overlay.Load(overlay.Dir())
		facts = append(facts, lf...)
		root = filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir))
		storeRecs := recs
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
		if !r.Live() || len(inFolder[folder]) < overviewMinFacts {
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
	return out, byName
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
