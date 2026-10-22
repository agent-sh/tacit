package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runDedupe implements `tacit dedupe`: the curated facts that only restate
// another curated fact are retired with superseded_by the fact they restate.
//
//	tacit dedupe --dry-run   read the clone as it is (no sync), print the clusters, change nothing
//	tacit dedupe             sync, supersede the copies, commit and push as curator
//
// No model is called. The contract is SPEC.md "Near-duplicates".
func runDedupe(args []string) int {
	fs := flag.NewFlagSet("dedupe", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print the clusters and the copies it would retire, write nothing (no sync)")
	out := fs.String("json", "", "also write the clusters to this file as JSON lines")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "dedupe: no store")
		return 2
	}
	// Before any work: a machine that cannot write as curator does nothing.
	if !*dry && refuseWriter(context.Background(), s, "curator", "dedupe") {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if *dry {
		cs, err := dedupePlan(s.Dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dedupe:", err)
			return 1
		}
		printDedupe(cs, true)
		return writeDedupeJSON(*out, cs)
	}
	var cs []curate.Copies
	pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		var err error
		if cs, err = dedupePlan(s.Dir); err != nil {
			return nil, "", err
		}
		files, msg, err := dedupeFiles(s.Dir, cs, time.Now())
		if err != nil || len(files) == 0 {
			return nil, "", err
		}
		return files, msg, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dedupe:", err)
		return 1
	}
	if !pushed {
		cs = nil
	}
	printDedupe(cs, false)
	return writeDedupeJSON(*out, cs)
}

// dedupePlan reads the clone: the clusters of near-identical curated facts,
// with the owner's decisions as guards.
func dedupePlan(dir string) ([]curate.Copies, error) {
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return nil, err
	}
	dl, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.DecisionsPath)))
	g := curate.DedupeGuard{Kept: map[string]bool{}}
	for name := range ledger.Endorsements(dl) {
		g.Kept[name] = true
	}
	answered := map[[2]string]bool{}
	ds, _ := ledger.Standing(dl)
	for _, d := range ds {
		if d.By == "" && d.A != "" && d.B != "" {
			answered[[2]string{d.A, d.B}], answered[[2]string{d.B, d.A}] = true, true
		}
	}
	g.Answered = func(a, b string) bool { return answered[[2]string{a, b}] }
	ch := storeChecker(context.Background(), dir, false)
	g.Refuted = refuter(context.Background(), ch)
	cs := curate.Duplicates(facts, g)
	_ = ch.Forge.Save()
	return cs, nil
}

// dedupeFiles is each copy with superseded_by the fact kept and updated
// today; every other byte of the file stays.
func dedupeFiles(dir string, cs []curate.Copies, now time.Time) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	var lines []string
	for _, c := range cs {
		for _, x := range c.Copies {
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(x.Path)))
			if err != nil {
				return nil, "", err
			}
			y := *x
			y.Meta.SupersededBy, y.Meta.Updated = c.Kept.Meta.Name, now.Format("2006-01-02")
			nb, err := fact.PatchRelations(data, &y)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", x.Path, err)
			}
			files[x.Path] = nb
			lines = append(lines, x.Meta.Name+" -> "+c.Kept.Meta.Name)
		}
	}
	// A chain of replacements ends at the live fact in one step: a fact
	// that points at a copy retired now, or at any fact that is itself
	// replaced (a migration's move, an earlier supersede), points at the
	// live end of its chain.
	kept := map[string]string{}
	for _, c := range cs {
		for _, x := range c.Copies {
			kept[x.Meta.Name] = c.Kept.Meta.Name
		}
	}
	repointed := 0
	facts, _, err := fact.Walk(dir)
	if err != nil {
		return nil, "", err
	}
	next := map[string]string{} // name -> its successor, this run's copies included
	retired := map[string]bool{}
	for _, f := range facts {
		if f.InInbox() {
			continue
		}
		if f.Meta.SupersededBy != "" {
			next[f.Meta.Name] = f.Meta.SupersededBy
		}
		if f.Meta.Retired != "" {
			retired[f.Meta.Name] = true
		}
	}
	for c, k := range kept {
		next[c] = k
	}
	end := func(n string) string {
		seen := map[string]bool{n: true}
		for {
			m, ok := next[n]
			if !ok || seen[m] || len(seen) > 32 {
				return n
			}
			seen[m] = true
			n = m
		}
	}
	for _, f := range facts {
		if f.InInbox() || files[f.Path] != nil {
			continue
		}
		cur := f.Meta.SupersededBy
		if cur == "" {
			continue
		}
		to := end(cur)
		if to == cur || to == f.Meta.Name || retired[to] {
			continue // already one step, a loop, or no live end
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, "", err
		}
		y := *f
		y.Meta.SupersededBy, y.Meta.Updated = to, now.Format("2006-01-02")
		nb, err := fact.PatchRelations(data, &y)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", f.Path, err)
		}
		files[f.Path] = nb
		lines = append(lines, f.Meta.Name+" -> "+to+" (was -> "+cur+")")
		repointed++
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	msg := fmt.Sprintf("curator: dedupe: %d near-duplicate fact(s) superseded by the fact they restate, in %d cluster(s)", len(files)-repointed, len(cs))
	if repointed > 0 {
		msg += fmt.Sprintf("; %d replaced fact(s) point at the live end of their chain", repointed)
	}
	msg += "\n\n" + strings.Join(lines, "\n")
	return files, msg, nil
}

func printDedupe(cs []curate.Copies, dry bool) {
	n := 0
	for _, c := range cs {
		n += len(c.Copies)
	}
	verb := "superseded"
	if dry {
		verb = "would supersede"
	}
	for _, c := range cs {
		fmt.Printf("- kept [[%s]] (%s): %s\n", c.Kept.Meta.Name, c.Kept.Meta.Scope, c.Kept.Meta.Description)
		for _, x := range c.Copies {
			fmt.Printf("  - copy [[%s]]: %s\n", x.Meta.Name, x.Meta.Description)
		}
	}
	fmt.Printf("dedupe: %d cluster(s); %s %d copy fact(s)\n", len(cs), verb, n)
}

// writeDedupeJSON writes one line per cluster: the fact kept and its copies.
func writeDedupeJSON(path string, cs []curate.Copies) int {
	if path == "" {
		return 0
	}
	type row struct {
		Name, Description string
	}
	fh, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dedupe:", err)
		return 1
	}
	defer fh.Close()
	enc := json.NewEncoder(fh)
	for _, c := range cs {
		v := struct {
			Scope  string `json:"scope"`
			Kept   row    `json:"kept"`
			Copies []row  `json:"copies"`
		}{Scope: c.Kept.Meta.Scope, Kept: row{c.Kept.Meta.Name, c.Kept.Meta.Description}}
		for _, x := range c.Copies {
			v.Copies = append(v.Copies, row{x.Meta.Name, x.Meta.Description})
		}
		if err := enc.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "dedupe:", err)
			return 1
		}
	}
	return 0
}
