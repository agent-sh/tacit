package search

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// Uses no new API so the same regression runs against the pre-change source.
func TestRelationshipRoutesUnknownRepository(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/padding-%d.md", i), fmt.Sprintf("padding-%d", i), "system", "ordinary unrelated guidance", "", "")
	}
	write(t, root, "machines/host-a/relay-unit.md", "relay-unit", "machines/host-a", "Restart the aster relay service with the user unit.", "", "applies_to:\n  repos: [historical-capture]\n  commands: [systemctl --user restart aster-relay]\n")
	write(t, root, "system/relay-protocol.md", "relay-protocol", "system", "The aster relay protocol carries events.", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "relationship-regression")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Host: "host-a", Query: "restart the aster relay service", RestrictRepos: true, PromptRelevance: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Name == "relay-unit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("explicit service relationship did not route to repo-restricted machine fact: %v", hits)
	}
}
