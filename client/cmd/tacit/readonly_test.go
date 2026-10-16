package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// roFixture is a read-only clone of the review fixture, a local overlay and
// an index directory of the test's own, and a PATH holding git and, when
// withScrump, a scrump that masks "Zq7Vx9" at the same length.
func roFixture(t *testing.T, withScrump bool) (*store.Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, _ := reviewFixture(t)
	root := t.TempDir()
	s := &store.Store{Dir: filepath.Join(root, "store")}
	if err := s.CloneReadOnly(context.Background(), bare, ""); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "local")
	t.Setenv("TACIT_LOCAL", local)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	bin := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	if withScrump {
		// sed is /bin/sed on Linux and /usr/bin/sed on macOS; resolve it before PATH is replaced.
		sed, err := exec.LookPath("sed")
		if err != nil {
			t.Skip("sed not installed")
		}
		if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec "+sed+" 's/Zq7Vx9/******/g'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return s, local
}

func cleanClone(t *testing.T, s *store.Store) {
	t.Helper()
	out, err := store.Git(context.Background(), s.Dir, "status", "--porcelain", "--ignored")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("the clone changed: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inbox")); !os.IsNotExist(err) {
		t.Fatal("a read-only clone got an inbox")
	}
}

// Redaction fails closed on a read-only clone too: no scrump, nothing
// written, neither in the clone nor in the overlay.
func TestReadOnlyAddWithoutScrumpWritesNothing(t *testing.T) {
	s, local := roFixture(t, false)
	rel, err := addFact(context.Background(), s, "claude", fact.Meta{Name: "x", Description: "a fact from the host-b", Scope: "user"}, "body")
	if err == nil || !strings.Contains(err.Error(), "redaction unavailable") || rel != "" {
		t.Fatalf("addFact = %q, %v; want a redaction failure", rel, err)
	}
	cleanClone(t, s)
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("the overlay was created without redaction")
	}
}

// On a read-only clone an agent's fact lands in this machine's overlay,
// redacted, harness-tagged, deduplicated; the clone is untouched; search, the
// hook block and `tacit fact` read it marked "(this machine)".
func TestReadOnlyAddGoesToTheLocalOverlay(t *testing.T) {
	s, local := roFixture(t, true)
	ctx := context.Background()
	host := store.ShortHost()

	rel, err := addFact(ctx, s, "claude", fact.Meta{Name: "host-b-brew-prefix", Description: "brew lives in /opt/homebrew on this machine, token Zq7Vx9", Scope: "user"}, "Homebrew is under /opt/homebrew here; token Zq7Vx9.")
	if err != nil {
		t.Fatal(err)
	}
	if want := "local/claude@" + host + "/host-b-brew-prefix.md"; rel != want {
		t.Fatalf("rel = %q, want %q", rel, want)
	}
	b, err := os.ReadFile(filepath.Join(local, "claude@"+host, "host-b-brew-prefix.md"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := fact.Parse("x.md", b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Zq7Vx9") || !strings.Contains(f.Body, "******") {
		t.Fatalf("not redacted:\n%s", b)
	}
	if f.Meta.Source.Harness != "claude" || f.Meta.Source.Host != host || f.Meta.Type != "unclassified" {
		t.Fatalf("meta = %+v", f.Meta)
	}
	cleanClone(t, s)

	// The same fact from another harness is not written twice.
	again, err := addFact(ctx, s, "codex", fact.Meta{Name: "other-name", Description: "brew lives in /opt/homebrew on this machine, token Zq7Vx9", Scope: "user"}, "Homebrew is under /opt/homebrew here; token Zq7Vx9.")
	if err != nil || again != rel {
		t.Fatalf("duplicate add = %q, %v; want %q", again, err, rel)
	}
	// Another fact under a taken name gets a suffix; nothing is overwritten.
	other, err := addFact(ctx, s, "claude", fact.Meta{Name: "host-b-brew-prefix", Description: "a different note about homebrew", Scope: "user"}, "different")
	if err != nil || other == rel || !strings.HasPrefix(other, "local/claude@"+host+"/host-b-brew-prefix-") {
		t.Fatalf("second add = %q, %v", other, err)
	}
	facts, errs := overlay.Load(local)
	if len(facts) != 2 || len(errs) != 0 {
		t.Fatalf("overlay holds %d facts (%v), want 2", len(facts), errs)
	}

	// A note an agent says is the owner's is still not a ruling here.
	if _, err := addFact(ctx, s, "codex", fact.Meta{Name: "says-owner", Description: "owner wants homebrew pinned on the host-b", Scope: "user", Source: fact.Source{Kind: "user"}}, "homebrew pinned"); err != nil {
		t.Fatal(err)
	}

	ix, err := openIndex(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(ctx, search.Situation{Host: host, Query: "homebrew opt prefix"}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	var got *search.Hit
	for i := range hits {
		if hits[i].Name == "host-b-brew-prefix" {
			got = &hits[i]
		}
		if hits[i].Name == "says-owner" && hits[i].Ruling {
			t.Fatal("a local note became a ruling")
		}
	}
	if got == nil || !got.Local || got.Inbox {
		t.Fatalf("hits = %+v", hits)
	}
	if blk := search.Block([]search.Hit{*got}, "#"); !strings.Contains(blk, "- host-b-brew-prefix (this machine): ") {
		t.Fatalf("block = %q", blk)
	}
	if p, ok := findFactOrLocal(s.Dir, "host-b-brew-prefix"); !ok || !strings.HasPrefix(p, local) {
		t.Fatalf("findFactOrLocal = %q, %v", p, ok)
	}
	if _, ok := findFact(s.Dir, "host-b-brew-prefix"); ok {
		t.Fatal("the local note is in the clone")
	}
	cleanClone(t, s)
}

// The overlay may not sit inside the clone, where a git add could reach it.
func TestReadOnlyAddRefusesAnOverlayInsideTheClone(t *testing.T) {
	s, _ := roFixture(t, true)
	t.Setenv("TACIT_LOCAL", filepath.Join(s.Dir, "local"))
	rel, err := addFact(context.Background(), s, "claude", fact.Meta{Name: "x", Description: "a fact", Scope: "user"}, "body")
	if !errors.Is(err, overlay.ErrInside) || rel != "" {
		t.Fatalf("addFact = %q, %v; want ErrInside", rel, err)
	}
	cleanClone(t, s)
}

// SessionStart takes at most startLocalLimit local notes, after the rest.
func TestStartSelectionCapsLocalNotes(t *testing.T) {
	var hits []search.Hit
	for i := 0; i < 6; i++ {
		hits = append(hits, search.Hit{Name: "local-" + string(rune('a'+i)), Scope: "user", Local: true, Score: 10})
	}
	hits = append(hits, search.Hit{Name: "ruling", Scope: "user", Ruling: true, Score: 2})
	got := startSelection(hits, "", startLimit)
	n := 0
	for _, h := range got {
		if h.Local {
			n++
		}
	}
	if n != startLocalLimit || got[0].Name != "ruling" {
		t.Fatalf("selection = %+v", got)
	}
}

// A reader key is never one an agent writes as.
func TestReaderIsNotAnAgentHarness(t *testing.T) {
	if !fact.Harnesses["reader"] || agentHarness("reader") {
		t.Fatal("reader must be a known harness that no agent writes as")
	}
}
