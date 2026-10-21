package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
)

// storeRecords are the folder records of the store at dir and, with the
// local layer on, of this machine's overlay, folder -> record: the layer's
// own folders, and the layer's status over a store folder's for this
// machine only (record.MergeLocal). A record that does not parse is left
// out (lint L17 reports it).
func storeRecords(dir string) map[string]*record.Record {
	recs, _ := record.Walk(dir)
	if overlay.LayerOn() && overlay.Outside(overlay.Dir(), dir) {
		return record.MergeLocal(recs, localRecords())
	}
	return recs
}

// localRecords are the records of this machine's overlay.
func localRecords() map[string]*record.Record {
	local, _ := record.Walk(filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir)))
	return local
}

// suppressionNames adds to names (a session's repository and its other
// names) the project folders suppression joins it with: the folder that
// replaces one of them, every folder suppressed into that one, and that
// folder's recorded aliases. A session in either repository reads both
// until the curator's migration has emptied the suppressed one.
func suppressionNames(recs map[string]*record.Record, names []string) []string {
	if len(recs) == 0 {
		return names
	}
	out := append([]string(nil), names...)
	have := map[string]bool{}
	for _, n := range names {
		have[strings.ToLower(n)] = true
	}
	addName := func(n string) {
		if n = strings.ToLower(n); n != "" && !have[n] {
			have[n] = true
			out = append(out, n)
		}
	}
	for _, n := range names {
		folder := record.RepoOwner(recs, n)
		if folder == "" {
			folder = "projects/" + strings.ToLower(n)
		}
		if recs[folder] == nil {
			continue
		}
		winner, _, err := record.Resolve(recs, folder)
		if err != nil {
			continue
		}
		addName(strings.TrimPrefix(winner, "projects/"))
		if w := recs[winner]; w != nil {
			for _, a := range w.Aliases {
				addName(a)
			}
		}
		for _, l := range record.Losers(recs, winner) {
			addName(strings.TrimPrefix(l, "projects/"))
		}
	}
	return out
}

// factText is what tacit_fact and `tacit fact` print for name: the file as
// it is, or, for a fact something replaced (superseded_by, followed to the
// live end of the chain), the newer fact first with a line naming the old
// one, then the old one. ok is false when no fact has the name.
func factText(storeDir, name string) (string, bool, error) {
	p, ok := findFactOrLocal(storeDir, name)
	if !ok {
		return "", false, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", true, err
	}
	f, perr := fact.Parse(name+".md", b)
	if perr != nil || f.Meta.SupersededBy == "" {
		return string(b), true, nil
	}
	seen := map[string]bool{name: true}
	cur, curBytes := f, b
	for cur.Meta.SupersededBy != "" && !seen[cur.Meta.SupersededBy] && len(seen) < 16 {
		next := cur.Meta.SupersededBy
		seen[next] = true
		np, ok := findFactOrLocal(storeDir, next)
		if !ok {
			break
		}
		nb, err := os.ReadFile(np)
		if err != nil {
			break
		}
		nf, err := fact.Parse(next+".md", nb)
		if err != nil {
			break
		}
		cur, curBytes = nf, nb
	}
	if cur == f {
		return string(b), true, nil
	}
	return fmt.Sprintf("%s was superseded by %s; the newer fact first, then the old one.\n\n%s\n--- the old fact, %s ---\n%s", name, cur.Meta.Name, curBytes, name, b), true, nil
}
