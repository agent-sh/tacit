package validate

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

func putRecord(t *testing.T, store string, r *record.Record) {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(store, filepath.FromSlash(record.Rel(r.Folder))), string(b))
}

func newRec(kind, folder string) *record.Record {
	name := folder[strings.LastIndex(folder, "/")+1:]
	return &record.Record{Kind: kind, Name: name, Description: "d", Status: record.StatusActive,
		Was:     []record.Entry{{Status: record.StatusActive, Date: "2026-10-21", By: record.ByOwner, Why: "x"}},
		Created: "2026-10-21", CreatedBy: record.ByOwner, Folder: folder}
}

// recordStore is a store with the format marker, a project gadget with a
// record and alias gizmo, a project old-gadget suppressed into it, an area
// ops with a topic q4, and a free topic launch.
func recordStore(t *testing.T, complete bool) (string, *repoid.Catalog) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	store := filepath.Join(home, "store")
	write(t, filepath.Join(store, filepath.FromSlash(ledger.FormatPath)), string(ledger.Format{Version: 1, At: "x", RecordsComplete: complete}.Encode()))
	g := newRec(record.KindRepo, "projects/gadget")
	g.Aliases = []string{"gizmo"}
	old := newRec(record.KindRepo, "projects/old-gadget")
	old.SuppressedBy = "projects/gadget"
	for _, r := range []*record.Record{g, old, newRec(record.KindArea, "areas/ops"), newRec(record.KindTopic, "areas/ops/topics/q4"), newRec(record.KindTopic, "topics/launch")} {
		putRecord(t, store, r)
	}
	write(t, filepath.Join(store, "projects", "legacy", "b.md"), "x")
	write(t, filepath.Join(store, "projects", "old-gadget", "c.md"), "x")
	return store, repoid.LoadCatalog(home, "", filepath.Join(home, "projects"))
}

func TestScopesOfRecordedFolders(t *testing.T) {
	store, cat := recordStore(t, false)
	e := NewEnv(store, "", cat, "box")
	for _, s := range []string{"projects/gadget", "projects/old-gadget", "areas/ops", "areas/ops/topics/q4", "topics/launch", "projects/legacy"} {
		if p := e.Scope(s); p != nil {
			t.Errorf("Scope(%s): %s", s, p.Message())
		}
	}
	for s, rule := range map[string]string{
		"areas/nope":           "names no area with a record",
		"topics/nope":          "names no topic with a record",
		"areas/ops/topics/new": "names no topic with a record",
		"areas/q4":             "names no area with a record", // a topic is no area
		"areas/ops/q4":         "not user, system",
	} {
		if p := e.Scope(s); p == nil || !strings.Contains(p.Rule, rule) {
			t.Errorf("Scope(%q) = %v, want %q", s, p, rule)
		}
	}
	// A repository record's name and alias are known repositories.
	if !e.KnownRepo("gadget") || !e.KnownRepo("gizmo") || e.KnownRepo("ops") {
		t.Error("KnownRepo of records")
	}
	if ps := e.Repos([]string{"gizmo"}); len(ps) != 0 {
		t.Errorf("an alias in applies_to.repos: %v", ps)
	}
	// The suppressed folder redirects to its winner.
	if w, ok := e.Redirect("projects/old-gadget"); !ok || w != "projects/gadget" {
		t.Errorf("redirect %s %v", w, ok)
	}
	if _, ok := e.Redirect("projects/gadget"); ok {
		t.Error("a live folder redirected")
	}
	f := &fact.Fact{Path: "projects/old-gadget/n.md", Meta: fact.Meta{Name: "n", Scope: "projects/old-gadget"}}
	if !e.RedirectFact(f) || f.Path != "projects/gadget/n.md" || f.Meta.Scope != "projects/gadget" {
		t.Errorf("RedirectFact %+v", f)
	}
	if e.UnknownRepo("projects/gadget") || e.UnknownRepo("projects/old-gadget") {
		t.Error("a recorded folder waits for a catalog")
	}
}

// The interim rule (a project folder holding facts is known) holds until
// the marker says records are complete; areas need the marker at all.
func TestInterimRuleBehindTheMarker(t *testing.T) {
	store, cat := recordStore(t, true)
	e := NewEnv(store, "", cat, "box")
	if p := e.Scope("projects/legacy"); p == nil {
		t.Error("records complete: a folder with facts and no record is still known")
	}
	if p := e.Scope("projects/gadget"); p != nil {
		t.Errorf("a recorded folder refused: %v", p)
	}
	if strings.Contains(strings.Join(e.RepoNames(), ","), "legacy") {
		t.Error("RepoNames lists an unrecorded folder")
	}
	// No marker: no areas, no topics.
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bare := filepath.Join(home, "store")
	putRecord(t, bare, newRec(record.KindArea, "areas/ops"))
	e = NewEnv(bare, "", repoid.LoadCatalog(home, "", ""), "box")
	if p := e.Scope("areas/ops"); p == nil || !strings.Contains(p.Rule, "format marker") {
		t.Errorf("an area in a store without the marker: %v", p)
	}
}
