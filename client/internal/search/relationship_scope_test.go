package search

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestRelationshipDoesNotOverrideExplicitProject(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/padding-%d.md", i), fmt.Sprintf("padding-%d", i), "system", "unrelated padding material", "", "")
	}
	write(t, root, "projects/alpha/alpha-browser.md", "alpha-browser", "projects/alpha", "alpha browser specialist reading list", "", "applies_to:\n  repos: [alpha]\n")
	write(t, root, "projects/beta/beta-browser.md", "beta-browser", "projects/beta", "browser specialist reading list", "", "applies_to:\n  repos: [beta]\n")
	write(t, root, "machines/host-a/beta-browser-local.md", "beta-browser-local", "machines/host-a", "browser specialist reading list", "", "applies_to:\n  repos: [beta]\n")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "named-project")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Query: "alpha browser specialist", Host: "host-a", RestrictRepos: true, PromptRelevance: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Name == "alpha-browser" {
			found = true
		}
		if h.Name == "beta-browser" || h.Name == "beta-browser-local" {
			t.Errorf("explicit project overridden by relationship: %s", h.Name)
		}
	}
	if !found {
		t.Fatal("lost requested project")
	}
}
