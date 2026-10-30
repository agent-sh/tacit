package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

func relevanceIndex(t *testing.T, specs []fact.Meta) *search.Index {
	t.Helper()
	return openRelevance(t, relevanceStore(t, specs))
}

// relevanceStore writes the specs among 6,000 unrelated fillers, so the
// rarity gates see a store of a real size.
func relevanceStore(t *testing.T, specs []fact.Meta) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	for i := 0; i < 6000; i++ {
		writeFact(t, root, fact.Meta{Name: fmt.Sprintf("filler-%d", i), Description: "unrelated filler material", Type: "machine_fact", Scope: "system"}, "filler")
	}
	for _, m := range specs {
		m.Type = "machine_fact"
		if m.Scope == "" {
			m.Scope = "system"
		}
		writeFact(t, root, m, "detail")
	}
	return root
}

func openRelevance(t *testing.T, root string) *search.Index {
	t.Helper()
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ix, err := search.Open(context.Background(), root, "relevance")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestPromptRelevanceSingleGenericTerm(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{{Name: "kernel-dkms", Description: "endpoint endpoint endpoint"}, {Name: "linkedin-policy", Description: "research research research"}})
	for _, p := range []string{"add a new endpoint to the api", "research fresh topics", "endpoint endpoint"} {
		got, err := promptHits(context.Background(), ix, search.Situation{}, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("%q leaked %s", p, names(got))
		}
	}
}

func TestHookRepoRestrictions(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{{Name: "tower-deploy", Description: "demo deploy version", Scope: "machines/host-a", AppliesTo: fact.AppliesTo{Repos: []string{"other"}, Hosts: []string{"host-a"}}}})
	for _, repo := range []string{"", "demo", "other"} {
		sit := search.Situation{Repo: repo, Host: "host-a", Query: "deploy version"}
		explicit, err := ix.Query(context.Background(), sit, 10, false)
		if err != nil || len(explicit) != 1 {
			t.Fatalf("explicit search changed: %v %v", explicit, err)
		}
		got, err := promptHits(context.Background(), ix, sit, "deploy version", nil)
		if err != nil {
			t.Fatal(err)
		}
		if (len(got) == 1) != (repo == "other") {
			t.Errorf("repo=%q prompt=%s", repo, names(got))
		}
		start, err := sessionStartHits(context.Background(), ix, sit)
		if err != nil {
			t.Fatal(err)
		}
		if repo == "demo" && len(start) != 0 {
			t.Errorf("session start leaked %s", names(start))
		}
	}
}

func TestHookRepoIntentAndAliases(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "widget-deploy", Description: "widget deploy version", AppliesTo: fact.AppliesTo{Repos: []string{"widget"}}},
		{Name: "widget-config", Description: "widget config path", Scope: "projects/widget"},
	})
	for _, tc := range []struct {
		query string
		sit   search.Situation
		want  bool
	}{
		{"widget deploy version", search.Situation{}, true},
		{"deploy version in widget.", search.Situation{}, true},
		{"widget.extension deploy version", search.Situation{}, false},
		{"WIDGET deploy version", search.Situation{Repo: "unrelated"}, true},
		{"widget config path", search.Situation{}, true},
		{"deploy version", search.Situation{}, false},
		{"widget-extra deploy version", search.Situation{}, false},
		{"deploy version", search.Situation{Repo: "canonical", RepoAliases: []string{"widget"}}, true},
	} {
		got, err := promptHits(context.Background(), ix, tc.sit, tc.query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (len(got) > 0) != tc.want {
			t.Errorf("%q %+v: %s", tc.query, tc.sit, names(got))
		}
	}
}

func TestHookRepoAliasesVerifiedCheckout(t *testing.T) {
	root := t.TempDir()
	real, link := filepath.Join(root, "canonical"), filepath.Join(root, "checkout-alias")
	if err := os.MkdirAll(filepath.Join(real, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", real}, {"-C", real, "remote", "add", "origin", "git@example.test:org/remote-name.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	got := hookRepoAliases(filepath.Join(link, "nested"))
	for _, name := range []string{"canonical", "checkout-alias", "remote-name"} {
		if !slices.Contains(got, name) {
			t.Errorf("missing %s in %v", name, got)
		}
	}
	if got := hookRepoAliases(root); len(got) != 0 {
		t.Errorf("non-repo aliases: %v", got)
	}
	// A linked worktree is its main repository (repoid): its directory's
	// name is no alias.
	wt := filepath.Join(root, "canonical-wt")
	for _, args := range [][]string{{"-C", real, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "--allow-empty", "-m", "x"}, {"-C", real, "worktree", "add", "-q", wt}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	got = hookRepoAliases(wt)
	if !slices.Contains(got, "canonical") || !slices.Contains(got, "remote-name") || slices.Contains(got, "canonical-wt") {
		t.Errorf("worktree aliases: %v", got)
	}
}

func TestPromptRelevanceBeforeLimit(t *testing.T) {
	var specs []fact.Meta
	for i := 0; i < 30; i++ {
		specs = append(specs, fact.Meta{Name: fmt.Sprintf("restricted-%d", i), Description: "endpoint api", AppliesTo: fact.AppliesTo{Repos: []string{"other"}}})
	}
	specs = append(specs, fact.Meta{Name: "valid", Description: "endpoint api conventions"})
	ix := relevanceIndex(t, specs)
	// Use the project floor here so this isolates candidate starvation, not
	// the separate precision/recall policy for repo-less prompts.
	got, err := promptHits(context.Background(), ix, search.Situation{Repo: "demo"}, "endpoint api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if names(got) != "valid" {
		t.Fatalf("valid candidate starved: %s", names(got))
	}
}

// A line whose words the request already holds (a scheduled job's own
// instructions, said back to it) is not printed, and nothing takes its place;
// a fact that matches the request and adds to it is printed.
func TestPromptBlockLeavesOutEchoesOfTheRequest(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "review-tool-failures-weekly", Description: "Review the tool failures weekly and fix their root cause"},
		{Name: "tool-failures-fix-at-source", Description: "Review tool failures weekly: fix the root cause in the mods, rules or scripts that let them happen, and log what changed"},
	})
	got, err := promptHits(context.Background(), ix, search.Situation{}, "Review the tool failures weekly and fix their root cause, then report", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Split(names(got), ","); slices.Contains(n, "review-tool-failures-weekly") || !slices.Contains(n, "tool-failures-fix-at-source") {
		t.Fatalf("got %v: want where to fix, not the instructions said back", n)
	}
}
