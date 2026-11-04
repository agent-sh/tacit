package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The vocabulary is the store's: a word only this machine's overlay holds
// is in none of its facts, and the overlay note's name is no fact of the
// store's; a word a store fact holds, in its description or its body, is.
func TestVocabularyIsTheStores(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	local := filepath.Join(base, "local")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", local)
	write(t, root, "user/curated.md", "curated", "user", "homebrew is pinned by the owner", "the zebrafish body word", "")
	write(t, local, "claude@host-b/brew-prefix.md", "brew-prefix", "user", "homebrew prefix is quokkaonly on this machine", "body", "")
	ctx := context.Background()
	ix, err := Open(ctx, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if got := strings.Join(ix.InVocabulary(ctx, []string{"homebrew", "quokkaonly", "zebrafish", "nowhere"}), " "); got != "homebrew zebrafish" {
		t.Fatalf("vocabulary: %q", got)
	}
	if !ix.HasFact(ctx, "curated") || ix.HasFact(ctx, "brew-prefix") || ix.HasFact(ctx, "nowhere") {
		t.Fatal("HasFact counts the overlay or misses the store")
	}
}
