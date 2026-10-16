package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestSelectionExportSynthetic(t *testing.T) {
	selectionOfflineEnv(t)
	root := t.TempDir()
	write := func(name, scope, extra string) {
		t.Helper()
		p := filepath.Join(root, scope, name+".md")
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("---\nname: %s\ndescription: quartz socket timeout CACHE_KEY_7\ntype: fact\nscope: %s\nsource:\n  kind: agent\ncreated: 2026-01-01\n%s---\nSynthetic body only.\n", name, scope, extra)
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("active", "projects/quartz", "origin: synthetic-origin\n")
	write("foreign", "projects/amber", "applies_to:\n  repos: [amber]\n")
	write("retired", "projects/quartz", "retired: 2026-01-02\n")
	write("superseded", "projects/quartz", "superseded_by: active\n")
	write("host-other", "machines/other", "applies_to:\n  hosts: [other]\n")
	write("inbox-note", "inbox/test@host", "")
	write("derived", "derived/quartz", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "empty"))
	ctx := context.Background()
	ix, err := search.Open(ctx, root, "synthetic-selection")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sit := search.Situation{Repo: "quartz", Host: "host"}
	for _, q := range []string{"quartz socket timeout", "CACHE_KEY_7", "", "thanks"} {
		req := selectionRequest{ID: "fixture", Query: q, Original: q, Ceiling: 6}
		got, err := selectionExport(ctx, ix, sit, req, root)
		if err != nil {
			t.Fatal(err)
		}
		var want []search.Hit
		if promptWorthy(q) {
			want, err = promptHits(ctx, ix, sit, q, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got.Control, want) {
			t.Fatalf("control mismatch for %q", q)
		}
		if q == "quartz socket timeout" && len(got.Candidates) == 0 {
			t.Fatal("fixture must exercise a nonempty candidate pool")
		}
		for _, h := range got.Candidates {
			if h.Name != "active" {
				t.Fatalf("ineligible candidate %s", h.Name)
			}
			if len(h.OriginIDs) != 1 || h.OriginIDs[0] != "synthetic-origin" {
				t.Fatal("missing origin")
			}
		}
	}
	t.Run("context-cannot-grant-foreign-scope", func(t *testing.T) {
		req := selectionRequest{ID: "switch", Original: "Fix that", Query: "Fix that\namber quartz socket timeout", Ceiling: 6}
		got, err := selectionExport(ctx, ix, sit, req, root)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range got.Candidates {
			if h.Name == "foreign" {
				t.Fatal("history granted foreign scope")
			}
		}
	})
	t.Run("control-ignores-global-hosted-utility", func(t *testing.T) {
		req := selectionRequest{ID: "local-control", Query: "quartz socket timeout", Original: "quartz socket timeout", Ceiling: 12}
		want, err := selectionExport(ctx, ix, sit, req, root)
		if err != nil {
			t.Fatal(err)
		}
		// No credentials. Even a hosted failure/fallback would write an audit
		// receipt, which would prove the exporter entered the wrong selector.
		audit := filepath.Join(t.TempDir(), "must-not-exist")
		t.Setenv("TACIT_UTILITY_PROVIDER", "jev")
		t.Setenv("TACIT_UTILITY_AUDIT_DIR", audit)
		got, err := selectionExport(ctx, ix, sit, req, root)
		if err != nil || !reflect.DeepEqual(got.Control, want.Control) {
			t.Fatalf("global hosted setting changed local control: %v", err)
		}
		if _, err := os.Stat(audit); !os.IsNotExist(err) {
			t.Fatal("exporter entered hosted utility/audit path")
		}
	})
}
