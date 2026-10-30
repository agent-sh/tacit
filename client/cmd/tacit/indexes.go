package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// A write whose mask changed a fact or a record writes the indexes again from
// the masked text, in the same commit, from the facts as that commit holds
// them (store.Reindex; SPEC.md, "Every write is masked").
func init() {
	store.Reindex = func(ctx context.Context, dir string, rels []string) ([]string, error) {
		facts, err := commitFacts(ctx, dir, rels)
		if err != nil {
			return nil, err
		}
		return regenIndexes(ctx, dir, facts, nil)
	}
}

// commitFacts is the facts as a commit of rels will hold them: a file the
// commit names as it is in the clone, any other tracked fact as HEAD holds it
// (a change the commit does not carry is not its: an edit, a file that no
// longer parses, a deletion), and no fact the clone does not track that the
// commit does not name (a killed batch's leftover).
func commitFacts(ctx context.Context, dir string, rels []string) ([]*fact.Fact, error) {
	all, _, err := fact.Walk(dir)
	if err != nil {
		return nil, err
	}
	st, err := fact.LoadState(dir)
	if err != nil {
		return nil, err
	}
	atHead := func(rel string) *fact.Fact {
		blob, err := store.Git(ctx, dir, "cat-file", "blob", "HEAD:"+rel)
		if err != nil {
			return nil
		}
		h, err := fact.Parse(rel, []byte(blob))
		if err != nil {
			return nil
		}
		st.Apply(h)
		return h
	}
	named := map[string]bool{}
	for _, r := range rels {
		named[r] = true
	}
	set := func(args ...string) (map[string]bool, error) {
		out, err := store.Git(ctx, dir, args...)
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				m[p] = true
			}
		}
		return m, nil
	}
	changed, err := set("diff", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	untracked, err := set("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var out []*fact.Fact
	walked := map[string]bool{}
	for _, f := range all {
		walked[f.Path] = true
		switch {
		case named[f.Path]:
			out = append(out, f)
		case untracked[f.Path]:
		case changed[f.Path]:
			if h := atHead(f.Path); h != nil {
				out = append(out, h)
			}
		default:
			out = append(out, f)
		}
	}
	// A tracked fact the walk did not read (deleted in the clone, or no
	// longer parsing there) and the commit does not name is in the commit's
	// tree as HEAD holds it.
	var gone []string
	for rel := range changed {
		if !walked[rel] && !named[rel] && walksAsFact(rel) {
			gone = append(gone, rel)
		}
	}
	sort.Strings(gone)
	for _, rel := range gone {
		if h := atHead(rel); h != nil {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// walksAsFact reports a store path fact.Walk reads as a fact: a .md file
// that is no store file, not MEMORY.md or a root README.md, and in no dot
// directory, keys/ or review/.
func walksAsFact(rel string) bool {
	if !strings.HasSuffix(rel, ".md") || rel == "MEMORY.md" || rel == "README.md" || fact.IsStoreFile(path.Base(rel)) {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, d := range parts[:len(parts)-1] {
		if strings.HasPrefix(d, ".") || d == "keys" || d == "review" {
			return false
		}
	}
	return true
}

// regenIndexes writes the store's generated indexes for facts into the clone:
// MEMORY.md and every folder's _index.md (internal/tree), removing index
// files of folders that hold no facts any more. It returns the paths a commit
// must name: MEMORY.md, every index file of the tree (unchanged ones commit
// nothing, and one written earlier by `tacit index` is carried by this
// commit), and each removed index file git tracks. snap, when not nil,
// records the bytes of each path before it is changed, so a failure puts it
// back. Every writer of curated facts calls it inside its locked write, so
// the indexes never drift from the facts.
//
// With the memory_md setting off (index.On) MEMORY.md is not generated: a
// copy in the working tree is removed, and named for the commit only while
// git tracks it, so the commit records its removal once.
func regenIndexes(ctx context.Context, dir string, facts []*fact.Fact, snap snapshot) ([]string, error) {
	recs, _ := record.Walk(dir)
	gen := tree.GenerateWith(facts, recs, "")
	write, remove, err := tree.Plan(dir, gen)
	if err != nil {
		return nil, err
	}
	rels := memoryPaths(ctx, dir)
	n := len(rels)
	for rel := range gen {
		rels = append(rels, rel)
	}
	sort.Strings(rels[n:])
	if snap != nil {
		for _, rel := range append(append([]string{}, rels...), remove...) {
			if _, ok := snap[rel]; !ok {
				if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
					snap[rel] = b
				} else {
					snap[rel] = nil
				}
			}
		}
	}
	if index.On() {
		if err := writeFileAtomic(dir, index.File, []byte(index.Generate(facts))); err != nil {
			return rels, err
		}
	} else if err := os.Remove(filepath.Join(dir, index.File)); err != nil && !os.IsNotExist(err) {
		return rels, err
	}
	if err := tree.Apply(dir, write, remove); err != nil {
		return rels, err
	}
	for _, rel := range remove {
		if out, err := store.Git(ctx, dir, "ls-files", "--", rel); err == nil && strings.TrimSpace(out) != "" {
			rels = append(rels, rel)
		}
	}
	return rels, nil
}

// indexFinding is lint L16, a folder index that differs from the facts in
// the working tree. A write's gate passes it over, as it passes L10 for
// MEMORY.md: the write regenerates every index from the facts its commit
// holds (regenIndexes), which a working tree with someone else's uncommitted
// facts does not show the linter.
func indexFinding(f lint.Finding) bool { return f.Rule == "L16" }

// restoreIndexes puts the tracked index files back to HEAD (a killed run may
// have left them dirty); untracked ones are the next regeneration's to
// remove.
func restoreIndexes(ctx context.Context, dir string) {
	args := append([]string{"checkout", "--quiet", "HEAD", "--"}, memoryPaths(ctx, dir)...)
	_, _ = store.Git(ctx, dir, append(args, ":(glob)"+tree.FileName, ":(glob)**/"+tree.FileName)...)
}

// memoryPaths is MEMORY.md for a commit's paths: always while the writers
// generate it (index.On), and with the setting off only while git still
// tracks it (the commit that removes it), never after.
func memoryPaths(ctx context.Context, dir string) []string {
	if index.On() {
		return []string{index.File}
	}
	if out, err := store.Git(ctx, dir, "ls-files", "--", index.File); err == nil && strings.TrimSpace(out) != "" {
		return []string{index.File}
	}
	return nil
}

// regenLocalIndexes writes the folder indexes of the local layer's curated
// facts (overlay curated/<scope>/) from the overlay's facts, through the
// overlay's checked writes (never inside the clone, never out of the
// overlay). The overlay is no git repository: there is nothing to commit.
func regenLocalIndexes(dir, storeDir string, local []*fact.Fact) error {
	var curated []*fact.Fact
	for _, f := range local {
		if overlay.IsCurated(f.Path) {
			curated = append(curated, f)
		}
	}
	root := filepath.Join(dir, filepath.FromSlash(overlay.CuratedDir))
	recs, _ := record.Walk(root)
	gen := tree.GenerateWith(curated, recs, overlay.Prefix+overlay.CuratedDir+"/")
	write, remove, err := tree.Plan(root, gen)
	if err != nil {
		return err
	}
	rels := make([]string, 0, len(write))
	for rel := range write {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if err := overlay.Write(dir, storeDir, overlay.CuratedDir+"/"+rel, write[rel]); err != nil {
			return err
		}
	}
	for _, rel := range remove {
		if err := overlay.Remove(dir, storeDir, overlay.CuratedDir+"/"+rel); err != nil {
			return err
		}
	}
	return nil
}
