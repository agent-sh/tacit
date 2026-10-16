package search

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestLinkedLocationsRespectScopeAndGlobalUniqueness(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/padding-%d.md", i), fmt.Sprintf("padding-%d", i), "system", "unrelated filler", "", "")
	}
	write(t, root, "system/relay-unit.md", "relay-unit", "system", "restart the aster relay service", "See [[good]], [[foreign]], [[other]], [[restricted]], [[ambiguous]], [[retired]], [[derived]], [[inbox]].", "")
	write(t, root, "system/protocol.md", "protocol", "system", "aster relay protocol", "", "")
	write(t, root, "system/good.md", "good", "system", "manual", "", "applies_to:\n  paths: [/recorded/manual]\n")
	write(t, root, "machines/host-b/foreign.md", "foreign", "machines/host-b", "manual", "", "applies_to:\n  commands: [ssh host-b restart-relay]\n")
	write(t, root, "projects/beta/other.md", "other", "projects/beta", "manual", "", "applies_to:\n  repos: [beta]\n")
	write(t, root, "system/restricted.md", "restricted", "system", "manual", "", "applies_to:\n  repos: [beta]\n")
	write(t, root, "system/ambiguous.md", "ambiguous", "system", "manual", "", "")
	write(t, root, "projects/beta/ambiguous.md", "ambiguous", "projects/beta", "manual", "", "")
	write(t, root, "system/retired.md", "retired", "system", "manual", "", "retired: replaced\n")
	write(t, root, "derived/alpha/derived.md", "derived", "derived/alpha", "manual", "", "")
	write(t, root, "inbox/claude@host-a/inbox.md", "inbox", "system", "manual", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "overlay"))
	ix, err := Open(context.Background(), root, "links-scope")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Repo: "alpha", Host: "host-a", Query: "restart the aster relay service", RestrictRepos: true, PromptRelevance: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Name == "relay-unit" {
			found = true
			if h.Evidence == nil {
				t.Fatal("missing explanation")
			}
			locs := h.Evidence.Locations
			if len(locs) != 2 || locs[0].Fact != "relay-unit" || locs[1].Fact != "good" {
				t.Fatalf("out-of-scope or globally ambiguous pointers: %+v", locs)
			}
		}
	}
	if !found {
		t.Fatal("missing eligible root")
	}
}
