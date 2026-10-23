package main

// The rotation's questions for the owner (SPEC.md, "Rotation"): when the
// curator's check of one of their facts (a ruling, or a fact with words of
// theirs) goes against it, the rotation changes nothing in it and writes a
// question instead, one line in review/<date>-rotation.md:
//
//	- check? [[name]] (scope) outcome=stale on=2026-10-21
//	  The curator reads it as: ...
//	  Proof: ...
//
// The page lists each as one finding about one fact, beside the stale
// checks, with the same choices: still true, or outdated. The statements
// step writes the same lines (review/<date>-statements.md) for a statement
// a check disagrees with (outcome=misread) or one that differs from a
// decision that stands (outcome=differs): optional items.

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

const kindCheck = "check"

var reCheckLine = regexp.MustCompile(`^- check\? \[\[([^\]]+)\]\] \(([^)]*)\) outcome=([a-z]+) on=(\d{4}-\d{2}-\d{2})`)

// optionalOutcome reports the statements step's items: shown, never owed.
// The assessment's item for a folder whose owner's words their later work
// overrode (outcome=activity) is one too.
func optionalOutcome(o string) bool { return o == "misread" || o == "differs" || o == "activity" }

// checkLine is a question's first line.
func checkLine(name, scope, outcome, on string) string {
	return "- check? [[" + name + "]] (" + scope + ") outcome=" + outcome + " on=" + on
}

// parseCheckFiles reads the rotation's questions of review/*.md in file
// order. The day stands where a stale finding has its commit.
func parseCheckFiles(dir string) []staleRef {
	files, _ := filepath.Glob(filepath.Join(dir, "review", "*.md"))
	sort.Strings(files)
	var out []staleRef
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		src := filepath.Base(f)
		cur := -1
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			l := sc.Text()
			if m := reCheckLine.FindStringSubmatch(l); m != nil {
				out = append(out, staleRef{name: m[1], scope: m[2], sha: m[4], since: m[4], source: src, outcome: m[3]})
				cur = len(out) - 1
				continue
			}
			if cur >= 0 && len(l) > 2 && l[:2] == "  " {
				out[cur].detail = append(out[cur].detail, l[2:])
				continue
			}
			cur = -1
		}
		fh.Close()
	}
	return out
}

// checkItems lists the latest question of every fact the rotation asked
// about, shaped as a stale finding (one fact, the same marks). Only the
// owner answers it; no agent mark lands on it.
func (rs *reviewServer) checkItems() []staleItem {
	if rs.labelling() {
		return nil
	}
	refs := parseCheckFiles(rs.s.Dir)
	last := map[string]int{}
	for i, r := range refs {
		last[r.name] = i
	}
	ds, asks := rs.decisions()
	notes := rs.ownerNotes()
	var out []staleItem
	for i, r := range refs {
		if last[r.name] != i {
			continue
		}
		id := pairID(kindCheck, r.name, r.sha)
		it := staleItem{ID: id, Kind: kindCheck, Source: r.source, A: rs.view(r.name), Single: true,
			Flag: staleFlag{Since: r.since, Detail: r.detail}, Decision: ds[id], Ask: asks[id], Notes: notes.For(id, r.name)}
		switch {
		case it.A.Missing || it.A.SupersededBy != "":
			it.State = "gone"
		case it.A.Retired != "":
			it.State = "retired"
		default:
			it.State = "live"
		}
		if d := it.Decision; d != nil {
			it.Mark = d.Action
			it.Stale = d.Action == "retire" && it.State != "retired"
		}
		it.Allowed = rs.allowedStale(it)
		// A statement's flag (a check disagrees, maybe misread) or its
		// difference from a decision that stands is optional: listed, never
		// waiting on the owner (SPEC.md, "Statements").
		it.NeedsOwner = it.State == "live" && it.Decision == nil && !optionalOutcome(r.outcome)
		out = append(out, it)
	}
	return out
}
