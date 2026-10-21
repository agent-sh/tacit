package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

func testRec(kind, folder string) *record.Record {
	name := folder[strings.LastIndex(folder, "/")+1:]
	return &record.Record{Kind: kind, Name: name, Description: "d", Status: record.StatusActive,
		Was:     []record.Entry{{Status: record.StatusActive, Date: today(), By: record.ByOwner, Why: "x"}},
		Created: today(), CreatedBy: record.ByOwner, Folder: folder}
}

// A session in either of two folders suppression joins reads both.
func TestSuppressionNamesJoinWinnerAndLosers(t *testing.T) {
	w, l := testRec(record.KindRepo, "projects/widget"), testRec(record.KindRepo, "projects/old-widget")
	w.Aliases = []string{"widget-svc"}
	l.SuppressedBy = "projects/widget"
	recs := map[string]*record.Record{w.Folder: w, l.Folder: l}
	if got := strings.Join(suppressionNames(recs, []string{"old-widget"}), ","); got != "old-widget,widget,widget-svc" {
		t.Errorf("from the loser: %s", got)
	}
	if got := strings.Join(suppressionNames(recs, []string{"Widget"}), ","); got != "Widget,widget-svc,old-widget" {
		t.Errorf("from the winner: %s", got)
	}
	if got := strings.Join(suppressionNames(recs, []string{"gadget"}), ","); got != "gadget" {
		t.Errorf("unrelated: %s", got)
	}
}

func putRecordFile(t *testing.T, dir string, r *record.Record) {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.FromSlash(record.Rel(r.Folder)))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tacit_search on a suppressed folder searches the folder replacing it and
// what is left in it; tacit_fact on a replaced fact shows the newer first.
func TestSearchFollowsSuppressionAndFactItsSuccessor(t *testing.T) {
	r, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-queue", Description: "Widget jobs go through one queue", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "gizmo-queue-left", Description: "Gizmo jobs had per-tenant queues", Type: "history", Scope: "projects/gizmo"},
		fact.Meta{Name: "other-queue", Description: "Other jobs use a queue too", Type: "history", Scope: "projects/other"},
		fact.Meta{Name: "queue-old", Description: "The queue was a list", Type: "history", Scope: "projects/widget", SupersededBy: "queue-mid"},
		fact.Meta{Name: "queue-mid", Description: "The queue was a deque", Type: "history", Scope: "projects/widget", SupersededBy: "queue-new"},
		fact.Meta{Name: "queue-new", Description: "The queue is a ring buffer", Type: "design_decision", Scope: "projects/widget"},
	)
	w, g := testRec(record.KindRepo, "projects/widget"), testRec(record.KindRepo, "projects/gizmo")
	g.SuppressedBy = "projects/widget"
	putRecordFile(t, r.clone, w)
	putRecordFile(t, r.clone, g)
	writeFormatForTest(t, r.clone)
	r.commitIndex(r.clone, "records")
	t.Chdir(cwd)
	out := callTool(t, "tacit_search", map[string]any{"scope": "projects/gizmo"})
	for _, want := range []string{"widget-queue", "gizmo-queue-left"} {
		if !strings.Contains(out, want) {
			t.Errorf("search of the suppressed folder lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other-queue") {
		t.Errorf("another folder in the answer:\n%s", out)
	}
	out = callTool(t, "tacit_fact", map[string]any{"name": "queue-old"})
	if !strings.HasPrefix(out, "queue-old was superseded by queue-new") || strings.Index(out, "ring buffer") > strings.Index(out, "was a list") {
		t.Errorf("tacit_fact of a replaced fact:\n%s", out)
	}
	if out := callTool(t, "tacit_fact", map[string]any{"name": "queue-new"}); strings.Contains(out, "superseded") {
		t.Errorf("a live fact:\n%s", out)
	}
	// The tree: the suppressed folder reads as the winner.
	tr := callTool(t, "tacit_tree", map[string]any{"path": "projects/gizmo", "depth": 0})
	if !strings.Contains(tr, "projects/gizmo is suppressed by projects/widget") {
		t.Errorf("tree:\n%s", tr)
	}
}

func writeFormatForTest(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(ledger.FormatPath))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, ledger.Format{Version: ledger.FormatVersion, At: "2026-10-21T00:00:00Z"}.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// noAssess turns the full curate's assessment off for a test about
// another step (its prompts and its fixture answers are not that test's).
func noAssess(t *testing.T) {
	t.Helper()
	prev := curateAssessStep
	t.Cleanup(func() { curateAssessStep = prev })
	curateAssessStep = func(context.Context, *store.Store, string, bool, *curate.Report) int { return 0 }
}
