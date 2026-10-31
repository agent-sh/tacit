package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

func TestLocalStoreAgentNoteOwnerRulingAndSearch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "facts")
	// root is the home: a checkout in one of the home's dot directories is
	// never a repository (repoid), and Go's temp dir may be one (GOTMPDIR or
	// TMPDIR under ~/.cache).
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	stubScrump(t)
	// demo is a checkout on this machine: a project scope names a real repository.
	if err := os.MkdirAll(filepath.Join(root, "projects", "demo", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init: %d", code)
	}
	s := &store.Store{Dir: dir}
	ctx := context.Background()
	if err := writerCheck(ctx, s, "curator"); err != nil {
		t.Fatalf("local mode wanted enrollment: %v", err)
	}
	if _, err := addFact(ctx, s, "claude", fact.Meta{Name: "agent-standard", Description: "Review every public change before merging it", Type: "user_standard", Scope: "projects/demo"}, "The agent guessed this standard."); err != nil {
		t.Fatal(err)
	}
	fs, errs, err := fact.Walk(dir)
	if err != nil || len(errs) != 0 {
		t.Fatalf("walk: %v %v", err, errs)
	}
	if len(fs) != 1 || fact.IsRuling(fs[0]) {
		t.Fatal("an unsupported agent standard became a ruling")
	}
	rs := &reviewServer{s: s}
	rs.loadFacts()
	res, err := rs.newRuling(ctx, rulingDraft{Name: "review-first", Body: "Review every public change before merging it.", Scope: "projects/demo"})
	if err != nil || !res.Saved {
		t.Fatalf("local owner write: %+v %v", res, err)
	}
	head, _ := s.Head(ctx)
	ix, err := search.Open(ctx, dir, head)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(ctx, search.Situation{Repo: "demo", Query: "review public change merging"}, 10, false)
	if err != nil || len(hits) != 1 || hits[0].Name != "review-first" || !hits[0].Ruling {
		t.Fatalf("local retrieval: %+v %v", hits, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review", "rulings-audit.json")); err != nil {
		t.Fatal(err)
	}
	if code := runFollowCode(nil); code != exitConfig {
		t.Fatalf("local follow: %d", code)
	}
}
