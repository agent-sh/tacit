package main

import (
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
)

// storeRecords are the folder records of the store at dir and, with the
// local layer on, of this machine's overlay (a store record wins), folder
// -> record. A record that does not parse is left out (lint L17 reports it).
func storeRecords(dir string) map[string]*record.Record {
	recs, _ := record.Walk(dir)
	if overlay.LayerOn() && overlay.Outside(overlay.Dir(), dir) {
		local, _ := record.Walk(filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir)))
		for f, r := range local {
			if recs[f] == nil {
				recs[f] = r
			}
		}
	}
	return recs
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
