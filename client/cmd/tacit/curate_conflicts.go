package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// judgeRun is the conflict judge's model call; tests substitute it.
var judgeRun curate.Runner = curate.JudgeCLI

// judgedPair says where a judged pair came from: the fact under check
// (newIdx), and either the existing fact the index found or another fact of
// the same set (otherIdx). sibling marks an existing fact that is not older
// than the one under check (a recheck pairs two facts of the same days): a
// "supersedes" on it is a review line, never a retirement.
type judgedPair struct {
	newIdx   int
	existing *search.Hit
	otherIdx int
	sibling  bool
}

// conflictIndex is what the check needs of the index; tests substitute it.
type conflictIndex interface {
	Query(ctx context.Context, sit search.Situation, limit int, includeInbox bool) ([]search.Hit, error)
	Close() error
}

var openConflictIndex = func(ctx context.Context, storeDir, head string) (conflictIndex, error) {
	return search.Open(ctx, storeDir, head)
}

// conflictPairs pairs every fact in facts with its closest curated fact in
// the index at the clone's HEAD (score 8 or more, never itself, never one
// skip rejects) and, with withinSet, with the other facts of the set in the
// same scope that share most of their terms.
//
// A failed index query opens the index again and asks once more; when that
// fails too the error is returned and the caller writes nothing: the
// 2026-10-11 run logged 509 failed queries and wrote 509 facts without their
// check.
func conflictPairs(ctx context.Context, s *store.Store, facts []*fact.Fact, host string, withinSet bool, skip func(f *fact.Fact, h search.Hit) bool) ([]curate.Pair, []judgedPair, error) {
	var ix conflictIndex
	open := func() error {
		if ix != nil {
			ix.Close()
			ix = nil
		}
		head, err := s.Head(ctx)
		if err != nil {
			return err
		}
		ix, err = openConflictIndex(ctx, s.Dir, head)
		return err
	}
	if err := open(); err != nil {
		return nil, nil, fmt.Errorf("index: %w", err)
	}
	defer func() {
		if ix != nil {
			ix.Close()
		}
	}()
	var pairs []curate.Pair
	var refs []judgedPair
	seen := map[[2]string]bool{}
	for i, f := range facts {
		sit := search.Situation{Host: host, Query: f.Meta.Description}
		if strings.HasPrefix(f.Meta.Scope, "projects/") {
			sit.Repo = strings.TrimPrefix(f.Meta.Scope, "projects/")
		}
		if strings.HasPrefix(f.Meta.Scope, "machines/") {
			sit.Host = strings.TrimPrefix(f.Meta.Scope, "machines/")
		}
		hits, err := ix.Query(ctx, sit, 4, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "curate: index query failed, opening the index again:", err)
			if oerr := open(); oerr != nil {
				return nil, nil, fmt.Errorf("index query: %v; reopening: %w", err, oerr)
			}
			if hits, err = ix.Query(ctx, sit, 4, false); err != nil {
				return nil, nil, fmt.Errorf("index query after reopening: %w", err)
			}
		}
		for _, h := range hits {
			if h.Name == f.Meta.Name || (skip != nil && skip(f, h)) {
				continue
			}
			key := [2]string{h.Name, f.Meta.Name}
			if key[0] > key[1] {
				key[0], key[1] = key[1], key[0]
			}
			if h.Score >= 8 && !seen[key] {
				seen[key] = true
				h := h
				pairs = append(pairs, curate.Pair{ExistingName: h.Name, ExistingDesc: h.Description, NewName: f.Meta.Name, NewDesc: f.Meta.Description})
				refs = append(refs, judgedPair{newIdx: i, existing: &h, otherIdx: -1})
			}
			break // the closest fact that is not this one
		}
		if !withinSet {
			continue
		}
		for j := i + 1; j < len(facts); j++ {
			g := facts[j]
			if g.Meta.Scope == f.Meta.Scope && overlap(f.Meta.Description, g.Meta.Description) >= 0.5 {
				pairs = append(pairs, curate.Pair{ExistingName: f.Meta.Name, ExistingDesc: f.Meta.Description, NewName: g.Meta.Name, NewDesc: g.Meta.Description})
				refs = append(refs, judgedPair{newIdx: j, otherIdx: i})
			}
		}
	}
	return pairs, refs, nil
}

// judgePairs asks the judge about pairs, 25 per call, and applies the
// verdicts: contradictions go to review lines, supersessions retire the
// existing fact when supersedeRoute allows it (written lists the files it
// changed; nothing is changed with dry). Returns the review lines.
func judgePairs(ctx context.Context, s *store.Store, pairs []curate.Pair, refs []judgedPair, facts []*fact.Fact, dry bool, rep *curate.Report) (review, written []string, err error) {
	verdicts, unjudged, err := judgeVerdicts(ctx, pairs, rep)
	if err != nil {
		return nil, nil, err
	}
	// Every call is checked before the first mutation, including later chunks.
	for k, v := range verdicts {
		pm := refs[k]
		nf := facts[pm.newIdx]
		if unjudged[k] {
			rep.JudgeUnchecked++
			review = append(review, uncheckedLine(nf, pm, facts))
			continue
		}
		switch v {
		case curate.Contradicts:
			rep.Contradicts++
			review = append(review, contradictionLine(nf, pm, facts, ""))
		case curate.Supersedes:
			rep.Supersedes++
			if pm.existing == nil {
				// Two new facts on one subject: keep both, flag for the owner.
				review = append(review, fmt.Sprintf("- duplicate in one run: [[%s]] and [[%s]]", nf.Meta.Name, facts[pm.otherIdx].Meta.Name))
				continue
			}
			if pm.sibling {
				review = append(review, fmt.Sprintf("- duplicate in one run: [[%s]] and [[%s]]", nf.Meta.Name, pm.existing.Name))
				continue
			}
			line, retire := supersedeRoute(s.Dir, nf, pm.existing)
			if !retire {
				review = append(review, line)
				continue
			}
			if !dry {
				if rel, ok := markSuperseded(s.Dir, pm.existing.Path, nf.Meta.Name); ok {
					written = append(written, rel)
				}
			}
			review = append(review, fmt.Sprintf("- supersedes: [[%s]] replaces [[%s]]", nf.Meta.Name, pm.existing.Name))
		}
	}
	return review, written, nil
}

// uncheckedLine is the review line of a pair the judge gave no valid verdict
// for: the contradiction-question format, so the review page and the app
// show the pair as the owner's call (parseReviewFiles).
func uncheckedLine(nf *fact.Fact, pm judgedPair, facts []*fact.Fact) string {
	other := ""
	if pm.existing != nil {
		other = pm.existing.Name
	} else {
		other = facts[pm.otherIdx].Meta.Name
	}
	return fmt.Sprintf("- CONTRADICTION?: [[%s]] (%s) vs [[%s]]\n  unchecked: the conflict judge gave no valid verdict for this pair; compare them", nf.Meta.Name, nf.Meta.Scope, other)
}

// contradictionLine is the review line of a "contradicts" verdict. Review
// only: a sampled judge precision of about 30 percent on "contradicts" is too
// low to hide either fact at delivery; conflicts_with is set when the owner
// confirms (tacit resolve). where, when set, says where the other fact is.
func contradictionLine(nf *fact.Fact, pm judgedPair, facts []*fact.Fact, where string) string {
	other, otherDesc := "", ""
	if pm.existing != nil {
		other, otherDesc = pm.existing.Name, pm.existing.Description
	} else {
		other, otherDesc = facts[pm.otherIdx].Meta.Name, facts[pm.otherIdx].Meta.Description
	}
	return fmt.Sprintf("- CONTRADICTION?: [[%s]] (%s) vs [[%s]]%s\n  new: %s\n  other: %s", nf.Meta.Name, nf.Meta.Scope, other, where, nf.Meta.Description, otherDesc)
}

// judgeVerdicts asks the judge about pairs, 25 per call, and returns one
// verdict per pair and whether the pair went unjudged; it changes nothing.
func judgeVerdicts(ctx context.Context, pairs []curate.Pair, rep *curate.Report) ([]curate.Verdict, []bool, error) {
	// judge returns one verdict per pair. An invalid answer (the wrong number
	// of verdicts, an unknown label) splits the chunk in two and asks again,
	// so one pair the judge stumbles on no longer fails every pair of the
	// run. A pair that gets no valid verdict alone is left unjudged and goes
	// to review. A model or transport error still stops the check.
	var judge func(ps []curate.Pair, top bool) ([]curate.Verdict, []bool, error)
	judge = func(ps []curate.Pair, top bool) ([]curate.Verdict, []bool, error) {
		ans, c2, err := judgeRun(ctx, curate.JudgeModel, fmt.Sprintf(curate.ConflictPrompt, curate.RenderPairs(ps)))
		rep.JudgeCalls++
		rep.CostUSD += c2
		if err != nil {
			return nil, nil, fmt.Errorf("judge: %w", err)
		}
		checked, err := curate.ParseVerdictsChecked(ans, len(ps))
		if err == nil {
			return checked, make([]bool, len(ps)), nil
		}
		if !errors.Is(err, modelrun.ErrInvalidResult) {
			return nil, nil, err
		}
		if len(ps) == 1 {
			return []curate.Verdict{curate.NoConflict}, []bool{true}, nil
		}
		rep.JudgeSplits++
		h := len(ps) / 2
		v1, u1, err := judge(ps[:h], false)
		if err != nil {
			return nil, nil, err
		}
		v2, u2, err := judge(ps[h:], false)
		if err != nil {
			return nil, nil, err
		}
		u := append(u1, u2...)
		// No valid verdict for any pair of a whole chunk is a judge that is
		// not working, not a few pairs it stumbles on: stop, as before
		// splitting existed. Only the top chunk: one bad fact's adjacent pairs
		// (its closest fact, its siblings) can fill a small sub-chunk alone.
		if top && !slices.Contains(u, false) {
			return nil, nil, fmt.Errorf("%w: the judge gave no valid verdict for any of %d pairs", modelrun.ErrInvalidResult, len(ps))
		}
		return append(v1, v2...), u, nil
	}
	var verdicts []curate.Verdict
	var unjudged []bool
	for start := 0; start < len(pairs); start += 25 {
		end := min(start+25, len(pairs))
		v, u, err := judge(pairs[start:end], true)
		if err != nil {
			return nil, nil, err
		}
		verdicts = append(verdicts, v...)
		unjudged = append(unjudged, u...)
	}
	return verdicts, unjudged, nil
}

// recheckConflicts implements `tacit curate --recheck-conflicts --since D`:
// the conflict check again for the curated facts the split made on or after
// D (and on or before until, when set), for a run whose check did not
// happen. Each fact is paired with its closest other curated fact that is
// not newer than it (a newer one was checked against it when it was
// curated). Verdicts land as in a curator run: review/<today>.md, and
// superseded_by on a fact the judge and supersedeRoute retire. The store
// must be synced; dry prints and writes nothing. limit > 0 checks only the
// first limit facts (trial runs).
func recheckConflicts(ctx context.Context, s *store.Store, host, since, until string, limit int, dry bool) int {
	if !rePlainDay(since) || (until != "" && !rePlainDay(until)) {
		fmt.Fprintln(os.Stderr, "curate: --since and --until take YYYY-MM-DD")
		return 2
	}
	all, _, err := fact.Walk(s.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate:", err)
		return 2
	}
	created, origin := map[string]string{}, map[string]string{}
	var set []*fact.Fact
	for _, f := range all {
		if f.InInbox() {
			continue
		}
		created[f.Meta.Name], origin[f.Meta.Name] = f.Meta.Created, f.Meta.Origin
		if f.Meta.Origin == "" || f.Meta.Source.Harness != "curator" || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		if f.Meta.Created < since || (until != "" && f.Meta.Created > until) {
			continue
		}
		set = append(set, f)
	}
	if limit > 0 && len(set) > limit {
		set = set[:limit]
	}
	rep := curate.Report{At: time.Now().Format(time.RFC3339), Recheck: true, Facts: len(set)}
	rep.ModelCalls = &modelrun.Trace{}
	ctx = modelrun.WithTrace(ctx, rep.ModelCalls)
	newer := func(f *fact.Fact, h search.Hit) bool { return created[h.Name] > f.Meta.Created }
	pairs, refs, err := conflictPairs(ctx, s, set, host, false, newer)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate: recheck:", err)
		return 1
	}
	for i := range refs {
		// A fact of the same day or the same note is a sibling: the curator
		// itself never retires one with the other.
		if ex := refs[i].existing; ex != nil {
			nf := set[refs[i].newIdx]
			refs[i].sibling = created[ex.Name] >= nf.Meta.Created || (nf.Meta.Origin != "" && origin[ex.Name] == nf.Meta.Origin)
		}
	}
	rep.Checked = len(pairs)
	review, written, err := judgePairs(ctx, s, pairs, refs, set, dry, &rep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate: recheck:", err)
		return 1
	}
	if dry {
		for _, r := range review {
			fmt.Println(r)
		}
	} else if len(review) > 0 || len(written) > 0 {
		if len(review) > 0 {
			rel := "review/" + time.Now().Format("2006-01-02") + ".md"
			full := filepath.Join(s.Dir, rel)
			os.MkdirAll(filepath.Dir(full), 0o755)
			fh, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				fmt.Fprintln(os.Stderr, "curate: recheck:", err)
				return 1
			}
			fmt.Fprintf(fh, "## conflict recheck %s, facts created %s..%s\n\n%s\n\n", rep.At, since, orToday(until), strings.Join(review, "\n"))
			fh.Close()
			written = append(written, rel)
		}
		facts, _, _ := fact.Walk(s.Dir)
		os.WriteFile(filepath.Join(s.Dir, "MEMORY.md"), []byte(index.Generate(facts)), 0o644)
		written = append(written, "MEMORY.md")
		if findings, err := lint.Run(s.Dir); err == nil && len(findings) > 0 {
			fmt.Fprintf(os.Stderr, "curate: lint: %d finding(s) remain (first: %s)\n", len(findings), findings[0])
		}
		msg := fmt.Sprintf("curator: conflict recheck of %d fact(s) from %s: %d contradiction(s), %d supersede(s)", len(set), since, rep.Contradicts, rep.Supersedes)
		if err := s.CommitPush(ctx, dedupe(written), msg, "curator"); err != nil {
			fmt.Fprintln(os.Stderr, "curate: recheck push:", err)
			return 1
		}
		home, _ := os.UserHomeDir()
		if f, err := os.OpenFile(filepath.Join(platform.StateDir(home), "curator.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			json.NewEncoder(f).Encode(rep)
			f.Close()
		}
	}
	if rep.JudgeUnchecked > 0 {
		rep.Errors++ // their facts stand, but the check is not complete
	}
	fmt.Printf("curate: recheck: %d fact(s) created %s..%s, %d pairs in %d judge calls, %d contradictions, %d supersedes, %d unchecked, %s, %d error(s)\n", len(set), since, orToday(until), rep.Checked, rep.JudgeCalls, rep.Contradicts, rep.Supersedes, rep.JudgeUnchecked, rep.ModelCalls.CostSummary(rep.CostUSD), rep.Errors)
	if rep.Errors > 0 {
		return 1
	}
	return 0
}

func rePlainDay(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func orToday(s string) string {
	if s == "" {
		return "today"
	}
	return s
}
