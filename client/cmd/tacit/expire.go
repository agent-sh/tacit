package main

// `tacit expire` (SPEC.md, Staleness check, "Expiry"): facts that name a
// place that is gone or a project that is dead, settled by code with no
// model.
//
//   - A fact whose description names a path on this machine that is not
//     there (goneProbe, the rotation's probe): when a registry of the
//     projects or data directories (an `entries` object keyed by
//     <category>/<name>) shows where every such path went, the fact gets an
//     in-place note per path (`> superseded <day>: <old> -> <new>`); when
//     one is gone with no new place, the fact is retired.
//   - A fact of a project folder the registry marks dead or archived is
//     retired.
//
// The owner's facts (a ruling, a fact they wrote, a fact with their words)
// are never changed: each gets a question for them (`check? ...
// outcome=stale`) in review/<date>-expire.md, once.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// registry is one directory's list of what it holds: <root>/<key>, where
// key is <category>/<name>, with the entry's status.
type registry struct {
	root    string
	entries map[string]string // key -> status
}

// readRegistries reads each registry file (JSON with an "entries" object
// whose keys are paths under the file's own directory). A file that is not
// there or does not read is passed over.
func readRegistries(files []string) []registry {
	var out []registry
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v struct {
			Entries map[string]struct {
				Status string `json:"status"`
			} `json:"entries"`
		}
		if json.Unmarshal(b, &v) != nil || len(v.Entries) == 0 {
			continue
		}
		r := registry{root: filepath.Dir(p), entries: map[string]string{}}
		for k, e := range v.Entries {
			k = strings.Trim(filepath.ToSlash(k), "/")
			if k != "" && !strings.Contains(k, "..") {
				r.entries[k] = e.Status
			}
		}
		out = append(out, r)
	}
	return out
}

// registryFiles are the registries code reads: $TACIT_REGISTRIES (paths
// separated like PATH), else the projects directory's projects.json.
func registryFiles(home string) []string {
	if v := os.Getenv("TACIT_REGISTRIES"); v != "" {
		return filepath.SplitList(v)
	}
	return []string{filepath.Join(projectsRoot(home), "projects.json")}
}

// moved is where a gone path is now by the registries: full is the gone
// path, absolute. The path's first parts under a registry's root name an
// entry by its last part (its name) at another key; the path with that
// prefix replaced must exist. Exactly one such place, or none.
func moved(regs []registry, full string) (oldPrefix, newPrefix string, ok bool) {
	var found [][2]string
	for _, r := range regs {
		rel, err := filepath.Rel(r.root, full)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := 1; i <= len(parts) && i <= 3; i++ {
			prefix := strings.Join(parts[:i], "/")
			if _, here := r.entries[prefix]; here {
				continue
			}
			for k := range r.entries {
				if k == prefix || k[strings.LastIndex(k, "/")+1:] != parts[i-1] {
					continue
				}
				np := filepath.Join(r.root, filepath.FromSlash(k), filepath.FromSlash(strings.Join(parts[i:], "/")))
				if _, err := os.Stat(np); err == nil {
					found = append(found, [2]string{filepath.Join(r.root, filepath.FromSlash(prefix)), filepath.Join(r.root, filepath.FromSlash(k))})
				}
			}
		}
	}
	if len(found) != 1 {
		return "", "", false
	}
	return found[0][0], found[0][1], true
}

// deadFolders are the project folders the registries mark dead or
// archived, by the entry's name: projects/<name>.
func deadFolders(regs []registry) map[string]string {
	out := map[string]string{}
	for _, r := range regs {
		for k, st := range r.entries {
			if st == "dead" || st == "archived" {
				out["projects/"+k[strings.LastIndex(k, "/")+1:]] = st
			}
		}
	}
	return out
}

// expireItem is one fact the step settles.
type expireItem struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Scope  string   `json:"scope"`
	Action string   `json:"action"` // retire, fix, ask
	Why    string   `json:"why"`    // gone, moved, dead
	Paths  []string `json:"paths,omitempty"`
	Notes  []string `json:"notes,omitempty"` // fix: the in-place lines
}

const (
	expireRetire = "retire"
	expireFix    = "fix"
	expireAsk    = "ask"
)

// ownersFact is a fact only the owner changes: their ruling, a fact they
// wrote, a fact with their words.
func ownersFact(f *fact.Fact) bool {
	return fact.IsRuling(f) || fact.OwnerWrote(f) || fact.OwnerWordsOf(f) != ""
}

// expirePlan reads facts: g finds the paths gone (the writer's machine:
// facts of this machine, and the facts of a project, area, topic or the user
// this machine wrote), regs the registries. asked are the facts a stale
// question already names.
func expirePlan(facts []*fact.Fact, g *goneProbe, regs []registry, home, today string, asked map[string]bool) []expireItem {
	dead := deadFolders(regs)
	var out []expireItem
	for _, f := range facts {
		m := f.Meta
		if f.InInbox() || strings.HasPrefix(m.Scope, "derived/") || m.SupersededBy != "" || m.Retired != "" || fact.IsResult(m.Type) || m.Type == "hot_spot" {
			continue
		}
		it := expireItem{Name: m.Name, Path: f.Path, Scope: m.Scope}
		if st, ok := dead[m.Scope]; ok {
			it.Why, it.Action = "dead", expireRetire
			it.Paths = []string{"the registry marks " + m.Scope + " " + st}
		} else if gone := g.paths(f); len(gone) > 0 {
			it.Paths = gone
			it.Why, it.Action = "moved", expireFix
			for _, p := range gone {
				full := p
				if strings.HasPrefix(p, "~/") {
					full = filepath.Join(home, p[2:])
				}
				oldP, newP, ok := moved(regs, full)
				if !ok {
					it.Why, it.Action, it.Notes = "gone", expireRetire, nil
					break
				}
				np := newP + strings.TrimPrefix(full, oldP)
				if strings.HasPrefix(p, "~/") {
					np = "~/" + strings.TrimPrefix(np, home+string(filepath.Separator))
				}
				l, err := fact.NoteLine(today, p, np)
				if err != nil {
					it.Why, it.Action, it.Notes = "gone", expireRetire, nil
					break
				}
				it.Notes = append(it.Notes, l)
			}
		} else {
			continue
		}
		if ownersFact(f) {
			if asked[m.Name] {
				continue
			}
			it.Action = expireAsk
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// expireAsked are the facts a stale check? question names.
func expireAsked(dir string) map[string]bool {
	out := map[string]bool{}
	for _, r := range parseCheckFiles(dir) {
		if r.outcome == "stale" {
			out[r.name] = true
		}
	}
	return out
}

// expireFiles are the step's writes and its commit message.
func expireFiles(dir string, items []expireItem, today string) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	var lines, questions []string
	n := map[string]int{}
	for _, it := range items {
		switch it.Action {
		case expireRetire, expireFix:
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(it.Path)))
			if err != nil {
				return nil, "", err
			}
			f, err := fact.Parse(it.Path, data)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", it.Path, err)
			}
			if it.Action == expireRetire {
				f.Meta.Retired, f.Meta.Updated = today, today
				data, err = fact.PatchRelations(data, f)
			} else {
				for _, l := range it.Notes {
					if data, err = fact.AppendNote(data, l); err != nil {
						break
					}
				}
				if err == nil {
					f.Meta.Updated = today
					data, err = fact.PatchRelations(data, f)
				}
			}
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", it.Path, err)
			}
			files[it.Path] = data
			lines = append(lines, fmt.Sprintf("- %s %s (%s): %s", it.Action, it.Name, it.Why, strings.Join(it.Paths, ", ")))
			n[it.Action+" "+it.Why]++
		case expireAsk:
			why := "names a path that is not on this machine: " + strings.Join(it.Paths, ", ")
			if it.Why == "dead" {
				why = "is in a folder that " + strings.Join(it.Paths, ", ")
			} else if it.Why == "moved" {
				why = "names a path the registry says moved: " + strings.Join(it.Notes, "; ")
			}
			questions = append(questions, checkLine(it.Name, it.Scope, "stale", today),
				"  The curator reads it as outdated: it "+why+".",
				"  Still true keeps it; outdated retires it.")
			n["ask"]++
		}
	}
	if len(questions) > 0 {
		rel := "review/" + today + "-expire.md"
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if len(cur) == 0 {
			cur = []byte("# Expired facts of the owner's\n\n")
		}
		files[rel] = append(withNewline(cur), []byte(strings.Join(questions, "\n")+"\n")...)
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	msg := fmt.Sprintf("curator: expire: %d retired (%d paths gone, %d project dead), %d fixed (paths moved), %d question(s) for the owner\n\n"+
		"Code read the paths and the registries; no model was called (SPEC.md, Staleness check, Expiry).\n\n%s",
		n["retire gone"]+n["retire dead"], n["retire gone"], n["retire dead"], n["fix moved"], n["ask"], strings.Join(lines, "\n"))
	return files, strings.TrimRight(msg, "\n"), nil
}

// runExpire implements `tacit expire [--dry-run] [--json FILE]`.
func runExpire(args []string) int {
	fs := flag.NewFlagSet("expire", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print what it would retire, fix and ask, write nothing, do not sync")
	jsonOut := fs.String("json", "", "write every item to this file as JSON lines")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "expire: no store")
		return 2
	}
	if !*dry && refuseWriter(context.Background(), s, "curator", "expire") {
		return 2
	}
	home, _ := os.UserHomeDir()
	host := store.ShortHost()
	regs := readRegistries(registryFiles(home))
	plan := func() ([]expireItem, error) {
		facts, _, err := fact.Walk(s.Dir)
		if err != nil {
			return nil, err
		}
		g := newGoneProbe(host, home, machineNames(facts))
		g.writer = true
		return expirePlan(facts, g, regs, home, today(), expireAsked(s.Dir)), nil
	}
	var items []expireItem
	if *dry {
		if items, err = plan(); err != nil {
			fmt.Fprintln(os.Stderr, "expire:", err)
			return 1
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
			var err error
			if items, err = plan(); err != nil {
				return nil, "", err
			}
			return expireFiles(s.Dir, items, today())
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "expire:", err)
			return 1
		}
		if !pushed {
			items = nil
		}
	}
	verb := map[bool]string{true: "would ", false: ""}[*dry]
	n := map[string]int{}
	for _, it := range items {
		n[it.Action]++
		fmt.Printf("%s%s %s (%s, %s): %s\n", verb, it.Action, it.Name, it.Scope, it.Why, strings.Join(append(append([]string(nil), it.Paths...), it.Notes...), "; "))
	}
	fmt.Printf("expire: %sretire %d, %sfix %d, %sask %d\n", verb, n[expireRetire], verb, n[expireFix], verb, n[expireAsk])
	if *jsonOut != "" {
		var b []byte
		for _, it := range items {
			l, _ := json.Marshal(it)
			b = append(append(b, l...), '\n')
		}
		if err := os.WriteFile(*jsonOut, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "expire:", err)
			return 1
		}
	}
	return 0
}
