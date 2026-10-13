package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// disownedPlan lists the curated facts their notes say are not their words (the
// fact each note is about: disownedTargets) and that still claim to be
// (source.kind user, or owner evidence, owner_words included), with the new
// bytes of each: source.kind observed, no evidence. A fact the owner wrote themself is
// left alone, and so is one the owner kept, or said is theirs, after the note.
func disownedPlan(ctx context.Context, dir string) (files map[string][]byte, notes map[string]string, err error) {
	facts, a, err := auditorFor(ctx, dir, nil)
	if err != nil {
		return nil, nil, err
	}
	files, notes = map[string][]byte{}, map[string]string{}
	for _, f := range facts {
		dis, ok := a.disowned[f.Meta.Name]
		src := f.Meta.Source
		if !ok || f.InInbox() || fact.OwnerWrote(f) {
			continue
		}
		if end := a.endorsed[f.Meta.Name]; end.ID != "" && later(end.At, dis.at) {
			continue
		}
		note := dis.note
		if src.Kind != "user" && !fact.ClaimsEvidence(f) {
			continue
		}
		if src.Kind == "user" {
			src.Kind = "observed"
		}
		src.OwnerQuote, src.OwnerQuoteFrom, src.Endorsed, src.OwnerWords, src.OwnerWordsFrom = "", "", "", "", ""
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, nil, err
		}
		c := *f
		c.Meta.Source = src
		nb, err := fact.PatchSource(data, &c)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		files[f.Path] = nb
		notes[f.Meta.Name] = note
	}
	return files, notes, nil
}

// curateDisowned is the curator's step for the owner's notes that say facts
// are not their words ("not owner wording", "agents' interpretation"): the
// facts they link lose source.kind user, through the curator's locked write
// (curatorWrite). No model. It returns the number of errors.
func curateDisowned(ctx context.Context, s *store.Store, dry bool) int {
	if dry {
		files, notes, err := disownedPlan(ctx, s.Dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "curate: disowned:", err)
			return 1
		}
		if len(files) > 0 {
			fmt.Printf("disowned: would set %d fact(s) to observed: %s\n", len(files), strings.Join(sortedKeys(notes), ", "))
		}
		return 0
	}
	var names []string
	pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		files, notes, err := disownedPlan(ctx, s.Dir)
		if err != nil || len(files) == 0 {
			return nil, "", err
		}
		names = sortedKeys(notes)
		var by []string
		seen := map[string]bool{}
		for _, n := range names {
			if !seen[notes[n]] {
				seen[notes[n]] = true
				by = append(by, notes[n])
			}
		}
		sort.Strings(by)
		msg := fmt.Sprintf("curator: %d fact(s) the owner says are not their words set to observed\n\nHis note(s): %s. Facts: %s.",
			len(files), strings.Join(by, ", "), strings.Join(names, ", "))
		return files, msg, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "curate: disowned:", err)
		return 1
	}
	if pushed {
		fmt.Printf("disowned: %d fact(s) set to observed: %s\n", len(names), strings.Join(names, ", "))
	}
	return 0
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
