package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/record"
)

func putRec(t *testing.T, root string, r *record.Record) {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, filepath.FromSlash(record.Rel(r.Folder)))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func srec(kind, folder, status string) *record.Record {
	return &record.Record{Kind: kind, Name: filepath.Base(folder), Description: "d " + folder, Status: status,
		Was:     []record.Entry{{Status: status, Date: "2026-10-21", By: record.ByOwner, Why: "x"}},
		Created: "2026-10-21", CreatedBy: record.ByOwner, Folder: folder}
}

// The index carries the records, and hooks deliver nothing from a dead or
// shipped folder while an explicit search still finds it.
func TestRecordsTableAndQuietFolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "projects/live/live-port.md", "live-port", "projects/live", "the frobnicator listens on port 7781", "frobnicator port", "applies_to:\n  commands:\n    - frob serve\n")
	write(t, root, "projects/done/done-port.md", "done-port", "projects/done", "the frobnicator used port 7000", "frobnicator port", "applies_to:\n  commands:\n    - frob serve\n")
	write(t, root, "areas/ops/topics/old/old-note.md", "old-note", "areas/ops/topics/old", "the frobnicator rota was weekly", "frobnicator", "")
	live, done := srec("repo", "projects/live", "active"), srec("repo", "projects/done", "shipped")
	live.Aliases = []string{"live-svc"}
	putRec(t, root, live)
	putRec(t, root, done)
	putRec(t, root, srec("area", "areas/ops", "dead"))
	putRec(t, root, srec("topic", "areas/ops/topics/old", "active"))
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ctx := context.Background()
	ix, err := Open(ctx, root, "rec1")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	recs, err := ix.Records(ctx)
	if err != nil || len(recs) != 4 || recs["projects/done"].Status != "shipped" || len(recs["projects/live"].Aliases) != 1 || recs["areas/ops"].Kind != "area" {
		t.Fatalf("records %v %v", recs, err)
	}
	scopes := []string{"projects/live", "projects/done", "areas/ops/topics/old"}
	names := func(hits []Hit) map[string]bool {
		m := map[string]bool{}
		for _, h := range hits {
			m[h.Name] = true
		}
		return m
	}
	explicit, err := ix.Query(ctx, Situation{Query: "frobnicator port", Scopes: scopes}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := names(explicit); !n["live-port"] || !n["done-port"] || !n["old-note"] {
		t.Fatalf("explicit search must find quiet folders: %v", n)
	}
	hook, err := ix.Query(ctx, Situation{Query: "frobnicator port", Scopes: scopes, RestrictRepos: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := names(hook); !n["live-port"] || n["done-port"] || n["old-note"] {
		t.Fatalf("hook delivery from a quiet folder: %v", n)
	}
	meta, err := ix.MetaFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := names(meta); !n["live-port"] || n["done-port"] {
		t.Fatalf("metadata facts from a quiet folder: %v", n)
	}
}
