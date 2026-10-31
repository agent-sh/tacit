package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
)

// How a file of a fact's name ranks for a lookup by name (factFiles): the
// name stands for its live curated fact.
const (
	rankLive = iota
	rankSuperseded
	rankRetired
	rankNote
	rankUnread
)

// nameFile is one file of a fact name.
type nameFile struct {
	path string // on disk
	rel  string // as the index names it: store-relative, or overlay.Prefix and overlay-relative
	rank int
}

// state says what the file is, for a reader shown several files of a name.
func (f nameFile) state() string {
	switch f.rank {
	case rankLive:
		return "live"
	case rankSuperseded:
		return "superseded"
	case rankRetired:
		return "retired"
	case rankNote:
		return "an unreviewed note"
	}
	return "does not read as this fact"
}

// factFiles are the files of the fact named name in the store at storeDir
// and in this machine's overlay at localDir ("" for none), best first.
// Names are unique store-wide, but a store can hold one twice: an agent's
// note named like a curated fact (a note's name is checked in its own inbox
// folder only), or a copy a tool outside tacit left in a second folder. The
// walk's first file is then whichever folder sorts first, while the name
// stands for its live curated fact. So a live curated fact comes first,
// then one superseded (its successor chain leads on), then one retired,
// then a note (an inbox note, an overlay note), then a file that does not
// read as a fact of that name; within a rank the store's before the
// overlay's, each in its walk's order. The walks pass over what fact.Walk
// and overlay.Load do.
func factFiles(storeDir, localDir, name string) []nameFile {
	if !fact.IsSlug(name) {
		return nil
	}
	var out []nameFile
	if storeDir != "" {
		_ = filepath.WalkDir(storeDir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != storeDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "keys" || d.Name() == "review") {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != name+".md" || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			rel, err := filepath.Rel(storeDir, p)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == "MEMORY.md" || rel == "README.md" {
				return nil
			}
			out = append(out, nameFile{path: p, rel: rel, rank: rankOf(p, rel, name, strings.HasPrefix(rel, "inbox/"))})
			return nil
		})
	}
	for _, p := range overlay.FindAll(localDir, name) {
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			continue
		}
		rel = overlay.Prefix + filepath.ToSlash(rel)
		out = append(out, nameFile{path: p, rel: rel, rank: rankOf(p, rel, name, !overlay.IsCurated(rel))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	return out
}

// curatedRank ranks a curated fact for a map by name as rankOf does: live,
// then superseded, then retired.
func curatedRank(f *fact.Fact) int {
	switch {
	case f.Meta.SupersededBy != "":
		return rankSuperseded
	case f.Meta.Retired != "":
		return rankRetired
	}
	return rankLive
}

// putByName sets m[f's name] to f unless m holds a file of that name that
// ranks above it, so a name two files hold reads as its live curated file
// (a copy retired into its own name never stands for it). Among files of one
// rank the last one put wins, as a plain assignment did.
func putByName(m map[string]*fact.Fact, f *fact.Fact) {
	if old := m[f.Meta.Name]; old == nil || curatedRank(f) <= curatedRank(old) {
		m[f.Meta.Name] = f
	}
}

// rankOf ranks one file of the name: a note (note), or a curated fact by
// its frontmatter.
func rankOf(p, rel, name string, note bool) int {
	b, err := os.ReadFile(p)
	if err != nil {
		return rankUnread
	}
	f, err := fact.Parse(rel, b)
	switch {
	case err != nil || f.Meta.Name != name:
		return rankUnread
	case note:
		return rankNote
	case f.Meta.SupersededBy != "":
		return rankSuperseded
	case f.Meta.Retired != "":
		return rankRetired
	}
	return rankLive
}

// findFact is the store's file of the fact named name, the best of
// factFiles: what tacit resolve reads.
func findFact(storeDir, name string) (string, bool) {
	if fs := factFiles(storeDir, "", name); len(fs) > 0 {
		return fs[0].path, true
	}
	return "", false
}

// findFactOrLocal is findFact with this machine's local overlay too (the
// notes harnesses wrote here on a read-only clone, the local layer's
// curated facts), for the read paths only.
func findFactOrLocal(storeDir, name string) (string, bool) {
	if fs := factFiles(storeDir, overlay.Dir(), name); len(fs) > 0 {
		return fs[0].path, true
	}
	return "", false
}

// alsoNamed is the line that names the other files of a name a reader is
// shown one of, best first, or "" when there are none.
func alsoNamed(name string, others []nameFile) string {
	if len(others) == 0 {
		return ""
	}
	var parts []string
	for _, f := range others {
		parts = append(parts, fmt.Sprintf("%s (%s)", f.rel, f.state()))
	}
	return "# also named " + name + ": " + strings.Join(parts, ", ") + "\n"
}
