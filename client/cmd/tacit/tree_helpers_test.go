package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// writeIndexesForTest writes MEMORY.md and the folder indexes of facts in
// dir, as a writer of curated facts does (regenIndexes, without git): a test
// that plays another writer keeps the store's indexes current.
func writeIndexesForTest(t testing.TB, dir string, facts []*fact.Fact) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(index.Generate(facts)), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, _ := record.Walk(dir)
	w, r, err := tree.Plan(dir, tree.GenerateWith(facts, recs, ""))
	if err == nil {
		err = tree.Apply(dir, w, r)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// withoutIndexes drops the folder index files from a list of paths.
func withoutIndexes(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !fact.IsIndexFile(filepath.Base(p)) {
			out = append(out, p)
		}
	}
	return out
}
