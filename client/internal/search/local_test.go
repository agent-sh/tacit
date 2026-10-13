package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This machine's local notes (a read-only clone's overlay) are indexed with
// the store's facts, marked Local, never as rulings; a note added later is
// found at the same store commit, since the index is keyed by the overlay too.
func TestLocalOverlayIsSearchedWithTheStore(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	local := filepath.Join(base, "local")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", local)
	write(t, root, "user/curated.md", "curated", "user", "homebrew is pinned by the owner", "body", "")
	ctx := context.Background()

	ix, err := Open(ctx, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.Query(ctx, Situation{Host: "host-b", Query: "homebrew prefix"}, 5, false)
	ix.Close()
	if len(hits) != 1 || hits[0].Local {
		t.Fatalf("before the note: %+v", hits)
	}

	// kind user: an agent's claim, which must not make it a ruling here.
	write(t, local, "claude@host-b/brew-prefix.md", "brew-prefix", "user", "homebrew prefix is /opt/homebrew on this machine", "body", "")
	p := filepath.Join(local, "claude@host-b", "brew-prefix.md")
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), "kind: observed", "kind: user", 1)), 0o644)

	ix, err = Open(ctx, root, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, _ = ix.Query(ctx, Situation{Host: "host-b", Query: "homebrew prefix"}, 5, false)
	var got *Hit
	for i := range hits {
		if hits[i].Name == "brew-prefix" {
			got = &hits[i]
		}
	}
	if got == nil || !got.Local || got.Inbox || got.Ruling || got.Path != "local/claude@host-b/brew-prefix.md" {
		t.Fatalf("after the note: %+v", hits)
	}
	if b := Block([]Hit{*got}, "#"); !strings.Contains(b, "- brew-prefix (this machine): ") {
		t.Fatalf("block: %q", b)
	}
	entries, _ := os.ReadDir(filepath.Join(base, "index"))
	if len(entries) != 1 || !strings.Contains(entries[0].Name(), "-abc123-l") {
		t.Fatalf("index files: %v", entries)
	}
}
