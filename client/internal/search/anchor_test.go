package search

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The rarest term of a request anchors coherence, and so does every term in
// at most anchorRareDF summaries: an incidental rare word ("newly") does not
// veto the fact the other rare words ask for. Terms more common than that
// still do not anchor alone, and in a small store (below) only the rarest does.
func TestRareTermsAnchor(t *testing.T) {
	for _, tc := range []struct {
		notes int
		want  bool
	}{{20, true}, {anchorRareDF, true}, {anchorRareDF + 1, false}} {
		root := filepath.Join(t.TempDir(), "store")
		write(t, root, "user/newly-added.md", "newly-added", "user", "newly added gadgets appear in the catalog", "x", "")
		write(t, root, "user/no-rotation-needed.md", "no-rotation-needed", "user", "Secret scan findings on a dead token need no rotation", "x", "")
		// Enough other facts that 2% of the store is above anchorRareDF.
		for i := 0; i < 2600; i++ {
			write(t, root, fmt.Sprintf("user/filler-%d.md", i), fmt.Sprintf("filler-%d", i), "user", "unrelated filler", "x", "")
		}
		// Each common term is in tc.notes summaries counting the fact's own.
		for i := 1; i < tc.notes; i++ {
			for _, w := range []string{"secret", "token", "finding"} {
				write(t, root, fmt.Sprintf("user/%s-note-%d.md", w, i), fmt.Sprintf("%s-note-%d", w, i), "user", w+" note", "x", "")
			}
		}
		t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
		t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
		ix, err := Open(context.Background(), root, "anchor")
		if err != nil {
			t.Fatal(err)
		}
		hits, err := ix.Query(context.Background(), Situation{Query: "a newly reported secret token finding", RestrictRepos: true, PromptRelevance: true}, 10, false)
		ix.Close()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, h := range hits {
			names = append(names, h.Name)
		}
		if got := strings.Contains(strings.Join(names, ","), "no-rotation-needed"); got != tc.want {
			t.Errorf("terms in %d summaries: delivered %v, want %v (%v)", tc.notes, got, tc.want, names)
		}
	}
}

func TestRareAnchorsNeedALargeStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "user/newly-added.md", "newly-added", "user", "newly added gadgets appear in the catalog", "x", "")
	write(t, root, "user/no-rotation-needed.md", "no-rotation-needed", "user", "Secret scan findings on a dead token need no rotation", "x", "")
	write(t, root, "user/token-note.md", "token-note", "user", "token handling note", "x", "")
	write(t, root, "user/secret-note.md", "secret-note", "user", "secret storage note", "x", "")
	write(t, root, "user/finding-note.md", "finding-note", "user", "finding triage note", "x", "")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ix, err := Open(context.Background(), root, "small")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Query: "a newly reported secret token finding", RestrictRepos: true, PromptRelevance: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Name == "no-rotation-needed" {
			t.Fatal("in a three-fact store every term is under 50 facts; only the rarest may anchor")
		}
	}
}
