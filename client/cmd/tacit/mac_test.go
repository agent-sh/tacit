package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/modelrun"
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
	other.CreatedBy, other.Was[0].By = record.ByOpus, record.ByOpus // another machine's curator decided it
	other.Status, other.Was[0].Status = record.StatusDormant, record.StatusDormant
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
	// A checkout of numbat, the folder the store has a record of, and a
	// session there: this machine has its own view of it.
	numbat := filepath.Join(r.root, "projects", "numbat")
	os.MkdirAll(numbat, 0o755)
	gitIn(t, numbat, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(numbat, "n.go"), []byte("x"), 0o644)
	gitIn(t, numbat, "add", "n.go")
	gitIn(t, numbat, "commit", "-q", "-m", "c")
	// (Two sessions: a checkout in a temporary folder is a repository only
	// after work from two.)
	f2 := toolSession(t, r.home, numbat, "m2", "EPSILON a look at numbat", "Read", filepath.Join(numbat, "n.go"))
	f3 := toolSession(t, r.home, numbat, "m3", "EPSILON another look at numbat", "Read", filepath.Join(numbat, "n.go"))
	dream.SaveState(r.statePath, &dream.State{Offsets: map[string]int64{r.transcript: 0, f: 0, f2: 0, f3: 0}})
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
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/gizmo":  aDecision("projects/gizmo", "repo", "active", "edited in session:m1"),
		// This machine's sessions edited it today: active here, code's status
		// from those dates (the model is not asked).
		"projects/numbat": aDecision("projects/numbat", "repo", "dormant", "only looked at in session:m2, no work"),
	}}
	fa.install(t)
	// The rotation runs in the local layer's curate (its daily job runs
	// curate, not maintain).
	prevRot := rotationRun
	t.Cleanup(func() { rotationRun = prevRot })
	var rotated []string
	rotationRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		var out []string
		for _, m := range regexp.MustCompile(`"name":"([a-z0-9-]+)"`).FindAllStringSubmatch(prompt, -1) {
			rotated = append(rotated, m[1])
			out = append(out, `{"name":"`+m[1]+`","outcome":"still"}`)
		}
		return "[" + strings.Join(out, ",") + "]", 0.01, nil
	}
	if code := runCurate(nil); code != 0 {
		t.Fatalf("local curate %d", code)
	}
	check("curate")
	if strings.Join(rotated, ",") != "gizmo-build-tracker" {
		t.Fatalf("the local curate's rotation judged %v", rotated)
	}
	// The store's record says dormant; this machine's work dates say active:
	// code decides that on its own (no model call), into the overlay.
	if len(fa.prompts) == 0 || strings.Contains(strings.Join(fa.prompts, "\n"), "folder=projects/numbat") {
		t.Fatalf("the local assessment: %d prompt(s); numbat's status is code's from this machine's dates", len(fa.prompts))
	}
	// The store's record, consolidated with this machine's view: its status
	// here is the overlay's, everything else the store's; the store's file
	// is untouched (check), and a second look is not asked for.
	ov, err := os.ReadFile(filepath.Join(r.local, overlay.CuratedDir, "projects", "numbat", record.FileName))
	if err != nil {
		t.Fatalf("no local view of the store's record: %v", err)
	}
	if lr, err := record.Parse("projects/numbat", ov); err != nil || lr.Status != record.StatusActive || !strings.HasPrefix(lr.Was[0].Why, "code: active from its dates") || lr.Description != other.Description || lr.CreatedBy != record.ByOpus || len(lr.Was) != 2 {
		t.Fatalf("the local view: %+v %v", lr, err)
	}
	if m := storeRecords(r.s.Dir)["projects/numbat"]; m == nil || m.Status != record.StatusActive || !m.LocalStatus {
		t.Fatalf("this machine reads %+v", m)
	}
	if ix, err := openIndex(ctx, r.s); err != nil {
		t.Fatal(err)
	} else {
		recs, _ := ix.Records(ctx)
		ix.Close()
		if recs["projects/numbat"] == nil || recs["projects/numbat"].Status != record.StatusActive {
			t.Fatalf("the index reads %+v", recs["projects/numbat"])
		}
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
	// The rotation checks this machine's own facts and stamps them in the
	// overlay; an overview of its own folder goes there too.
	for _, n := range []string{"gizmo-runs-tests", "gizmo-ships-weekly"} {
		lf := rotFile(n, "projects/gizmo", "design_decision", "the "+n+" fact", "the "+n+" fact", "2026-09-01", "")
		if err := overlay.Write(r.local, r.s.Dir, overlay.CuratedDir+"/projects/gizmo/"+n+".md", []byte(lf)); err != nil {
			t.Fatal(err)
		}
	}
	rotated = nil
	if code := runRotation(nil); code != 0 {
		t.Fatalf("rotation %d", code)
	}
	check("rotation")
	if len(rotated) != 2 {
		t.Fatalf("the rotation judged %v, want this machine's two new facts (the curate's rotation checked the first)", rotated)
	}
	local, _ := overlay.Load(r.local)
	for _, lf := range local {
		if overlay.IsCurated(lf.Path) && lf.Meta.LastChecked == "" {
			t.Errorf("%s not stamped in the overlay", lf.Path)
		}
	}
	prevOv := overviewRun
	t.Cleanup(func() { overviewRun = prevOv })
	overviewRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		return "The gizmo build [[gizmo-build-tracker]] [[gizmo-runs-tests]] [[gizmo-ships-weekly]].", 0.01, nil
	}
	if n, errs := overviewStep(ctx, r.s, "m", 5, false, &curate.Report{ModelCalls: &modelrun.Trace{}}); n != 1 || errs != 0 {
		t.Fatalf("local overview: %d written, %d errors", n, errs)
	}
	check("overview")
	if _, err := os.Stat(filepath.Join(r.local, overlay.CuratedDir, "projects", "gizmo", "_overview.md")); err != nil {
		t.Fatalf("no local overview: %v", err)
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
