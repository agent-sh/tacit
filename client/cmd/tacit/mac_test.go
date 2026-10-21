package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
)

// The Mac: a read-only clone with the local layer, against a server whose
// hook refuses every push and logs each attempt. Dream, curate (split,
// assess, migrate), tacit assess, tacit migrate, and the writers of folder
// records all run; the clone's HEAD, refs and working tree stay as they
// were, no push is attempted, and the records and indexes they make exist
// only in the overlay.
func TestMacReadOnlyCloneSendsNothing(t *testing.T) {
	r := newLocalRig(t)
	// The server: the store's format marker and a store record (another
	// machine's), and a hook that refuses every push.
	seed := filepath.Join(r.root, "seed2")
	gitIn(t, r.root, "clone", "-q", r.bare, seed)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	os.WriteFile(filepath.Join(seed, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	other := testRec(record.KindRepo, "projects/numbat")
	putRecordFile(t, seed, other)
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "format and a record")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	hookLog := filepath.Join(r.root, "push-attempts.log")
	os.WriteFile(filepath.Join(r.bare, "hooks", "pre-receive"), []byte("#!/bin/sh\necho attempt >> '"+hookLog+"'\necho 'pushes refused' >&2\nexit 1\n"), 0o755)
	ctx := context.Background()
	if err := r.s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	r.serverHead = gitIn(t, r.bare, "rev-parse", "main")
	r.cloneHead = gitIn(t, r.s.Dir, "rev-parse", "HEAD")
	if r.cloneHead != r.serverHead {
		t.Fatal("the clone did not take the marker")
	}
	r.cloneFiles = treeBytes(t, r.s.Dir)
	check := func(what string) {
		t.Helper()
		if got := gitIn(t, r.bare, "rev-parse", "main"); got != r.serverHead {
			t.Fatalf("%s: the server moved", what)
		}
		if got := gitIn(t, r.s.Dir, "rev-parse", "HEAD"); got != r.cloneHead {
			t.Fatalf("%s: the clone's HEAD moved", what)
		}
		if out := gitIn(t, r.s.Dir, "status", "--porcelain", "--ignored"); out != "" {
			t.Fatalf("%s: the clone changed:\n%s", what, out)
		}
		got := treeBytes(t, r.s.Dir)
		if len(got) != len(r.cloneFiles) {
			t.Fatalf("%s: the clone holds %d files, was %d", what, len(got), len(r.cloneFiles))
		}
		for p, b := range r.cloneFiles {
			if got[p] != b {
				t.Fatalf("%s: the clone's %s changed", what, p)
			}
		}
		if _, err := os.Stat(hookLog); err == nil {
			b, _ := os.ReadFile(hookLog)
			t.Fatalf("%s: a push was attempted:\n%s", what, b)
		}
	}

	// This machine: a work repository and a session in it with a tool.
	gizmo := filepath.Join(r.root, "projects", "gizmo")
	os.MkdirAll(gizmo, 0o755)
	gitIn(t, gizmo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(gizmo, "main.go"), []byte("x"), 0o644)
	gitIn(t, gizmo, "add", "main.go")
	gitIn(t, gizmo, "commit", "-q", "-m", "c")
	f := toolSession(t, r.home, gizmo, "m1", "DELTA the gizmo build uses the tracker", "mcp__tracker__TicketRead", filepath.Join(gizmo, "main.go"))
	dream.SaveState(r.statePath, &dream.State{Offsets: map[string]int64{r.transcript: 0, f: 0}})
	prev := dreamRun
	t.Cleanup(func() { dreamRun = prev })
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		if strings.Contains(prompt, "DELTA") {
			return `[{"description":"The gizmo build reads tickets from the tracker","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
		}
		return `[]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream %d", code)
	}
	check("dream")
	notes, _ := overlay.Load(r.local)
	var note *fact.Fact
	for _, n := range notes {
		if n.Meta.Source.Session == "m1" {
			note = n
		}
	}
	if note == nil || note.Meta.Scope != "projects/gizmo" {
		t.Fatalf("the local note: %+v", note)
	}
	r.split(entry(note.Meta.Name, "gizmo-build-tracker", "The gizmo build reads tickets from the tracker", "design_decision", "projects/gizmo"))
	r.judge()
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/gizmo": aDecision("projects/gizmo", "repo", "active", "edited in session:m1")}}
	fa.install(t)
	if code := runCurate(nil); code != 0 {
		t.Fatalf("local curate %d", code)
	}
	check("curate")
	if len(fa.prompts) == 0 || strings.Contains(fa.prompts[0], "folder=projects/numbat") {
		t.Fatalf("the local assessment: %d prompt(s); the store's own record was asked again", len(fa.prompts))
	}
	rec := filepath.Join(r.local, overlay.CuratedDir, "projects", "gizmo", record.FileName)
	if _, err := os.Stat(rec); err != nil {
		t.Fatalf("no local record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.s.Dir, "projects", "gizmo")); err == nil {
		t.Fatal("a record or index in the clone")
	}
	if _, err := os.Stat(filepath.Join(r.local, overlay.CuratedDir, "projects", "gizmo", "_index.md")); err != nil {
		t.Errorf("no local index: %v", err)
	}
	for _, args := range [][]string{nil} {
		if code := runAssess(args); code != 0 {
			t.Fatalf("assess %d", code)
		}
		check("assess")
		if code := runMigrate(args); code != 0 {
			t.Fatalf("migrate %d", code)
		}
		check("migrate")
	}
	// The store's writers refuse on the read-only clone.
	if code := runFormat([]string{"--set"}); code != 2 {
		t.Fatalf("format --set on a read-only clone: %d", code)
	}
	if code := runFolder([]string{"create", "--folder", "projects/gizmo2", "--kind", "repo", "--desc", "x", "--why", "session:m1"}); code != 2 {
		t.Fatalf("tacit folder on a read-only clone: %d", code)
	}
	rs := &reviewServer{s: r.s, viewOnly: viewOnlyReason(ctx, r.s)}
	if _, err := rs.folderSave(ctx, folderDraft{Action: "create", Folder: "projects/gizmo3", Kind: "repo", Description: "x"}); err == nil {
		t.Fatal("the app wrote a record on a read-only clone")
	}
	check("the writers")
	if _, err := os.Stat(filepath.Join(platform.StateDir(r.home), dream.ToolsFile)); err != nil {
		t.Errorf("no tool usage in this machine's state: %v", err)
	}
}
