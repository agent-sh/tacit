package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
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

// nameLineRx finds the name line of each fact a factText answer holds.
var nameLineRx = regexp.MustCompile(`(?m)^name: *"?([a-z0-9][a-z0-9-]*)"?\s*$`)

// withProvenance is what tacit_fact and `tacit fact` print: first, for each
// fact text shows, whose it is (search.Hit.ProvenanceLabel, read from the
// index's code-checked fields: an owner rule, their words, or an agent's
// note, marked when it claims their authority without evidence), then the
// text. The text alone when the index cannot say in time.
func withProvenance(s *store.Store, text string) string {
	var names []string
	for _, m := range nameLineRx.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	if len(names) == 0 {
		return text
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ix, err := openHookIndex(ctx, s)
	if err != nil {
		return text
	}
	defer ix.Close()
	hs, err := ix.Named(ctx, names)
	if err != nil {
		return text
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, n := range names {
		if h, ok := hs[n]; ok && !seen[n] {
			seen[n] = true
			fmt.Fprintf(&b, "# whose: %s is %s\n", n, h.ProvenanceLabel())
		}
	}
	if b.Len() == 0 {
		return text
	}
	return b.String() + text
}

// rootFolder is the project folder of kind folder (no git repository) whose
// record says it lives on host at a path that holds cwd, the longest such
// root, "" for none: a session there reads that folder's facts as a
// repository's. Only a live folder that is not quiet counts; a path "~/..."
// is under home; paths are compared with symlinks resolved.
func rootFolder(recs map[string]*record.Record, host, home, cwd string) string {
	if cwd == "" || len(recs) == 0 {
		return ""
	}
	c := repoid.RealPath(filepath.Clean(cwd))
	sep := string(filepath.Separator)
	best, bestLen := "", -1
	for f, r := range recs {
		if r.Kind != record.KindFolder || !r.Live() || r.Quiet() || !strings.HasPrefix(f, "projects/") {
			continue
		}
		for _, rt := range r.Roots {
			if !strings.EqualFold(rt.Host, host) {
				continue
			}
			p := rt.Path
			if p == "~" || strings.HasPrefix(p, "~/") {
				if home == "" {
					continue
				}
				p = filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(p[1:], "/")))
			}
			if !filepath.IsAbs(p) {
				continue
			}
			p = repoid.RealPath(filepath.Clean(p))
			if (c == p || strings.HasPrefix(c, p+sep)) && len(p) > bestLen {
				best, bestLen = r.Name, len(p)
			}
		}
	}
	return best
}

// withRootFolder is sit with the project folder cwd is in as its
// repository (rootFolder), when no git repository holds cwd.
func withRootFolder(sit search.Situation, recs map[string]*record.Record, cwd string) search.Situation {
	if sit.Repo != "" {
		return sit
	}
	home, _ := os.UserHomeDir()
	if n := rootFolder(recs, sit.Host, home, cwd); n != "" {
		sit.Repo, sit.RepoAliases = n, nil
	}
	return sit
}
